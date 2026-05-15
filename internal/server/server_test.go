package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PiDmitrius/picocdn/internal/auth"
	"github.com/PiDmitrius/picocdn/internal/store"
)

// --- helpers ---

type testServer struct {
	srv         *Server
	authStore   *auth.Store
	defaultNS   string
	ownerToken  string
	rootToken   string
	rootEnabled bool
}

func buildServer(t testing.TB, configure func(*Config)) *testServer {
	t.Helper()
	dataDir := t.TempDir()
	nsDir := filepath.Join(dataDir, "namespaces")

	rootPlain, rootMeta, err := auth.NewRootToken("ops")
	if err != nil {
		t.Fatal(err)
	}
	authStore, err := auth.NewStore(nsDir, []auth.RootToken{rootMeta})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := authStore.CreateNamespace("default")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		DataDir:        dataDir,
		MaxUploadBytes: 1 << 20,
	}
	if configure != nil {
		configure(&cfg)
	}
	blobStore := store.New(dataDir)
	srv, err := New(cfg, authStore, blobStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return &testServer{
		srv:         srv,
		authStore:   authStore,
		defaultNS:   "default",
		ownerToken:  owner.Token,
		rootToken:   rootPlain,
		rootEnabled: true,
	}
}

// issueSubToken creates a sub-token in defaultNS with the given permissions.
func (ts *testServer) issueSubToken(t testing.TB, name string, perms []string) string {
	t.Helper()
	created, err := ts.authStore.CreateToken(ts.defaultNS, name, perms)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	return created.Token
}

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

func authedReq(method, urlPath, token string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, "http://127.0.0.1:8080"+urlPath, body)
	r.Host = "127.0.0.1:8080"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func adminReq(method, urlPath, token string, body any) *http.Request {
	var br io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		br = bytes.NewReader(data)
	}
	r := httptest.NewRequest(method, "http://127.0.0.1:8080"+urlPath, br)
	r.Host = "127.0.0.1:8080"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func decodeJSON(t testing.TB, r io.Reader, v any) {
	t.Helper()
	if err := json.NewDecoder(r).Decode(v); err != nil {
		t.Fatal(err)
	}
}

func withSubdomain(cfg *Config) {
	cfg.BaseDomain = "example.test"
}

// --- object plane: PUT/GET/HEAD/DELETE/LIST ---

func TestUploadDownloadAndRange(t *testing.T) {
	ts := buildServer(t, nil)
	rec := doPut(t, ts.srv, "", "/default/docs/hello.txt", ts.ownerToken, "hello picocdn", "text/plain")
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, authedReq(http.MethodGet, "/default/docs/hello.txt", ts.ownerToken, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rec.Code)
	}
	if rec.Body.String() != "hello picocdn" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("missing ETag")
	}

	// Range
	req := authedReq(http.MethodGet, "/default/docs/hello.txt", ts.ownerToken, nil)
	req.Header.Set("Range", "bytes=0-4")
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d", rec.Code)
	}
	if rec.Body.String() != "hello" {
		t.Fatalf("range body = %q", rec.Body.String())
	}
}

func TestSubTokenScopeRespected(t *testing.T) {
	ts := buildServer(t, nil)
	readOnly := ts.issueSubToken(t, "reader", []string{"read"})

	rec := doPut(t, ts.srv, "", "/default/x.txt", readOnly, "body", "text/plain")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("sub-token write should be forbidden, got %d", rec.Code)
	}
	// seed via owner
	if rec := doPut(t, ts.srv, "", "/default/x.txt", ts.ownerToken, "body", "text/plain"); rec.Code != http.StatusCreated {
		t.Fatalf("owner seed PUT status = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, authedReq(http.MethodGet, "/default/x.txt", readOnly, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("sub-token read should pass, got %d", rec.Code)
	}
}

