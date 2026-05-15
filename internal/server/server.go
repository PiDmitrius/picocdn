package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/PiDmitrius/picocdn/internal/auth"
	"github.com/PiDmitrius/picocdn/internal/store"
)

type Config struct {
	Addr            string
	DataDir         string
	BaseDomain      string
	MaxUploadBytes  int64
	TrustedProxyIPs []string
}

type Server struct {
	cfg        Config
	logger     *slog.Logger
	auth       *auth.Store
	store      *store.Store
	mux        *http.ServeMux
	hostSuffix string // "."+BaseDomain precomputed once, "" disables subdomain routing
}

func New(cfg Config, authStore *auth.Store, blobStore *store.Store, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if authStore == nil {
		return nil, fmt.Errorf("auth store is required")
	}
	if blobStore == nil {
		return nil, fmt.Errorf("blob store is required")
	}
	baseDomain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(cfg.BaseDomain), "."))
	hostSuffix := ""
	if baseDomain != "" {
		hostSuffix = "." + baseDomain
	}
	s := &Server{
		cfg:        cfg,
		logger:     logger,
		auth:       authStore,
		store:      blobStore,
		mux:        http.NewServeMux(),
		hostSuffix: hostSuffix,
	}
	s.routes()
	return s, nil
}

var recorderPool = sync.Pool{
	New: func() any { return &responseRecorder{} },
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rw := recorderPool.Get().(*responseRecorder)
	rw.reset(w)
	defer recorderPool.Put(rw)

	start := time.Now()
	if namespace, ok := s.namespaceFromHost(r.Host); ok && r.URL.Path != "/healthz" && !strings.HasPrefix(r.URL.Path, "/_/") {
		s.dispatchObject(rw, r, namespace, r.URL.Path)
	} else {
		s.mux.ServeHTTP(rw, r)
	}
	s.logger.Info("request",
		"method", r.Method,
		"host", r.Host,
		"path", r.URL.Path,
		"status", rw.status,
		"bytes", rw.bytes,
		"ms", time.Since(start).Milliseconds(),
		"actor", rw.actor,
		"remote", clientIP(r),
		"ua", r.Header.Get("User-Agent"),
	)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)

	// Admin plane under /_/. Namespace names cannot start with `_`, so this
	// prefix never collides with object-plane routing.
	s.mux.HandleFunc("POST /_/namespaces", s.handleAdminNamespaceCreate)
	s.mux.HandleFunc("GET /_/namespaces", s.handleAdminNamespaceList)
	s.mux.HandleFunc("DELETE /_/namespaces/{ns}", s.handleAdminNamespaceDelete)
	s.mux.HandleFunc("POST /_/namespaces/{ns}/rotate-owner", s.handleAdminRotateOwner)
	s.mux.HandleFunc("POST /_/namespaces/{ns}/tokens", s.handleAdminTokenCreate)
	s.mux.HandleFunc("GET /_/namespaces/{ns}/tokens", s.handleAdminTokenList)
	s.mux.HandleFunc("DELETE /_/namespaces/{ns}/tokens/{id}", s.handleAdminTokenRevoke)
	s.mux.HandleFunc("POST /_/namespaces/{ns}/public", s.handleAdminSetPublic)
	s.mux.HandleFunc("POST /_/namespaces/{ns}/index", s.handleAdminSetIndex)

	// Object plane.
	s.mux.HandleFunc("/{namespace}", s.handlePathFallback)
	s.mux.HandleFunc("/{namespace}/{objectPath...}", s.handlePathFallback)
}

// tryServeIndex looks up the namespace's configured index file under urlPath
// and, if the object exists, serves it via handleGet (so public_read / auth
// rules apply uniformly). Returns true if it handled the response. urlPath
// must be "/" or end with "/".
func (s *Server) tryServeIndex(w http.ResponseWriter, r *http.Request, namespace, urlPath string) bool {
	indexFile := s.auth.IndexFile(namespace)
	if indexFile == "" {
		return false
	}
	indexPath := urlPath + indexFile
	if !s.store.HasAlias(namespace, indexPath) {
		return false
	}
	s.handleGet(w, r, namespace, indexPath)
	return true
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
	})
}

