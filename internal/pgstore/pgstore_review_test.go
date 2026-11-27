package pgstore_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
)

func TestMigration0006KeepsEntriesVisible(t *testing.T) {
	st, schema := openSchema(t)
	ctx := context.Background()
	conn := connSchema(t, schema)
	if _, err := conn.Exec(ctx, `CREATE TABLE schema_migrations (version INT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	names := []string{"0001_init.sql", "0002_versions_no_truncate.sql", "0003_skills_yank_terminal.sql", "0004_mcp_upstreams.sql", "0005_gateways.sql"}
	for i, name := range names {
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
		INSERT INTO namespaces (name, kind) VALUES ('com.example', 'domain'), ('ai.openlinker.old', 'openlinker');
		INSERT INTO skills (namespace_id, name, description, visibility, mode)
			SELECT id, 'demo', 'before', 'public', 'hosted' FROM namespaces WHERE name = 'com.example';
		INSERT INTO skill_versions (skill_id, version, mode, frontmatter_name, frontmatter_description, license_kind, core_compatible,
			files, artifact_digest, artifact_type, artifact_size, source, created_by)
			SELECT id, '1', 'hosted', 'demo', 'before', 'spdx', true, '[]', 'sha256:`+strings.Repeat("b", 64)+`', 'skill-md', 1,
				'{"kind": "upload"}', 't' FROM skills;
		INSERT INTO mcp_servers (namespace_id, name, mode, visibility, created_by)
			SELECT id, 'com.example/server', 'registered', 'public', 't' FROM namespaces WHERE name = 'com.example'`); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	sk, err := st.GetSkill(ctx, "com.example", "demo")
	if err != nil || sk.ReviewState != model.ReviewApproved || sk.ShownVisibility() != model.VisibilityPublic {
		t.Fatalf("skill after 0006: %+v %v", sk, err)
	}
	srv, err := st.GetMCPServer(ctx, "com.example/server")
	if err != nil || srv.ReviewState != model.ReviewApproved || srv.ShownVisibility() != model.VisibilityPublic {
		t.Fatalf("server after 0006: %+v %v", srv, err)
	}
	found, err := st.Search(ctx, model.SearchQuery{})
	if err != nil || len(found) != 1 {
		t.Fatalf("search after 0006: %+v %v", found, err)
	}
	ns, err := st.GetNamespace(ctx, "ai.openlinker.old")
	if err != nil || ns.OpenLinkerSubject != "" || ns.ReviewTrusted {
		t.Fatalf("namespace after 0006: %+v %v", ns, err)
	}
	mustFail(t, conn, "a subject on a non-openlinker namespace", `UPDATE namespaces SET openlinker_subject = 'x' WHERE name = 'com.example'`)
	mustFail(t, conn, "an unknown review state", `UPDATE skills SET review_state = 'maybe'`)
	mustFail(t, conn, "writing the shown visibility", `UPDATE skills SET shown_visibility = 'public'`)
	if _, err := conn.Exec(ctx, `UPDATE skills SET review_state = 'pending'`); err != nil {
		t.Fatal(err)
	}
	if found, err := st.Search(ctx, model.SearchQuery{}); err != nil || len(found) != 0 {
		t.Fatalf("pending skill listed: %+v %v", found, err)
	}
}

func TestMigration0007KeepsEntriesVisible(t *testing.T) {
	st, schema := openSchema(t)
	ctx := context.Background()
	conn := connSchema(t, schema)
	if _, err := conn.Exec(ctx, `CREATE TABLE schema_migrations (version INT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	names := []string{"0001_init.sql", "0002_versions_no_truncate.sql", "0003_skills_yank_terminal.sql", "0004_mcp_upstreams.sql",
		"0005_gateways.sql", "0006_review_reports.sql"}
	for i, name := range names {
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
		INSERT INTO namespaces (name, kind) VALUES ('com.example', 'domain');
		INSERT INTO skills (namespace_id, name, description, visibility, mode, review_state)
			SELECT id, 'demo', 'before', 'public', 'hosted', 'approved' FROM namespaces WHERE name = 'com.example';
		INSERT INTO skills (namespace_id, name, description, visibility, mode, review_state)
			SELECT id, 'held', 'before', 'public', 'hosted', 'pending' FROM namespaces WHERE name = 'com.example';
		INSERT INTO skill_versions (skill_id, version, mode, frontmatter_name, frontmatter_description, license_kind, core_compatible,
			files, artifact_digest, artifact_type, artifact_size, source, created_by)
			SELECT id, '1', 'hosted', name, 'before', 'spdx', true, '[]', 'sha256:`+strings.Repeat("b", 64)+`', 'skill-md', 1,
				'{"kind": "upload"}', 't' FROM skills`); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	sk, err := st.GetSkill(ctx, "com.example", "demo")
	if err != nil || sk.Managed.IsManaged() || sk.ShownVisibility() != model.VisibilityPublic {
		t.Fatalf("demo after 0007: %+v %v", sk, err)
	}
	held, err := st.GetSkill(ctx, "com.example", "held")
	if err != nil || held.ShownVisibility() != model.VisibilityReview {
		t.Fatalf("held after 0007: %+v %v", held, err)
	}
	v, err := st.GetVersion(ctx, sk.ID, "1")
	if err != nil || v.Hidden {
		t.Fatalf("version after 0007: %+v %v", v, err)
	}
	mustFail(t, conn, "half a managed identity", `UPDATE skills SET managed_by = 'core' WHERE name = 'demo'`)
	mustFail(t, conn, "rewriting a version's content", `UPDATE skill_versions SET artifact_size = 2`)
	if _, err := conn.Exec(ctx, `UPDATE skill_versions SET projection_hidden = true`); err != nil {
		t.Fatalf("projection_hidden must stay mutable: %v", err)
	}
}
