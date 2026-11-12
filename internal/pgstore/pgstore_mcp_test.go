package pgstore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/pgstore"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
)

var testAudit = model.AuditEvent{Actor: "t", Action: "test"}

func appliedMigrations(t *testing.T, conn *pgx.Conn) []int {
	t.Helper()
	rows, err := conn.Query(context.Background(), `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		t.Fatal(err)
	}
	return versions
}

// mustFail runs a raw statement that the schema has to reject and returns
// the error text.
func mustFail(t *testing.T, conn *pgx.Conn, what, sql string, args ...any) string {
	t.Helper()
	_, err := conn.Exec(context.Background(), sql, args...)
	if err == nil {
		t.Fatalf("%s must be rejected by the schema", what)
	}
	return err.Error()
}

func publishTestMCP(t *testing.T, st *pgstore.Store, name, version string) (model.MCPServer, model.MCPVersion) {
	t.Helper()
	ns, _, _ := strings.Cut(name, "/")
	srv, v, err := st.PublishMCPVersion(context.Background(), model.MCPPublishInput{Namespace: ns, Name: name,
		Mode: model.MCPModeRegistered, Visibility: model.VisibilityPublic, Listed: true,
		Version: model.MCPVersion{Version: version, ServerJSON: []byte(`{"name": "` + name + `", "version": "` + version + `"}`),
			Digest: "sha256:" + strings.Repeat("d", 64), CreatedBy: "t"}}, testAudit)
	if err != nil {
		t.Fatalf("publish %s@%s: %v", name, version, err)
	}
	return srv, v
}

// TestMigration0004Fresh checks the schema a new database ends up with.
func TestMigration0004Fresh(t *testing.T) {
	st, schema := newStoreSchema(t)
	ctx := context.Background()
	conn := connSchema(t, schema)
	if got := appliedMigrations(t, conn); !slices.Equal(got, []int{1, 2, 3, 4}) {
		t.Fatalf("applied migrations = %v", got)
	}
	for _, table := range []string{"upstreams", "mcp_servers", "mcp_server_versions"} {
		var n int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("table %s: %d rows, %v", table, n, err)
		}
	}
	if _, err := st.CreateNamespace(ctx, model.Namespace{Name: "com.example", Kind: "domain"}, testAudit); err != nil {
		t.Fatal(err)
	}
	up, err := st.CreateUpstream(ctx, model.Upstream{Name: "up", Kind: model.UpstreamMCP, BaseURL: "https://up.example",
		Visibility: model.VisibilityPublic}, testAudit)
	if err != nil {
		t.Fatal(err)
	}
	srv, v1 := publishTestMCP(t, st, "com.example/server", "1.0.0")
	_, v2 := publishTestMCP(t, st, "com.example/server", "2.0.0")

	// The kind and the owner of a namespace always agree.
	mustFail(t, conn, "an upstream namespace without an upstream", `INSERT INTO namespaces (name, kind) VALUES ('org.a', 'upstream')`)
	mustFail(t, conn, "a local namespace with an upstream", `INSERT INTO namespaces (name, kind, upstream_id) VALUES ('org.b', 'domain', $1)`, up.ID)
	mustFail(t, conn, "a namespace of a missing upstream", `INSERT INTO namespaces (name, kind, upstream_id) VALUES ('org.c', 'upstream', $1)`, up.ID+1000)
	// One latest version per server, never a deleted one, never named "latest".
	mustFail(t, conn, "a second latest version", `UPDATE mcp_server_versions SET is_latest = true WHERE id = $1`, v1.ID)
	mustFail(t, conn, "a deleted latest version", `UPDATE mcp_server_versions SET status = 'deleted' WHERE id = $1`, v2.ID)
	mustFail(t, conn, "an active version with a message", `UPDATE mcp_server_versions SET status_message = 'x' WHERE id = $1`, v1.ID)
	mustFail(t, conn, `a version named "latest"`, `INSERT INTO mcp_server_versions (server_id, version, server_json, digest, created_by)
		VALUES ($1, 'latest', '{}', $2, 't')`, srv.ID, "sha256:"+strings.Repeat("d", 64))
	mustFail(t, conn, "a server name differing by letter case", `INSERT INTO mcp_servers (namespace_id, name, mode, visibility, created_by)
		SELECT namespace_id, 'com.example/SERVER', mode, visibility, created_by FROM mcp_servers WHERE id = $1`, srv.ID)
	mustFail(t, conn, "an agent-skills upstream without a target namespace", `INSERT INTO upstreams (name, kind, base_url, visibility)
		VALUES ('skills', 'agent-skills', 'https://s.example', 'public')`)
	mustFail(t, conn, "an upstream mirroring as private", `INSERT INTO upstreams (name, kind, base_url, visibility)
		VALUES ('private', 'mcp-registry', 'https://p.example', 'private')`)
}

// TestMigration0004Upgrade applies 0004 on a database that holds R1 data.
func TestMigration0004Upgrade(t *testing.T) {
	st, schema := openSchema(t)
	ctx := context.Background()
	conn := connSchema(t, schema)
	if _, err := conn.Exec(ctx, `CREATE TABLE schema_migrations (version INT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"0001_init.sql", "0002_versions_no_truncate.sql", "0003_skills_yank_terminal.sql"} {
		body, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, string(body)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := conn.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, i+1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO namespaces (name, kind, github_id, display_name) VALUES ('io.github.alice', 'github', 42, 'Alice'), ('com.example', 'domain', NULL, '');
		INSERT INTO skills (namespace_id, name, description, visibility, mode)
			SELECT id, 'demo', 'R1 skill', 'public', 'hosted' FROM namespaces WHERE name = 'com.example';
		INSERT INTO skill_versions (skill_id, version, mode, frontmatter_name, frontmatter_description, license_kind, core_compatible,
			files, artifact_digest, artifact_type, artifact_size, source, created_by)
			SELECT id, '1', 'hosted', 'demo', 'R1 skill', 'spdx', true, '[]', 'sha256:`+strings.Repeat("b", 64)+`', 'skill-md', 1,
				'{"kind": "upload"}', 't' FROM skills`); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate from R1: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate is not idempotent: %v", err)
	}
	if got := appliedMigrations(t, conn); !slices.Equal(got, []int{1, 2, 3, 4}) {
		t.Fatalf("applied migrations = %v", got)
	}

	// Existing namespaces stay local and existing skills stay readable.
	all, err := st.ListNamespaces(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("%d namespaces, %v", len(all), err)
	}
	if ns := all[1]; ns.Name != "io.github.alice" || ns.Kind != "github" || ns.GitHubID == nil || *ns.GitHubID != 42 ||
		ns.DisplayName != "Alice" || ns.UpstreamID != 0 || all[0].Name != "com.example" || all[0].UpstreamID != 0 {
		t.Fatalf("namespaces = %+v", all)
	}
	sk, err := st.GetSkill(ctx, "com.example", "demo")
	if err != nil || sk.Upstream != "" || sk.Description != "R1 skill" {
		t.Fatalf("R1 skill = %+v, %v", sk, err)
	}
	v := model.Version{Version: "2", FrontmatterName: "demo", FrontmatterDescription: "after upgrade", LicenseKind: skill.LicenseSPDX,
		Files: []skill.ManifestEntry{}, ArtifactDigest: "sha256:" + strings.Repeat("c", 64), ArtifactType: "skill-md",
		Source: &model.Source{Kind: "upload"}, CreatedBy: "t"}
	if _, _, err := st.PublishVersion(ctx, model.PublishInput{Namespace: "com.example", Name: "demo", Mode: skill.ModeHosted,
		Visibility: model.VisibilityPublic, Listed: true, Version: v}, testAudit); err != nil {
		t.Fatalf("publish into an R1 skill: %v", err)
	}
	if found, err := st.Search(ctx, model.SearchQuery{}); err != nil || len(found) != 1 || found[0].LatestVersion != "2" {
		t.Fatalf("search after upgrade: %+v, %v", found, err)
	}

	// The new features work next to the old data, and R1 namespaces cannot
	// be taken over by an upstream.
	up, err := st.CreateUpstream(ctx, model.Upstream{Name: "up", Kind: model.UpstreamMCP, BaseURL: "https://up.example",
		Visibility: model.VisibilityPublic}, testAudit)
	if err != nil {
		t.Fatal(err)
	}
	var ce *model.ConflictError
	if _, err := st.EnsureUpstreamNamespace(ctx, "com.example", up.ID); !errors.As(err, &ce) || ce.Code != "namespace_upstream_managed" {
		t.Fatalf("upstream claims an R1 namespace: %v", err)
	}
	if _, err := st.EnsureUpstreamNamespace(ctx, "org.mirror", up.ID); err != nil {
		t.Fatal(err)
	}
	publishTestMCP(t, st, "com.example/server", "1.0.0")
}

// TestMCPVersionsImmutable proves the schema rejects rewriting, deleting or
// truncating published MCP versions, for statements that bypass the store.
func TestMCPVersionsImmutable(t *testing.T) {
	st, schema := newStoreSchema(t)
	ctx := context.Background()
	for _, ns := range []string{"com.example", "org.other"} {
		if _, err := st.CreateNamespace(ctx, model.Namespace{Name: ns, Kind: "domain"}, testAudit); err != nil {
			t.Fatal(err)
		}
	}
	srv, ver := publishTestMCP(t, st, "com.example/server", "1.0.0")
	other, _ := publishTestMCP(t, st, "org.other/server", "1.0.0")
	conn := connSchema(t, schema)

	for column, value := range map[string]any{
		"server_json":  `{"name": "forged"}`,
		"digest":       "sha256:" + strings.Repeat("e", 64),
		"version":      "9.9.9",
		"published_at": time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		"created_by":   "someone else",
		"server_id":    other.ID,
		"id":           ver.ID + 1000,
	} {
		msg := mustFail(t, conn, "an update of "+column, `UPDATE mcp_server_versions SET `+column+` = $2 WHERE id = $1`, ver.ID, value)
		if !strings.Contains(msg, "content columns are immutable") {
			t.Fatalf("update of %s was rejected by something else: %s", column, msg)
		}
	}
	if msg := mustFail(t, conn, "a delete", `DELETE FROM mcp_server_versions WHERE id = $1`, ver.ID); !strings.Contains(msg, "cannot be deleted") {
		t.Fatalf("delete was rejected by something else: %s", msg)
	}
	for _, stmt := range []string{`TRUNCATE mcp_server_versions`, `TRUNCATE mcp_servers CASCADE`, `TRUNCATE namespaces CASCADE`} {
		if msg := mustFail(t, conn, stmt, stmt); !strings.Contains(msg, "cannot be truncated") {
			t.Fatalf("%s was rejected by something else: %s", stmt, msg)
		}
	}
	mustFail(t, conn, "a delete of a server with versions", `DELETE FROM mcp_servers WHERE id = $1`, srv.ID)

	// Status fields, updated_at and the latest flag stay writable.
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := conn.Exec(ctx, `UPDATE mcp_server_versions SET status = 'deprecated', status_message = 'old', status_changed_at = $2,
		updated_at = $2, is_latest = false WHERE id = $1`, ver.ID, at); err != nil {
		t.Fatalf("status update must be allowed: %v", err)
	}
	got, err := st.GetMCPVersion(ctx, srv.ID, "1.0.0")
	if err != nil || got.ID != ver.ID || got.Digest != ver.Digest || string(got.ServerJSON) != string(ver.ServerJSON) ||
		got.CreatedBy != "t" || !got.PublishedAt.Equal(ver.PublishedAt) || got.Status != model.MCPStatusDeprecated ||
		got.StatusMessage != "old" || !got.UpdatedAt.Equal(at) || got.IsLatest {
		t.Fatalf("version after raw statements = %+v, %v", got, err)
	}
	if all, err := st.ListMCPVersions(ctx, other.ID); err != nil || len(all) != 1 {
		t.Fatalf("%d versions of the other server, %v", len(all), err)
	}
}

// TestNamespaceOwnerImmutable proves a namespace never changes hands, name
// or kind, even for statements that bypass the store.
func TestNamespaceOwnerImmutable(t *testing.T) {
	st, schema := newStoreSchema(t)
	ctx := context.Background()
	if _, err := st.CreateNamespace(ctx, model.Namespace{Name: "com.example", Kind: "domain"}, testAudit); err != nil {
		t.Fatal(err)
	}
	var ups [2]model.Upstream
	for i, name := range []string{"up", "other"} {
		var err error
		if ups[i], err = st.CreateUpstream(ctx, model.Upstream{Name: name, Kind: model.UpstreamMCP, BaseURL: "https://" + name + ".example",
			Visibility: model.VisibilityPublic}, testAudit); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.EnsureUpstreamNamespace(ctx, "org.mirror", ups[0].ID); err != nil {
		t.Fatal(err)
	}
	conn := connSchema(t, schema)
	for what, stmt := range map[string]struct {
		sql  string
		args []any
	}{
		"giving a local namespace to an upstream":   {`UPDATE namespaces SET kind = 'upstream', upstream_id = $1 WHERE name = 'com.example'`, []any{ups[0].ID}},
		"setting an upstream on a local namespace":  {`UPDATE namespaces SET upstream_id = $1 WHERE name = 'com.example'`, []any{ups[0].ID}},
		"making an upstream namespace local":        {`UPDATE namespaces SET kind = 'domain', upstream_id = NULL WHERE name = 'org.mirror'`, nil},
		"moving a namespace to another upstream":    {`UPDATE namespaces SET upstream_id = $1 WHERE name = 'org.mirror'`, []any{ups[1].ID}},
		"changing the kind of a local namespace":    {`UPDATE namespaces SET kind = 'internal' WHERE name = 'com.example'`, nil},
		"renaming a local namespace":                {`UPDATE namespaces SET name = 'com.renamed' WHERE name = 'com.example'`, nil},
		"renaming an upstream namespace":            {`UPDATE namespaces SET name = 'org.renamed' WHERE name = 'org.mirror'`, nil},
		"changing every namespace in one statement": {`UPDATE namespaces SET name = name || '.x'`, nil},
		"an upsert that rewrites the owner on conflict": {`INSERT INTO namespaces (name, kind, upstream_id) VALUES ('com.example', 'upstream', $1)
			ON CONFLICT (name) DO UPDATE SET kind = EXCLUDED.kind, upstream_id = EXCLUDED.upstream_id`, []any{ups[0].ID}},
	} {
		if msg := mustFail(t, conn, what, stmt.sql, stmt.args...); !strings.Contains(msg, "namespace name, kind and upstream are immutable") {
			t.Fatalf("%s was rejected by something else: %s", what, msg)
		}
	}
	mustFail(t, conn, "deleting an upstream that manages a namespace", `DELETE FROM upstreams WHERE id = $1`, ups[0].ID)

	// Other columns stay writable.
	if _, err := conn.Exec(ctx, `UPDATE namespaces SET display_name = 'Example' WHERE name = 'com.example'`); err != nil {
		t.Fatalf("display name update must be allowed: %v", err)
	}
	all, err := st.ListNamespaces(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("%d namespaces, %v", len(all), err)
	}
	if local, mirror := all[0], all[1]; local.Name != "com.example" || local.Kind != "domain" || local.UpstreamID != 0 || local.DisplayName != "Example" ||
		mirror.Name != "org.mirror" || mirror.Kind != model.NSKindUpstream || mirror.UpstreamID != ups[0].ID {
		t.Fatalf("namespaces = %+v", all)
	}
}

// TestAuditIsAtomicMCPAndUpstreams proves the MCP and upstream mutations are
// rolled back when their audit row cannot be written.
func TestAuditIsAtomicMCPAndUpstreams(t *testing.T) {
	st, schema := newStoreSchema(t)
	ctx := context.Background()
	if _, err := st.CreateNamespace(ctx, model.Namespace{Name: "com.example", Kind: "domain"}, testAudit); err != nil {
		t.Fatal(err)
	}
	up, err := st.CreateUpstream(ctx, model.Upstream{Name: "up", Kind: model.UpstreamMCP, BaseURL: "https://up.example",
		Visibility: model.VisibilityPublic}, testAudit)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := publishTestMCP(t, st, "com.example/server", "1.0.0")
	conn := connSchema(t, schema)
	if _, err := conn.Exec(ctx, `DROP TABLE audit_events`); err != nil {
		t.Fatal(err)
	}

	in := model.MCPPublishInput{Namespace: "com.example", Name: "com.example/server", Mode: model.MCPModeRegistered,
		Visibility: model.VisibilityPublic, Listed: true, Version: model.MCPVersion{Version: "2.0.0", ServerJSON: []byte(`{}`),
			Digest: "sha256:" + strings.Repeat("d", 64), CreatedBy: "t"}}
	if _, _, err := st.PublishMCPVersion(ctx, in, testAudit); err == nil {
		t.Fatal("publish must fail when the audit row cannot be written")
	}
	in.Name = "com.example/new"
	if _, _, err := st.PublishMCPVersion(ctx, in, testAudit); err == nil {
		t.Fatal("first publish must fail when the audit row cannot be written")
	}
	if _, err := st.GetMCPServer(ctx, "com.example/new"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("server creation was not rolled back: %v", err)
	}
	if _, err := st.SetMCPStatus(ctx, srv.ID, "", model.MCPStatusDeleted, "gone", time.Now(), testAudit); err == nil {
		t.Fatal("status change must fail without audit")
	}
	private := model.VisibilityPrivate
	if _, err := st.UpdateMCPServer(ctx, srv.ID, model.MCPPatch{Visibility: &private}, testAudit); err == nil {
		t.Fatal("server patch must fail without audit")
	}
	// The failed publish of 2.0.0 had already moved the latest flag inside
	// its transaction; all of it is undone.
	all, err := st.ListMCPVersions(ctx, srv.ID)
	if err != nil || len(all) != 1 || all[0].Version != "1.0.0" || !all[0].IsLatest || all[0].Status != model.MCPStatusActive ||
		!all[0].UpdatedAt.Equal(all[0].PublishedAt) {
		t.Fatalf("versions after failed mutations = %+v, %v", all, err)
	}
	if got, err := st.GetMCPServer(ctx, "com.example/server"); err != nil || got.Visibility != model.VisibilityPublic || !got.UpdatedAt.Equal(srv.UpdatedAt) {
		t.Fatalf("server after failed mutations = %+v, %v", got, err)
	}

	if _, err := st.CreateUpstream(ctx, model.Upstream{Name: "new", Kind: model.UpstreamMCP, BaseURL: "https://new.example",
		Visibility: model.VisibilityPublic}, testAudit); err == nil {
		t.Fatal("upstream creation must fail without audit")
	}
	if _, err := st.GetUpstream(ctx, "new"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("upstream creation was not rolled back: %v", err)
	}
	moved := "https://moved.example"
	if _, err := st.UpdateUpstream(ctx, "up", model.UpstreamPatch{BaseURL: &moved}, testAudit); err == nil {
		t.Fatal("upstream patch must fail without audit")
	}
	if err := st.DeleteUpstream(ctx, "up", testAudit); err == nil {
		t.Fatal("upstream delete must fail without audit")
	}
	if got, err := st.GetUpstream(ctx, "up"); err != nil || got.ID != up.ID || got.BaseURL != "https://up.example" || !got.UpdatedAt.Equal(up.UpdatedAt) {
		t.Fatalf("upstream after failed mutations = %+v, %v", got, err)
	}
}
