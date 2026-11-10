package blobstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalPutGetEvict(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l, err := NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	d, err := l.Put(ctx, Artifacts, []byte("hello"))
	if err != nil || !IsDigest(d) {
		t.Fatalf("%s %v", d, err)
	}
	if b, err := l.Get(ctx, Artifacts, d); err != nil || string(b) != "hello" {
		t.Fatal("get failed")
	}
	if _, err := l.Get(ctx, Artifacts, "sha256:../../etc"); err == nil {
		t.Fatal("bad digest accepted")
	}
	// Corrupt artifacts are reported, not served.
	p, _ := l.path(Artifacts, d)
	os.WriteFile(p, []byte("evil!"), 0o640)
	if _, err := l.Get(ctx, Artifacts, d); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got %v", err)
	}

	old, _ := l.Put(ctx, Cache, []byte(strings.Repeat("o", 100)))
	newer, _ := l.Put(ctx, Cache, []byte(strings.Repeat("n", 100)))
	op, _ := l.path(Cache, old)
	past := time.Now().Add(-48 * time.Hour)
	os.Chtimes(op, past, past)
	n, err := l.Evict(ctx, 24*time.Hour, 1<<20)
	if err != nil || n != 1 {
		t.Fatalf("evict by age: %d %v", n, err)
	}
	if _, err := l.Get(ctx, Cache, old); !errors.Is(err, ErrNotFound) {
		t.Fatal("old entry survived")
	}
	if _, err := l.Get(ctx, Cache, newer); err != nil {
		t.Fatal("new entry evicted")
	}
	if n, _ := l.Evict(ctx, 24*time.Hour, 0); n != 1 {
		t.Fatal("capacity eviction failed")
	}
	// Artifacts are never evicted.
	if _, err := os.Stat(filepath.Join(dir, "artifacts")); err != nil {
		t.Fatal(err)
	}
}
