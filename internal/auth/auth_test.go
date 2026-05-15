package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T, roots ...RootToken) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(dir, roots)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s, dir
}

func TestCreateNamespaceOwnerToken(t *testing.T) {
	s, dir := newStore(t)
	created, err := s.CreateNamespace("default")
	if err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	if created.Token == "" || !strings.HasPrefix(created.Token, namespaceTokenPrefix) {
		t.Fatalf("expected plaintext token with prefix, got %q", created.Token)
	}
	if !created.Owner {
		t.Fatal("created token must be marked as owner")
	}
	actor, ok := s.Authorize("default", created.Token, "write")
	if !ok || actor.Kind != "owner" {
		t.Fatalf("owner token should authorize write as owner, got %+v ok=%v", actor, ok)
	}
	actor, ok = s.Authorize("default", created.Token, "delete")
	if !ok || actor.Kind != "owner" {
		t.Fatalf("owner should authorize delete, got %+v ok=%v", actor, ok)
	}
	if _, ok := s.Authorize("default", "garbage", "read"); ok {
		t.Fatal("garbage token must not authorize")
	}

	path := filepath.Join(dir, "default.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat namespace file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("namespace file mode = %v, want 0600", info.Mode().Perm())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), created.Token) {
		t.Fatal("plaintext token must not be persisted")
	}
}

