package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	namespaceTokenPrefix = "pcd_"
	rootTokenPrefix      = "prt_"
	tokenHashPrefix      = "sha256:"
	tokenHashHexLen      = 64
)

// Namespace name: ASCII lowercase letter first, letters/digits/dash inside,
// must end with letter or digit, 1..63 chars. Leading underscore is impossible
// by construction — the admin plane lives under /_/.
var namespacePattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

var (
	validSubPermissions   = map[string]struct{}{"read": {}, "write": {}, "delete": {}}
	validNamespacePerm    = map[string]struct{}{"read": {}, "write": {}, "delete": {}, "owner": {}}
	defaultOwnerPermSet   = []string{"owner"}
	subPermissionsOrdered = []string{"read", "write", "delete"}
)

// Namespace is the on-disk format of <data-dir>/namespaces/<name>.json.
type Namespace struct {
	Version      int       `json:"version"`
	Name         string    `json:"name"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	OwnerTokenID string    `json:"owner_token_id"`
	PublicRead   bool      `json:"public_read,omitempty"`
	Tokens       []Token   `json:"tokens"`
}

type Token struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	TokenHash   string    `json:"token_hash"`
	Permissions []string  `json:"permissions"`
	CreatedAt   time.Time `json:"created_at"`
}

// RootToken lives inside config.json. Whoever owns config.json owns the CDN.
type RootToken struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	TokenHash string    `json:"token_hash"`
	CreatedAt time.Time `json:"created_at"`
}

// CreatedToken is the one-shot response with plaintext token after a write
// operation (CLI or HTTP). The plaintext is never persisted.
type CreatedToken struct {
	Namespace   string   `json:"namespace,omitempty"`
	TokenID     string   `json:"token_id"`
	Token       string   `json:"token"`
	Permissions []string `json:"permissions,omitempty"`
	Owner       bool     `json:"owner,omitempty"`
}

type CreatedRootToken struct {
	TokenID string `json:"token_id"`
	Token   string `json:"token"`
	Name    string `json:"name"`
}

type TokenInfo struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Permissions []string  `json:"permissions"`
	CreatedAt   time.Time `json:"created_at"`
	Owner       bool      `json:"owner"`
}

type NamespaceInfo struct {
	Name         string    `json:"name"`
	OwnerTokenID string    `json:"owner_token_id"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	TokenCount   int       `json:"token_count"`
	PublicRead   bool      `json:"public_read"`
}

type RootTokenInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Actor identifies who passed authorization, for audit logging.
type Actor struct {
	Kind      string // "root", "owner", "sub", "anon"
	TokenID   string
	Namespace string
}

func (a Actor) String() string {
	switch a.Kind {
	case "root":
		return "root:" + a.TokenID
	case "owner":
		return "owner:" + a.TokenID
	case "sub":
		return "sub:" + a.TokenID
	case "anon":
		return "anon"
	}
	return "unknown"
}

const (
	permRead uint8 = 1 << iota
	permWrite
	permDelete
	permOwner
)

func permBit(name string) uint8 {
	switch name {
	case "read":
		return permRead
	case "write":
		return permWrite
	case "delete":
		return permDelete
	case "owner":
		return permOwner
	}
	return 0
}

func permsToMask(perms []string) uint8 {
	var m uint8
	for _, p := range perms {
		switch p {
		case "read":
			m |= permRead
		case "write":
			m |= permWrite
		case "delete":
			m |= permDelete
		case "owner":
			m |= permOwner | permRead | permWrite | permDelete
		}
	}
	return m
}

type compiledToken struct {
	id    string
	perms uint8
}

type compiledNS struct {
	ns     *Namespace
	tokens map[[32]byte]compiledToken
}

// Store is the authoritative in-memory authorization state for the process.
// Created once at startup from disk; all mutations go through this object,
// which atomically writes to <dir>/<ns>.json and updates the in-memory map
// under one Lock. Hot-path reads use RLock.
type Store struct {
	mu         sync.RWMutex
	dir        string
	namespaces map[string]*compiledNS
	roots      map[[32]byte]rootEntry
}

type rootEntry struct {
	id   string
	name string
}