func (s *Server) handlePathFallback(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	if err := auth.ValidateNamespaceName(namespace); err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	objectPath := r.PathValue("objectPath")
	if objectPath == "" {
		s.dispatchObject(w, r, namespace, "/")
		return
	}
	s.dispatchObject(w, r, namespace, "/"+objectPath)
}

// dispatchObject routes object-plane requests. urlPath is "/" for namespace
// root (listing) and "/foo/bar" for an object operation.
func (s *Server) dispatchObject(w http.ResponseWriter, r *http.Request, namespace, urlPath string) {
	switch r.Method {
	case http.MethodGet:
		if urlPath == "/" || strings.HasSuffix(urlPath, "/") {
			if served := s.tryServeIndex(w, r, namespace, urlPath); served {
				return
			}
			if urlPath == "/" {
				s.handleList(w, r, namespace)
				return
			}
			writeError(w, http.StatusNotFound, "object not found")
			return
		}
		s.handleGet(w, r, namespace, urlPath)
	case http.MethodHead:
		if urlPath == "/" || strings.HasSuffix(urlPath, "/") {
			if served := s.tryServeIndex(w, r, namespace, urlPath); served {
				return
			}
			if urlPath == "/" {
				if !s.requirePermission(w, r, namespace, "read") {
					return
				}
				w.WriteHeader(http.StatusOK)
				return
			}
			writeError(w, http.StatusNotFound, "object not found")
			return
		}
		s.handleGet(w, r, namespace, urlPath)
	case http.MethodPut:
		if urlPath == "/" {
			writeError(w, http.StatusBadRequest, "PUT requires a path")
			return
		}
		s.handlePut(w, r, namespace, urlPath)
	case http.MethodDelete:
		if urlPath == "/" {
			writeError(w, http.StatusBadRequest, "DELETE requires a path")
			return
		}
		s.handleDelete(w, r, namespace, urlPath)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request, namespace, objectPath string) {
	if !s.requirePermission(w, r, namespace, "write") {
		return
	}
	if s.cfg.MaxUploadBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	}
	defer r.Body.Close()

	contentType := r.Header.Get("Content-Type")

	obj, err := s.store.PutObject(r.Context(), r.Body, store.PutOptions{
		Namespace:   namespace,
		Path:        objectPath,
		ContentType: contentType,
	})
	if err != nil {
		s.logger.Warn("upload failed", "err", err, "namespace", namespace, "path", objectPath)
		writeError(w, statusForUploadError(err), err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", fmt.Sprintf(`"%s:%s"`, obj.Algo, obj.Hash))
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(obj)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, namespace, objectPath string) {
	if !s.requirePermission(w, r, namespace, "delete") {
		return
	}
	obj, err := s.store.DeleteObject(namespace, objectPath)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "object not found")
		return
	}
	if err != nil {
		s.logger.Warn("delete failed", "err", err, "namespace", namespace, "path", objectPath)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "deleted",
		"namespace": obj.Namespace,
		"path":      obj.Path,
		"hash":      obj.Hash,
	})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request, namespace string) {
	if !s.requirePermission(w, r, namespace, "read") {
		return
	}
	prefix := r.URL.Query().Get("prefix")

	w.Header().Set("Content-Type", "application/json")
	bw := bufio.NewWriterSize(w, 16<<10)
	defer bw.Flush()

	if _, err := bw.WriteString(`{"namespace":`); err != nil {
		return
	}
	if err := json.NewEncoder(bw).Encode(namespace); err != nil {
		return
	}
	if _, err := bw.WriteString(`,"objects":[`); err != nil {
		return
	}
	enc := json.NewEncoder(bw)
	first := true
	walkErr := s.store.WalkObjects(namespace, prefix, func(obj *store.Object) error {
		if !first {
			if _, err := bw.WriteString(","); err != nil {
				return err
			}
		}
		first = false
		return enc.Encode(obj)
	})
	if walkErr != nil {
		s.logger.Warn("list failed", "err", walkErr, "namespace", namespace)
	}
	_, _ = bw.WriteString(`]}`)
}

