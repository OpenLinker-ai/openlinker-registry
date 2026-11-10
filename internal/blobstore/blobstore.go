// Package blobstore is a content-addressed byte store with two areas:
// permanent artifacts for hosted versions and an evictable cache for indexed
// content. The Store interface is small so an S3-compatible backend can be
// added later without touching callers.
package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Area selects the storage area.
type Area string

const (
	// Artifacts are permanent (hosted versions).
	Artifacts Area = "artifacts"
	// Cache is evictable (indexed content); misses are refilled from source.
	Cache Area = "cache"
)

// ErrNotFound is returned for missing blobs.
var ErrNotFound = errors.New("blob not found")

// ErrCorrupt is returned when stored bytes do not match their digest.
var ErrCorrupt = errors.New("blob digest mismatch")

// Store is a content-addressed blob store. Digests are "sha256:<hex>".
type Store interface {
	// Put stores data and returns its digest. Storing existing content only
	// refreshes its modification time, which garbage collection uses as a
	// grace period so a blob being (re)published is never collected.
	Put(ctx context.Context, area Area, data []byte) (string, error)
	// Get returns the bytes for digest after verifying them.
	Get(ctx context.Context, area Area, digest string) ([]byte, error)
	// Evict removes cache entries older than maxAge, then the oldest entries
	// until the cache is at most maxBytes. It never touches Artifacts.
	Evict(ctx context.Context, maxAge time.Duration, maxBytes int64) (int, error)
	// Older lists digests in area last written or refreshed before t.
	Older(ctx context.Context, area Area, t time.Time) ([]string, error)
	// DeleteIfOlder removes a blob only if it was not written or refreshed
	// since t (re-checked immediately before removal); missing is not an error.
	DeleteIfOlder(ctx context.Context, area Area, digest string, t time.Time) (bool, error)
}

var digestRe = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)

// Local stores blobs on local disk under <dir>/<area>/sha256/<aa>/<hex>.
type Local struct{ dir string }

// NewLocal creates the directory layout.
func NewLocal(dir string) (*Local, error) {
	for _, a := range []Area{Artifacts, Cache} {
		if err := os.MkdirAll(filepath.Join(dir, string(a), "sha256"), 0o750); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Join(dir, string(a), "tmp"), 0o750); err != nil {
			return nil, err
		}
	}
	return &Local{dir: dir}, nil
}

func (l *Local) path(area Area, digest string) (string, error) {
	m := digestRe.FindStringSubmatch(digest)
	if m == nil || (area != Artifacts && area != Cache) {
		return "", fmt.Errorf("invalid blob reference")
	}
	return filepath.Join(l.dir, string(area), "sha256", m[1][:2], m[1]), nil
}

// Put implements Store.
func (l *Local) Put(_ context.Context, area Area, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	p, err := l.path(area, digest)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(p); err == nil {
		now := time.Now()
		if err := os.Chtimes(p, now, now); err != nil {
			return "", err
		}
		return digest, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return "", err
	}
	tmpDir := filepath.Join(l.dir, string(area), "tmp")
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(tmpDir, "put-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o640); err != nil {
		return "", err
	}
	return digest, os.Rename(tmp.Name(), p)
}

// Get implements Store.
func (l *Local) Get(_ context.Context, area Area, digest string) ([]byte, error) {
	p, err := l.path(area, digest)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if "sha256:"+hex.EncodeToString(sum[:]) != digest {
		if area == Cache {
			_ = os.Remove(p)
			return nil, ErrNotFound
		}
		return nil, ErrCorrupt
	}
	if area == Cache {
		now := time.Now()
		_ = os.Chtimes(p, now, now)
	}
	return data, nil
}

// Evict implements Store. The cache's modification time is refreshed on
// every hit, so eviction is least-recently-used.
func (l *Local) Evict(_ context.Context, maxAge time.Duration, maxBytes int64) (int, error) {
	type item struct {
		path string
		size int64
		mod  time.Time
	}
	root := filepath.Join(l.dir, string(Cache), "sha256")
	var items []item
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		items = append(items, item{p, info.Size(), info.ModTime()})
		return nil
	})
	if err != nil {
		return 0, err
	}
	slices.SortFunc(items, func(a, b item) int { return a.mod.Compare(b.mod) })
	var total int64
	for _, it := range items {
		total += it.size
	}
	removed := 0
	cutoff := time.Now().Add(-maxAge)
	for _, it := range items {
		if (maxAge > 0 && it.mod.Before(cutoff)) || (maxBytes >= 0 && total > maxBytes) {
			if err := os.Remove(it.path); err == nil || errors.Is(err, fs.ErrNotExist) {
				removed++
				total -= it.size
			}
		}
	}
	return removed, nil
}

// Older implements Store.
func (l *Local) Older(_ context.Context, area Area, t time.Time) ([]string, error) {
	if area != Artifacts && area != Cache {
		return nil, fmt.Errorf("invalid area")
	}
	var out []string
	root := filepath.Join(l.dir, string(area), "sha256")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		digest := "sha256:" + d.Name()
		if digestRe.MatchString(digest) && info.ModTime().Before(t) {
			out = append(out, digest)
		}
		return nil
	})
	return out, err
}

// DeleteIfOlder implements Store.
func (l *Local) DeleteIfOlder(_ context.Context, area Area, digest string, t time.Time) (bool, error) {
	p, err := l.path(area, digest)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.ModTime().Before(t) {
		return false, nil
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return true, nil
}

// IsDigest reports whether s is a well-formed sha256 digest reference.
func IsDigest(s string) bool { return digestRe.MatchString(s) && !strings.ContainsAny(s, "/\\") }