// NewStore loads namespace files from dir (fail-fast on any error) and
// indexes root tokens for hot-path lookup. The caller passes a snapshot of
// root tokens from config.json — Store does not own config.json.
func NewStore(dir string, rootTokens []RootToken) (*Store, error) {
	s := &Store{
		dir:        dir,
		namespaces: make(map[string]*compiledNS),
		roots:      make(map[[32]byte]rootEntry, len(rootTokens)),
	}
	for i, rt := range rootTokens {
		if err := validateRootToken(rt); err != nil {
			return nil, fmt.Errorf("root_tokens[%d]: %w", i, err)
		}
		key, ok := decodeTokenHash(rt.TokenHash)
		if !ok {
			return nil, fmt.Errorf("root_tokens[%d]: invalid token_hash", i)
		}
		if _, exists := s.roots[key]; exists {
			return nil, fmt.Errorf("root_tokens[%d]: duplicate token_hash", i)
		}
		s.roots[key] = rootEntry{id: rt.ID, name: rt.Name}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("namespaces dir: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read namespaces dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		ns, err := loadNamespaceFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		expected := strings.TrimSuffix(e.Name(), ".json")
		if ns.Name != expected {
			return nil, fmt.Errorf("%s: namespace name %q does not match filename", path, ns.Name)
		}
		if _, dup := s.namespaces[ns.Name]; dup {
			return nil, fmt.Errorf("%s: duplicate namespace %q", path, ns.Name)
		}
		s.namespaces[ns.Name] = compileNamespace(ns)
	}
	return s, nil
}

func loadNamespaceFile(path string) (*Namespace, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ns Namespace
	if err := json.Unmarshal(data, &ns); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if err := validateNamespaceFile(&ns); err != nil {
		return nil, err
	}
	return &ns, nil
}

func validateNamespaceFile(ns *Namespace) error {
	if ns.Version != 1 {
		return fmt.Errorf("unsupported version %d", ns.Version)
	}
	if err := ValidateNamespaceName(ns.Name); err != nil {
		return err
	}
	if ns.OwnerTokenID == "" {
		return fmt.Errorf("missing owner_token_id")
	}
	seenIDs := make(map[string]struct{}, len(ns.Tokens))
	seenHashes := make(map[string]struct{}, len(ns.Tokens))
	ownerCount := 0
	for i, t := range ns.Tokens {
		if t.ID == "" {
			return fmt.Errorf("tokens[%d]: missing id", i)
		}
		if _, dup := seenIDs[t.ID]; dup {
			return fmt.Errorf("tokens[%d]: duplicate id %q", i, t.ID)
		}
		seenIDs[t.ID] = struct{}{}
		if t.Name == "" {
			return fmt.Errorf("tokens[%d]: missing name", i)
		}
		if _, ok := decodeTokenHash(t.TokenHash); !ok {
			return fmt.Errorf("tokens[%d]: invalid token_hash", i)
		}
		if _, dup := seenHashes[t.TokenHash]; dup {
			return fmt.Errorf("tokens[%d]: duplicate token_hash", i)
		}
		seenHashes[t.TokenHash] = struct{}{}
		if len(t.Permissions) == 0 {
			return fmt.Errorf("tokens[%d]: empty permissions", i)
		}
		hasOwner := false
		for _, p := range t.Permissions {
			if _, ok := validNamespacePerm[p]; !ok {
				return fmt.Errorf("tokens[%d]: unknown permission %q", i, p)
			}
			if p == "owner" {
				hasOwner = true
			}
		}
		if hasOwner {
			ownerCount++
			if t.ID != ns.OwnerTokenID {
				return fmt.Errorf("tokens[%d]: non-owner token has 'owner' permission", i)
			}
			if len(t.Permissions) != 1 {
				return fmt.Errorf("tokens[%d]: owner permission must not be combined", i)
			}
		}
	}
	if ownerCount != 1 {
		return fmt.Errorf("must have exactly one owner token, got %d", ownerCount)
	}
	if _, ok := seenIDs[ns.OwnerTokenID]; !ok {
		return fmt.Errorf("owner_token_id %q not present in tokens", ns.OwnerTokenID)
	}
	return nil
}

func compileNamespace(ns *Namespace) *compiledNS {
	cns := &compiledNS{ns: ns, tokens: make(map[[32]byte]compiledToken, len(ns.Tokens))}
	for _, t := range ns.Tokens {
		key, ok := decodeTokenHash(t.TokenHash)
		if !ok {
			continue
		}
		cns.tokens[key] = compiledToken{id: t.ID, perms: permsToMask(t.Permissions)}
	}
	return cns
}

// --- Hot-path API (RLock) ---

// Authorize checks token against a namespace for a specific object-plane
// permission ("read","write","delete"). Root tokens pass any check. Returns
// the actor that passed and a boolean.
func (s *Store) Authorize(namespace, token, permission string) (Actor, bool) {
	if token == "" {
		return Actor{Kind: "anon"}, false
	}
	bit := permBit(permission)
	if bit == 0 {
		return Actor{}, false
	}
	key := sha256.Sum256([]byte(token))
	s.mu.RLock()
	defer s.mu.RUnlock()
	if rt, ok := s.roots[key]; ok {
		return Actor{Kind: "root", TokenID: rt.id}, true
	}
	cns, ok := s.namespaces[namespace]
	if !ok {
		return Actor{}, false
	}
	t, ok := cns.tokens[key]
	if !ok {
		return Actor{}, false
	}
	if t.perms&bit == 0 {
		return Actor{}, false
	}
	kind := "sub"
	if t.id == cns.ns.OwnerTokenID {
		kind = "owner"
	}
	return Actor{Kind: kind, TokenID: t.id, Namespace: namespace}, true
}

// AuthorizeNamespaceAdmin authorizes admin operations inside a namespace
// (issue tokens, set-public, revoke). Only owner or root pass.
func (s *Store) AuthorizeNamespaceAdmin(namespace, token string) (Actor, bool) {
	if token == "" {
		return Actor{Kind: "anon"}, false
	}
	key := sha256.Sum256([]byte(token))
	s.mu.RLock()
	defer s.mu.RUnlock()
	if rt, ok := s.roots[key]; ok {
		return Actor{Kind: "root", TokenID: rt.id}, true
	}
	cns, ok := s.namespaces[namespace]
	if !ok {
		return Actor{}, false
	}
	t, ok := cns.tokens[key]
	if !ok {
		return Actor{}, false
	}
	if t.perms&permOwner == 0 {
		return Actor{}, false
	}
	return Actor{Kind: "owner", TokenID: t.id, Namespace: namespace}, true
}

// AuthorizeRoot validates token against root tokens only.
func (s *Store) AuthorizeRoot(token string) (Actor, bool) {
	if token == "" {
		return Actor{Kind: "anon"}, false
	}
	key := sha256.Sum256([]byte(token))
	s.mu.RLock()
	defer s.mu.RUnlock()
	if rt, ok := s.roots[key]; ok {
		return Actor{Kind: "root", TokenID: rt.id}, true
	}
	return Actor{}, false
}

func (s *Store) HasNamespace(namespace string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.namespaces[namespace]
	return ok
}

func (s *Store) IsPublicRead(namespace string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cns, ok := s.namespaces[namespace]
	if !ok {
		return false
	}
	return cns.ns.PublicRead
}

// --- Admin API (Lock) ---

// CreateNamespace creates a new namespace and returns its owner token.
// The plaintext token is only returned here, never persisted.
func (s *Store) CreateNamespace(name string) (*CreatedToken, error) {
	if err := ValidateNamespaceName(name); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.namespaces[name]; ok {
		return nil, ErrNamespaceExists
	}
	plain, tok, err := newToken("owner", defaultOwnerPermSet)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	ns := &Namespace{
		Version:      1,
		Name:         name,
		CreatedAt:    now,
		UpdatedAt:    now,
		OwnerTokenID: tok.ID,
		Tokens:       []Token{tok},
	}
	if err := s.persistLocked(ns); err != nil {
		return nil, err
	}
	s.namespaces[name] = compileNamespace(ns)
	return &CreatedToken{
		Namespace:   name,
		TokenID:     tok.ID,
		Token:       plain,
		Permissions: append([]string(nil), tok.Permissions...),
		Owner:       true,
	}, nil
}

// DeleteNamespace removes the namespace file and in-memory entry. The caller
// is responsible for removing the namespace's aliases directory; blobs are
// reclaimed by GC.
func (s *Store) DeleteNamespace(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.namespaces[name]; !ok {
		return ErrNamespaceNotFound
	}
	path := s.namespacePath(name)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := syncDir(s.dir); err != nil {
		return err
	}
	delete(s.namespaces, name)
	return nil
}

// RotateOwner issues a new owner token, replacing the previous one. The new
// plaintext is returned once.
func (s *Store) RotateOwner(name string) (*CreatedToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cns, ok := s.namespaces[name]
	if !ok {
		return nil, ErrNamespaceNotFound
	}
	plain, tok, err := newToken("owner", defaultOwnerPermSet)
	if err != nil {
		return nil, err
	}
	newTokens := make([]Token, 0, len(cns.ns.Tokens))
	for _, t := range cns.ns.Tokens {
		if t.ID == cns.ns.OwnerTokenID {
			continue
		}
		newTokens = append(newTokens, t)
	}
	newTokens = append(newTokens, tok)
	updated := *cns.ns
	updated.Tokens = newTokens
	updated.OwnerTokenID = tok.ID
	updated.UpdatedAt = time.Now().UTC()
	if err := s.persistLocked(&updated); err != nil {
		return nil, err
	}
	s.namespaces[name] = compileNamespace(&updated)
	return &CreatedToken{
		Namespace:   name,
		TokenID:     tok.ID,
		Token:       plain,
		Permissions: append([]string(nil), tok.Permissions...),
		Owner:       true,
	}, nil
}

// CreateToken issues a sub-token. "owner" permission is rejected.
func (s *Store) CreateToken(namespace, name string, permissions []string) (*CreatedToken, error) {
	if name == "" {
		return nil, fmt.Errorf("missing token name")
	}
	cleaned, err := validateSubPermissions(permissions)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cns, ok := s.namespaces[namespace]
	if !ok {
		return nil, ErrNamespaceNotFound
	}
	plain, tok, err := newToken(name, cleaned)
	if err != nil {
		return nil, err
	}
	updated := *cns.ns
	updated.Tokens = append(append([]Token(nil), cns.ns.Tokens...), tok)
	updated.UpdatedAt = time.Now().UTC()
	if err := s.persistLocked(&updated); err != nil {
		return nil, err
	}
	s.namespaces[namespace] = compileNamespace(&updated)
	return &CreatedToken{
		Namespace:   namespace,
		TokenID:     tok.ID,
		Token:       plain,
		Permissions: append([]string(nil), tok.Permissions...),
	}, nil
}

// RevokeToken removes a sub-token. Owner token cannot be removed this way;
// use RotateOwner instead.
func (s *Store) RevokeToken(namespace, tokenID string) error {
	if tokenID == "" {
		return fmt.Errorf("missing token id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cns, ok := s.namespaces[namespace]
	if !ok {
		return ErrNamespaceNotFound
	}
	if tokenID == cns.ns.OwnerTokenID {
		return ErrCannotRevokeOwner
	}
	updated := *cns.ns
	idx := -1
	for i, t := range cns.ns.Tokens {
		if t.ID == tokenID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ErrTokenNotFound
	}
	updated.Tokens = append(append([]Token(nil), cns.ns.Tokens[:idx]...), cns.ns.Tokens[idx+1:]...)
	updated.UpdatedAt = time.Now().UTC()
	if err := s.persistLocked(&updated); err != nil {
		return err
	}
	s.namespaces[namespace] = compileNamespace(&updated)
	return nil
}

// SetPublicRead flips the namespace public-read flag.
func (s *Store) SetPublicRead(namespace string, public bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cns, ok := s.namespaces[namespace]
	if !ok {
		return ErrNamespaceNotFound
	}
	if cns.ns.PublicRead == public {
		return nil
	}
	updated := *cns.ns
	updated.Tokens = append([]Token(nil), cns.ns.Tokens...)
	updated.PublicRead = public
	updated.UpdatedAt = time.Now().UTC()
	if err := s.persistLocked(&updated); err != nil {
		return err
	}
	s.namespaces[namespace] = compileNamespace(&updated)
	return nil
}

// ListNamespaces returns metadata for all namespaces in deterministic order.
func (s *Store) ListNamespaces() []NamespaceInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.namespaces))
	for name := range s.namespaces {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]NamespaceInfo, 0, len(names))
	for _, name := range names {
		ns := s.namespaces[name].ns
		out = append(out, NamespaceInfo{
			Name:         ns.Name,
			OwnerTokenID: ns.OwnerTokenID,
			CreatedAt:    ns.CreatedAt,
			UpdatedAt:    ns.UpdatedAt,
			TokenCount:   len(ns.Tokens),
			PublicRead:   ns.PublicRead,
		})
	}
	return out
}

