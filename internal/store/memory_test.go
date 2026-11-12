package store_test

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store/storetest"
)

func TestMemoryConformance(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return store.NewMemory() })
}

func TestLatestMCPVersion(t *testing.T) {
	t0 := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	// Each case lists versions in publication order; "-" marks a deleted one.
	for _, c := range []struct {
		versions []string
		want     string
	}{
		{nil, ""},
		{[]string{"-1.0.0", "-2.0.0"}, ""},
		{[]string{"1.0.0"}, "1.0.0"},
		{[]string{"1.10.0", "1.2.0", "1.9.9"}, "1.10.0"},
		{[]string{"1.0.0", "-2.0.0", "1.5.0", "1.4.0"}, "1.5.0"},
		{[]string{"2.0.0", "2.0.0-rc.1"}, "2.0.0"},
		// Equal precedence (build metadata) and incomparable versions: the
		// newest one.
		{[]string{"1.0.0+a", "1.0.0+b"}, "1.0.0+b"},
		{[]string{"mon", "tue", "-wed"}, "tue"},
		// Mixed: the publish rule replayed in order. "nightly" replaces
		// 2.0.0 (they do not compare), then 1.0.0 replaces "nightly".
		{[]string{"2.0.0", "nightly", "1.0.0"}, "1.0.0"},
		{[]string{"nightly", "1.0.0", "2.0.0", "1.5.0"}, "2.0.0"},
	} {
		var vs []model.MCPVersion
		for i, ver := range c.versions {
			v := model.MCPVersion{ID: int64(i + 1), PublishedAt: t0.Add(time.Duration(i) * time.Hour), Status: model.MCPStatusActive}
			if v.Version = strings.TrimPrefix(ver, "-"); v.Version != ver {
				v.Status = model.MCPStatusDeleted
			}
			// The input order must not matter.
			vs = append([]model.MCPVersion{v}, vs...)
		}
		id, ok := store.LatestMCPVersion(vs)
		got := ""
		if ok {
			got = strings.TrimPrefix(c.versions[id-1], "-")
		}
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%v: latest = %q (%v), want %q", c.versions, got, ok, c.want)
		}
	}
	// Same publication time: the higher id is the later one.
	tie := []model.MCPVersion{{ID: 7, Version: "b", PublishedAt: t0}, {ID: 3, Version: "a", PublishedAt: t0}}
	if id, ok := store.LatestMCPVersion(tie); !ok || id != 7 {
		t.Errorf("tie: latest = %d, %v", id, ok)
	}
}

func TestSyncError(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"plain", "plain"},
		{strings.Repeat("a", store.MaxSyncError), strings.Repeat("a", store.MaxSyncError)},
		{strings.Repeat("a", store.MaxSyncError+1), strings.Repeat("a", store.MaxSyncError)},
		// The limit falls inside the last two-byte rune.
		{strings.Repeat("a", store.MaxSyncError-1) + "é", strings.Repeat("a", store.MaxSyncError-1)},
		{"a\xffb\x00c", "a�bc"},
	} {
		if got := store.SyncError(c.in); got != c.want || !utf8.ValidString(got) || len(got) > store.MaxSyncError {
			t.Errorf("SyncError(%d bytes) = %d bytes %q", len(c.in), len(got), got)
		}
	}
}