func statusForUploadError(err error) int {
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		return http.StatusRequestEntityTooLarge
	}
	if errors.Is(err, http.ErrBodyReadAfterClose) {
		return http.StatusBadRequest
	}
	if strings.Contains(err.Error(), "request body too large") {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, namespace, objectPath string) {
	if !s.requireReadAccess(w, r, namespace) {
		return
	}

	obj, err := s.store.GetObject(namespace, objectPath)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "object not found")
		return
	}
	if err != nil {
		s.logger.Warn("object lookup failed", "err", err, "namespace", namespace, "path", objectPath)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	file, err := os.Open(obj.BlobPath)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "blob not found")
		return
	}
	if err != nil {
		s.logger.Error("blob open failed", "err", err)
		writeError(w, http.StatusInternalServerError, "blob open failed")
		return
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		s.logger.Error("blob stat failed", "err", err)
		writeError(w, http.StatusInternalServerError, "blob stat failed")
		return
	}

	cacheControl := "no-cache"
	if s.auth.IsPublicRead(namespace) {
		cacheControl = "public, max-age=300"
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("Content-Type", obj.ContentType)
	w.Header().Set("ETag", fmt.Sprintf(`"%s:%s"`, obj.Algo, obj.Hash))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, path.Base(obj.Path), stat.ModTime(), file)
}

// --- Admin handlers ---

type namespaceCreateRequest struct {
	Name string `json:"name"`
}

type tokenCreateRequest struct {
	Name        string   `json:"name"`
	Permissions []string `json:"permissions"`
}

type publicRequest struct {
	On bool `json:"on"`
}

type indexRequest struct {
	File     string `json:"file"`
	Disabled bool   `json:"disabled"`
}

func (s *Server) handleAdminNamespaceCreate(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRoot(w, r)
	if !ok {
		return
	}
	setActor(w, actor)
	var req namespaceCreateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	created, err := s.auth.CreateNamespace(req.Name)
	if errors.Is(err, auth.ErrNamespaceExists) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, auth.ErrInvalidNamespaceName) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		s.logger.Warn("namespace create failed", "err", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"namespace":      created.Namespace,
		"owner_token_id": created.TokenID,
		"owner_token":    created.Token,
		"public_read":    s.auth.IsPublicRead(created.Namespace),
		"index_file":     s.auth.IndexFile(created.Namespace),
	})
}

func (s *Server) handleAdminNamespaceList(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRoot(w, r)
	if !ok {
		return
	}
	setActor(w, actor)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.auth.ListNamespaces())
}

