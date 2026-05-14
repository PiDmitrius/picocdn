package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PiDmitrius/picocdn/internal/auth"
)

// putReq builds a PUT request. `host` may be empty for default localhost.
func putReq(t testing.TB, host, urlPath, token, body, contentType string) *http.Request {
	t.Helper()
	if host == "" {
		host = "127.0.0.1:8080"
	}
	r := httptest.NewRequest(http.MethodPut, "http://"+host+urlPath, strings.NewReader(body))
	r.Host = host
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	return r
}

func doPut(t testing.TB, srv *Server, host, urlPath, token, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, putReq(t, host, urlPath, token, body, contentType))
	return rec
}

// authedReq builds a non-PUT request for path-fallback (no Host magic).
func authedReq(method, urlPath, token string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, "http://127.0.0.1:8080"+urlPath, body)
	r.Host = "127.0.0.1:8080"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

// ---------- path-fallback tests (no BaseDomain required) ----------

func TestUploadDownloadAndRange(t *testing.T) {
	srv, token := newTestServer(t, []string{"read", "write"})

	rec := doPut(t, srv, "", "/default/docs/hello.txt", token, "hello picocdn", "text/plain")
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, authedReq(http.MethodGet, "/default/docs/hello.txt", token, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "hello picocdn" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("missing ETag")
	}

	// Range
	req := authedReq(http.MethodGet, "/default/docs/hello.txt", token, nil)
	req.Header.Set("Range", "bytes=0-4")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "hello" {
		t.Fatalf("range body = %q", rec.Body.String())
	}

	// HEAD
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, authedReq(http.MethodHead, "/default/docs/hello.txt", token, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD returned body: %q", rec.Body.String())
	}
}

func TestSingleSegmentPath(t *testing.T) {
	srv, token := newTestServer(t, []string{"read", "write"})

	rec := doPut(t, srv, "", "/default/file.txt", token, "single", "text/plain")
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, authedReq(http.MethodGet, "/default/file.txt", token, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rec.Code)
	}
	if rec.Body.String() != "single" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestNamespaceIsolation(t *testing.T) {
	srv, tokens := newTestServerWithNamespaces(t, map[string][]string{
		"default": {"read", "write"},
		"other":   {"read"},
	}, nil)

	rec := doPut(t, srv, "", "/default/docs/private.txt",
		tokens["default"], "secret", "text/plain")
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}

	// default's token cannot read 'other'.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, authedReq(http.MethodGet, "/other/docs/private.txt", tokens["default"], nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-namespace token status = %d", rec.Code)
	}

	// 'other' has its own token but object is in 'default'.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, authedReq(http.MethodGet, "/other/docs/private.txt", tokens["other"], nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-namespace object status = %d", rec.Code)
	}
}