func TestRejectInvalidNamespaceNames(t *testing.T) {
	s, _ := newStore(t)
	invalid := []string{"", "Upper", "_admin", "1starts-with-digit", "-leading-dash", "trailing-dash-", "under_score", "has.dot", strings.Repeat("a", 64)}
	for _, name := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := s.CreateNamespace(name); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestSubTokenCannotHaveOwnerPermission(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.CreateNamespace("ns"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateToken("ns", "evil", []string{"owner"}); err == nil {
		t.Fatal("sub-token with 'owner' permission must be rejected")
	}
}

func TestCreateTokenAndRevoke(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.CreateNamespace("ns"); err != nil {
		t.Fatal(err)
	}
	created, err := s.CreateToken("ns", "ci", []string{"read", "write"})
	if err != nil {
		t.Fatal(err)
	}
	if actor, ok := s.Authorize("ns", created.Token, "write"); !ok || actor.Kind != "sub" {
		t.Fatalf("sub-token should authorize write as sub, got %+v ok=%v", actor, ok)
	}
	if _, ok := s.Authorize("ns", created.Token, "delete"); ok {
		t.Fatal("sub-token without delete must not authorize delete")
	}
	if err := s.RevokeToken("ns", created.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Authorize("ns", created.Token, "read"); ok {
		t.Fatal("revoked token must not authorize")
	}
}

func TestRevokeOwnerForbidden(t *testing.T) {
	s, _ := newStore(t)
	created, err := s.CreateNamespace("ns")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeToken("ns", created.TokenID); err == nil {
		t.Fatal("revoking owner token must fail")
	}
}

func TestRotateOwnerInvalidatesOld(t *testing.T) {
	s, _ := newStore(t)
	first, err := s.CreateNamespace("ns")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.RotateOwner("ns")
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == second.Token {
		t.Fatal("rotated owner must be different")
	}
	if _, ok := s.Authorize("ns", first.Token, "read"); ok {
		t.Fatal("old owner token must not authorize after rotation")
	}
	if actor, ok := s.Authorize("ns", second.Token, "write"); !ok || actor.Kind != "owner" {
		t.Fatalf("new owner token must authorize as owner, got %+v ok=%v", actor, ok)
	}
}

func TestIndexFileDefault(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.CreateNamespace("ns"); err != nil {
		t.Fatal(err)
	}
	if got := s.IndexFile("ns"); got != DefaultIndexFile {
		t.Fatalf("default index = %q, want %q", got, DefaultIndexFile)
	}
}

func TestSetIndexOverride(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.CreateNamespace("ns"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIndex("ns", "main.html", false); err != nil {
		t.Fatal(err)
	}
	if got := s.IndexFile("ns"); got != "main.html" {
		t.Fatalf("after override: %q", got)
	}
	// reset to default with empty file
	if err := s.SetIndex("ns", "", false); err != nil {
		t.Fatal(err)
	}
	if got := s.IndexFile("ns"); got != DefaultIndexFile {
		t.Fatalf("after reset: %q, want default", got)
	}
}

func TestSetIndexDisable(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.CreateNamespace("ns"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIndex("ns", "", true); err != nil {
		t.Fatal(err)
	}
	if got := s.IndexFile("ns"); got != "" {
		t.Fatalf("after disable: %q, want empty", got)
	}
	// re-enable with default
	if err := s.SetIndex("ns", "", false); err != nil {
		t.Fatal(err)
	}
	if got := s.IndexFile("ns"); got != DefaultIndexFile {
		t.Fatalf("after re-enable: %q, want default", got)
	}
}

func TestSetIndexRejectsBadFile(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.CreateNamespace("ns"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"..", "../etc", "a/b", "with space", "\x00"} {
		t.Run("bad="+bad, func(t *testing.T) {
			if err := s.SetIndex("ns", bad, false); err == nil {
				t.Fatalf("expected error for %q", bad)
			}
		})
	}
}

func TestIndexPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateNamespace("ns"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIndex("ns", "main.html", false); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.IndexFile("ns"); got != "main.html" {
		t.Fatalf("after reload: %q", got)
	}
}

func TestIndexDisabledPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateNamespace("ns"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIndex("ns", "", true); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.IndexFile("ns"); got != "" {
		t.Fatalf("after reload disabled: %q, want empty", got)
	}
}

func TestSetPublicRead(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.CreateNamespace("ns"); err != nil {
		t.Fatal(err)
	}
	if !s.IsPublicRead("ns") {
		t.Fatal("default must be public")
	}
	if err := s.SetPublicRead("ns", false); err != nil {
		t.Fatal(err)
	}
	if s.IsPublicRead("ns") {
		t.Fatal("public_read should be cleared")
	}
	if err := s.SetPublicRead("ns", true); err != nil {
		t.Fatal(err)
	}
	if !s.IsPublicRead("ns") {
		t.Fatal("public_read should be set back")
	}
}

func TestRootAuthorizesEverything(t *testing.T) {
	rootPlain, rootMeta, err := NewRootToken("ops")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := newStore(t, rootMeta)
	if _, err := s.CreateNamespace("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateNamespace("b"); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"a", "b"} {
		for _, p := range []string{"read", "write", "delete"} {
			actor, ok := s.Authorize(ns, rootPlain, p)
			if !ok || actor.Kind != "root" {
				t.Fatalf("root should authorize %s on %s, got %+v ok=%v", p, ns, actor, ok)
			}
		}
		if actor, ok := s.AuthorizeNamespaceAdmin(ns, rootPlain); !ok || actor.Kind != "root" {
			t.Fatalf("root should authorize admin on %s, got %+v ok=%v", ns, actor, ok)
		}
	}
	if actor, ok := s.AuthorizeRoot(rootPlain); !ok || actor.Kind != "root" {
		t.Fatalf("AuthorizeRoot should pass, got %+v ok=%v", actor, ok)
	}
	if _, ok := s.AuthorizeRoot("garbage"); ok {
		t.Fatal("garbage must not pass root check")
	}
}

func TestSubTokenCannotAdmin(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.CreateNamespace("ns"); err != nil {
		t.Fatal(err)
	}
	sub, err := s.CreateToken("ns", "ci", []string{"read", "write"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.AuthorizeNamespaceAdmin("ns", sub.Token); ok {
		t.Fatal("sub-token must not pass namespace admin check")
	}
	if _, ok := s.AuthorizeRoot(sub.Token); ok {
		t.Fatal("sub-token must not pass root check")
	}
}

func TestOwnerCanAdmin(t *testing.T) {
	s, _ := newStore(t)
	owner, err := s.CreateNamespace("ns")
	if err != nil {
		t.Fatal(err)
	}
	actor, ok := s.AuthorizeNamespaceAdmin("ns", owner.Token)
	if !ok || actor.Kind != "owner" {
		t.Fatalf("owner should pass admin check, got %+v ok=%v", actor, ok)
	}
}

func TestPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.CreateNamespace("foo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateToken("foo", "ci", []string{"read"}); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.HasNamespace("foo") {
		t.Fatal("namespace must survive reload")
	}
	if _, ok := reloaded.Authorize("foo", owner.Token, "write"); !ok {
		t.Fatal("owner token must still authorize after reload")
	}
}

func TestFailFastOnBadJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(dir, nil); err == nil {
		t.Fatal("expected NewStore to fail on bad JSON")
	}
}

func TestFailFastOnFilenameMismatch(t *testing.T) {
	dir := t.TempDir()
	// Create a valid namespace then rename the file so name differs.
	s, err := NewStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateNamespace("alpha"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "alpha.json"), filepath.Join(dir, "beta.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(dir, nil); err == nil {
		t.Fatal("expected NewStore to fail when filename does not match namespace name")
	}
}

func TestFailFastOnMissingOwner(t *testing.T) {
	dir := t.TempDir()
	// Hand-craft a namespace file without owner token.
	ns := Namespace{
		Version:      1,
		Name:         "broken",
		OwnerTokenID: "doesnotexist",
		Tokens: []Token{
			{ID: "x", Name: "x", TokenHash: HashToken("pcd_anything"), Permissions: []string{"read"}},
		},
	}
	data, _ := json.Marshal(&ns)
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(dir, nil); err == nil {
		t.Fatal("expected NewStore to fail when owner_token_id not in tokens")
	}
}

func TestFailFastOnDuplicateRootToken(t *testing.T) {
	plain, rt, err := NewRootToken("ops")
	if err != nil {
		t.Fatal(err)
	}
	_ = plain
	rt2 := rt
	rt2.ID = "different"
	dir := t.TempDir()
	if _, err := NewStore(dir, []RootToken{rt, rt2}); err == nil {
		t.Fatal("expected NewStore to reject duplicate root token hash")
	}
}

func TestDeleteNamespace(t *testing.T) {
	s, dir := newStore(t)
	if _, err := s.CreateNamespace("doomed"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNamespace("doomed"); err != nil {
		t.Fatal(err)
	}
	if s.HasNamespace("doomed") {
		t.Fatal("namespace must be gone in memory")
	}
	if _, err := os.Stat(filepath.Join(dir, "doomed.json")); !os.IsNotExist(err) {
		t.Fatalf("file should be gone, got err=%v", err)
	}
}

func TestActorString(t *testing.T) {
	cases := []struct {
		actor Actor
		want  string
	}{
		{Actor{Kind: "root", TokenID: "r1"}, "root:r1"},
		{Actor{Kind: "owner", TokenID: "o1"}, "owner:o1"},
		{Actor{Kind: "sub", TokenID: "s1"}, "sub:s1"},
		{Actor{Kind: "anon"}, "anon"},
		{Actor{}, "unknown"},
	}
	for _, c := range cases {
		if got := c.actor.String(); got != c.want {
			t.Errorf("Actor{%+v}.String() = %q, want %q", c.actor, got, c.want)
		}
	}
}

func TestListTokensReportsOwner(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.CreateNamespace("ns"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateToken("ns", "ci", []string{"read"}); err != nil {
		t.Fatal(err)
	}
	tokens, err := s.ListTokens("ns")
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 {
		t.Fatalf("expected 2 tokens, got %d", len(tokens))
	}
	owners := 0
	for _, t := range tokens {
		if t.Owner {
			owners++
		}
	}
	if owners != 1 {
		t.Fatalf("expected exactly 1 owner, got %d", owners)
	}
}

func BenchmarkAuthorize(b *testing.B) {
	dir := b.TempDir()
	s, err := NewStore(dir, nil)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := s.CreateNamespace("default"); err != nil {
		b.Fatal(err)
	}
	var last *CreatedToken
	for i := 0; i < 64; i++ {
		ct, err := s.CreateToken("default", "extra", []string{"read"})
		if err != nil {
			b.Fatal(err)
		}
		last = ct
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := s.Authorize("default", last.Token, "read"); !ok {
			b.Fatal("expected authorize")
		}
	}
}

func BenchmarkAuthorizeRoot(b *testing.B) {
	plain, rt, err := NewRootToken("ops")
	if err != nil {
		b.Fatal(err)
	}
	dir := b.TempDir()
	s, err := NewStore(dir, []RootToken{rt})
	if err != nil {
		b.Fatal(err)
	}
	if _, err := s.CreateNamespace("default"); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := s.Authorize("default", plain, "write"); !ok {
			b.Fatal("expected authorize")
		}
	}
}

func BenchmarkHashToken(b *testing.B) {
	const tok = "pcd_abcdefghijklmnopqrstuvwxyz012345"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = HashToken(tok)
	}
}
