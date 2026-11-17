package pgstore_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenLinker-ai/openlinker-registry/internal/apitest"
	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/pgstore"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store/storetest"
)

// DSNEnv names the variable that enables Postgres integration tests.
const DSNEnv = "REGISTRY_TEST_DATABASE_URL"

// newStore creates an isolated schema, migrates it and drops it afterwards.
func newStore(t *testing.T) *pgstore.Store {
	st, _ := newStoreSchema(t)
	return st
}

func newStoreSchema(t *testing.T) (*pgstore.Store, string) {
	t.Helper()
	st, schema := openSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate is not idempotent: %v", err)
	}
	return st, schema
}

// openSchema creates an isolated, empty schema (no migration applied) and a
// store on it; the schema is dropped afterwards.
func openSchema(t *testing.T) (*pgstore.Store, string) {
	t.Helper()
	dsn := os.Getenv(DSNEnv)
	if dsn == "" {
		t.Skip(DSNEnv + " is not set; skipping Postgres integration tests")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close(context.Background())
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	st, err := pgstore.OpenConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st, schema
}

func TestEndToEndPostgres(t *testing.T) {
	if os.Getenv(DSNEnv) == "" {
		t.Skip(DSNEnv + " is not set; skipping Postgres integration tests")
	}
	apitest.Run(t, func(t *testing.T) store.Store { return newStore(t) })
}

func TestStoreConformancePostgres(t *testing.T) {
	if os.Getenv(DSNEnv) == "" {
		t.Skip(DSNEnv + " is not set; skipping Postgres integration tests")
	}
	storetest.Run(t, func(t *testing.T) store.Store { return newStore(t) })
}

// TestYankTerminalTrigger proves the schema rejects leaving the yanked state
// even for statements that bypass the store.
func TestYankTerminalTrigger(t *testing.T) {
	st, schema := newStoreSchema(t)
	ctx := context.Background()
	if _, err := st.CreateNamespace(ctx, model.Namespace{Name: "com.example", Kind: "domain"}, model.AuditEvent{Actor: "t", Action: "ns"}); err != nil {
		t.Fatal(err)
	}
	v := model.Version{Version: "1", FrontmatterName: "x", FrontmatterDescription: "y", LicenseKind: skill.LicenseSPDX,
		Files: []skill.ManifestEntry{}, ArtifactDigest: "sha256:" + strings.Repeat("b", 64), ArtifactType: "skill-md",
		Source: &model.Source{Kind: "upload"}, CreatedBy: "t"}
	sk, _, err := st.PublishVersion(ctx, model.PublishInput{Namespace: "com.example", Name: "x", Mode: skill.ModeHosted,
		Visibility: model.VisibilityPublic, Listed: true, Version: v}, model.AuditEvent{Actor: "t", Action: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetSkillStatus(ctx, sk.ID, model.StatusYanked, "r", model.AuditEvent{Actor: "t", Action: "y"}); err != nil {
		t.Fatal(err)
	}
	conn := connSchema(t, schema)
	if _, err := conn.Exec(ctx, `UPDATE skills SET status = 'deprecated' WHERE id = $1`, sk.ID); err == nil {
		t.Fatal("raw un-yank must be rejected by the schema")
	}
}

func TestImmutabilityAndAppendOnly(t *testing.T) {
	st, schema := newStoreSchema(t)
	ctx := context.Background()
	if _, err := st.CreateNamespace(ctx, model.Namespace{Name: "com.example", Kind: "domain"}, model.AuditEvent{Actor: "t", Action: "namespace.create"}); err != nil {
		t.Fatal(err)
	}
	v := model.Version{Version: "1", FrontmatterName: "x", FrontmatterDescription: "y", LicenseKind: skill.LicenseSPDX,
		LicenseID: "MIT", Files: []skill.ManifestEntry{{Path: "SKILL.md", Size: 1, SHA256: strings.Repeat("a", 64)}},
		ArtifactDigest: "sha256:" + strings.Repeat("b", 64), ArtifactType: "skill-md", ArtifactSize: 1,
		Source: &model.Source{Kind: "upload"}, CreatedBy: "test"}
	sk, ver, err := st.PublishVersion(ctx, model.PublishInput{Namespace: "com.example", Name: "x", Mode: skill.ModeHosted,
		Visibility: model.VisibilityPublic, Listed: true, Version: v}, model.AuditEvent{Actor: "t", Action: "skill.publish"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.PublishVersion(ctx, model.PublishInput{Namespace: "com.example", Name: "x", Mode: skill.ModeIndexed,
		SourceKey: "github:a/b:/", Visibility: model.VisibilityPublic, Version: v}, model.AuditEvent{Actor: "t", Action: "x"}); err == nil {
		t.Fatal("mode change must conflict")
	}
	// Direct SQL cannot rewrite or delete a version or an audit event.
	pool := connSchema(t, schema)
	if _, err := pool.Exec(ctx, `UPDATE skill_versions SET artifact_digest = $2 WHERE id = $1`, ver.ID, "sha256:"+strings.Repeat("c", 64)); err == nil {
		t.Fatal("content update must be rejected")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM skill_versions WHERE id = $1`, ver.ID); err == nil {
		t.Fatal("delete must be rejected")
	}
	if _, err := pool.Exec(ctx, `TRUNCATE skill_versions`); err == nil {
		t.Fatal("truncate of versions must be rejected")
	}
	if _, err := pool.Exec(ctx, `TRUNCATE skills CASCADE`); err == nil {
		t.Fatal("cascading truncate of versions must be rejected")
	}
	if err := st.YankVersion(ctx, ver.ID, "r", time.Now(), model.AuditEvent{Actor: "t", Action: "version.yank"}); err != nil {
		t.Fatalf("yank must be allowed: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE skill_versions SET yanked_at = NULL WHERE id = $1`, ver.ID); err == nil {
		t.Fatal("unyank must be rejected")
	}
	if err := st.AppendAudit(ctx, model.AuditEvent{Actor: "a", Action: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_events SET actor = 'b'`); err == nil {
		t.Fatal("audit update must be rejected")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM audit_events`); err == nil {
		t.Fatal("audit delete must be rejected")
	}
	if _, err := pool.Exec(ctx, `TRUNCATE audit_events`); err == nil {
		t.Fatal("audit truncate must be rejected")
	}
	_ = sk
}

// connSchema opens a raw connection on the test schema for SQL checks.
func connSchema(t *testing.T, schema string) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, os.Getenv(DSNEnv))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	if _, err := c.Exec(ctx, "SET search_path = "+schema); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestAuditIsAtomic proves a mutation is rolled back when its audit row
// cannot be written.
func TestAuditIsAtomic(t *testing.T) {
	st, schema := newStoreSchema(t)
	ctx := context.Background()
	if _, err := st.CreateNamespace(ctx, model.Namespace{Name: "com.example", Kind: "domain"}, model.AuditEvent{Actor: "t", Action: "ns"}); err != nil {
		t.Fatal(err)
	}
	conn := connSchema(t, schema)
	if _, err := conn.Exec(ctx, `DROP TABLE audit_events`); err != nil {
		t.Fatal(err)
	}
	v := model.Version{Version: "1", FrontmatterName: "x", FrontmatterDescription: "y", LicenseKind: skill.LicenseSPDX,
		Files: []skill.ManifestEntry{}, ArtifactDigest: "sha256:" + strings.Repeat("b", 64), ArtifactType: "skill-md",
		Source: &model.Source{Kind: "upload"}, CreatedBy: "t"}
	if _, _, err := st.PublishVersion(ctx, model.PublishInput{Namespace: "com.example", Name: "x", Mode: skill.ModeHosted,
		Visibility: model.VisibilityPublic, Listed: true, Version: v}, model.AuditEvent{Actor: "t", Action: "skill.publish"}); err == nil {
		t.Fatal("publish must fail when the audit row cannot be written")
	}
	if _, err := st.GetSkill(ctx, "com.example", "x"); err != model.ErrNotFound {
		t.Fatalf("publish was not rolled back: %v", err)
	}
	if _, err := st.CreateNamespace(ctx, model.Namespace{Name: "org.example", Kind: "domain"}, model.AuditEvent{Actor: "t", Action: "ns"}); err == nil {
		t.Fatal("namespace creation must fail without audit")
	}
	if _, err := st.GetNamespace(ctx, "org.example"); err != model.ErrNotFound {
		t.Fatalf("namespace creation was not rolled back: %v", err)
	}
}
