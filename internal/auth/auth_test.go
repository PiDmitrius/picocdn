package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCreateNamespaceAndAuthorize(t *testing.T) {
	file := emptyFile()
	created, err := CreateNamespace(file, "default")
	if err != nil {
		t.Fatal(err)
	}
	if created.Token == "" {
		t.Fatal("expected plaintext token")
	}
	if !file.Authorize("default", created.Token, "write") {
		t.Fatal("owner token should authorize write")
	}
	if !file.Authorize("default", created.Token, "read") {
		t.Fatal("owner token should authorize read")
	}
	if file.Authorize("default", "wrong", "read") {
		t.Fatal("wrong token should not authorize")
	}

	stored := file.Namespaces["default"].Tokens[0]
	if stored.TokenHash == "" || stored.TokenHash == created.Token {
		t.Fatal("stored token must be hashed")
	}
}

func TestRejectsInvalidNamespaceNames(t *testing.T) {
	invalid := []string{"Upper", "under_score", "has.dot", "-start", "end-"}
	for _, namespace := range invalid {
		t.Run(namespace, func(t *testing.T) {
			_, err := CreateNamespace(emptyFile(), namespace)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestTokenPermissions(t *testing.T) {
	file := emptyFile()
	if _, err := CreateNamespace(file, "default"); err != nil {
		t.Fatal(err)
	}
	created, err := CreateToken(file, "default", "reader", []string{"read"})
	if err != nil {
		t.Fatal(err)
	}
	if !file.Authorize("default", created.Token, "read") {
		t.Fatal("reader token should authorize read")
	}
	if file.Authorize("default", created.Token, "write") {
		t.Fatal("reader token should not authorize write")
	}
}

func TestListAndRevokeToken(t *testing.T) {
	file := emptyFile()
	owner, err := CreateNamespace(file, "default")
	if err != nil {
		t.Fatal(err)
	}
	created, err := CreateToken(file, "default", "reader", []string{"read"})
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := ListTokens(file, "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 {
		t.Fatalf("len(tokens) = %d, want 2", len(tokens))
	}
	if err := RevokeToken(file, "default", created.TokenID); err != nil {
		t.Fatal(err)
	}
	if file.Authorize("default", created.Token, "read") {
		t.Fatal("revoked token should not authorize")
	}
	if err := RevokeToken(file, "default", owner.TokenID); err == nil {
		t.Fatal("owner token revoke should fail")
	}
}

func TestReloaderRefusesEmptyOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	file := emptyFile()
	if _, err := CreateNamespace(file, "default"); err != nil {
		t.Fatal(err)
	}
	if err := SaveFile(path, file); err != nil {
		t.Fatal(err)
	}
	r, err := NewReloader(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	empty := emptyFile()
	if err := SaveFile(path, empty); err != nil {
		t.Fatal(err)
	}
	if err := r.ForceReload(); err == nil {
		t.Fatal("expected reload to refuse replacing non-empty auth with empty")
	}
	if !r.HasNamespace("default") {
		t.Fatal("expected default namespace to remain after refused reload")
	}
}

func TestReloaderKeepsLastGoodOnDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	file := emptyFile()
	if _, err := CreateNamespace(file, "default"); err != nil {
		t.Fatal(err)
	}
	if err := SaveFile(path, file); err != nil {
		t.Fatal(err)
	}
	r, err := NewReloader(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := r.MaybeReload(); err == nil {
		t.Fatal("expected error when file vanished")
	}
	if !r.HasNamespace("default") {
		t.Fatal("expected default namespace to remain")
	}
}

func BenchmarkAuthorize(b *testing.B) {
	file := emptyFile()
	_, _ = CreateNamespace(file, "default")
	var last *CreatedToken
	for i := 0; i < 64; i++ {
		ct, _ := CreateToken(file, "default", "extra", []string{"read"})
		last = ct
	}
	r, err := NewReloader("/tmp/picocdn-bench-auth.json", nil)
	if err != nil {
		b.Fatal(err)
	}
	r.current.Store(compileFile(file))
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !r.Authorize("default", last.Token, "read") {
			b.Fatal("expected authorize")
		}
	}
}

func BenchmarkFileAuthorize(b *testing.B) {
	file := emptyFile()
	_, _ = CreateNamespace(file, "default")
	var last *CreatedToken
	for i := 0; i < 64; i++ {
		ct, _ := CreateToken(file, "default", "extra", []string{"read"})
		last = ct
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !file.Authorize("default", last.Token, "read") {
			b.Fatal("expected authorize")
		}
	}
}

func BenchmarkHashToken(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = HashToken("pcd_abcdefghijklmnopqrstuvwxyz")
	}
}

func TestSaveAndLoadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	file := emptyFile()
	created, err := CreateNamespace(file, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveFile(path, file); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("auth file mode = %v, want 0600", info.Mode().Perm())
	}
	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Authorize("default", created.Token, "admin") {
		t.Fatal("loaded auth should authorize owner token")
	}
}
