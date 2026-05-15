package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Store struct {
	dataDir     string
	blobsRoot   string
	aliasesRoot string
	tmpRoot     string
}

var sha256Pool = sync.Pool{
	New: func() any { return sha256.New() },
}

func getHasher() hash.Hash {
	h := sha256Pool.Get().(hash.Hash)
	h.Reset()
	return h
}

func putHasher(h hash.Hash) {
	sha256Pool.Put(h)
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func isValidNamespace(s string) bool {
	n := len(s)
	if n == 0 || n > 63 {
		return false
	}
	for i := 0; i < n; i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-'
		if !ok {
			return false
		}
	}
	if s[0] == '-' || s[n-1] == '-' {
		return false
	}
	return true
}

type Object struct {
	Namespace   string    `json:"namespace"`
	Path        string    `json:"path"`
	Hash        string    `json:"hash"`
	Algo        string    `json:"algo"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	CreatedAt   time.Time `json:"created_at"`
	BlobPath    string    `json:"-"`
}

type PutOptions struct {
	Namespace   string
	Path        string
	ContentType string
}

func New(dataDir string) *Store {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		abs = dataDir
	}
	return &Store{
		dataDir:     abs,
		blobsRoot:   filepath.Join(abs, "blobs", "sha256"),
		aliasesRoot: filepath.Join(abs, "aliases"),
		tmpRoot:     filepath.Join(abs, "tmp", "uploads"),
	}
}

func (s *Store) GetObject(namespace, objectPath string) (*Object, error) {
	if !isValidNamespace(namespace) {
		return nil, errInvalidNamespace
	}
	objectPath, err := cleanObjectPath(objectPath)
	if err != nil {
		return nil, err
	}

	aliasPath, err := s.safeAliasPath(namespace, objectPath)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(aliasPath)
	if err != nil {
		return nil, err
	}

	var obj Object
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	if obj.Namespace != namespace || obj.Path != objectPath {
		return nil, fmt.Errorf("alias metadata mismatch")
	}
	if obj.Algo != "sha256" || !isHex64(obj.Hash) {
		return nil, fmt.Errorf("invalid object hash")
	}
	obj.BlobPath = s.blobPath(obj.Hash)
	return &obj, nil
}

// HasAlias reports whether an alias file exists for the given namespace+path
// without parsing it. Cheap stat-only check, useful for index-file lookup.
func (s *Store) HasAlias(namespace, objectPath string) bool {
	if !isValidNamespace(namespace) {
		return false
	}
	cleaned, err := cleanObjectPath(objectPath)
	if err != nil {
		return false
	}
	aliasPath, err := s.safeAliasPath(namespace, cleaned)
	if err != nil {
		return false
	}
	info, err := os.Stat(aliasPath)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}

// DeleteNamespaceAliases removes <aliases-root>/<namespace> recursively. Blobs
// are intentionally left behind for GC to reclaim. Missing dir is not an error.
func (s *Store) DeleteNamespaceAliases(namespace string) error {
	if !isValidNamespace(namespace) {
		return errInvalidNamespace
	}
	dir := filepath.Join(s.aliasesRoot, namespace)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := syncDir(s.aliasesRoot); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) DeleteObject(namespace, objectPath string) (*Object, error) {
	obj, err := s.GetObject(namespace, objectPath)
	if err != nil {
		return nil, err
	}
	aliasPath, err := s.safeAliasPath(obj.Namespace, obj.Path)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(aliasPath); err != nil {
		return nil, err
	}
	_ = syncDir(filepath.Dir(aliasPath))
	return obj, nil
}

// WalkObjects streams Object metadata to fn without buffering the full
// listing in memory. fn must not retain the *Object after returning; the
// pointer is reused for each call. Returning a non-nil error stops the walk.
func (s *Store) WalkObjects(namespace, prefix string, fn func(*Object) error) error {
	if !isValidNamespace(namespace) {
		return errInvalidNamespace
	}
	if prefix != "" {
		if _, err := cleanObjectPath(prefix); err != nil {
			return err
		}
	}
	prefix = strings.TrimSuffix(prefix, "/")
	root := filepath.Join(s.aliasesRoot, namespace)
	return filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(p, ".json") {
			return nil
		}
		name := d.Name()
		if len(name) > 0 && name[0] == '.' {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var obj Object
		if err := json.Unmarshal(data, &obj); err != nil {
			return nil
		}
		if obj.Namespace != namespace {
			return nil
		}
		if prefix != "" && !strings.HasPrefix(obj.Path, prefix) {
			return nil
		}
		obj.BlobPath = s.blobPath(obj.Hash)
		return fn(&obj)
	})
}

// ListObjects collects all matching objects into a slice; for big namespaces
// prefer WalkObjects which streams.
func (s *Store) ListObjects(namespace, prefix string) ([]*Object, error) {
	var objects []*Object
	if err := s.WalkObjects(namespace, prefix, func(obj *Object) error {
		copy := *obj
		objects = append(objects, &copy)
		return nil
	}); err != nil {
		return nil, err
	}
	return objects, nil
}

// GC sweeps orphan blob files. To avoid racing with a concurrent PutObject
// (which writes blob+alias non-atomically), files newer than gracePeriod
// are skipped. Pass 0 to disable the protection (tests only).
func (s *Store) GC(gracePeriod time.Duration) (deleted int, freedBytes int64, err error) {
	cutoff := time.Now().Add(-gracePeriod)
	referenced := make(map[string]struct{})
	err = filepath.WalkDir(s.aliasesRoot, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(p, ".json") {
			return nil
		}
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		var obj Object
		if json.Unmarshal(data, &obj) != nil {
			return nil
		}
		if !isHex64(obj.Hash) {
			return nil
		}
		referenced[obj.Hash] = struct{}{}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}

	err = filepath.WalkDir(s.blobsRoot, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !isHex64(name) {
			return nil
		}
		if _, ok := referenced[name]; ok {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return statErr
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		if removeErr := os.Remove(p); removeErr != nil {
			return removeErr
		}
		deleted++
		freedBytes += info.Size()
		return nil
	})
	if err != nil {
		return deleted, freedBytes, err
	}
	return deleted, freedBytes, nil
}

func (s *Store) PutObject(ctx context.Context, r io.Reader, opts PutOptions) (*Object, error) {
	if !isValidNamespace(opts.Namespace) {
		return nil, errInvalidNamespace
	}
	namespace := opts.Namespace
	objectPath, err := cleanObjectPath(opts.Path)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(s.tmpRoot, 0o755); err != nil {
		return nil, err
	}

	tmp, err := os.CreateTemp(s.tmpRoot, "upload-*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	keepTmp := false
	defer func() {
		if !keepTmp {
			_ = os.Remove(tmpName)
		}
	}()

	hasher := getHasher()
	defer putHasher(hasher)
	src := io.Reader(r)
	if ctx != nil && ctx.Done() != nil {
		src = readerWithContext{ctx: ctx, r: r}
	}
	written, err := io.Copy(tmp, io.TeeReader(src, hasher))
	if err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}

	hashSum := hex.EncodeToString(hasher.Sum(nil))
	blobPath := s.blobPath(hashSum)
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
		return nil, err
	}

	if _, err := os.Stat(blobPath); err == nil {
		_ = os.Remove(tmpName)
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(tmpName, blobPath); err != nil {
			return nil, err
		}
		keepTmp = true
		if err := syncDir(filepath.Dir(blobPath)); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}

	contentType := opts.ContentType
	if contentType == "" {
		contentType = mime.TypeByExtension(path.Ext(objectPath))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	obj := &Object{
		Namespace:   namespace,
		Path:        objectPath,
		Hash:        hashSum,
		Algo:        "sha256",
		Size:        written,
		ContentType: contentType,
		CreatedAt:   time.Now().UTC(),
		BlobPath:    blobPath,
	}
	if err := s.writeAlias(obj); err != nil {
		return nil, err
	}

	return obj, nil
}

func (s *Store) blobPath(hash string) string {
	return filepath.Join(s.blobsRoot, hash[:2], hash[2:4], hash)
}

func (s *Store) aliasPath(namespace, objectPath string) string {
	rel := strings.TrimPrefix(objectPath, "/") + ".json"
	return filepath.Join(s.aliasesRoot, namespace, filepath.FromSlash(rel))
}

// safeAliasPath returns the alias path after defense-in-depth check that
// the resolved path stays under the namespace directory. cleanObjectPath
// already rejects "..", null byte and backslash, so this check is a belt
// on top of the suspenders.
func (s *Store) safeAliasPath(namespace, objectPath string) (string, error) {
	aliasPath := s.aliasPath(namespace, objectPath)
	root := filepath.Join(s.aliasesRoot, namespace) + string(filepath.Separator)
	if !strings.HasPrefix(aliasPath+string(filepath.Separator), root) {
		return "", fmt.Errorf("alias path escapes data dir")
	}
	return aliasPath, nil
}

func (s *Store) writeAlias(obj *Object) error {
	aliasPath, err := s.safeAliasPath(obj.Namespace, obj.Path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(aliasPath), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(aliasPath), ".alias-*")
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
	if err := enc.Encode(obj); err != nil {
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
	if err := os.Rename(tmpName, aliasPath); err != nil {
		return err
	}
	keepTmp = true
	return syncDir(filepath.Dir(aliasPath))
}

var errInvalidNamespace = fmt.Errorf("invalid namespace")

func cleanObjectPath(objectPath string) (string, error) {
	if objectPath == "" {
		return "", fmt.Errorf("missing path")
	}
	for i := 0; i < len(objectPath); i++ {
		c := objectPath[i]
		if c == 0 || c == '\\' {
			return "", fmt.Errorf("invalid path")
		}
	}
	raw := strings.TrimPrefix(objectPath, "/")
	// Single pass to detect ".." segments without strings.Split allocation.
	start := 0
	for i := 0; i <= len(raw); i++ {
		if i == len(raw) || raw[i] == '/' {
			if i-start == 2 && raw[start] == '.' && raw[start+1] == '.' {
				return "", fmt.Errorf("invalid path")
			}
			start = i + 1
		}
	}
	cleaned := path.Clean("/" + raw)
	if cleaned == "/" {
		return "", fmt.Errorf("invalid path")
	}
	return cleaned, nil
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

type readerWithContext struct {
	ctx context.Context
	r   io.Reader
}

func (r readerWithContext) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.r.Read(p)
	}
}