func TestRootAuthorizesAnyNamespace(t *testing.T) {
	ts := buildServer(t, nil)
	if _, err := ts.authStore.CreateNamespace("other"); err != nil {
		t.Fatal(err)
	}
	rec := doPut(t, ts.srv, "", "/other/r.txt", ts.rootToken, "body", "text/plain")
	if rec.Code != http.StatusCreated {
		t.Fatalf("root PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, authedReq(http.MethodGet, "/other/r.txt", ts.rootToken, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("root GET status = %d", rec.Code)
	}
}

func TestAuthRequired(t *testing.T) {
	ts := buildServer(t, nil)
	rec := doPut(t, ts.srv, "", "/default/x.txt", "", "body", "text/plain")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d", rec.Code)
	}
}

// TestNamespaceExistenceNotLeaked verifies that an unauthenticated caller
// or a caller with a wrong-scope token cannot distinguish a missing
// namespace from a private one. Both must return 401, never 404.
func TestNamespaceExistenceNotLeaked(t *testing.T) {
	ts := buildServer(t, nil)
	// flip default off — this test is about the private branch
	if err := ts.authStore.SetPublicRead("default", false); err != nil {
		t.Fatal(err)
	}
	// anon to private existing ns
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, authedReq(http.MethodGet, "/default/x.txt", "", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon private status = %d, want 401", rec.Code)
	}
	// anon to missing ns
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, authedReq(http.MethodGet, "/no-such-ns/x.txt", "", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon missing status = %d, want 401", rec.Code)
	}
	// owner-of-default to missing ns must look the same
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, authedReq(http.MethodGet, "/no-such-ns/x.txt", ts.ownerToken, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("cross-ns owner status = %d, want 401", rec.Code)
	}
	// only root sees the truth (404) for a missing ns
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, authedReq(http.MethodGet, "/no-such-ns/x.txt", ts.rootToken, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("root missing status = %d, want 404", rec.Code)
	}
}

// TestAdminExistenceNotLeaked is the same property for the /_/ plane.
func TestAdminExistenceNotLeaked(t *testing.T) {
	ts := buildServer(t, nil)
	// anon
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodGet, "/_/namespaces/no-such-ns/tokens", "", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon admin missing = %d, want 401", rec.Code)
	}
	// owner-of-default to missing ns
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodGet, "/_/namespaces/no-such-ns/tokens", ts.ownerToken, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("cross-ns owner admin missing = %d, want 401", rec.Code)
	}
	// root distinguishes
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodGet, "/_/namespaces/no-such-ns/tokens", ts.rootToken, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("root admin missing = %d, want 404", rec.Code)
	}
}

func TestDeleteObject(t *testing.T) {
	ts := buildServer(t, nil)
	rec := doPut(t, ts.srv, "", "/default/d.txt", ts.ownerToken, "x", "text/plain")
	if rec.Code != http.StatusCreated {
		t.Fatal()
	}
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, authedReq(http.MethodDelete, "/default/d.txt", ts.ownerToken, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d", rec.Code)
	}
}

