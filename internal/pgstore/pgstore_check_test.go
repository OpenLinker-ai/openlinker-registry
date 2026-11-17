package pgstore_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
)

// A check result is recorded for the upstream settings it was made under.
// While it is being recorded those settings cannot change, so a result made
// under earlier settings can never be written after one made under later
// ones: the change of settings waits for the write, and a write that comes
// after the change is dropped.
func TestCheckResultHoldsUpstreamSettings(t *testing.T) {
	st, schema := newStoreSchema(t)
	ctx := context.Background()
	audit := model.AuditEvent{Actor: "t", Action: "test"}
	up, err := st.CreateUpstream(ctx, model.Upstream{Name: "up", Kind: model.UpstreamRegistry, BaseURL: "https://old.example",
		Enabled: true, Priority: 100, Visibility: model.VisibilityPublic}, audit)
	if err != nil {
		t.Fatal(err)
	}
	_, v, err := st.PublishVersion(ctx, model.PublishInput{Namespace: "org.mirror", Name: "tool", Mode: skill.ModeIndexed,
		SourceKey: "upstream:up:org.mirror/tool", Visibility: model.VisibilityPublic, Listed: true, UpstreamID: up.ID,
		UpstreamConfigVersion: up.ConfigVersion, Version: model.Version{Version: "1", FrontmatterName: "tool", FrontmatterDescription: "d",
			LicenseKind: skill.LicenseSPDX, Files: []skill.ManifestEntry{}, ArtifactDigest: "sha256:" + strings.Repeat("c", 64),
			ArtifactType: "skill-md", Locator: &model.Locator{Kind: "registry", Upstream: "up", Namespace: "org.mirror", Name: "tool", Version: "1"},
			CreatedBy: "t"}}, audit)
	if err != nil {
		t.Fatal(err)
	}
	old := model.CheckSource{UpstreamID: up.ID, ConfigVersion: up.ConfigVersion}

	// Something else holds the version row, so the check is under way but
	// cannot finish yet.
	conn := connSchema(t, schema)
	hold, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec(ctx, `SELECT 1 FROM skill_versions WHERE id = $1 FOR UPDATE`, v.ID); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Second)
	checked := make(chan error, 1)
	go func() { checked <- st.SetVersionCheck(ctx, v.ID, old, model.CheckSourceUnavailable, at) }()
	select {
	case err := <-checked:
		t.Fatalf("the check finished although the version row is held: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	base := "https://new.example"
	updated := make(chan model.Upstream, 1)
	failed := make(chan error, 1)
	go func() {
		u, err := st.UpdateUpstream(ctx, "up", model.UpstreamPatch{BaseURL: &base}, audit)
		if err != nil {
			failed <- err
			return
		}
		updated <- u
	}()
	select {
	case <-updated:
		t.Fatal("the upstream's settings changed while a check made under them was still being recorded")
	case err := <-failed:
		t.Fatalf("update: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-checked; err != nil {
		t.Fatalf("check: %v", err)
	}
	var now model.Upstream
	select {
	case now = <-updated:
	case err := <-failed:
		t.Fatalf("update: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the update did not finish after the check was recorded")
	}
	if now.ConfigVersion == up.ConfigVersion {
		t.Fatal("a new base URL is a change of settings")
	}
	got, err := st.GetVersion(ctx, v.SkillID, "1")
	if err != nil || got.CheckResult != model.CheckSourceUnavailable {
		t.Fatalf("the check made before the change: %q, %v", got.CheckResult, err)
	}
	// The source is verified through the new settings; a result of the old
	// ones that arrives now is dropped.
	if err := st.SetVersionCheck(ctx, v.ID, model.CheckSource{UpstreamID: up.ID, ConfigVersion: now.ConfigVersion}, model.CheckOK, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetVersionCheck(ctx, v.ID, old, model.CheckSourceUnavailable, at.Add(2*time.Minute)); err != nil {
		t.Fatalf("a stale result is dropped, not an error: %v", err)
	}
	if got, _ := st.GetVersion(ctx, v.SkillID, "1"); got.CheckResult != model.CheckOK {
		t.Fatalf("a result made under earlier settings replaced a newer one: %q", got.CheckResult)
	}
}

// Versions that are not semantic versions are ordered by publication. A
// publish whose transaction began before another one committed, but that
// took the server lock after it, is the later publish: it is the latest
// version, and it must stay the latest when "latest" is decided again.
func TestPublicationOrderIsLockOrder(t *testing.T) {
	st, schema := newStoreSchema(t)
	ctx := context.Background()
	if _, err := st.CreateNamespace(ctx, model.Namespace{Name: "com.acme", Kind: "domain"}, testAudit); err != nil {
		t.Fatal(err)
	}
	// An upstream row that the first publish is made to wait for, after its
	// transaction has begun.
	gate, err := st.CreateUpstream(ctx, model.Upstream{Name: "gate", Kind: model.UpstreamMCP, BaseURL: "https://gate.example",
		Enabled: true, Priority: 100, Visibility: model.VisibilityPublic}, testAudit)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := publishTestMCP(t, st, "com.acme/s", "nightly-0")
	publish := func(ctx context.Context, version string) error {
		_, _, err := st.PublishMCPVersion(ctx, model.MCPPublishInput{Namespace: "com.acme", Name: "com.acme/s",
			Mode: model.MCPModeRegistered, Visibility: model.VisibilityPublic, Listed: true,
			Version: model.MCPVersion{Version: version, ServerJSON: []byte(`{"name": "com.acme/s", "version": "` + version + `"}`),
				Digest: "sha256:" + strings.Repeat("e", 64), CreatedBy: "t"}}, testAudit)
		return err
	}
	conn := connSchema(t, schema)
	hold, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec(ctx, `SELECT 1 FROM upstreams WHERE id = $1 FOR UPDATE`, gate.ID); err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() { first <- publish(model.WithUpstreamGuard(ctx, gate.ID, gate.ConfigVersion), "nightly-a") }()
	select {
	case err := <-first:
		t.Fatalf("the first publish did not wait: %v", err)
	case <-time.After(300 * time.Millisecond): // its transaction has begun
	}
	if err := publish(ctx, "nightly-b"); err != nil {
		t.Fatal(err)
	}
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	latest := func() string {
		v, err := st.GetMCPVersion(ctx, srv.ID, "latest")
		if err != nil {
			t.Fatal(err)
		}
		return v.Version
	}
	if got := latest(); got != "nightly-a" {
		t.Fatalf("latest after both publishes = %s, want the one that was published last", got)
	}
	a, _ := st.GetMCPVersion(ctx, srv.ID, "nightly-a")
	b, _ := st.GetMCPVersion(ctx, srv.ID, "nightly-b")
	if !a.PublishedAt.After(b.PublishedAt) {
		t.Fatalf("published_at does not follow the order of publication: a %v, b %v", a.PublishedAt, b.PublishedAt)
	}
	// Deciding "latest" again must not change it: a status update that
	// changes nothing, and one that does.
	for _, status := range []string{model.MCPStatusActive, model.MCPStatusDeprecated} {
		if _, err := st.SetMCPStatus(ctx, srv.ID, "nightly-0", status, "", time.Now(), testAudit); err != nil {
			t.Fatal(err)
		}
		if got := latest(); got != "nightly-a" {
			t.Fatalf("latest after setting an old version %s = %s", status, got)
		}
	}
}
