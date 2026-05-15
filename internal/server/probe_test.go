package server

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProbeNoRedirects probes a battery of method/path combinations to
// guard against any redirect surprises in the mux. Every response must be
// non-3xx; this is a regression sweep for the kind of bug that produced
// the "/" -> 301 -> "/" loop.
func TestProbeNoRedirects(t *testing.T) {
	ts := buildServer(t, nil)
	cases := []struct{ method, path string }{
		{"GET", "/"},
		{"GET", "/healthz"},
		{"GET", "/healthz/"},
		{"HEAD", "/healthz"},
		{"POST", "/healthz"},
		{"GET", "/_/"},
		{"GET", "/_"},
		{"GET", "/_/namespaces"},
		{"GET", "/_/namespaces/"},
		{"POST", "/_/namespaces"},
		{"POST", "/_/namespaces/"},
		{"GET", "/_/namespaces/foo"},
		{"GET", "/_/namespaces/foo/"},
		{"GET", "/_/namespaces/foo/tokens"},
		{"GET", "/_/namespaces/foo/tokens/"},
		{"GET", "/_/namespaces/foo/tokens/abc"},
		{"DELETE", "/_/namespaces/foo/tokens/abc"},
		{"DELETE", "/_/namespaces/foo/tokens/"},
		{"POST", "/_/namespaces/foo/rotate-owner"},
		{"POST", "/_/namespaces/foo/rotate-owner/"},
		{"POST", "/_/namespaces/foo/public"},
		{"POST", "/_/namespaces/foo/index"},
		{"POST", "/_/namespaces/foo/unknown-subaction"},
		{"GET", "/default"},
		{"GET", "/default/"},
		{"GET", "/default/file.txt"},
		{"GET", "/default/file.txt/"},
		{"GET", "/default/sub/"},
		{"OPTIONS", "/default"},
		{"OPTIONS", "/default/file"},
		{"OPTIONS", "/_/namespaces"},
		{"PROPFIND", "/default/file"},
		// note: '/./default' would 301 to '/default' via Go's standard
		// path-cleanup — that's a one-shot canonicalisation, not a loop,
		// so it's intentionally NOT in this list.
	}
	var report []string
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "http://localhost"+c.path, nil)
		r.Host = "localhost"
		rw := httptest.NewRecorder()
		ts.srv.ServeHTTP(rw, r)
		loc := rw.Header().Get("Location")
		if rw.Code >= 300 && rw.Code < 400 {
			report = append(report, fmt.Sprintf("REDIRECT %-7s %-45s -> %d Location=%q", c.method, c.path, rw.Code, loc))
		}
	}
	if len(report) > 0 {
		t.Fatalf("found %d redirects:\n%s", len(report), strings.Join(report, "\n"))
	}
}