func TestEncodedTraversalRejected(t *testing.T) {
	srv, token := newTestServer(t, []string{"read", "write"})

	rec := doPut(t, srv, "", "/default/docs/secret.txt", token, "secret", "text/plain")
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed status = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, authedReq(http.MethodGet, "/default/docs/%2e%2e/docs/secret.txt", token, nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("encoded traversal status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestUploadLimit(t *testing.T) {
	srv, token := newTestServerWithConfig(t, []string{"write"}, func(cfg *Config) {
		cfg.MaxUploadBytes = 512
	})

	rec := doPut(t, srv, "", "/default/docs/too-large.txt", token,
		strings.Repeat("x", 2048), "application/octet-stream")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAuthRequired(t *testing.T) {
	srv, token := newTestServer(t, []string{"read"})

	rec := doPut(t, srv, "", "/default/x.txt", "", "body", "text/plain")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d", rec.Code)
	}

	rec = doPut(t, srv, "", "/default/x.txt", token, "body", "text/plain")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read-only upload status = %d", rec.Code)
	}
}

func TestDeleteObject(t *testing.T) {
	srv, token := newTestServer(t, []string{"read", "write", "delete"})

	rec := doPut(t, srv, "", "/default/docs/del.txt", token, "to delete", "text/plain")
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, authedReq(http.MethodDelete, "/default/docs/del.txt", token, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, authedReq(http.MethodGet, "/default/docs/del.txt", token, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("post-delete get status = %d", rec.Code)
	}
}

func TestDeleteRequiresDeletePerm(t *testing.T) {
	srv, token := newTestServer(t, []string{"read"})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedReq(http.MethodDelete, "/default/x.txt", token, nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestListObjects(t *testing.T) {
	srv, token := newTestServer(t, []string{"read", "write"})

	for _, p := range []string{"/a/1.txt", "/a/2.txt", "/b/3.txt"} {
		rec := doPut(t, srv, "", "/default"+p, token, "body", "text/plain")
		if rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s status = %d body=%s", p, rec.Code, rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedReq(http.MethodGet, "/default?prefix=/a", token, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Objects []struct {
			Path string `json:"path"`
		} `json:"objects"`
	}
	decodeJSON(t, rec.Body, &resp)
	if len(resp.Objects) != 2 {
		t.Fatalf("len = %d, want 2", len(resp.Objects))
	}
}

func TestUploadRejectsTraversal(t *testing.T) {
	srv, token := newTestServer(t, []string{"write"})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, putReq(t, "", "/default/docs/%2e%2e/secret.txt", token, "body", ""))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPutRequiresPath(t *testing.T) {
	srv, token := newTestServer(t, []string{"write"})

	// PUT to bare /{namespace} (path-fallback list endpoint, which only accepts GET).
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, putReq(t, "", "/default", token, "body", ""))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

// ---------- subdomain tests (BaseDomain enabled) ----------

func TestSubdomainUploadDownload(t *testing.T) {
	srv, token := newTestServerWithConfig(t, []string{"read", "write"}, withSubdomain)

	rec := doPut(t, srv, "default.example.test", "/docs/hello.txt", token, "hi via subdomain", "text/plain")
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "http://default.example.test/docs/hello.txt", nil)
	req.Host = "default.example.test"
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "hi via subdomain" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestSubdomainBadMethod(t *testing.T) {
	srv, token := newTestServerWithConfig(t, []string{"read", "write"}, withSubdomain)
	req := httptest.NewRequest("POST", "http://default.example.test/x", nil)
	req.Host = "default.example.test"
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("Allow") == "" {
		t.Fatal("missing Allow header")
	}
}

func TestSubdomainBaseHostNotNamespace(t *testing.T) {
	srv, token := newTestServerWithConfig(t, []string{"read"}, withSubdomain)
	req := httptest.NewRequest(http.MethodGet, "http://example.test/docs/missing.txt", nil)
	req.Host = "example.test"
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSubdomainListAtRoot(t *testing.T) {
	srv, token := newTestServerWithConfig(t, []string{"read", "write"}, withSubdomain)

	rec := doPut(t, srv, "default.example.test", "/a/1.txt", token, "body", "text/plain")
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "http://default.example.test/?prefix=/a", nil)
	req.Host = "default.example.test"
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSubdomainDisabledWhenBaseDomainEmpty(t *testing.T) {
	// BaseDomain="" — subdomain header is just ignored, path-fallback rules.
	srv, token := newTestServer(t, []string{"read", "write"})

	rec := doPut(t, srv, "default.example.test", "/docs/x.txt", token, "body", "text/plain")
	// /docs/x.txt has no leading namespace segment when subdomain dispatch is off,
	// so the mux can't resolve a namespace and returns 404.
	if rec.Code == http.StatusCreated {
		t.Fatalf("subdomain PUT unexpectedly succeeded; status = %d body=%s", rec.Code, rec.Body.String())
	}
}

// ---------- public-read (subdomain-driven path) ----------

func TestPublicReadAlias(t *testing.T) {
	srv, token := newTestServer(t, []string{"read", "write"})

	rec := doPut(t, srv, "", "/default/docs/public.txt", token, "public body", "text/plain")
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}

	// before public-read: 401
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, authedReq(http.MethodGet, "/default/docs/public.txt", "", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d", rec.Code)
	}

	if err := auth.SetNamespacePublicRead(srv.AuthReloader().Get(), "default", true); err != nil {
		t.Fatal(err)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, authedReq(http.MethodGet, "/default/docs/public.txt", "", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("public-read status = %d body=%s", rec.Code, rec.Body.String())
	}
}

// ---------- hot reload (no HTTP) ----------

func TestAuthHotReload(t *testing.T) {
	dir := t.TempDir()
	authFilePath := filepath.Join(dir, "auth.json")
	authFile := &auth.File{Version: 1, Namespaces: map[string]*auth.Namespace{}}
	if _, err := auth.CreateNamespace(authFile, "default"); err != nil {
		t.Fatal(err)
	}
	if err := auth.SaveFile(authFilePath, authFile); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{
		DataDir:        dir,
		AuthFile:       authFilePath,
		MaxUploadBytes: 1 << 20,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if srv.AuthReloader().HasNamespace("late") {
		t.Fatal("late namespace should not exist yet")
	}
	reloaded, err := auth.LoadFile(authFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.CreateNamespace(reloaded, "late"); err != nil {
		t.Fatal(err)
	}
	if err := auth.SaveFile(authFilePath, reloaded); err != nil {
		t.Fatal(err)
	}
	if err := srv.AuthReloader().ForceReload(); err != nil {
		t.Fatal(err)
	}
	if !srv.AuthReloader().HasNamespace("late") {
		t.Fatal("late namespace should exist after reload")
	}
}

func TestAuthMaybeReloadDetectsMtime(t *testing.T) {
	dir := t.TempDir()
	authFilePath := filepath.Join(dir, "auth.json")
	authFile := &auth.File{Version: 1, Namespaces: map[string]*auth.Namespace{}}
	if _, err := auth.CreateNamespace(authFile, "default"); err != nil {
		t.Fatal(err)
	}
	if err := auth.SaveFile(authFilePath, authFile); err != nil {
		t.Fatal(err)
	}
	r, err := auth.NewReloader(authFilePath, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(authFilePath, future, future); err != nil {
		t.Fatal(err)
	}
	reloaded, err := r.MaybeReload()
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded {
		t.Fatal("expected MaybeReload to detect mtime change")
	}
}

// ---------- helpers ----------

func newTestServer(t *testing.T, permissions []string) (*Server, string) {
	return newTestServerWithConfig(t, permissions, nil)
}

func newTestServerWithConfig(t *testing.T, permissions []string, configure func(*Config)) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	authFilePath := filepath.Join(dir, "auth.json")
	authFile := &auth.File{Version: 1, Namespaces: map[string]*auth.Namespace{}}
	owner, err := auth.CreateNamespace(authFile, "default")
	if err != nil {
		t.Fatal(err)
	}
	token := owner.Token
	if strings.Join(permissions, ",") != "read,write" {
		created, err := auth.CreateToken(authFile, "default", "test", permissions)
		if err != nil {
			t.Fatal(err)
		}
		token = created.Token
	}
	if err := auth.SaveFile(authFilePath, authFile); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		DataDir:        dir,
		AuthFile:       authFilePath,
		MaxUploadBytes: 1 << 20,
	}
	if configure != nil {
		configure(&cfg)
	}
	srv, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return srv, token
}

// withSubdomain enables the optional subdomain dispatcher in test config.
func withSubdomain(cfg *Config) {
	cfg.BaseDomain = "example.test"
}

func newTestServerWithNamespaces(t *testing.T, namespaces map[string][]string, configure func(*Config)) (*Server, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	authFilePath := filepath.Join(dir, "auth.json")
	authFile := &auth.File{Version: 1, Namespaces: map[string]*auth.Namespace{}}
	tokens := make(map[string]string, len(namespaces))
	for namespace, permissions := range namespaces {
		owner, err := auth.CreateNamespace(authFile, namespace)
		if err != nil {
			t.Fatal(err)
		}
		tokens[namespace] = owner.Token
		if strings.Join(permissions, ",") == "owner" {
			continue
		}
		created, err := auth.CreateToken(authFile, namespace, "test", permissions)
		if err != nil {
			t.Fatal(err)
		}
		tokens[namespace] = created.Token
	}
	if err := auth.SaveFile(authFilePath, authFile); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		DataDir:        dir,
		AuthFile:       authFilePath,
		MaxUploadBytes: 1 << 20,
	}
	if configure != nil {
		configure(&cfg)
	}
	srv, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return srv, tokens
}

// ---------- benchmarks ----------

func BenchmarkNamespaceFromHost(b *testing.B) {
	srv := &Server{hostSuffix: ".example.test"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if ns, ok := srv.namespaceFromHost("default.example.test"); !ok || ns != "default" {
			b.Fatal("want default")
		}
	}
}

func BenchmarkNamespaceFromHostMixedCase(b *testing.B) {
	srv := &Server{hostSuffix: ".example.test"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if ns, ok := srv.namespaceFromHost("Default.Example.TEST"); !ok || ns != "default" {
			b.Fatal("want default")
		}
	}
}

func BenchmarkClientIP(b *testing.B) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "10.0.0.1, 10.0.0.2")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = clientIP(req)
	}
}

func BenchmarkServeHTTPList(b *testing.B) {
	srv, token := newBenchServer(b)
	for i := 0; i < 100; i++ {
		rec := doPut(b, srv, "",
			"/default/dir/"+strings.Repeat("a", i+1)+".txt", token, "body", "text/plain")
		if rec.Code != http.StatusCreated {
			b.Fatalf("PUT %d status = %d", i, rec.Code)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, authedReq(http.MethodGet, "/default", token, nil))
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d", rec.Code)
		}
	}
}