func TestEncodedTraversalRejected(t *testing.T) {
	ts := buildServer(t, nil)
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, putReq(t, "", "/default/docs/%2e%2e/secret.txt", ts.ownerToken, "x", "text/plain"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestUploadLimit(t *testing.T) {
	ts := buildServer(t, func(cfg *Config) {
		cfg.MaxUploadBytes = 512
	})
	rec := doPut(t, ts.srv, "", "/default/big.bin", ts.ownerToken, strings.Repeat("x", 2048), "application/octet-stream")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestListObjects(t *testing.T) {
	ts := buildServer(t, nil)
	for _, p := range []string{"/a/1.txt", "/a/2.txt", "/b/3.txt"} {
		if rec := doPut(t, ts.srv, "", "/default"+p, ts.ownerToken, "x", "text/plain"); rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s status = %d", p, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, authedReq(http.MethodGet, "/default?prefix=/a", ts.ownerToken, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
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

// --- admin plane ---

func TestAdminCreateNamespace(t *testing.T) {
	ts := buildServer(t, nil)
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodPost, "/_/namespaces", ts.rootToken, map[string]string{"name": "newone"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Namespace    string `json:"namespace"`
		OwnerToken   string `json:"owner_token"`
		OwnerTokenID string `json:"owner_token_id"`
	}
	decodeJSON(t, rec.Body, &resp)
	if resp.Namespace != "newone" || resp.OwnerToken == "" {
		t.Fatalf("bad response %+v", resp)
	}
	// owner token must be able to PUT
	rec = doPut(t, ts.srv, "", "/newone/x.txt", resp.OwnerToken, "body", "text/plain")
	if rec.Code != http.StatusCreated {
		t.Fatalf("owner PUT status = %d", rec.Code)
	}
}

func TestAdminCreateRequiresRoot(t *testing.T) {
	ts := buildServer(t, nil)
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodPost, "/_/namespaces", ts.ownerToken, map[string]string{"name": "newone"}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-root status = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodPost, "/_/namespaces", "", map[string]string{"name": "newone"}))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token status = %d", rec.Code)
	}
}

func TestAdminCreateRejectsLeadingUnderscore(t *testing.T) {
	ts := buildServer(t, nil)
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodPost, "/_/namespaces", ts.rootToken, map[string]string{"name": "_evil"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminCreateConflict(t *testing.T) {
	ts := buildServer(t, nil)
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodPost, "/_/namespaces", ts.rootToken, map[string]string{"name": "default"}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestAdminListNamespaces(t *testing.T) {
	ts := buildServer(t, nil)
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodGet, "/_/namespaces", ts.rootToken, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var items []map[string]any
	decodeJSON(t, rec.Body, &items)
	if len(items) != 1 {
		t.Fatalf("len = %d, want 1", len(items))
	}
}

func TestAdminDeleteNamespace(t *testing.T) {
	ts := buildServer(t, nil)
	rec := doPut(t, ts.srv, "", "/default/file.txt", ts.ownerToken, "x", "text/plain")
	if rec.Code != http.StatusCreated {
		t.Fatal()
	}
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodDelete, "/_/namespaces/default", ts.rootToken, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d body=%s", rec.Code, rec.Body.String())
	}
	if ts.authStore.HasNamespace("default") {
		t.Fatal("namespace must be gone")
	}
}

func TestAdminTokenCreate(t *testing.T) {
	ts := buildServer(t, nil)
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodPost, "/_/namespaces/default/tokens", ts.ownerToken, map[string]any{
		"name":        "ci",
		"permissions": []string{"read", "write"},
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Token       string   `json:"token"`
		Permissions []string `json:"permissions"`
	}
	decodeJSON(t, rec.Body, &resp)
	if resp.Token == "" {
		t.Fatal("missing plaintext token")
	}

	// sub-token cannot issue: 401 (unified with cross-namespace/anon to
	// avoid leaking that the sub-token has any admin-shaped privileges).
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodPost, "/_/namespaces/default/tokens", resp.Token, map[string]any{
		"name":        "evil",
		"permissions": []string{"read"},
	}))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("sub-token issue status = %d", rec.Code)
	}
}

func TestAdminTokenCreateRejectsOwnerPerm(t *testing.T) {
	ts := buildServer(t, nil)
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodPost, "/_/namespaces/default/tokens", ts.ownerToken, map[string]any{
		"name":        "evil",
		"permissions": []string{"owner"},
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminTokenList(t *testing.T) {
	ts := buildServer(t, nil)
	if _, err := ts.authStore.CreateToken("default", "ci", []string{"read"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodGet, "/_/namespaces/default/tokens", ts.ownerToken, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var tokens []map[string]any
	decodeJSON(t, rec.Body, &tokens)
	if len(tokens) != 2 {
		t.Fatalf("len = %d, want 2", len(tokens))
	}
}

func TestAdminTokenRevoke(t *testing.T) {
	ts := buildServer(t, nil)
	created, err := ts.authStore.CreateToken("default", "ci", []string{"read"})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodDelete, "/_/namespaces/default/tokens/"+created.TokenID, ts.ownerToken, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminTokenRevokeOwnerBlocked(t *testing.T) {
	ts := buildServer(t, nil)
	infos, err := ts.authStore.ListTokens("default")
	if err != nil {
		t.Fatal(err)
	}
	var ownerID string
	for _, ti := range infos {
		if ti.Owner {
			ownerID = ti.ID
		}
	}
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodDelete, "/_/namespaces/default/tokens/"+ownerID, ts.ownerToken, nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestAdminRotateOwner(t *testing.T) {
	ts := buildServer(t, nil)
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodPost, "/_/namespaces/default/rotate-owner", ts.rootToken, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OwnerToken string `json:"owner_token"`
	}
	decodeJSON(t, rec.Body, &resp)
	if resp.OwnerToken == "" {
		t.Fatal("missing new owner token")
	}
	// old owner token now rejected: 401 (token is unknown to the store).
	rec = doPut(t, ts.srv, "", "/default/x.txt", ts.ownerToken, "x", "text/plain")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("old owner status = %d", rec.Code)
	}
}

func TestDefaultIndexServedWithoutAdminAction(t *testing.T) {
	ts := buildServer(t, nil)
	// seed index.html under namespace root — no admin call needed
	if rec := doPut(t, ts.srv, "", "/default/index.html", ts.ownerToken, "<h1>root</h1>", "text/html"); rec.Code != http.StatusCreated {
		t.Fatalf("seed PUT status = %d", rec.Code)
	}
	if err := ts.authStore.SetPublicRead("default", true); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/default/", "/default"} {
		rec := httptest.NewRecorder()
		ts.srv.ServeHTTP(rec, authedReq(http.MethodGet, path, "", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "root") {
			t.Fatalf("%s body = %q", path, rec.Body.String())
		}
	}
}

func TestAdminIndexOverrideAndDisable(t *testing.T) {
	ts := buildServer(t, nil)
	if rec := doPut(t, ts.srv, "", "/default/index.html", ts.ownerToken, "<h1>root</h1>", "text/html"); rec.Code != http.StatusCreated {
		t.Fatalf("seed root PUT status = %d", rec.Code)
	}
	if rec := doPut(t, ts.srv, "", "/default/main.html", ts.ownerToken, "<h1>main</h1>", "text/html"); rec.Code != http.StatusCreated {
		t.Fatalf("seed main PUT status = %d", rec.Code)
	}
	if err := ts.authStore.SetPublicRead("default", true); err != nil {
		t.Fatal(err)
	}

	// override to main.html
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodPost, "/_/namespaces/default/index", ts.ownerToken, map[string]any{"file": "main.html"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("override status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, authedReq(http.MethodGet, "/default/", "", nil))
	if !strings.Contains(rec.Body.String(), "main") {
		t.Fatalf("override body = %q", rec.Body.String())
	}

	// reset to default by setting file=""
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodPost, "/_/namespaces/default/index", ts.ownerToken, map[string]any{"file": ""}))
	if rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, authedReq(http.MethodGet, "/default/", "", nil))
	if !strings.Contains(rec.Body.String(), "root") {
		t.Fatalf("default body after reset = %q", rec.Body.String())
	}

	// disable entirely — anon GET / now requires token, falls through to handleList
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodPost, "/_/namespaces/default/index", ts.ownerToken, map[string]any{"disabled": true}))
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, authedReq(http.MethodGet, "/default/", "", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled anon root status = %d, want 401 (listing requires token)", rec.Code)
	}
}

func TestAdminSetIndexRejectsBadValue(t *testing.T) {
	ts := buildServer(t, nil)
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodPost, "/_/namespaces/default/index", ts.ownerToken, map[string]string{"file": "../etc/passwd"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminSetPublic(t *testing.T) {
	ts := buildServer(t, nil)
	// seed
	if rec := doPut(t, ts.srv, "", "/default/p.txt", ts.ownerToken, "body", "text/plain"); rec.Code != http.StatusCreated {
		t.Fatalf("seed PUT status = %d", rec.Code)
	}
	// default is public — anon read works
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, authedReq(http.MethodGet, "/default/p.txt", "", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("default-public anon read status = %d, want 200", rec.Code)
	}
	// flip public off
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodPost, "/_/namespaces/default/public", ts.ownerToken, map[string]bool{"on": false}))
	if rec.Code != http.StatusOK {
		t.Fatalf("set public off status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, authedReq(http.MethodGet, "/default/p.txt", "", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("after public off, anon read status = %d, want 401", rec.Code)
	}
	// flip back on
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodPost, "/_/namespaces/default/public", ts.ownerToken, map[string]bool{"on": true}))
	if rec.Code != http.StatusOK {
		t.Fatalf("set public on status = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, authedReq(http.MethodGet, "/default/p.txt", "", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("anon read after re-enable status = %d", rec.Code)
	}
}

func TestNamespaceCreateReportsDefaults(t *testing.T) {
	ts := buildServer(t, nil)
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodPost, "/_/namespaces", ts.rootToken, map[string]string{"name": "fresh"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Namespace  string `json:"namespace"`
		PublicRead bool   `json:"public_read"`
		IndexFile  string `json:"index_file"`
	}
	decodeJSON(t, rec.Body, &resp)
	if !resp.PublicRead {
		t.Fatal("new namespace should be public_read=true")
	}
	if resp.IndexFile != "index.html" {
		t.Fatalf("index_file = %q, want index.html", resp.IndexFile)
	}
}

func TestAdminListingStillRequiresToken(t *testing.T) {
	ts := buildServer(t, nil)
	if err := ts.authStore.SetPublicRead("default", true); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, authedReq(http.MethodGet, "/default", "", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("listing unauth status = %d", rec.Code)
	}
}

func TestAdminRejectInvalidNamespace(t *testing.T) {
	ts := buildServer(t, nil)
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, adminReq(http.MethodPost, "/_/namespaces/_bad/tokens", ts.rootToken, map[string]any{"name": "x", "permissions": []string{"read"}}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

// TestBaseRootDoesNotRedirect guards against a regression where Go's
// ServeMux returned 301 -> "/" on a bare base-host request and caused an
// infinite redirect loop in browsers.
func TestBaseRootDoesNotRedirect(t *testing.T) {
	ts := buildServer(t, nil)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := httptest.NewRecorder()
		ts.srv.ServeHTTP(rec, authedReq(method, "/", "", nil))
		if rec.Code == http.StatusMovedPermanently || rec.Code == http.StatusFound {
			t.Fatalf("%s / returned redirect %d (Location=%q); want 404", method, rec.Code, rec.Header().Get("Location"))
		}
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s / status = %d, want 404", method, rec.Code)
		}
	}
}

func TestAdminUnderscorePrefixPathFallback(t *testing.T) {
	ts := buildServer(t, nil)
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, authedReq(http.MethodGet, "/_/something/random", ts.rootToken, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

// --- subdomain ---

func TestSubdomainRouting(t *testing.T) {
	ts := buildServer(t, withSubdomain)
	rec := doPut(t, ts.srv, "default.example.test", "/docs/hello.txt", ts.ownerToken, "hi", "text/plain")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSubdomainUnderscoreNotMapped(t *testing.T) {
	ts := buildServer(t, withSubdomain)
	// _evil.example.test should not map to namespace "_evil"
	if ns, ok := ts.srv.namespaceFromHost("_evil.example.test"); ok {
		t.Fatalf("namespace must not be %q", ns)
	}
}

// --- benchmarks ---

func BenchmarkServeHTTPGet(b *testing.B) {
	ts := buildServer(b, nil)
	if rec := doPut(b, ts.srv, "", "/default/x.txt", ts.ownerToken, "body", "text/plain"); rec.Code != http.StatusCreated {
		b.Fatal()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		ts.srv.ServeHTTP(rec, authedReq(http.MethodGet, "/default/x.txt", ts.ownerToken, nil))
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d", rec.Code)
		}
	}
}

func BenchmarkServeHTTPPut(b *testing.B) {
	ts := buildServer(b, nil)
	body := strings.Repeat("a", 256)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := doPut(b, ts.srv, "", "/default/bench.bin", ts.ownerToken, body, "application/octet-stream")
		if rec.Code != http.StatusCreated {
			b.Fatalf("status = %d", rec.Code)
		}
	}
}

func BenchmarkNamespaceFromHost(b *testing.B) {
	srv := &Server{hostSuffix: ".example.test"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if ns, ok := srv.namespaceFromHost("default.example.test"); !ok || ns != "default" {
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
