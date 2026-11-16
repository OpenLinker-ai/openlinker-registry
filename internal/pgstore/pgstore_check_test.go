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
