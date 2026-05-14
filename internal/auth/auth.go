package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync/atomic"
	"time"
)

var namespacePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

type File struct {
	Version    int                   `json:"version"`
	UpdatedAt  time.Time             `json:"updated_at"`
	Namespaces map[string]*Namespace `json:"namespaces"`
}

type Namespace struct {
	Name         string    `json:"name"`
	OwnerTokenID string    `json:"owner_token_id"`
	CreatedAt    time.Time `json:"created_at"`
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

type CreatedToken struct {
	Namespace   string   `json:"namespace"`
	TokenID     string   `json:"token_id"`
	Token       string   `json:"token"`
	Permissions []string `json:"permissions"`
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
	TokenCount   int       `json:"token_count"`
	PublicRead   bool      `json:"public_read,omitempty"`
}

func (f *File) Authorize(namespace, token, permission string) bool {
	if f == nil || token == "" {
		return false
	}
	ns, ok := f.Namespaces[namespace]
	if !ok {
		return false
	}
	tokenHash := HashToken(token)
	for _, stored := range ns.Tokens {
		if subtle.ConstantTimeCompare([]byte(stored.TokenHash), []byte(tokenHash)) != 1 {
			continue
		}
		return hasPermission(stored.Permissions, permission)
	}
	return false
}

func (f *File) HasNamespace(namespace string) bool {
	if f == nil {
		return false
	}
	_, ok := f.Namespaces[namespace]
	return ok
}

func LoadFile(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return emptyFile(), nil
	}
	if err != nil {
		return nil, err
	}

	var file File
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	if file.Version == 0 {
		file.Version = 1
	}
	if file.Namespaces == nil {
		file.Namespaces = make(map[string]*Namespace)
	}
	return &file, nil
}

func SaveFile(path string, file *File) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file.Version = 1
	file.UpdatedAt = time.Now().UTC()

	tmp, err := os.CreateTemp(filepath.Dir(path), ".auth-*")
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
	if err := enc.Encode(file); err != nil {
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
	return syncDir(filepath.Dir(path))
}

func CreateNamespace(file *File, namespace string) (*CreatedToken, error) {
	if err := validateNamespace(namespace); err != nil {
		return nil, err
	}
	if _, ok := file.Namespaces[namespace]; ok {
		return nil, fmt.Errorf("namespace already exists")
	}

	token, stored, err := newToken("owner", []string{"owner", "read", "write", "delete", "admin"})
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	file.Namespaces[namespace] = &Namespace{
		Name:         namespace,
		OwnerTokenID: stored.ID,
		CreatedAt:    now,
		Tokens:       []Token{stored},
	}
	return &CreatedToken{
		Namespace:   namespace,
		TokenID:     stored.ID,
		Token:       token,
		Permissions: stored.Permissions,
	}, nil
}

func CreateToken(file *File, namespace, name string, permissions []string) (*CreatedToken, error) {
	if err := validateNamespace(namespace); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, fmt.Errorf("missing token name")
	}
	if len(permissions) == 0 {
		return nil, fmt.Errorf("missing permissions")
	}

	ns, ok := file.Namespaces[namespace]
	if !ok {
		return nil, fmt.Errorf("namespace not found")
	}
	token, stored, err := newToken(name, permissions)
	if err != nil {
		return nil, err
	}
	ns.Tokens = append(ns.Tokens, stored)
	return &CreatedToken{
		Namespace:   namespace,
		TokenID:     stored.ID,
		Token:       token,
		Permissions: stored.Permissions,
	}, nil
}

func ListTokens(file *File, namespace string) ([]TokenInfo, error) {
	if err := validateNamespace(namespace); err != nil {
		return nil, err
	}
	ns, ok := file.Namespaces[namespace]
	if !ok {
		return nil, fmt.Errorf("namespace not found")
	}
	tokens := make([]TokenInfo, 0, len(ns.Tokens))
	for _, token := range ns.Tokens {
		tokens = append(tokens, TokenInfo{
			ID:          token.ID,
			Name:        token.Name,
			Permissions: token.Permissions,
			CreatedAt:   token.CreatedAt,
			Owner:       token.ID == ns.OwnerTokenID,
		})
	}
	return tokens, nil
}

