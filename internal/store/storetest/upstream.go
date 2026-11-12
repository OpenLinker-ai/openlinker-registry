package storetest

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

func getUpstream(t *testing.T, st store.Store, name string) model.Upstream {
	t.Helper()
	u, err := st.GetUpstream(context.Background(), name)
	if err != nil {
		t.Fatalf("get upstream %s: %v", name, err)
	}
	return u
}

var upstreamTests = []testCase{
	{"upstream create, read, update, delete", func(t *testing.T, st store.Store) {
		ctx := context.Background()
		in := model.Upstream{Name: "official", Kind: model.UpstreamRegistry, BaseURL: "https://registry.example/base", Enabled: true,
			Priority: 10, Visibility: model.VisibilityInternal, NamespaceAllow: []string{"io.github.*", "com.example"},
			NamespaceDeny: []string{"io.github.evil"}, SyncInterval: 90*time.Second + 700*time.Millisecond, ProxyOnMiss: true, LatestOnly: true,
			// Sync state is the store's, never the caller's.
			ID: 12345, Cursor: "ignored", SyncState: model.SyncRunning, LastSyncStatus: model.SyncFailed, LastSyncError: "ignored",
			LastSyncStats: map[string]int{"ignored": 1}}
		u, err := st.CreateUpstream(ctx, in, audit)
		if err != nil {
			t.Fatal(err)
		}
		check := func(got model.Upstream) {
			t.Helper()
			if got.ID == 0 || got.ID != u.ID || got.Name != "official" || got.Kind != model.UpstreamRegistry || got.BaseURL != in.BaseURL ||
				!got.Enabled || got.Priority != 10 || got.Visibility != model.VisibilityInternal ||
				!slices.Equal(got.NamespaceAllow, in.NamespaceAllow) || !slices.Equal(got.NamespaceDeny, in.NamespaceDeny) ||
				got.TargetNamespace != "" || got.SyncInterval != 90*time.Second || !got.ProxyOnMiss || !got.LatestOnly ||
				got.Cursor != "" || got.SyncState != model.SyncIdle || got.SyncStartedAt != nil || got.LastSyncAt != nil ||
				got.LastSyncStatus != "" || got.LastSyncError != "" || len(got.LastSyncStats) != 0 ||
				!isNow(got.CreatedAt) || !sameTime(got.UpdatedAt, got.CreatedAt) {
				t.Fatalf("upstream = %+v", got)
			}
		}
		check(u)
		check(getUpstream(t, st, "official"))
		if _, err := st.CreateUpstream(ctx, model.Upstream{Name: "official", Kind: model.UpstreamMCP, BaseURL: "https://other.example",
			Visibility: model.VisibilityPublic}, audit); conflictCode(err) != "upstream_exists" {
			t.Fatalf("duplicate name: %v", err)
		}
		check(getUpstream(t, st, "official"))
		if _, err := st.GetUpstream(ctx, "missing"); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("missing upstream: %v", err)
		}

		// Zero values are stored as given; empty lists stay empty.
		skills, err := st.CreateUpstream(ctx, model.Upstream{Name: "a-b", Kind: model.UpstreamAgentSkills, BaseURL: "http://skills.example",
			Visibility: model.VisibilityPublic, TargetNamespace: "org.skills"}, audit)
		if err != nil || skills.Enabled || skills.Priority != 0 || len(skills.NamespaceAllow) != 0 || len(skills.NamespaceDeny) != 0 ||
			skills.TargetNamespace != "org.skills" || skills.SyncInterval != 0 || skills.ProxyOnMiss || skills.LatestOnly {
			t.Fatalf("minimal upstream = %+v, %v", skills, err)
		}
		// Priority first, then names in byte order ('-' before letters).
		for _, c := range []struct {
			name     string
			priority int
		}{{"ab", 0}, {"zz", 5}, {"aa", 10}} {
			if _, err := st.CreateUpstream(ctx, model.Upstream{Name: c.name, Kind: model.UpstreamMCP, BaseURL: "https://x.example",
				Priority: c.priority, Visibility: model.VisibilityPublic}, audit); err != nil {
				t.Fatal(err)
			}
		}
		all, err := st.ListUpstreams(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, u := range all {
			names = append(names, u.Name)
		}
		if want := []string{"a-b", "ab", "zz", "aa", "official"}; !slices.Equal(names, want) {
			t.Fatalf("upstreams = %v, want %v", names, want)
		}
		check(all[4])

		// Patch: only the given fields change.
		if _, err := st.UpdateUpstream(ctx, "missing", model.UpstreamPatch{}, audit); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("patch a missing upstream: %v", err)
		}
		got, err := st.UpdateUpstream(ctx, "official", model.UpstreamPatch{}, audit)
		if err != nil || got.UpdatedAt.Before(u.UpdatedAt) {
			t.Fatalf("empty patch: %+v, %v", got, err)
		}
		got.UpdatedAt = got.CreatedAt
		check(got)
		off, prio, vis := false, 0, model.VisibilityPublic
		allow, deny := []string{"org.only"}, []string(nil)
		interval := 2*time.Hour + 999*time.Millisecond
		got, err = st.UpdateUpstream(ctx, "official", model.UpstreamPatch{Enabled: &off, Priority: &prio, Visibility: &vis,
			NamespaceAllow: &allow, NamespaceDeny: &deny, SyncInterval: &interval, ProxyOnMiss: &off, LatestOnly: &off}, audit)
		checkPatched := func(got model.Upstream) {
			t.Helper()
			if got.ID != u.ID || got.BaseURL != in.BaseURL || got.Kind != model.UpstreamRegistry || got.Enabled || got.Priority != 0 ||
				got.Visibility != model.VisibilityPublic || !slices.Equal(got.NamespaceAllow, allow) || len(got.NamespaceDeny) != 0 ||
				got.SyncInterval != 2*time.Hour || got.ProxyOnMiss || got.LatestOnly || !sameTime(got.CreatedAt, u.CreatedAt) {
				t.Fatalf("patched upstream = %+v", got)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		checkPatched(got)
		checkPatched(getUpstream(t, st, "official"))
		// The patch input stays the caller's.
		allow[0] = "changed.after"
		if got := getUpstream(t, st, "official"); !slices.Equal(got.NamespaceAllow, []string{"org.only"}) {
			t.Fatalf("stored list aliases the patch: %v", got.NamespaceAllow)
		}

		// An upstream that manages a namespace cannot be deleted.
		if _, err := st.EnsureUpstreamNamespace(ctx, "org.mirror", u.ID); err != nil {
			t.Fatal(err)
		}
		if err := st.DeleteUpstream(ctx, "official", audit); conflictCode(err) != "upstream_in_use" {
			t.Fatalf("delete an upstream in use: %v", err)
		}
		if got := getUpstream(t, st, "official"); got.ID != u.ID {
			t.Fatalf("upstream after refused delete = %+v", got)
		}
		if err := st.DeleteUpstream(ctx, "missing", audit); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("delete a missing upstream: %v", err)
		}
		if err := st.DeleteUpstream(ctx, "a-b", audit); err != nil {
			t.Fatal(err)
		}
		if _, err := st.GetUpstream(ctx, "a-b"); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("deleted upstream: %v", err)
		}
		if _, err := st.EnsureUpstreamNamespace(ctx, "org.late", skills.ID); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("namespace for a deleted upstream: %v", err)
		}
		if all, err := st.ListUpstreams(ctx); err != nil || len(all) != 4 {
			t.Fatalf("%d upstreams after delete, %v", len(all), err)
		}
	}},

	{"upstream sync claim and result", func(t *testing.T, st store.Store) {
		ctx := context.Background()
		u := newUpstream(t, st, "up")
		t0 := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
		stale := 10 * time.Minute
		if _, err := st.ClaimUpstreamSync(ctx, 1<<40, t0, t0.Add(-stale)); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("claim a missing upstream: %v", err)
		}
		if err := st.FinishUpstreamSync(ctx, 1<<40, model.UpstreamSyncResult{At: t0, Status: model.SyncOK}); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("finish a missing upstream: %v", err)
		}

		// Of concurrent claimers exactly one wins, every time.
		claim := func(at time.Time) int {
			t.Helper()
			won := make([]bool, 20)
			for i, err := range parallel(len(won), func(i int) error {
				var err error
				won[i], err = st.ClaimUpstreamSync(ctx, u.ID, at, at.Add(-stale))
				return err
			}) {
				if err != nil {
					t.Fatalf("claimer %d: %v", i, err)
				}
			}
			n := 0
			for _, w := range won {
				if w {
					n++
				}
			}
			return n
		}
		if n := claim(t0); n != 1 {
			t.Fatalf("%d claimers won an idle upstream", n)
		}
		if got := getUpstream(t, st, "up"); got.SyncState != model.SyncRunning || got.SyncStartedAt == nil || !sameTime(*got.SyncStartedAt, t0) ||
			!sameTime(got.UpdatedAt, u.UpdatedAt) {
			t.Fatalf("claimed upstream = %+v", got)
		}
		// Still fresh: started exactly at the stale bound is not stale.
		if n := claim(t0.Add(stale)); n != 0 {
			t.Fatalf("%d claimers won a running sync", n)
		}
		// A sync that stopped reporting is taken over, again by one claimer.
		t1 := t0.Add(stale + time.Second)
		if n := claim(t1); n != 1 {
			t.Fatalf("%d claimers took over a stale sync", n)
		}
		if got := getUpstream(t, st, "up"); got.SyncState != model.SyncRunning || !sameTime(*got.SyncStartedAt, t1) {
			t.Fatalf("taken-over upstream = %+v", got)
		}

		// A good result stores the cursor.
		stats := map[string]int{"created": 3, "updated": 0, "failed": 1}
		if err := st.FinishUpstreamSync(ctx, u.ID, model.UpstreamSyncResult{At: t1.Add(time.Minute), Status: model.SyncOK,
			Cursor: "c1", Stats: stats}); err != nil {
			t.Fatal(err)
		}
		stats["created"] = 99 // the caller's map is not the stored one
		got := getUpstream(t, st, "up")
		if got.SyncState != model.SyncIdle || got.SyncStartedAt != nil || got.LastSyncAt == nil || !sameTime(*got.LastSyncAt, t1.Add(time.Minute)) ||
			got.LastSyncStatus != model.SyncOK || got.LastSyncError != "" || got.Cursor != "c1" ||
			!maps.Equal(got.LastSyncStats, map[string]int{"created": 3, "updated": 0, "failed": 1}) {
			t.Fatalf("after a good sync = %+v", got)
		}

		// A failed result keeps the cursor and stores a bounded error, cut
		// between runes.
		if ok, err := st.ClaimUpstreamSync(ctx, u.ID, t1.Add(time.Hour), t1); err != nil || !ok {
			t.Fatalf("claim an idle upstream: %v, %v", ok, err)
		}
		long := "x" + strings.Repeat("世", 400) // 1201 bytes; byte 1000 is inside a rune
		if err := st.FinishUpstreamSync(ctx, u.ID, model.UpstreamSyncResult{At: t1.Add(2 * time.Hour), Status: model.SyncFailed,
			Error: long, Cursor: "must-not-be-stored"}); err != nil {
			t.Fatal(err)
		}
		got = getUpstream(t, st, "up")
		if got.SyncState != model.SyncIdle || got.SyncStartedAt != nil || !sameTime(*got.LastSyncAt, t1.Add(2*time.Hour)) ||
			got.LastSyncStatus != model.SyncFailed || got.Cursor != "c1" || len(got.LastSyncStats) != 0 {
			t.Fatalf("after a failed sync = %+v", got)
		}
		if want := "x" + strings.Repeat("世", 333); got.LastSyncError != want || !utf8.ValidString(got.LastSyncError) {
			t.Fatalf("stored error: %d bytes, want %d", len(got.LastSyncError), len(want))
		}
		// Bytes Postgres text cannot hold do not lose the result.
		if err := st.FinishUpstreamSync(ctx, u.ID, model.UpstreamSyncResult{At: t1.Add(3 * time.Hour), Status: model.SyncFailed,
			Error: "bad \xff byte and \x00 nul"}); err != nil {
			t.Fatal(err)
		}
		if got := getUpstream(t, st, "up"); got.LastSyncError != "bad � byte and  nul" {
			t.Fatalf("stored error = %q", got.LastSyncError)
		}

		// A base URL change, and only a change, drops the cursor.
		same, moved := "https://up.example", "https://moved.example"
		if got, err := st.UpdateUpstream(ctx, "up", model.UpstreamPatch{BaseURL: &same}, audit); err != nil || got.Cursor != "c1" || got.BaseURL != same {
			t.Fatalf("same base URL: %+v, %v", got, err)
		}
		off := false
		if got, err := st.UpdateUpstream(ctx, "up", model.UpstreamPatch{Enabled: &off}, audit); err != nil || got.Cursor != "c1" {
			t.Fatalf("unrelated patch: %+v, %v", got, err)
		}
		got, err := st.UpdateUpstream(ctx, "up", model.UpstreamPatch{BaseURL: &moved}, audit)
		if err != nil || got.Cursor != "" || got.BaseURL != moved || got.LastSyncStatus != model.SyncFailed {
			t.Fatalf("new base URL: %+v, %v", got, err)
		}
		if got := getUpstream(t, st, "up"); got.Cursor != "" || got.BaseURL != moved {
			t.Fatalf("stored after new base URL = %+v", got)
		}
	}},
}