// ListTokens returns metadata for tokens in a namespace.
func (s *Store) ListTokens(namespace string) ([]TokenInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cns, ok := s.namespaces[namespace]
	if !ok {
		return nil, ErrNamespaceNotFound
	}
	out := make([]TokenInfo, 0, len(cns.ns.Tokens))
	for _, t := range cns.ns.Tokens {
		out = append(out, TokenInfo{
			ID:          t.ID,
			Name:        t.Name,
			Permissions: append([]string(nil), t.Permissions...),
			CreatedAt:   t.CreatedAt,
			Owner:       t.ID == cns.ns.OwnerTokenID,
		})
	}
	return out, nil
}

// --- Disk I/O ---

func (s *Store) namespacePath(name string) string {
	return filepath.Join(s.dir, name+".json")
}

func (s *Store) persistLocked(ns *Namespace) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	path := s.namespacePath(ns.Name)
	tmp, err := os.CreateTemp(s.dir, ".ns-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	keepTmp := false
	defer func() {
		if !keepTmp {
			_ = os.Remove(tmpName)
		}
	}()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(ns); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	keepTmp = true
	return syncDir(s.dir)
}

// --- Helpers ---

func newToken(name string, permissions []string) (string, Token, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", Token{}, err
	}
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return "", Token{}, err
	}
	secret := namespaceTokenPrefix + base64.RawURLEncoding.EncodeToString(tokenBytes)
	id := base64.RawURLEncoding.EncodeToString(idBytes)
	return secret, Token{
		ID:          id,
		Name:        name,
		TokenHash:   HashToken(secret),
		Permissions: append([]string(nil), permissions...),
		CreatedAt:   time.Now().UTC(),
	}, nil
}