func ListNamespaces(file *File) []NamespaceInfo {
	if file == nil {
		return nil
	}
	names := make([]string, 0, len(file.Namespaces))
	for name := range file.Namespaces {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]NamespaceInfo, 0, len(names))
	for _, name := range names {
		ns := file.Namespaces[name]
		out = append(out, NamespaceInfo{
			Name:         ns.Name,
			OwnerTokenID: ns.OwnerTokenID,
			CreatedAt:    ns.CreatedAt,
			TokenCount:   len(ns.Tokens),
			PublicRead:   ns.PublicRead,
		})
	}
	return out
}

func ShowNamespace(file *File, namespace string) (*NamespaceInfo, []TokenInfo, error) {
	if err := validateNamespace(namespace); err != nil {
		return nil, nil, err
	}
	ns, ok := file.Namespaces[namespace]
	if !ok {
		return nil, nil, fmt.Errorf("namespace not found")
	}
	info := &NamespaceInfo{
		Name:         ns.Name,
		OwnerTokenID: ns.OwnerTokenID,
		CreatedAt:    ns.CreatedAt,
		TokenCount:   len(ns.Tokens),
		PublicRead:   ns.PublicRead,
	}
	tokens, err := ListTokens(file, namespace)
	if err != nil {
		return nil, nil, err
	}
	return info, tokens, nil
}

func SetNamespacePublicRead(file *File, namespace string, public bool) error {
	if err := validateNamespace(namespace); err != nil {
		return err
	}
	ns, ok := file.Namespaces[namespace]
	if !ok {
		return fmt.Errorf("namespace not found")
	}
	ns.PublicRead = public
	return nil
}

func DeleteNamespace(file *File, namespace string) error {
	if err := validateNamespace(namespace); err != nil {
		return err
	}
	if _, ok := file.Namespaces[namespace]; !ok {
		return fmt.Errorf("namespace not found")
	}
	delete(file.Namespaces, namespace)
	return nil
}

func RevokeToken(file *File, namespace, tokenID string) error {
	if err := validateNamespace(namespace); err != nil {
		return err
	}
	ns, ok := file.Namespaces[namespace]
	if !ok {
		return fmt.Errorf("namespace not found")
	}
	if tokenID == "" {
		return fmt.Errorf("missing token id")
	}
	if tokenID == ns.OwnerTokenID {
		return fmt.Errorf("cannot revoke owner token")
	}

	for i, token := range ns.Tokens {
		if token.ID != tokenID {
			continue
		}
		ns.Tokens = append(ns.Tokens[:i], ns.Tokens[i+1:]...)
		return nil
	}
	return fmt.Errorf("token not found")
}

func emptyFile() *File {
	return &File{
		Version:    1,
		Namespaces: make(map[string]*Namespace),
	}
}

