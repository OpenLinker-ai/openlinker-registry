package storetest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

// auditCount returns how many audit events carry the action.
func auditCount(t *testing.T, st store.Store, action string) int {
	t.Helper()
	events, err := st.ListAudit(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Action == action {
			n++
		}
	}
	return n
}

// Writes made on behalf of an upstream are audited like any other write:
// one event per change, none when nothing changed.
var auditTests = []testCase{{"mirror writes are audited", func(t *testing.T, st store.Store) {
	ctx := context.Background()
	up := newUpstream(t, st, "up")
	ev := func(action string) model.AuditEvent { return model.AuditEvent{Actor: "upstream:up", Action: action} }

	if _, err := st.EnsureUpstreamNamespace(ctx, "org.first", up.ID, ev("ns.ensure")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureUpstreamNamespace(ctx, "org.first", up.ID, ev("ns.ensure")); err != nil {
		t.Fatal(err)
	}
	if n := auditCount(t, st, "ns.ensure"); n != 1 {
		t.Fatalf("namespace creation audited %d times, want once (and not when it already exists)", n)
	}

	in := mirrorInput(up, "org.mirror/server", "1.0.0")
	for i, want := range []string{model.MirrorCreated, model.MirrorUnchanged} {
		if got, err := st.UpsertMirroredMCP(ctx, in, ev("mirror.write")); err != nil || got != want {
			t.Fatalf("upsert %d: %s, %v", i, got, err)
		}
	}
	if n := auditCount(t, st, "mirror.write"); n != 1 {
		t.Fatalf("a created version is audited once and an unchanged one not at all; got %d", n)
	}
	// The namespace the upsert created for itself is audited too.
	if n := auditCount(t, st, "namespace.mirror"); n != 1 {
		t.Fatalf("namespace created by a mirror write audited %d times", n)
	}
	in.Version.Status, in.Version.StatusMessage = model.MCPStatusDeprecated, "old"
	if got, err := st.UpsertMirroredMCP(ctx, in, ev("mirror.write")); err != nil || got != model.MirrorUpdated {
		t.Fatalf("status update: %s, %v", got, err)
	}
	if n := auditCount(t, st, "mirror.write"); n != 2 {
		t.Fatalf("an updated version must be audited; got %d events", n)
	}

	if err := st.FinishUpstreamSync(ctx, up.ID, model.UpstreamSyncResult{At: time.Now(), Status: model.SyncOK}, ev("sync.done")); err != nil {
		t.Fatal(err)
	}
	if n := auditCount(t, st, "sync.done"); n != 1 {
		t.Fatalf("sync result audited %d times", n)
	}
}}}

// Rules a mirror relies on to stay faithful to its upstream.
var mirrorTests = []testCase{
	{"skill versions keep the upstream order", func(t *testing.T, st store.Store) {
		ctx := context.Background()
		up := newUpstream(t, st, "up")
		if _, err := st.EnsureUpstreamNamespace(ctx, "org.mirror", up.ID, audit); err != nil {
			t.Fatal(err)
		}
		put := func(ver, expect string) error {
			v := version(ver, "spdx")
			v.Source, v.Locator = nil, &model.Locator{Kind: "registry", Upstream: "up"}
			_, _, err := st.PublishVersion(ctx, model.PublishInput{Namespace: "org.mirror", Name: "tool", Mode: "indexed",
				SourceKey: "upstream:up:org.mirror/tool", Visibility: model.VisibilityPublic, Listed: true, UpstreamID: up.ID,
				ExpectNewest: &expect, Version: v}, audit)
			return err
		}
		if err := put("1", "0"); conflictCode(err) != "version_order" {
			t.Fatalf("first version with a wrong expectation: %v", err)
		}
		if _, err := st.GetSkill(ctx, "org.mirror", "tool"); err == nil {
			t.Fatal("a refused first version created the skill")
		}
		if err := put("1", ""); err != nil {
			t.Fatal(err)
		}
		if err := put("2", "1"); err != nil {
			t.Fatal(err)
		}
		// A refresh that read the versions before "2" arrived must not add an
		// older version behind it.
		if err := put("0", "1"); conflictCode(err) != "version_order" {
			t.Fatalf("stale refresh: %v", err)
		}
		if err := put("3", ""); conflictCode(err) != "version_order" {
			t.Fatalf("refresh that saw no versions: %v", err)
		}
		sk, _ := st.GetSkill(ctx, "org.mirror", "tool")
		vs, _ := st.ListVersions(ctx, sk.ID)
		if len(vs) != 2 || vs[0].Version != "2" || vs[1].Version != "1" {
			t.Fatalf("versions = %+v", vs)
		}
	}},
	{"a skill's current version", func(t *testing.T, st store.Store) {
		// A source may name a version recorded earlier as its current one.
		// Detail, search and the discovery index then present that version.
		ctx := context.Background()
		up := newUpstream(t, st, "up")
		put := func(ver, description string) {
			t.Helper()
			v := version(ver, "spdx")
			v.FrontmatterDescription = description
			v.Source, v.Locator = nil, &model.Locator{Kind: "url", Upstream: "up"}
			if _, _, err := st.PublishVersion(ctx, model.PublishInput{Namespace: "org.site", Name: "tool", Mode: "indexed",
				SourceKey: "upstream:up:tool", Visibility: model.VisibilityPublic, Listed: true, UpstreamID: up.ID,
				UpstreamConfigVersion: up.ConfigVersion, Version: v}, audit); err != nil {
				t.Fatal(err)
			}
		}
		latest := func() (string, string, string) {
			t.Helper()
			found, err := st.Search(ctx, model.SearchQuery{Namespace: "org.site"})
			if err != nil || len(found) != 1 {
				t.Fatalf("search: %d rows, %v", len(found), err)
			}
			cands, err := st.IndexCandidates(ctx, "org.site", false)
			if err != nil || len(cands) != 1 {
				t.Fatalf("index candidates: %d rows, %v", len(cands), err)
			}
			return found[0].LatestVersion, cands[0].Version.Version, found[0].Skill.Description
		}
		put("sha-a", "first")
		put("sha-b", "second")
		if s, c, d := latest(); s != "sha-b" || c != "sha-b" || d != "second" {
			t.Fatalf("latest = %s / %s (%s)", s, c, d)
		}
		sk, _ := st.GetSkill(ctx, "org.site", "tool")
		events := auditCount(t, st, "test")
		out, err := st.SetSkillCurrentVersion(ctx, sk.ID, "sha-a", audit)
		if err != nil || out.CurrentVersion != "sha-a" || out.Description != "first" || out.Upstream != "up" {
			t.Fatalf("set current version: %+v, %v", out, err)
		}
		if n := auditCount(t, st, "test"); n != events+1 {
			t.Fatalf("audit events: %d", n-events)
		}
		if s, c, d := latest(); s != "sha-a" || c != "sha-a" || d != "first" {
			t.Fatalf("after going back: latest = %s / %s (%s)", s, c, d)
		}
		if got, _ := st.GetSkill(ctx, "org.site", "tool"); got.CurrentVersion != "sha-a" {
			t.Fatalf("stored skill = %+v", got)
		}
		if vs, _ := st.ListVersions(ctx, sk.ID); len(vs) != 2 || vs[0].Version != "sha-b" {
			t.Fatalf("the version history changed: %+v", vs)
		}
		if _, err := st.SetSkillCurrentVersion(ctx, sk.ID, "sha-zz", audit); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("a version that is not recorded: %v", err)
		}
		if _, err := st.SetSkillCurrentVersion(ctx, sk.ID+1000, "sha-a", audit); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("a skill that does not exist: %v", err)
		}
		// A change of the upstream's settings stops it like any mirror write.
		changed, err := st.UpdateUpstream(ctx, "up", model.UpstreamPatch{NamespaceDeny: &[]string{"org.other"}}, audit)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetSkillCurrentVersion(model.WithUpstreamGuard(ctx, up.ID, up.ConfigVersion), sk.ID, "sha-b", audit); conflictCode(err) != "upstream_config_changed" {
			t.Fatalf("under settings that changed: %v", err)
		}
		up = changed
		// A yanked current version is not presented.
		vs, _ := st.ListVersions(ctx, sk.ID)
		if err := st.YankVersion(ctx, vs[1].ID, "bad", time.Now(), audit); err != nil {
			t.Fatal(err)
		}
		if s, c, _ := latest(); s != "sha-b" || c != "sha-b" {
			t.Fatalf("with the current version yanked: latest = %s / %s", s, c)
		}
		// A new version is the latest again.
		put("sha-c", "third")
		if got, _ := st.GetSkill(ctx, "org.site", "tool"); got.CurrentVersion != "" {
			t.Fatalf("publishing kept the current version: %+v", got)
		}
		if s, c, d := latest(); s != "sha-c" || c != "sha-c" || d != "third" {
			t.Fatalf("after a new version: latest = %s / %s (%s)", s, c, d)
		}
	}},
	{"a URL-indexed version that moved", func(t *testing.T, st store.Store) {
		// The locator of a version is immutable, except where its content
		// is served now.
		ctx := context.Background()
		up := newUpstream(t, st, "up")
		put := func(name string, l *model.Locator) model.Version {
			t.Helper()
			v := version("sha-a", "spdx")
			v.Source, v.Locator = nil, l
			_, out, err := st.PublishVersion(ctx, model.PublishInput{Namespace: "org.site", Name: name, Mode: "indexed",
				SourceKey: "upstream:up:" + name, Visibility: model.VisibilityPublic, Listed: true, UpstreamID: up.ID,
				UpstreamConfigVersion: up.ConfigVersion, Version: v}, audit)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
		digest := "sha256:" + strings.Repeat("b", 64)
		moved := put("moved", &model.Locator{Kind: "url", URL: "https://site.example/old/SKILL.md", Digest: digest, EntryType: "skill-md", Upstream: "up"})
		other := put("other", &model.Locator{Kind: "registry", Upstream: "up", Namespace: "org.site", Name: "other", Version: "sha-a"})
		events := auditCount(t, st, "test")
		if err := st.SetVersionLocatorURL(ctx, moved.ID, "https://site.example/new/SKILL.md", audit); err != nil {
			t.Fatal(err)
		}
		if n := auditCount(t, st, "test"); n != events+1 {
			t.Fatalf("audit events: %d", n-events)
		}
		want := model.Locator{Kind: "url", URL: "https://site.example/new/SKILL.md", Digest: digest, EntryType: "skill-md", Upstream: "up"}
		got, err := st.GetVersion(ctx, moved.SkillID, "sha-a")
		if err != nil || got.Locator == nil || *got.Locator != want {
			t.Fatalf("version = %+v, %v", got.Locator, err)
		}
		if vs, _ := st.ListVersions(ctx, moved.SkillID); len(vs) != 1 || *vs[0].Locator != want {
			t.Fatalf("listed versions = %+v", vs)
		}
		cands, err := st.IndexCandidates(ctx, "org.site", false)
		if err != nil || len(cands) != 2 || *cands[0].Version.Locator != want {
			t.Fatalf("index candidates = %+v, %v", cands, err)
		}
		// Only a URL-indexed version has a URL to move.
		if err := st.SetVersionLocatorURL(ctx, other.ID, "https://site.example/x", audit); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("a version with another kind of locator: %v", err)
		}
		if err := st.SetVersionLocatorURL(ctx, moved.ID+1000, "https://site.example/x", audit); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("a version that does not exist: %v", err)
		}
		if _, err := st.UpdateUpstream(ctx, "up", model.UpstreamPatch{NamespaceDeny: &[]string{"org.other"}}, audit); err != nil {
			t.Fatal(err)
		}
		if err := st.SetVersionLocatorURL(model.WithUpstreamGuard(ctx, up.ID, up.ConfigVersion), moved.ID, "https://site.example/y", audit); conflictCode(err) != "upstream_config_changed" {
			t.Fatalf("under settings that changed: %v", err)
		}
	}},
	{"every mirrored entry can be enumerated", func(t *testing.T, st store.Store) {
		ctx := context.Background()
		up, other := newUpstream(t, st, "up"), newUpstream(t, st, "other")
		for _, ns := range []string{"org.b", "org.a"} {
			if _, err := st.EnsureUpstreamNamespace(ctx, ns, up.ID, audit); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := st.EnsureUpstreamNamespace(ctx, "org.z", other.ID, audit); err != nil {
			t.Fatal(err)
		}
		skillIn := func(ns, name string, upstreamID int64, source string) model.PublishInput {
			v := version("1", "spdx")
			v.Source, v.Locator = nil, &model.Locator{Kind: "registry"}
			return model.PublishInput{Namespace: ns, Name: name, Mode: "indexed", SourceKey: source + ":" + ns + "/" + name,
				Visibility: model.VisibilityPublic, Listed: true, UpstreamID: upstreamID, Version: v}
		}
		var ids = map[string]int64{}
		for _, in := range []model.PublishInput{skillIn("org.b", "one", up.ID, "up"), skillIn("org.a", "two", up.ID, "up"),
			skillIn("org.a", "one", up.ID, "up"), skillIn("org.z", "x", other.ID, "other")} {
			sk, _, err := st.PublishVersion(ctx, in, audit)
			if err != nil {
				t.Fatal(err)
			}
			ids[in.Namespace+"/"+in.Name] = sk.ID
		}
		if _, _, err := publish(st, "local", model.VisibilityPublic, "1", "spdx"); err != nil {
			t.Fatal(err)
		}
		// Unlisted, private and yanked entries are exactly the ones a listing
		// hides and a mirror must still reconcile.
		private, unlisted := model.VisibilityPrivate, false
		if _, err := st.UpdateSkill(ctx, ids["org.a/one"], model.SkillPatch{Visibility: &private, Listed: &unlisted}, audit); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetSkillStatus(ctx, ids["org.b/one"], model.StatusYanked, "", audit); err != nil {
			t.Fatal(err)
		}
		var got []string
		afterNS, afterName := "", ""
		for {
			page, err := st.ListMirroredSkills(ctx, up.ID, afterNS, afterName, 2)
			if err != nil {
				t.Fatal(err)
			}
			for _, sk := range page {
				if sk.Upstream != "up" {
					t.Fatalf("skill of another owner: %+v", sk)
				}
				got = append(got, sk.Namespace+"/"+sk.Name)
				afterNS, afterName = sk.Namespace, sk.Name
			}
			if len(page) < 2 {
				break
			}
		}
		if want := "org.a/one,org.a/two,org.b/one"; strings.Join(got, ",") != want {
			t.Fatalf("mirrored skills = %v, want %s", got, want)
		}

		for _, name := range []string{"org.b/Srv", "org.a/srv", "org.a/Hidden"} {
			mirrorMCP(t, st, mirrorInput(up, name, "1.0.0"), model.MirrorCreated)
		}
		mirrorMCP(t, st, mirrorInput(other, "org.z/srv", "1.0.0"), model.MirrorCreated)
		hidden := mcpServer(t, st, "org.a/Hidden")
		if _, err := st.UpdateMCPServer(ctx, hidden.ID, model.MCPPatch{Visibility: &private, Listed: &unlisted}, audit); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetMCPStatus(ctx, mcpServer(t, st, "org.b/Srv").ID, "", model.MCPStatusDeleted, "gone", time.Now(), audit); err != nil {
			t.Fatal(err)
		}
		got, after := nil, ""
		for {
			page, err := st.ListMirroredMCP(ctx, up.ID, after, 2)
			if err != nil {
				t.Fatal(err)
			}
			for _, srv := range page {
				got = append(got, srv.Name)
				after = srv.Name
			}
			if len(page) < 2 {
				break
			}
		}
		if want := "org.a/Hidden,org.a/srv,org.b/Srv"; strings.Join(got, ",") != want {
			t.Fatalf("mirrored servers = %v, want %s", got, want)
		}
	}},
	{"changing who may see a server stamps its versions", func(t *testing.T, st store.Store) {
		ctx := context.Background()
		srv, v := publishMCP(t, st, mcpInput("com.example/server", "1.0.0"))
		time.Sleep(2 * time.Millisecond)
		internal := model.VisibilityInternal
		if _, err := st.UpdateMCPServer(ctx, srv.ID, model.MCPPatch{Visibility: &internal}, audit); err != nil {
			t.Fatal(err)
		}
		after := mcpVersion(t, st, srv.ID, "1.0.0")
		if !after.UpdatedAt.After(v.UpdatedAt) {
			t.Fatal("a visibility change must show up in the change feed of the server's versions")
		}
		// A patch that changes nothing does not.
		if _, err := st.UpdateMCPServer(ctx, srv.ID, model.MCPPatch{Visibility: &internal}, audit); err != nil {
			t.Fatal(err)
		}
		if again := mcpVersion(t, st, srv.ID, "1.0.0"); !sameTime(again.UpdatedAt, after.UpdatedAt) {
			t.Fatal("a patch without a change stamped the versions")
		}
	}},
	{"mirror writes obey the upstream's current visibility", func(t *testing.T, st store.Store) {
		// A sync that started while the upstream mirrored as public keeps
		// writing "public" after an administrator lowered it. The store
		// decides by the setting at the time of the write.
		ctx := context.Background()
		up := newUpstream(t, st, "up")
		internal, public := model.VisibilityInternal, model.VisibilityPublic
		if _, err := st.UpdateUpstream(ctx, "up", model.UpstreamPatch{Visibility: &internal}, audit); err != nil {
			t.Fatal(err)
		}
		// Written by the settings the sync read before the change: refused,
		// and nothing is left behind, not even the namespace.
		stale := mirrorInput(up, "org.mirror/srv", "1.0.0")
		if _, err := st.UpsertMirroredMCP(ctx, stale, audit); conflictCode(err) != "upstream_config_changed" {
			t.Fatalf("mirror write under old settings: %v", err)
		}
		v := version("1", "spdx")
		v.Source, v.Locator = nil, &model.Locator{Kind: "registry"}
		skillIn := model.PublishInput{Namespace: "org.mirror", Name: "tool", Mode: "indexed", SourceKey: "upstream:up:org.mirror/tool",
			Visibility: public, Listed: true, UpstreamID: up.ID, UpstreamConfigVersion: up.ConfigVersion, Version: v}
		if _, _, err := st.PublishVersion(ctx, skillIn, audit); conflictCode(err) != "upstream_config_changed" {
			t.Fatalf("mirror publish under old settings: %v", err)
		}
		if _, err := st.GetNamespace(ctx, "org.mirror"); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("a refused mirror write created the namespace: %v", err)
		}
		// Under the current settings the write goes through, and can never
		// be more public than the upstream is configured to mirror.
		now, err := st.GetUpstream(ctx, "up")
		if err != nil {
			t.Fatal(err)
		}
		current := mirrorInput(now, "org.mirror/srv", "1.0.0")
		current.Visibility = public
		mirrorMCP(t, st, current, model.MirrorCreated)
		if got := mcpServer(t, st, "org.mirror/srv"); got.Visibility != internal {
			t.Fatalf("mirrored server more public than its upstream allows: %+v", got)
		}
		mirrorMCP(t, st, current, model.MirrorUnchanged)
		skillIn.UpstreamConfigVersion = now.ConfigVersion
		sk, _, err := st.PublishVersion(ctx, skillIn, audit)
		if err != nil || sk.Visibility != internal {
			t.Fatalf("mirrored skill more public than its upstream allows: %+v, %v", sk, err)
		}
		if got, err := st.UpdateSkill(ctx, sk.ID, model.SkillPatch{Visibility: &public}, audit); err != nil || got.Visibility != internal {
			t.Fatalf("mirrored skill patched more public than its upstream allows: %+v, %v", got, err)
		}
		// Local entries are not affected by any upstream.
		local, _, err := publish(st, "local", internal, "1", "spdx")
		if err != nil {
			t.Fatal(err)
		}
		if got, err := st.UpdateSkill(ctx, local.ID, model.SkillPatch{Visibility: &public}, audit); err != nil || got.Visibility != public {
			t.Fatalf("local skill: %+v, %v", got, err)
		}

		// The cursor of a run that started under older settings is dropped.
		before, err := st.GetUpstream(ctx, "up")
		if err != nil {
			t.Fatal(err)
		}
		allow := []string{"org.*"}
		if _, err := st.UpdateUpstream(ctx, "up", model.UpstreamPatch{NamespaceAllow: &allow}, audit); err != nil {
			t.Fatal(err)
		}
		finish := func(configVersion int64, cursor string) string {
			t.Helper()
			if err := st.FinishUpstreamSync(ctx, up.ID, model.UpstreamSyncResult{At: time.Now(), Status: model.SyncOK, Cursor: cursor,
				ConfigVersion: configVersion}, audit); err != nil {
				t.Fatal(err)
			}
			u, err := st.GetUpstream(ctx, "up")
			if err != nil {
				t.Fatal(err)
			}
			return u.Cursor
		}
		if got := finish(before.ConfigVersion, "stale"); got != "" {
			t.Fatalf("a run under older settings stored its cursor %q", got)
		}
		now, _ = st.GetUpstream(ctx, "up")
		if now.ConfigVersion <= before.ConfigVersion {
			t.Fatalf("a selection change must advance the config version: %d -> %d", before.ConfigVersion, now.ConfigVersion)
		}
		if got := finish(now.ConfigVersion, "current"); got != "current" {
			t.Fatalf("cursor of a current run = %q", got)
		}
	}},
	{"a listing document only adds or narrows", func(t *testing.T, st store.Store) {
		// A document that came with a listing page may be older than what a
		// fresh read applied meanwhile. It may add a version and narrow who
		// sees the server; it may not undo anything.
		up := newUpstream(t, st, "up")
		fresh := mirrorInput(up, "org.mirror/srv", "1.0.0")
		fresh.Visibility, fresh.Listed = model.VisibilityInternal, false
		fresh.Version.Status, fresh.Version.StatusMessage = model.MCPStatusDeleted, "gone"
		mirrorMCP(t, st, fresh, model.MirrorCreated)

		// The listing still says: public, listed, active.
		listed := mirrorInput(up, "org.mirror/srv", "1.0.0")
		listed.Fresh, listed.Version.IsLatest = false, true
		events := auditCount(t, st, "test")
		mirrorMCP(t, st, listed, model.MirrorStale)
		srv := mcpServer(t, st, "org.mirror/srv")
		if v := mcpVersion(t, st, srv.ID, "1.0.0"); srv.Visibility != model.VisibilityInternal || srv.Listed ||
			v.Status != model.MCPStatusDeleted || v.IsLatest {
			t.Fatalf("a stale document undid a fresher state: %+v %+v", srv, v)
		}
		if n := auditCount(t, st, "test"); n != events {
			t.Fatalf("a refused document wrote %d audit events", n-events)
		}
		// It may add a version, without widening the server.
		next := mirrorInput(up, "org.mirror/srv", "2.0.0")
		next.Fresh = false
		mirrorMCP(t, st, next, model.MirrorStale)
		if got := mcpServer(t, st, "org.mirror/srv"); got.Visibility != model.VisibilityInternal || got.Listed {
			t.Fatalf("adding a version widened the server: %+v", got)
		}
		if v := mcpVersion(t, st, srv.ID, "2.0.0"); v.Status != model.MCPStatusActive {
			t.Fatalf("new version = %+v", v)
		}
		// A version it adds does not take "latest" from one that is here: a
		// fresh read may have recorded a newer version than the page knew.
		newest := mirrorInput(up, "org.mirror/latest", "2.0.0")
		newest.Version.IsLatest = true
		mirrorMCP(t, st, newest, model.MirrorCreated)
		old := mirrorInput(up, "org.mirror/latest", "1.0.0")
		old.Fresh, old.Version.IsLatest = false, true // the page predates 2.0.0
		mirrorMCP(t, st, old, model.MirrorStale)
		latestSrv := mcpServer(t, st, "org.mirror/latest")
		if got := mcpVersion(t, st, latestSrv.ID, "latest"); got.Version != "2.0.0" {
			t.Fatalf("a stale page moved latest to %s", got.Version)
		}
		if got := mcpVersion(t, st, latestSrv.ID, "1.0.0"); got.IsLatest {
			t.Fatalf("stale version = %+v", got)
		}
		// On a server without a latest version it may set one.
		first := mirrorInput(up, "org.mirror/first", "1.0.0")
		first.Version.IsLatest = false
		mirrorMCP(t, st, first, model.MirrorCreated)
		second := mirrorInput(up, "org.mirror/first", "2.0.0")
		second.Fresh, second.Version.IsLatest = false, true
		mirrorMCP(t, st, second, model.MirrorCreated)
		if got := mcpVersion(t, st, mcpServer(t, st, "org.mirror/first").ID, "latest"); got.Version != "2.0.0" {
			t.Fatalf("latest = %s", got.Version)
		}
		// It may narrow.
		narrow := mirrorInput(up, "org.mirror/other", "1.0.0")
		mirrorMCP(t, st, narrow, model.MirrorCreated)
		narrow.Fresh, narrow.Visibility, narrow.Listed = false, model.VisibilityInternal, false
		mirrorMCP(t, st, narrow, model.MirrorUpdated)
		if got := mcpServer(t, st, "org.mirror/other"); got.Visibility != model.VisibilityInternal || got.Listed {
			t.Fatalf("narrowing by a listing document: %+v", got)
		}
		// A fresh document does all of it.
		fresh = mirrorInput(up, "org.mirror/srv", "1.0.0")
		mirrorMCP(t, st, fresh, model.MirrorUpdated)
		if got := mcpServer(t, st, "org.mirror/srv"); got.Visibility != model.VisibilityPublic || !got.Listed {
			t.Fatalf("fresh document: %+v", got)
		}
	}},
	{"a status-only change leaves the server's settings", func(t *testing.T, st store.Store) {
		// A version that is gone upstream is marked deleted by a caller that
		// read the server before the upstream's current versions were
		// applied. What those versions said about who sees the server stands.
		ctx := context.Background()
		up := newUpstream(t, st, "up")
		v1 := mirrorInput(up, "org.mirror/gone", "1.0.0")
		v1.Version.IsLatest = true
		mirrorMCP(t, st, v1, model.MirrorCreated)
		v2 := mirrorInput(up, "org.mirror/gone", "2.0.0")
		v2.Visibility, v2.Listed, v2.Version.IsLatest = model.VisibilityInternal, false, true
		mirrorMCP(t, st, v2, model.MirrorCreated)

		gone := v1 // still says public and listed
		gone.StatusOnly, gone.Version.IsLatest = true, false
		gone.Version.Status, gone.Version.StatusMessage = model.MCPStatusDeleted, "gone"
		mirrorMCP(t, st, gone, model.MirrorUpdated)
		srv := mcpServer(t, st, "org.mirror/gone")
		if srv.Visibility != model.VisibilityInternal || srv.Listed {
			t.Fatalf("marking a version deleted changed who sees the server: %+v", srv)
		}
		if v := mcpVersion(t, st, srv.ID, "1.0.0"); v.Status != model.MCPStatusDeleted || v.StatusMessage != "gone" || v.IsLatest {
			t.Fatalf("version = %+v", v)
		}
		if v := mcpVersion(t, st, srv.ID, "latest"); v.Version != "2.0.0" {
			t.Fatalf("latest = %s", v.Version)
		}
		mirrorMCP(t, st, gone, model.MirrorUnchanged)

		// Which version is latest is not recomputed.
		last := v2
		last.Visibility, last.Listed, last.StatusOnly, last.Version.IsLatest = model.VisibilityPublic, true, true, false
		last.Version.Status = model.MCPStatusDeleted
		mirrorMCP(t, st, last, model.MirrorUpdated)
		if _, err := st.GetMCPVersion(ctx, srv.ID, "latest"); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("latest after its version was marked deleted: %v", err)
		}
		if got := mcpServer(t, st, "org.mirror/gone"); got.Visibility != model.VisibilityInternal || got.Listed {
			t.Fatalf("server = %+v", got)
		}

		// It never creates a version, a server or a namespace.
		absent := mirrorInput(up, "org.mirror/gone", "3.0.0")
		absent.StatusOnly = true
		if _, err := st.UpsertMirroredMCP(ctx, absent, audit); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("status of a version that is not stored: %v", err)
		}
		absent = mirrorInput(up, "org.nowhere/srv", "1.0.0")
		absent.StatusOnly = true
		if _, err := st.UpsertMirroredMCP(ctx, absent, audit); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("status of a server that is not stored: %v", err)
		}
		if _, err := st.GetMCPServer(ctx, "org.nowhere/srv"); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("a status-only change created a server: %v", err)
		}
		if _, err := st.GetNamespace(ctx, "org.nowhere"); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("a status-only change created a namespace: %v", err)
		}

		// Nor is it a way around the upstream's own limit.
		wide := mirrorInput(up, "org.mirror/wide", "1.0.0")
		mirrorMCP(t, st, wide, model.MirrorCreated)
		internal := model.VisibilityInternal
		lowered, err := st.UpdateUpstream(ctx, "up", model.UpstreamPatch{Visibility: &internal}, audit)
		if err != nil {
			t.Fatal(err)
		}
		wide.ConfigVersion, wide.StatusOnly, wide.Version.Status = lowered.ConfigVersion, true, model.MCPStatusDeprecated
		mirrorMCP(t, st, wide, model.MirrorUpdated)
		if got := mcpServer(t, st, "org.mirror/wide"); got.Visibility != model.VisibilityInternal {
			t.Fatalf("server of an internal upstream = %+v", got)
		}
	}},
	{"mirror locks exclude each other by key", func(t *testing.T, st store.Store) {
		ctx := context.Background()
		release, err := st.LockMirror(ctx, "skill:org.a/x")
		if err != nil {
			t.Fatal(err)
		}
		other, err := st.LockMirror(ctx, "skill:org.a/y")
		if err != nil {
			t.Fatalf("another key must not wait: %v", err)
		}
		other()
		short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		if _, err := st.LockMirror(short, "skill:org.a/x"); err == nil {
			t.Fatal("the same key was locked twice")
		}
		cancel()
		got := make(chan error, 1)
		go func() {
			again, err := st.LockMirror(ctx, "skill:org.a/x")
			if err == nil {
				again()
			}
			got <- err
		}()
		select {
		case err := <-got:
			t.Fatalf("a waiter got the lock while it was held: %v", err)
		case <-time.After(150 * time.Millisecond):
		}
		release()
		select {
		case err := <-got:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the waiter never got the lock after release")
		}
	}},
	{"mutations under a guard stop when the settings change", func(t *testing.T, st store.Store) {
		// A sync carries the version of the settings it read in its context
		// (model.WithUpstreamGuard). Once they change, none of its writes go
		// through any more: not new entries, and not changes to existing ones.
		ctx := context.Background()
		up := newUpstream(t, st, "up")
		guarded := model.WithUpstreamGuard(ctx, up.ID, up.ConfigVersion)
		mirror := mirrorInput(up, "org.mirror/srv", "1.0.0")
		if got, err := st.UpsertMirroredMCP(guarded, mirror, audit); err != nil || got != model.MirrorCreated {
			t.Fatalf("mirror write under current settings: %s, %v", got, err)
		}
		v := version("1", "spdx")
		v.Source, v.Locator = nil, &model.Locator{Kind: "registry"}
		sk, ver, err := st.PublishVersion(guarded, model.PublishInput{Namespace: "org.mirror", Name: "tool", Mode: "indexed",
			SourceKey: "upstream:up:org.mirror/tool", Visibility: model.VisibilityPublic, Listed: true, UpstreamID: up.ID,
			UpstreamConfigVersion: up.ConfigVersion, Version: v}, audit)
		if err != nil {
			t.Fatal(err)
		}
		srv := mcpServer(t, st, "org.mirror/srv")

		deny := []string{"org.mirror"}
		if _, err := st.UpdateUpstream(ctx, "up", model.UpstreamPatch{NamespaceDeny: &deny}, audit); err != nil {
			t.Fatal(err)
		}
		private, unlisted := model.VisibilityPrivate, false
		for name, mutate := range map[string]func() error{
			"UpdateSkill": func() error {
				_, err := st.UpdateSkill(guarded, sk.ID, model.SkillPatch{Visibility: &private, Listed: &unlisted}, audit)
				return err
			},
			"SetSkillStatus": func() error {
				_, err := st.SetSkillStatus(guarded, sk.ID, model.StatusDeprecated, "x", audit)
				return err
			},
			"YankVersion":            func() error { return st.YankVersion(guarded, ver.ID, "x", time.Now(), audit) },
			"ApproveLicense":         func() error { return st.ApproveLicense(guarded, ver.ID, audit) },
			"SetVersionMirrorServed": func() error { return st.SetVersionMirrorServed(guarded, ver.ID, true, audit) },
			"UpdateMCPServer": func() error {
				_, err := st.UpdateMCPServer(guarded, srv.ID, model.MCPPatch{Visibility: &private}, audit)
				return err
			},
			"SetMCPStatus": func() error {
				_, err := st.SetMCPStatus(guarded, srv.ID, "", model.MCPStatusDeleted, "x", time.Now(), audit)
				return err
			},
			"EnsureUpstreamNamespace": func() error {
				_, err := st.EnsureUpstreamNamespace(guarded, "org.other", up.ID, audit)
				return err
			},
			"UpsertMirroredMCP": func() error {
				in := mirrorInput(up, "org.mirror/srv", "2.0.0")
				_, err := st.UpsertMirroredMCP(guarded, in, audit)
				return err
			},
		} {
			if err := mutate(); conflictCode(err) != "upstream_config_changed" {
				t.Errorf("%s under changed settings: %v", name, err)
			}
		}
		// Nothing was applied.
		if got, _ := st.GetSkill(ctx, "org.mirror", "tool"); got.Visibility != model.VisibilityPublic || !got.Listed || got.Status != model.StatusActive {
			t.Errorf("skill changed under a stale guard: %+v", got)
		}
		if got, _ := st.GetVersion(ctx, sk.ID, "1"); got.YankedAt != nil || got.LicenseApproved || got.MirrorServed != nil {
			t.Errorf("version changed under a stale guard: %+v", got)
		}
		if got := mcpServer(t, st, "org.mirror/srv"); got.Visibility != model.VisibilityPublic {
			t.Errorf("server changed under a stale guard: %+v", got)
		}
		if got := mcpVersion(t, st, srv.ID, "1.0.0"); got.Status != model.MCPStatusActive {
			t.Errorf("server version changed under a stale guard: %+v", got)
		}
		if _, err := st.GetNamespace(ctx, "org.other"); !errors.Is(err, model.ErrNotFound) {
			t.Errorf("namespace created under a stale guard: %v", err)
		}
		// Without a guard (an administrator, a publisher) the same calls work.
		if _, err := st.SetSkillStatus(ctx, sk.ID, model.StatusDeprecated, "x", audit); err != nil {
			t.Errorf("unguarded mutation: %v", err)
		}
		// Recording a source check is not a mirror write and is never refused.
		if err := st.SetVersionCheck(guarded, ver.ID, model.CheckOK, time.Now()); err != nil {
			t.Errorf("source check under a stale guard: %v", err)
		}
	}},
	{"mirrored namespaces have no members", func(t *testing.T, st store.Store) {
		ctx := context.Background()
		up := newUpstream(t, st, "up")
		if _, err := st.EnsureUpstreamNamespace(ctx, "org.mirror", up.ID, audit); err != nil {
			t.Fatal(err)
		}
		if err := st.SetRole(ctx, "org.mirror", "token:aaaaaaaaaaaaaaaa", model.RoleReader, audit); conflictCode(err) != "namespace_upstream_managed" {
			t.Fatalf("role in a mirrored namespace: %v", err)
		}
		tok := model.Token{ID: "bbbbbbbbbbbbbbbb", SecretHash: make([]byte, 32), Name: "t", Scopes: []string{"skills:read"}, CreatedBy: "test"}
		err := st.CreateToken(ctx, tok, []model.RoleBinding{{Namespace: "com.example", Role: model.RoleReader}, {Namespace: "org.mirror", Role: model.RoleReader}}, audit)
		if conflictCode(err) != "namespace_upstream_managed" {
			t.Fatalf("token with a role in a mirrored namespace: %v", err)
		}
		if _, err := st.GetToken(ctx, tok.ID); err == nil {
			t.Fatal("the refused token was created")
		}
		if roles, _ := st.RolesForSubject(ctx, "token:"+tok.ID); len(roles) != 0 {
			t.Fatalf("roles of the refused token: %v", roles)
		}
		if err := st.SetRole(ctx, "com.example", "token:aaaaaaaaaaaaaaaa", model.RoleReader, audit); err != nil {
			t.Fatalf("role in a local namespace: %v", err)
		}
	}},
	{"upstream policy changes", func(t *testing.T, st store.Store) {
		ctx := context.Background()
		up := newUpstream(t, st, "up")
		mirrorMCP(t, st, mirrorInput(up, "org.mirror/srv", "1.0.0"), model.MirrorCreated)
		v := version("1", "spdx")
		v.Source, v.Locator = nil, &model.Locator{Kind: "registry"}
		sk, _, err := st.PublishVersion(ctx, model.PublishInput{Namespace: "org.mirror", Name: "tool", Mode: "indexed",
			SourceKey: "upstream:up:org.mirror/tool", Visibility: model.VisibilityPublic, Listed: true, UpstreamID: up.ID, Version: v}, audit)
		if err != nil {
			t.Fatal(err)
		}
		local, _, err := publish(st, "local", model.VisibilityPublic, "1", "spdx")
		if err != nil {
			t.Fatal(err)
		}
		setCursor := func() {
			t.Helper()
			if ok, err := st.ClaimUpstreamSync(ctx, up.ID, time.Now(), time.Now().Add(-time.Hour)); err != nil || !ok {
				t.Fatalf("claim: %v %v", ok, err)
			}
			cur, err := st.GetUpstream(ctx, "up")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.FinishUpstreamSync(ctx, up.ID, model.UpstreamSyncResult{At: time.Now(), Status: model.SyncOK, Cursor: "c",
				ConfigVersion: cur.ConfigVersion}, audit); err != nil {
				t.Fatal(err)
			}
			if cur, _ = st.GetUpstream(ctx, "up"); cur.Cursor != "c" {
				t.Fatalf("cursor not stored: %+v", cur)
			}
		}
		cursor := func() string {
			t.Helper()
			u, err := st.GetUpstream(ctx, "up")
			if err != nil {
				t.Fatal(err)
			}
			return u.Cursor
		}
		// A change to what is selected starts the sync over; other settings
		// and patches that change nothing keep the cursor.
		yes, no, allow, sameAllow, deny := true, false, []string{"org.*"}, []string{"org.*"}, []string{"org.x"}
		prio, public := 5, model.VisibilityPublic
		for _, tc := range []struct {
			name  string
			patch model.UpstreamPatch
			reset bool
		}{
			{"allow list", model.UpstreamPatch{NamespaceAllow: &allow}, true},
			{"same allow list", model.UpstreamPatch{NamespaceAllow: &sameAllow}, false},
			{"deny list", model.UpstreamPatch{NamespaceDeny: &deny}, true},
			{"latest only on", model.UpstreamPatch{LatestOnly: &yes}, true},
			{"latest only off", model.UpstreamPatch{LatestOnly: &no}, true},
			{"same visibility", model.UpstreamPatch{Visibility: &public}, false},
			{"priority", model.UpstreamPatch{Priority: &prio, ProxyOnMiss: &yes}, false},
		} {
			name := tc.name
			setCursor()
			if _, err := st.UpdateUpstream(ctx, "up", tc.patch, audit); err != nil {
				t.Fatal(err)
			}
			if got := cursor(); (got == "") != tc.reset {
				t.Errorf("%s: cursor %q, reset wanted %v", name, got, tc.reset)
			}
		}
		// Lowering visibility applies to what is already mirrored, at once,
		// and to nothing else.
		setCursor()
		internal := model.VisibilityInternal
		if _, err := st.UpdateUpstream(ctx, "up", model.UpstreamPatch{Visibility: &internal}, audit); err != nil {
			t.Fatal(err)
		}
		if cursor() != "" {
			t.Error("a visibility change must reset the cursor")
		}
		if got, _ := st.GetSkill(ctx, "org.mirror", "tool"); got.ID != sk.ID || got.Visibility != model.VisibilityInternal {
			t.Errorf("mirrored skill after lowering visibility: %+v", got)
		}
		if got := mcpServer(t, st, "org.mirror/srv"); got.Visibility != model.VisibilityInternal {
			t.Errorf("mirrored server after lowering visibility: %+v", got)
		}
		if got, _ := st.GetSkill(ctx, "com.example", "local"); got.ID != local.ID || got.Visibility != model.VisibilityPublic {
			t.Errorf("a local skill was touched: %+v", got)
		}
	}},
}