// NewRootToken generates a fresh root token. The plaintext is returned once;
// only the RootToken metadata (with hashed value) should be persisted.
func NewRootToken(name string) (string, RootToken, error) {
	if name == "" {
		return "", RootToken{}, fmt.Errorf("missing root token name")
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", RootToken{}, err
	}
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return "", RootToken{}, err
	}
	secret := rootTokenPrefix + base64.RawURLEncoding.EncodeToString(tokenBytes)
	id := base64.RawURLEncoding.EncodeToString(idBytes)
	return secret, RootToken{
		ID:        id,
		Name:      name,
		TokenHash: HashToken(secret),
		CreatedAt: time.Now().UTC(),
	}, nil
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return tokenHashPrefix + hex.EncodeToString(sum[:])
}

func decodeTokenHash(s string) ([32]byte, bool) {
	if len(s) != len(tokenHashPrefix)+tokenHashHexLen || s[:len(tokenHashPrefix)] != tokenHashPrefix {
		return [32]byte{}, false
	}
	var out [32]byte
	for i := 0; i < 32; i++ {
		hi, ok1 := hexNibble(s[len(tokenHashPrefix)+2*i])
		lo, ok2 := hexNibble(s[len(tokenHashPrefix)+2*i+1])
		if !ok1 || !ok2 {
			return [32]byte{}, false
		}
		out[i] = hi<<4 | lo
	}
	return out, true
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// ValidateNamespaceName enforces ASCII lowercase letter-led, dash-allowed
// names of length 1..63. Leading underscore is structurally impossible.
func ValidateNamespaceName(name string) error {
	if !namespacePattern.MatchString(name) {
		return ErrInvalidNamespaceName
	}
	return nil
}

func validateSubPermissions(perms []string) ([]string, error) {
	if len(perms) == 0 {
		return nil, fmt.Errorf("missing permissions")
	}
	seen := make(map[string]struct{}, len(perms))
	for _, p := range perms {
		if p == "owner" {
			return nil, fmt.Errorf("sub-token cannot have 'owner' permission")
		}
		if _, ok := validSubPermissions[p]; !ok {
			return nil, fmt.Errorf("unknown permission %q", p)
		}
		seen[p] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for _, p := range subPermissionsOrdered {
		if _, ok := seen[p]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func validateRootToken(rt RootToken) error {
	if rt.ID == "" {
		return fmt.Errorf("missing id")
	}
	if rt.Name == "" {
		return fmt.Errorf("missing name")
	}
	if _, ok := decodeTokenHash(rt.TokenHash); !ok {
		return fmt.Errorf("invalid token_hash")
	}
	return nil
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// --- Error sentinels ---

var (
	ErrNamespaceExists      = errors.New("namespace already exists")
	ErrNamespaceNotFound    = errors.New("namespace not found")
	ErrTokenNotFound        = errors.New("token not found")
	ErrCannotRevokeOwner    = errors.New("cannot revoke owner token; use rotate-owner")
	ErrInvalidNamespaceName = errors.New("invalid namespace name")
)
