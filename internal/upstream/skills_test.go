package upstream_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/safehttp"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
	"github.com/OpenLinker-ai/openlinker-registry/internal/upstream"
)

// The bodies below follow internal/api/read.go: listSkills and skillJSON,
// getSkill, and versionJSON (keys sorted as encoding/json writes a map).

const skillListPage1 = `{"next_cursor":"aW8uZ2l0aHViLmJvYgBvbGQtdG9vbA","skills":[` +
	`{"created_at":"2026-10-01T08:00:00Z","description":"Demo skill <b>","latest_version":"1.2.0","listed":true,"mode":"hosted","name":"demo","namespace":"io.github.alice","status":"active","updated_at":"2026-11-02T11:30:00.123456Z","visibility":"public"},` +
	`{"created_at":"2026-09-01T08:00:00Z","description":"Old tool","global_alias":"old-tool","latest_version":null,"listed":false,"mode":"indexed","name":"old-tool","namespace":"io.github.bob","status":"deprecated","status_reason":"use demo","updated_at":"2026-09-15T00:00:00Z","visibility":"internal"}` +
	`]}` + "\n"

const skillListPage2 = `{"next_cursor":null,"skills":[` +
	`{"created_at":"2026-08-01T08:00:00Z","description":"","latest_version":"0.1.0","listed":true,"mode":"hosted","name":"zeta","namespace":"io.github.carol","status":"active","updated_at":"2026-08-01T08:00:00Z","visibility":"public"}` +
	`]}` + "\n"

const skillDetail = `{"created_at":"2026-10-01T08:00:00Z","description":"Demo skill","latest_version":"1.2.0","listed":true,"mode":"hosted","name":"demo","namespace":"io.github.alice","status":"active","updated_at":"2026-11-02T11:30:00Z","versions":[` +
	`{"artifact_digest":"sha256:3333333333333333333333333333333333333333333333333333333333333333","core_compatible":true,"created_at":"2026-11-02T11:30:00Z","license_kind":"spdx","version":"1.3.0","yanked":true},` +
	`{"artifact_digest":"sha256:2222222222222222222222222222222222222222222222222222222222222222","core_compatible":true,"created_at":"2026-10-20T00:00:00Z","license_kind":"spdx","version":"1.2.0","yanked":false},` +
	`{"artifact_digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","core_compatible":false,"created_at":"2026-10-01T08:00:00Z","license_kind":"none","version":"1.0.0","yanked":false}` +
	`],"visibility":"public"}` + "\n"

const skillVersion = `{"archive":{"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","url":"https://reg.example/v1/skills/io.github.alice/demo/versions/1.2.0/archive.tar.gz"},` +
	`"artifact":{"digest":"sha256:2222222222222222222222222222222222222222222222222222222222222222","size":4096,"type":"tar","url":"https://reg.example/v1/skills/io.github.alice/demo/versions/1.2.0/artifact"},` +
	`"content_access":{"artifact":true,"full_text":true,"reason":null},"core_compatible":true,"created_at":"2026-10-20T00:00:00.5Z","created_by":"github:1234",` +
	`"files":[{"path":"SKILL.md","size":120,"sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},{"path":"scripts/run.sh","size":20,"sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","git_blob_sha":"dddddddddddddddddddddddddddddddddddddddd"}],` +
	`"frontmatter":{"description":"Demo skill","name":"demo"},"license":{"approved":true,"id":"MIT","kind":"spdx","text":"MIT License\n\nCopyright (c) Alice"},` +
	`"mode":"hosted","name":"demo","namespace":"io.github.alice","source":{"type":"upload"},"source_url":"https://github.com/alice/demo","version":"1.2.0","yanked":null}` + "\n"

// An indexed, yanked version: no archive, no artifact URL, no license text,
// content access refused.
const skillVersionYanked = `{"artifact":{"digest":"sha256:3333333333333333333333333333333333333333333333333333333333333333","size":77,"type":"skill-md"},` +
	`"check":{"checked_at":"2026-11-02T11:30:00Z","result":"ok"},"content_access":{"artifact":false,"full_text":false,"reason":"license_not_approved"},"core_compatible":false,` +
	`"created_at":"2026-11-02T11:30:00Z","created_by":"github:1234","files":[{"path":"SKILL.md","size":77,"sha256":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}],` +
	`"frontmatter":{"description":"Demo skill","name":"demo"},"indexed_by":"github:99","license":{"approved":false,"id":"","kind":"none"},"locator":{"repo":"alice/demo"},` +
	`"mode":"indexed","name":"demo","namespace":"io.github.alice","version":"1.3.0","yanked":{"at":"2026-11-05T00:00:00Z","reason":"leaked a key"}}` + "\n"

