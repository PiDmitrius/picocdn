package server

import (
	"bufio"
	"context"
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

	"picocdn/internal/auth"
	"picocdn/internal/store"
)

type Config struct {
	Addr            string
	DataDir         string
	AuthFile        string
	BaseDomain      string
	MaxUploadBytes  int64
	ReloadInterval  time.Duration
	TrustedProxyIPs []string
}

type Server struct {
	cfg        Config
	logger     *slog.Logger
	auth       *auth.Reloader
	store      *store.Store
	mux        *http.ServeMux
	hostSuffix string // "."+BaseDomain precomputed once, "" disables subdomain routing
}

func New(cfg Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	reloader, err := auth.NewReloader(cfg.AuthFile, logger)
	if err != nil {
		return nil, err
	}
	baseDomain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(cfg.BaseDomain), "."))
	hostSuffix := ""
	if baseDomain != "" {
		hostSuffix = "." + baseDomain
	}
	s := &Server{
		cfg:        cfg,
		logger:     logger,
		auth:       reloader,
		store:      store.New(cfg.DataDir),
		mux:        http.NewServeMux(),
		hostSuffix: hostSuffix,
	}
	s.routes()
	return s, nil
}

var recorderPool = sync.Pool{
	New: func() any { return &responseRecorder{} },
}

func (s *Server) AuthReloader() *auth.Reloader {
	return s.auth
}

func (s *Server) StartReloadWatcher(ctx context.Context) {
	if s.cfg.ReloadInterval <= 0 {
		return
	}
	go s.auth.Watch(ctx, s.cfg.ReloadInterval)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rw := recorderPool.Get().(*responseRecorder)
	rw.reset(w)
	defer recorderPool.Put(rw)

	start := time.Now()
	if namespace, ok := s.namespaceFromHost(r.Host); ok && r.URL.Path != "/healthz" {
		s.dispatch(rw, r, namespace, r.URL.Path)
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
		"remote", clientIP(r),
		"ua", r.Header.Get("User-Agent"),
	)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	// Path-fallback for dev / local: <host>/<namespace>[/<path>].
	s.mux.HandleFunc("/{namespace}", s.handlePathFallback)
	s.mux.HandleFunc("/{namespace}/{objectPath...}", s.handlePathFallback)
}

func (s *Server) handlePathFallback(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	objectPath := r.PathValue("objectPath")
	if objectPath == "" {
		s.dispatch(w, r, namespace, "/")
		return
	}
	s.dispatch(w, r, namespace, "/"+objectPath)
}

// dispatch routes a request once namespace and the in-namespace URL path are
// known. urlPath is "/" for the namespace root (list) and "/foo/bar" for an
// object operation.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, namespace, urlPath string) {
	switch r.Method {
	case http.MethodGet:
		if urlPath == "/" {
			s.handleList(w, r, namespace)
			return
		}
		s.handleGet(w, r, namespace, urlPath)
	case http.MethodHead:
		if urlPath == "/" {
			if !s.requirePermission(w, r, namespace, "read") {
				return
			}
			w.WriteHeader(http.StatusOK)
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

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
	})
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
	// Fast-path: hosts are typically already lowercase. Only fall back to
	// strings.ToLower if we see an uppercase letter.
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

func (s *Server) requirePermission(w http.ResponseWriter, r *http.Request, namespace, permission string) bool {
	if !s.auth.HasNamespace(namespace) {
		writeError(w, http.StatusNotFound, "namespace not found")
		return false
	}
	token := bearerToken(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing bearer token")
		return false
	}
	if !s.auth.Authorize(namespace, token, permission) {
		writeError(w, http.StatusForbidden, "permission denied")
		return false
	}
	return true
}

func (s *Server) requireReadAccess(w http.ResponseWriter, r *http.Request, namespace string) bool {
	if !s.auth.HasNamespace(namespace) {
		writeError(w, http.StatusNotFound, "namespace not found")
		return false
	}
	if s.auth.IsPublicRead(namespace) {
		return true
	}
	token := bearerToken(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing bearer token")
		return false
	}
	if !s.auth.Authorize(namespace, token, "read") {
		writeError(w, http.StatusForbidden, "permission denied")
		return false
	}
	return true
}

func bearerToken(r *http.Request) string {
	value := r.Header.Get("Authorization")
	if strings.HasPrefix(value, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
	}
	return r.Header.Get("X-Picocdn-Token")
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": message,
	})
}

type responseRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *responseRecorder) reset(w http.ResponseWriter) {
	r.ResponseWriter = w
	r.status = http.StatusOK
	r.bytes = 0
	r.wroteHeader = false
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