func (s *Server) handleAdminNamespaceDelete(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRoot(w, r)
	if !ok {
		return
	}
	setActor(w, actor)
	name := r.PathValue("ns")
	if err := auth.ValidateNamespaceName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.auth.DeleteNamespace(name); err != nil {
		if errors.Is(err, auth.ErrNamespaceNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		s.logger.Warn("namespace delete failed", "err", err, "namespace", name)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.store.DeleteNamespaceAliases(name); err != nil {
		s.logger.Warn("alias cleanup failed", "err", err, "namespace", name)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdminRotateOwner(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRoot(w, r)
	if !ok {
		return
	}
	setActor(w, actor)
	name := r.PathValue("ns")
	if err := auth.ValidateNamespaceName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := s.auth.RotateOwner(name)
	if errors.Is(err, auth.ErrNamespaceNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		s.logger.Warn("rotate owner failed", "err", err, "namespace", name)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"namespace":      created.Namespace,
		"owner_token_id": created.TokenID,
		"owner_token":    created.Token,
	})
}

func (s *Server) handleAdminTokenCreate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("ns")
	if err := auth.ValidateNamespaceName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	actor, ok := s.requireNamespaceAdmin(w, r, name)
	if !ok {
		return
	}
	setActor(w, actor)
	var req tokenCreateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	created, err := s.auth.CreateToken(name, req.Name, req.Permissions)
	if errors.Is(err, auth.ErrNamespaceNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(created)
}

func (s *Server) handleAdminTokenList(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("ns")
	if err := auth.ValidateNamespaceName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	actor, ok := s.requireNamespaceAdmin(w, r, name)
	if !ok {
		return
	}
	setActor(w, actor)
	tokens, err := s.auth.ListTokens(name)
	if errors.Is(err, auth.ErrNamespaceNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tokens)
}

func (s *Server) handleAdminTokenRevoke(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("ns")
	if err := auth.ValidateNamespaceName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	actor, ok := s.requireNamespaceAdmin(w, r, name)
	if !ok {
		return
	}
	setActor(w, actor)
	id := r.PathValue("id")
	err := s.auth.RevokeToken(name, id)
	if errors.Is(err, auth.ErrNamespaceNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if errors.Is(err, auth.ErrTokenNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if errors.Is(err, auth.ErrCannotRevokeOwner) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdminSetIndex(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("ns")
	if err := auth.ValidateNamespaceName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	actor, ok := s.requireNamespaceAdmin(w, r, name)
	if !ok {
		return
	}
	setActor(w, actor)
	var req indexRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if err := s.auth.SetIndex(name, req.File, req.Disabled); err != nil {
		if errors.Is(err, auth.ErrNamespaceNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, auth.ErrInvalidIndexFile) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"namespace":      name,
		"index_file":     s.auth.IndexFile(name),
		"index_disabled": req.Disabled,
	})
}

func (s *Server) handleAdminSetPublic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("ns")
	if err := auth.ValidateNamespaceName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	actor, ok := s.requireNamespaceAdmin(w, r, name)
	if !ok {
		return
	}
	setActor(w, actor)
	var req publicRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if err := s.auth.SetPublicRead(name, req.On); err != nil {
		if errors.Is(err, auth.ErrNamespaceNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"namespace":   name,
		"public_read": req.On,
	})
}

// --- Authorization helpers ---

func (s *Server) namespaceFromHost(host string) (string, bool) {
	if s.hostSuffix == "" {
		return "", false
	}
	if i := strings.IndexByte(host, ':'); i > -1 {
		host = host[:i]
	}
	if len(host) > 0 && host[len(host)-1] == '.' {
		host = host[:len(host)-1]
	}
	if hasUpper(host) {
		host = strings.ToLower(host)
	}
	if !strings.HasSuffix(host, s.hostSuffix) {
		return "", false
	}
	namespace := host[:len(host)-len(s.hostSuffix)]
	if namespace == "" || strings.IndexByte(namespace, '.') >= 0 {
		return "", false
	}
	if err := auth.ValidateNamespaceName(namespace); err != nil {
		return "", false
	}
	return namespace, true
}

func hasUpper(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			return true
		}
	}
	return false
}

// requirePermission gates an object-plane write/delete. To prevent
// namespace-existence enumeration, callers without a valid scope for the
// target namespace see 401 regardless of whether the namespace exists. Only
// a valid root token differentiates: it gets 404 when the namespace is
// missing (root already knows the full namespace list).
func (s *Server) requirePermission(w http.ResponseWriter, r *http.Request, namespace, permission string) bool {
	token := bearerToken(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing bearer token")
		return false
	}
	if rootActor, ok := s.auth.AuthorizeRoot(token); ok {
		if !s.auth.HasNamespace(namespace) {
			writeError(w, http.StatusNotFound, "namespace not found")
			return false
		}
		setActor(w, rootActor)
		return true
	}
	if !s.auth.HasNamespace(namespace) {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return false
	}
	actor, ok := s.auth.Authorize(namespace, token, permission)
	if !ok {
		// Distinguish "wrong namespace / unknown token" (401) from
		// "valid token but missing permission" (403). The namespace
		// exists at this point; if the token is not registered here
		// at all, surface 401 — same shape as the cross-namespace case.
		if _, known := s.auth.Authorize(namespace, token, "read"); !known {
			if _, knownW := s.auth.Authorize(namespace, token, "write"); !knownW {
				if _, knownD := s.auth.Authorize(namespace, token, "delete"); !knownD {
					writeError(w, http.StatusUnauthorized, "invalid token")
					return false
				}
			}
		}
		writeError(w, http.StatusForbidden, "permission denied")
		return false
	}
	setActor(w, actor)
	return true
}

func (s *Server) requireReadAccess(w http.ResponseWriter, r *http.Request, namespace string) bool {
	if s.auth.IsPublicRead(namespace) {
		setActor(w, auth.Actor{Kind: "anon"})
		return true
	}
	token := bearerToken(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing bearer token")
		return false
	}
	if rootActor, ok := s.auth.AuthorizeRoot(token); ok {
		if !s.auth.HasNamespace(namespace) {
			writeError(w, http.StatusNotFound, "namespace not found")
			return false
		}
		setActor(w, rootActor)
		return true
	}
	if !s.auth.HasNamespace(namespace) {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return false
	}
	actor, ok := s.auth.Authorize(namespace, token, "read")
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return false
	}
	setActor(w, actor)
	return true
}

func (s *Server) requireRoot(w http.ResponseWriter, r *http.Request) (auth.Actor, bool) {
	token := bearerToken(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing bearer token")
		return auth.Actor{}, false
	}
	actor, ok := s.auth.AuthorizeRoot(token)
	if !ok {
		writeError(w, http.StatusForbidden, "root token required")
		return auth.Actor{}, false
	}
	return actor, true
}

// requireNamespaceAdmin gates /_/namespaces/{ns}/... endpoints. Same
// existence-enumeration story as requirePermission: callers who are
// neither root nor the namespace owner get 401 regardless of whether
// the namespace exists. Only root sees 404 for a missing namespace.
func (s *Server) requireNamespaceAdmin(w http.ResponseWriter, r *http.Request, namespace string) (auth.Actor, bool) {
	token := bearerToken(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing bearer token")
		return auth.Actor{}, false
	}
	if rootActor, ok := s.auth.AuthorizeRoot(token); ok {
		if !s.auth.HasNamespace(namespace) {
			writeError(w, http.StatusNotFound, "namespace not found")
			return auth.Actor{}, false
		}
		return rootActor, true
	}
	if !s.auth.HasNamespace(namespace) {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return auth.Actor{}, false
	}
	actor, ok := s.auth.AuthorizeNamespaceAdmin(namespace, token)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return auth.Actor{}, false
	}
	return actor, true
}

func bearerToken(r *http.Request) string {
	value := r.Header.Get("Authorization")
	if strings.HasPrefix(value, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
	}
	return r.Header.Get("X-Picocdn-Token")
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": message,
	})
}

func setActor(w http.ResponseWriter, actor auth.Actor) {
	if rr, ok := w.(*responseRecorder); ok {
		rr.actor = actor.String()
	}
}

type responseRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
	actor       string
}

func (r *responseRecorder) reset(w http.ResponseWriter) {
	r.ResponseWriter = w
	r.status = http.StatusOK
	r.bytes = 0
	r.wroteHeader = false
	r.actor = ""
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.wroteHeader = true
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}
	if i := strings.LastIndex(r.RemoteAddr, ":"); i > -1 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}