func skillFake(t *testing.T) *fake {
	return newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/registry/v1/skills":
			switch r.URL.Query().Get("cursor") {
			case "":
				writeJSON(w, skillListPage1)
			case "aW8uZ2l0aHViLmJvYgBvbGQtdG9vbA":
				writeJSON(w, skillListPage2)
			default:
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, `{"error":{"code":"invalid_query","message":"invalid cursor"}}`)
			}
		case "/registry/v1/skills/io.github.alice/demo":
			writeJSON(w, skillDetail)
		case "/registry/v1/skills/io.github.alice/demo/versions/1.2.0":
			writeJSON(w, skillVersion)
		case "/registry/v1/skills/io.github.alice/demo/versions/1.3.0":
			writeJSON(w, skillVersionYanked)
		case "/registry/v1/skills/io.github.alice/demo/versions/1.2.0/artifact":
			w.Header().Set("Content-Type", "application/x-tar")
			w.Header().Set("Content-Disposition", `attachment; filename="demo-1.2.0.tar"`)
			_, _ = w.Write([]byte("tar-bytes-\x00\x01\x02"))
		case "/registry/v1/skills/io.github.alice/demo/versions/1.3.0/artifact":
			w.WriteHeader(http.StatusGone)
			writeJSON(w, `{"error":{"code":"version_yanked","message":"yanked"}}`)
		case "/registry/v1/skills/io.github.alice/other", "/registry/v1/skills/io.github.alice/demo/versions/2.0.0":
			// A confused upstream answering for something else.
			if strings.Contains(r.URL.Path, "/versions/") {
				writeJSON(w, skillVersion)
			} else {
				writeJSON(w, skillDetail)
			}
		case "/registry/v1/skills/io.github.alice/broken":
			writeJSON(w, `{"namespace":"io.github.alice","name":"broken","listed":"yes"}`)
		case "/registry/v1/skills/io.github.alice/demo/versions/broken":
			writeJSON(w, `{"namespace":"io.github.alice","name":"demo","version":"broken","files":"SECRET-BODY-MARKER"}`)
		case "/registry/v1/skills/a%2Fb/c%20d/versions/1.0%2Bx":
			writeJSON(w, `{"namespace":"a/b","name":"c d","version":"1.0+x","yanked":true}`)
		default:
			http.NotFound(w, r)
		}
	})
}

func TestListSkills(t *testing.T) {
	f := skillFake(t)
	c := newClient(f.URL + "/registry")
	items, next, err := c.ListSkills(ctx, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || next != "aW8uZ2l0aHViLmJvYgBvbGQtdG9vbA" {
		t.Fatalf("%d items, next %q", len(items), next)
	}
	want := upstream.SkillSummary{
		Namespace: "io.github.alice", Name: "demo", Description: "Demo skill <b>", Visibility: "public", Status: "active",
		Mode: "hosted", LatestVersion: "1.2.0", Listed: true, UpdatedAt: time.Date(2026, 11, 2, 11, 30, 0, 123456000, time.UTC),
	}
	if items[0] != want {
		t.Errorf("item 0 = %+v", items[0])
	}
	want = upstream.SkillSummary{
		Namespace: "io.github.bob", Name: "old-tool", Description: "Old tool", Visibility: "internal", Status: "deprecated",
		StatusReason: "use demo", Mode: "indexed", LatestVersion: "", Listed: false, UpdatedAt: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC),
	}
	if items[1] != want {
		t.Errorf("item 1 = %+v", items[1])
	}
	items, next, err = c.ListSkills(ctx, next, 0)
	if err != nil || len(items) != 1 || next != "" || items[0].Name != "zeta" {
		t.Fatalf("page 2: %+v %q %v", items, next, err)
	}
	if got := f.uris(); got[0] != "/registry/v1/skills?limit=2" || got[1] != "/registry/v1/skills?cursor=aW8uZ2l0aHViLmJvYgBvbGQtdG9vbA" {
		t.Fatalf("requests = %v", got)
	}
	// The upstream's own 400 is a status error, not a decode attempt.
	_, _, err = c.ListSkills(ctx, "bogus", 0)
	if code, status := scode(err); code != safehttp.CodeStatus || status != http.StatusBadRequest {
		t.Fatalf("err = %v", err)
	}
	// An upstream without the skill API.
	_, _, err = newClient(f.URL+"/nothing-here").ListSkills(ctx, "", 0)
	wantNotFound(t, "ListSkills", err)
}