func newToken(name string, permissions []string) (string, Token, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", Token{}, err
	}
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return "", Token{}, err
	}

	secret := "pcd_" + base64.RawURLEncoding.EncodeToString(tokenBytes)
	id := base64.RawURLEncoding.EncodeToString(idBytes)
	return secret, Token{
		ID:          id,
		Name:        name,
		TokenHash:   HashToken(secret),
		Permissions: dedupeStrings(permissions),
		CreatedAt:   time.Now().UTC(),
	}, nil
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validateNamespace(namespace string) error {
	if !namespacePattern.MatchString(namespace) {
		return fmt.Errorf("invalid namespace")
	}
	return nil
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func hasPermission(permissions []string, required string) bool {
	for _, permission := range permissions {
		switch permission {
		case required, "owner", "admin", "*":
			return true
		}
	}
	return false
}

// compiled is a read-optimized view of *File. The token lookup table is
// keyed by token hash string and built once per reload, so Authorize is
// O(1) instead of a linear scan.
type compiled struct {
	raw        *File
	namespaces map[string]*compiledNS
}

type compiledNS struct {
	ns     *Namespace
	tokens map[[32]byte]compiledToken
}

type compiledToken struct {
	perms uint8
}

const (
	permRead uint8 = 1 << iota
	permWrite
	permDelete
	permAdmin
	permOwner
)

func compilePermissions(perms []string) uint8 {
	var mask uint8
	for _, p := range perms {
		switch p {
		case "read":
			mask |= permRead
		case "write":
			mask |= permWrite
		case "delete":
			mask |= permDelete
		case "admin":
			mask |= permAdmin | permRead | permWrite | permDelete
		case "owner":
			mask |= permOwner | permAdmin | permRead | permWrite | permDelete
		case "*":
			mask |= permOwner | permAdmin | permRead | permWrite | permDelete
		}
	}
	return mask
}

func permBit(name string) uint8 {
	switch name {
	case "read":
		return permRead
	case "write":
		return permWrite
	case "delete":
		return permDelete
	case "admin":
		return permAdmin
	case "owner":
		return permOwner
	}
	return 0
}

func compileFile(f *File) *compiled {
	if f == nil {
		return &compiled{raw: emptyFile(), namespaces: map[string]*compiledNS{}}
	}
	out := &compiled{raw: f, namespaces: make(map[string]*compiledNS, len(f.Namespaces))}
	for name, ns := range f.Namespaces {
		cns := &compiledNS{ns: ns, tokens: make(map[[32]byte]compiledToken, len(ns.Tokens))}
		for _, t := range ns.Tokens {
			key, ok := decodeTokenHashKey(t.TokenHash)
			if !ok {
				continue
			}
			cns.tokens[key] = compiledToken{perms: compilePermissions(t.Permissions)}
		}
		out.namespaces[name] = cns
	}
	return out
}

func decodeTokenHashKey(s string) ([32]byte, bool) {
	const prefix = "sha256:"
	if len(s) != len(prefix)+64 || s[:len(prefix)] != prefix {
		return [32]byte{}, false
	}
	var out [32]byte
	for i := 0; i < 32; i++ {
		hi, ok1 := hexNibble(s[len(prefix)+2*i])
		lo, ok2 := hexNibble(s[len(prefix)+2*i+1])
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

func (c *compiled) authorize(namespace, token, permission string) bool {
	if c == nil || token == "" {
		return false
	}
	cns, ok := c.namespaces[namespace]
	if !ok {
		return false
	}
	bit := permBit(permission)
	if bit == 0 {
		return false
	}
	key := sha256.Sum256([]byte(token))
	stored, ok := cns.tokens[key]
	if !ok {
		return false
	}
	return stored.perms&bit != 0
}

func (c *compiled) hasNamespace(namespace string) bool {
	if c == nil {
		return false
	}
	_, ok := c.namespaces[namespace]
	return ok
}

func (c *compiled) isPublicRead(namespace string) bool {
	if c == nil {
		return false
	}
	cns, ok := c.namespaces[namespace]
	if !ok {
		return false
	}
	return cns.ns.PublicRead
}

type Reloader struct {
	path    string
	logger  *slog.Logger
	current atomic.Pointer[compiled]
	mtime   atomic.Int64
}

func NewReloader(path string, logger *slog.Logger) (*Reloader, error) {
	r := &Reloader{path: path, logger: logger}
	if err := r.reload(true); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Reloader) Get() *File {
	c := r.current.Load()
	if c == nil {
		return emptyFile()
	}
	return c.raw
}

func (r *Reloader) HasNamespace(namespace string) bool {
	return r.current.Load().hasNamespace(namespace)
}

func (r *Reloader) Authorize(namespace, token, permission string) bool {
	return r.current.Load().authorize(namespace, token, permission)
}

func (r *Reloader) IsPublicRead(namespace string) bool {
	return r.current.Load().isPublicRead(namespace)
}

func (r *Reloader) MaybeReload() (bool, error) {
	info, err := os.Stat(r.path)
	if errors.Is(err, os.ErrNotExist) {
		cur := r.current.Load()
		if cur != nil && len(cur.namespaces) > 0 {
			return false, fmt.Errorf("auth file %s vanished, keeping last good", r.path)
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	cur := info.ModTime().UnixNano()
	if r.mtime.Load() == cur {
		return false, nil
	}
	if err := r.reload(false); err != nil {
		return false, err
	}
	return true, nil
}

func (r *Reloader) ForceReload() error {
	return r.reload(false)
}

func (r *Reloader) reload(initial bool) error {
	loaded, err := LoadFile(r.path)
	if err != nil {
		return err
	}
	if !initial {
		prev := r.current.Load()
		if prev != nil && len(prev.namespaces) > 0 && len(loaded.Namespaces) == 0 {
			return fmt.Errorf("refusing to replace %d namespaces with empty auth file", len(prev.namespaces))
		}
	}
	r.current.Store(compileFile(loaded))
	if info, err := os.Stat(r.path); err == nil {
		r.mtime.Store(info.ModTime().UnixNano())
	} else {
		r.mtime.Store(0)
	}
	if r.logger != nil && !initial {
		r.logger.Info("auth reloaded", "namespaces", len(loaded.Namespaces))
	}
	return nil
}

func (r *Reloader) Watch(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := r.MaybeReload(); err != nil && r.logger != nil {
				r.logger.Warn("auth reload failed", "err", err)
			}
		}
	}
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