func BenchmarkServeHTTPGet(b *testing.B) {
	srv, token := newBenchServer(b)
	rec := doPut(b, srv, "", "/default/h.txt", token, "body", "text/plain")
	if rec.Code != http.StatusCreated {
		b.Fatalf("seed PUT status = %d body=%s", rec.Code, rec.Body.String())
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, authedReq(http.MethodGet, "/default/h.txt", token, nil))
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d", rec.Code)
		}
	}
}

func BenchmarkServeHTTPPut(b *testing.B) {
	srv, token := newBenchServer(b)
	body := strings.Repeat("a", 256)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := doPut(b, srv, "", "/default/bench.bin", token, body, "application/octet-stream")
		if rec.Code != http.StatusCreated {
			b.Fatalf("status = %d", rec.Code)
		}
	}
}

func newBenchServer(b *testing.B) (*Server, string) {
	b.Helper()
	dir := b.TempDir()
	authFilePath := filepath.Join(dir, "auth.json")
	authFile := &auth.File{Version: 1, Namespaces: map[string]*auth.Namespace{}}
	owner, err := auth.CreateNamespace(authFile, "default")
	if err != nil {
		b.Fatal(err)
	}
	if err := auth.SaveFile(authFilePath, authFile); err != nil {
		b.Fatal(err)
	}
	srv, err := New(Config{
		DataDir:        dir,
		AuthFile:       authFilePath,
		MaxUploadBytes: 1 << 20,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		b.Fatal(err)
	}
	return srv, owner.Token
}

func decodeJSON(t *testing.T, r io.Reader, v any) {
	t.Helper()
	if err := json.NewDecoder(r).Decode(v); err != nil {
		t.Fatal(err)
	}
}