func TestListSkillsBadResponses(t *testing.T) {
	for _, body := range []string{
		`{"next_cursor":null}`,
		`{"skills":{}}`,
		`{"skills":[],"next_cursor":"cur"}`, // does not advance
		`[]`, `SECRET-BODY-MARKER`,
	} {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, body) })
		items, next, err := newClient(f.URL).ListSkills(ctx, "cur", 0)
		if ucode(err) != upstream.CodeBadResponse || items != nil || next != "" {
			t.Errorf("%s: %v %q %v, want bad_response", body, items, next, err)
		} else if strings.Contains(err.Error(), "SECRET-BODY-MARKER") {
			t.Errorf("error echoes the body: %v", err)
		}
	}
	// One malformed entry is skipped; it does not hide its neighbours.
	const good = `{"namespace":"io.github.alice","name":"good"}`
	for _, bad := range []string{`{"namespace":"io.github.alice"}`, `{"name":"demo"}`, `"SECRET-BODY-MARKER"`,
		`{"namespace":"io.github.alice","name":"demo","updated_at":"SECRET-BODY-MARKER"}`} {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, `{"skills":[`+bad+`,`+good+`]}`) })
		items, _, err := newClient(f.URL).ListSkills(ctx, "", 0)
		if err != nil || len(items) != 1 || items[0].Name != "good" {
			t.Errorf("%s: %+v %v, want only the good entry", bad, items, err)
		}
	}
	// Empty and null lists are fine.
	for _, body := range []string{`{"skills":[],"next_cursor":null}`, `{"skills":null}`, `{"skills":[],"next_cursor":""}`} {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, body) })
		if items, next, err := newClient(f.URL).ListSkills(ctx, "cur", 0); err != nil || len(items) != 0 || next != "" {
			t.Errorf("%s: %v %q %v", body, items, next, err)
		}
	}
}

