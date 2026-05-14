package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPutAndGetObject(t *testing.T) {
	s := New(t.TempDir())
	obj, err := s.PutObject(context.Background(), strings.NewReader("hello"), PutOptions{
		Namespace:   "default",
		Path:        "/docs/hello.txt",
		ContentType: "text/plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("hello"))
	wantHash := hex.EncodeToString(sum[:])
	if obj.Hash != wantHash {
		t.Fatalf("hash = %s, want %s", obj.Hash, wantHash)
	}
	if _, err := os.Stat(obj.BlobPath); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetObject("default", "/docs/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got.Hash != obj.Hash || got.Size != 5 || got.ContentType != "text/plain" {
		t.Fatalf("unexpected object: %+v", got)
	}
}

func TestRejectsInvalidPaths(t *testing.T) {
	s := New(t.TempDir())
	invalid := []string{
		"",
		"/",
		"../secret.txt",
		"/docs/../secret.txt",
		`docs\secret.txt`,
	}
	for _, objectPath := range invalid {
		t.Run(objectPath, func(t *testing.T) {
			_, err := s.PutObject(context.Background(), strings.NewReader("x"), PutOptions{
				Namespace: "default",
				Path:      objectPath,
			})
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestGetObjectRejectsTraversalHash(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	maliciousHashes := []string{
		strings.Repeat("g", 64),
		"../../../../../../../../../../../etc/passwd_xxxxxxxxxxxxxxxxxxxx",
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"shorthash",
	}
	for _, hash := range maliciousHashes {
		t.Run(hash, func(t *testing.T) {
			obj := Object{
				Namespace: "default",
				Path:      "/x.txt",
				Hash:      hash,
				Algo:      "sha256",
			}
			aliasPath := filepath.Join(dir, "aliases", "default", "x.txt.json")
			if err := os.MkdirAll(filepath.Dir(aliasPath), 0o755); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(obj)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(aliasPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.GetObject("default", "/x.txt"); err == nil {
				t.Fatal("expected error for malicious hash")
			}
		})
	}
}

func TestGetObjectRejectsAliasPathTraversal(t *testing.T) {
	s := New(t.TempDir())
	invalid := []string{
		"../../etc/passwd",
		"/../../etc/passwd",
		"foo/../../bar",
	}
	for _, p := range invalid {
		t.Run(p, func(t *testing.T) {
			if _, err := s.GetObject("default", p); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestGCSweepsOrphanBlob(t *testing.T) {
	s := New(t.TempDir())
	obj, err := s.PutObject(context.Background(), strings.NewReader("gc-test"), PutOptions{
		Namespace: "default",
		Path:      "/x.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteObject("default", "/x.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(obj.BlobPath); err != nil {
		t.Fatalf("blob should still exist before GC: %v", err)
	}
	deleted, _, err := s.GC(0)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 blob deleted, got %d", deleted)
	}
	if _, err := os.Stat(obj.BlobPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("blob should be gone after GC")
	}
}

func BenchmarkGetObject(b *testing.B) {
	s := New(b.TempDir())
	if _, err := s.PutObject(context.Background(), strings.NewReader("hello"), PutOptions{
		Namespace: "default",
		Path:      "/h.txt",
	}); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := s.GetObject("default", "/h.txt"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPutObjectSmall(b *testing.B) {
	body := []byte(strings.Repeat("a", 256))
	for i := 0; i < b.N; i++ {
		dir := b.TempDir()
		s := New(dir)
		if _, err := s.PutObject(nil, strings.NewReader(string(body)), PutOptions{
			Namespace: "default",
			Path:      "/x.txt",
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkIsHex64(b *testing.B) {
	h := strings.Repeat("a", 64)
	for i := 0; i < b.N; i++ {
		if !isHex64(h) {
			b.Fatal("want true")
		}
	}
}

func BenchmarkIsValidNamespace(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if !isValidNamespace("default") {
			b.Fatal("want true")
		}
	}
}

func TestRejectsInvalidNamespace(t *testing.T) {
	s := New(t.TempDir())
	_, err := s.PutObject(context.Background(), strings.NewReader("x"), PutOptions{
		Namespace: "has.dot",
		Path:      "/x.txt",
	})
	if err == nil {
		t.Fatal("expected error")
	}
}