func TestGetSkill(t *testing.T) {
	f := skillFake(t)
	c := newClient(f.URL + "/registry/")
	d, err := c.GetSkill(ctx, "io.github.alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if d.Namespace != "io.github.alice" || d.Name != "demo" || d.LatestVersion != "1.2.0" || !d.Listed || d.Mode != "hosted" ||
		d.Visibility != "public" || d.Status != "active" || !d.UpdatedAt.Equal(time.Date(2026, 11, 2, 11, 30, 0, 0, time.UTC)) {
		t.Fatalf("detail = %+v", d)
	}
	want := []upstream.SkillVersionRef{
		{Version: "1.3.0", Yanked: true, ArtifactDigest: "sha256:" + strings.Repeat("3", 64)},
		{Version: "1.2.0", Yanked: false, ArtifactDigest: "sha256:" + strings.Repeat("2", 64)},
		{Version: "1.0.0", Yanked: false, ArtifactDigest: "sha256:" + strings.Repeat("1", 64)},
	}
	if len(d.Versions) != len(want) {
		t.Fatalf("versions = %+v", d.Versions)
	}
	for i := range want {
		if d.Versions[i] != want[i] {
			t.Errorf("version %d = %+v", i, d.Versions[i])
		}
	}
	_, err = c.GetSkill(ctx, "io.github.alice", "missing")
	wantNotFound(t, "GetSkill", err)
	for _, name := range []string{"other", "broken"} {
		if _, err := c.GetSkill(ctx, "io.github.alice", name); ucode(err) != upstream.CodeBadResponse {
			t.Errorf("%s: err = %v, want bad_response", name, err)
		}
	}
}

func TestGetSkillVersion(t *testing.T) {
	f := skillFake(t)
	c := newClient(f.URL + "/registry")
	v, err := c.GetSkillVersion(ctx, "io.github.alice", "demo", "1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	files := v.Files
	v.Files = nil
	want := upstream.SkillVersion{
		Namespace: "io.github.alice", Name: "demo", Version: "1.2.0", Mode: "hosted",
		FrontmatterName: "demo", FrontmatterDescription: "Demo skill",
		LicenseKind: "spdx", LicenseID: "MIT", LicenseText: "MIT License\n\nCopyright (c) Alice", LicenseApproved: true,
		CoreCompatible: true,
		ArtifactType:   "tar", ArtifactDigest: "sha256:" + strings.Repeat("2", 64), ArtifactSize: 4096,
		ArchiveDigest:  "sha256:" + strings.Repeat("a", 64),
		AccessArtifact: true, AccessFullText: true,
		CreatedAt: time.Date(2026, 10, 20, 0, 0, 0, 5e8, time.UTC),
	}
	if !v.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt = %v", v.CreatedAt)
	}
	v.CreatedAt, want.CreatedAt = time.Time{}, time.Time{}
	if v.Namespace != want.Namespace || v.Name != want.Name || v.Version != want.Version || v.Mode != want.Mode ||
		v.FrontmatterName != want.FrontmatterName || v.FrontmatterDescription != want.FrontmatterDescription ||
		v.LicenseKind != want.LicenseKind || v.LicenseID != want.LicenseID || v.LicenseText != want.LicenseText ||
		v.LicenseApproved != want.LicenseApproved || v.CoreCompatible != want.CoreCompatible ||
		v.ArtifactType != want.ArtifactType || v.ArtifactDigest != want.ArtifactDigest || v.ArtifactSize != want.ArtifactSize ||
		v.ArchiveDigest != want.ArchiveDigest || v.AccessArtifact != want.AccessArtifact || v.AccessFullText != want.AccessFullText ||
		v.Yanked || v.YankReason != "" {
		t.Errorf("version = %+v", v)
	}
	wantFiles := []skill.ManifestEntry{
		{Path: "SKILL.md", Size: 120, SHA256: strings.Repeat("b", 64)},
		{Path: "scripts/run.sh", Size: 20, SHA256: strings.Repeat("c", 64), GitBlobSHA: strings.Repeat("d", 40)},
	}
	if len(files) != 2 || files[0] != wantFiles[0] || files[1] != wantFiles[1] {
		t.Errorf("files = %+v", files)
	}

	y, err := c.GetSkillVersion(ctx, "io.github.alice", "demo", "1.3.0")
	if err != nil {
		t.Fatal(err)
	}
	if !y.Yanked || y.YankReason != "leaked a key" || y.AccessArtifact || y.AccessFullText || y.LicenseApproved || y.LicenseText != "" ||
		y.ArchiveDigest != "" || y.ArtifactType != "skill-md" || y.ArtifactSize != 77 || y.Mode != "indexed" || y.CoreCompatible || len(y.Files) != 1 {
		t.Errorf("yanked version = %+v", y)
	}

	// Each path segment is escaped on its own.
	e, err := newClient(f.URL+"/registry").GetSkillVersion(ctx, "a/b", "c d", "1.0+x")
	if err != nil || !e.Yanked || e.YankReason != "" {
		t.Fatalf("%+v %v", e, err)
	}
	if got := f.uris()[len(f.uris())-1]; got != "/registry/v1/skills/a%2Fb/c%20d/versions/1.0%2Bx" {
		t.Fatalf("request = %q", got)
	}

	_, err = c.GetSkillVersion(ctx, "io.github.alice", "demo", "0.0.1")
	wantNotFound(t, "GetSkillVersion", err)
	for _, version := range []string{"2.0.0", "broken"} {
		_, err := c.GetSkillVersion(ctx, "io.github.alice", "demo", version)
		if ucode(err) != upstream.CodeBadResponse || strings.Contains(err.Error(), "SECRET-BODY-MARKER") {
			t.Errorf("%s: err = %v, want bad_response", version, err)
		}
	}
}

func TestGetSkillArtifact(t *testing.T) {
	f := skillFake(t)
	c := newClient(f.URL + "/registry")
	const want = "tar-bytes-\x00\x01\x02"
	data, err := c.GetSkillArtifact(ctx, "io.github.alice", "demo", "1.2.0", int64(len(want)))
	if err != nil || string(data) != want {
		t.Fatalf("%q %v", data, err)
	}
	r := f.requests()[0]
	if r.RequestURI != "/registry/v1/skills/io.github.alice/demo/versions/1.2.0/artifact" || r.Header.Get("Authorization") != "Bearer "+token {
		t.Fatalf("%s %v", r.RequestURI, r.Header)
	}
	// The explicit limit applies, not MaxBody.
	c.MaxBody = 1 << 20
	_, err = c.GetSkillArtifact(ctx, "io.github.alice", "demo", "1.2.0", int64(len(want))-1)
	if code, _ := scode(err); code != safehttp.CodeTooLarge {
		t.Fatalf("err = %v, want too_large", err)
	}
	c.MaxBody = 4
	if data, err = c.GetSkillArtifact(ctx, "io.github.alice", "demo", "1.2.0", 1<<10); err != nil || string(data) != want {
		t.Fatalf("%q %v", data, err)
	}
	// MaxBody caps JSON documents.
	_, err = c.GetSkill(ctx, "io.github.alice", "demo")
	if code, _ := scode(err); code != safehttp.CodeTooLarge {
		t.Fatalf("err = %v, want too_large", err)
	}
	// A yanked version answers 410: a status error, distinct from not found.
	_, err = c.GetSkillArtifact(ctx, "io.github.alice", "demo", "1.3.0", 1<<10)
	if code, status := scode(err); code != safehttp.CodeStatus || status != http.StatusGone {
		t.Fatalf("err = %v, want status 410", err)
	}
	_, err = c.GetSkillArtifact(ctx, "io.github.alice", "demo", "0.0.1", 1<<10)
	wantNotFound(t, "GetSkillArtifact", err)
}
