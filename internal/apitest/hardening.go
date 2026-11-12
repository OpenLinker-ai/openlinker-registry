package apitest

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

// runHardening covers what an upstream, or a caller working through one,
// must not be able to do.
func runHardening(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Run("mirror never widens visibility", func(t *testing.T) { runMirrorVisibility(t, newStore) })
	t.Run("forwarding serves only callers who could see the result", func(t *testing.T) { runForwardAudience(t, newStore) })
	t.Run("hostile upstream data", func(t *testing.T) { runHostileUpstream(t, newStore) })
	t.Run("mcp sync", func(t *testing.T) { runMCPSyncRules(t, newStore) })
	t.Run("skill sync", func(t *testing.T) { runSkillSyncRules(t, newStore) })
	t.Run("limits and error formats", func(t *testing.T) { runLimitsAndFormats(t, newStore) })
}

// runMirrorVisibility mirrors from an upstream that shows this instance more
// than it shows the public (the mirror holds a member token). The mirror is
// configured to publish what it mirrors; entries the upstream restricts must
// stay restricted.
func runMirrorVisibility(t *testing.T, newStore func(t *testing.T) store.Store) {
	up := NewEnv(t, newStore(t), Options{})
	up.namespace("com.acme", "domain")
	owner := up.publisher("com.acme")
	post := func(name, query string) {
		t.Helper()
		up.Expect(up.Do("POST", "/v1/skills/com.acme/"+name+"/versions?version=1.0.0"+query, owner, "text/markdown", md(name, "Skill "+name, "MIT")), 201, "upstream "+name)
	}
	post("open", "")
	post("staff", "&visibility=internal")
	post("secret", "&visibility=private")
	post("quiet", "&listed=false")
	mcpPost := func(name, query string) {
		t.Helper()
		up.Expect(up.Do("POST", "/v1/mcp/servers/com.acme"+query, owner, "application/json",
			serverJSON("com.acme/"+name, "1.0.0", "https://"+name+".acme.example/mcp")), 201, "upstream mcp "+name)
	}
	mcpPost("open-mcp", "")
	mcpPost("staff-mcp", "?visibility=internal")
	mcpPost("secret-mcp", "?visibility=private")
	mcpPost("quiet-mcp", "?listed=false")

	// The mirror reads the upstream as a member of com.acme.
	down := NewEnv(t, newStore(t), Options{Loopback: true, UpstreamTokens: map[string]string{"hub": owner}})
	down.Expect(down.upstream(map[string]any{"name": "hub", "kind": "openlinker-registry", "base_url": up.Base,
		"proxy_on_miss": true, "visibility": "public"}), 201, "upstream")
	reader := down.token([]string{"skills:read"}, nil)
	s := down.sync("hub")
	if s["last_status"] != "ok" {
		t.Fatalf("sync: %v", s)
	}

	skillStatus := func(name, token string) int {
		return down.Do("GET", "/v1/skills/com.acme/"+name, token, "", nil).Status
	}
	mcpStatus := func(name, token string) int {
		return down.Do("GET", "/v0.1/servers/"+url.PathEscape("com.acme/"+name)+"/versions/latest", token, "", nil).Status
	}
	for name, want := range map[string][2]int{ // anonymous, authenticated
		"open": {200, 200}, "staff": {404, 200}, "secret": {404, 404}, "quiet": {200, 200},
	} {
		if a, b := skillStatus(name, ""), skillStatus(name, reader); a != want[0] || b != want[1] {
			t.Errorf("skill %s: anonymous %d, authenticated %d, want %v", name, a, b, want)
		}
		if a, b := mcpStatus(name+"-mcp", ""), mcpStatus(name+"-mcp", reader); a != want[0] || b != want[1] {
			t.Errorf("mcp %s-mcp: anonymous %d, authenticated %d, want %v", name, a, b, want)
		}
	}
	// Private entries are not stored at all; unlisted ones stay out of
	// listings and the discovery index.
	for _, admin := range []string{"/v1/skills/com.acme/secret", "/v0.1/servers/com.acme%2Fsecret-mcp/versions/latest"} {
		down.Expect(down.Do("GET", admin, AdminToken, "", nil), 404, "private upstream entry must not be mirrored")
	}
	names := func(r Resp, key, field string) string {
		var out []string
		for _, item := range r.JSON[key].([]any) {
			m := item.(map[string]any)
			if s, ok := m["server"].(map[string]any); ok {
				m = s
			}
			out = append(out, m[field].(string))
		}
		return strings.Join(out, ",")
	}
	if got := names(down.Do("GET", "/v1/skills", "", "", nil), "skills", "name"); got != "open" {
		t.Errorf("anonymous skill listing: %s", got)
	}
	if got := names(down.Do("GET", "/v1/skills", reader, "", nil), "skills", "name"); got != "open,staff" {
		t.Errorf("authenticated skill listing: %s", got)
	}
	if got := names(down.Do("GET", "/v0.1/servers", "", "", nil), "servers", "name"); got != "com.acme/open-mcp" {
		t.Errorf("anonymous mcp listing: %s", got)
	}
	if got := names(down.Do("GET", "/ns/com.acme/.well-known/agent-skills/index.json", "", "", nil), "skills", "name"); got != "open" {
		t.Errorf("discovery index: %s", got)
	}

	// An openlinker-registry configured as a plain MCP registry is refused:
	// that kind cannot follow per-entry visibility.
	plain := NewEnv(t, newStore(t), Options{Loopback: true, UpstreamTokens: map[string]string{"hub": owner}})
	plain.Expect(plain.upstream(map[string]any{"name": "hub", "kind": "mcp-registry", "base_url": up.Base, "visibility": "public"}), 201, "upstream")
	if s := plain.sync("hub"); s["last_status"] != "failed" || !strings.Contains(s["last_error"].(string), "openlinker-registry") {
		t.Fatalf("sync of an openlinker-registry as mcp-registry: %v", s)
	}
	if got := servers(plain.Do("GET", "/v0.1/servers?include_deleted=true", AdminToken, "", nil)); len(got) != 0 {
		t.Fatalf("nothing may be mirrored through the wrong kind: %d entries", len(got))
	}

	// --- restrictions applied upstream later ---
	artifact := func(name, version, token string) int {
		return down.Do("GET", "/v1/skills/com.acme/"+name+"/versions/"+version+"/artifact", token, "", nil).Status
	}
	// Pull the bytes once so they are cached here.
	if artifact("open", "1.0.0", "") != 200 || artifact("quiet", "1.0.0", "") != 200 {
		t.Fatal("public mirrored content must be readable")
	}

	// 1. Without waiting for a sync: the upstream restricts a skill and a
	// server and publishes a new version of each. Asking for the new version
	// refreshes the entry, and the answer must follow the new restriction.
	up.Expect(up.Do("PATCH", "/v1/skills/com.acme/open", owner, "", map[string]any{"visibility": "internal"}), 200, "restrict upstream skill")
	up.Expect(up.Do("POST", "/v1/skills/com.acme/open/versions?version=2.0.0", owner, "text/markdown", md("open", "Skill open v2", "MIT")), 201, "new version")
	if got := down.Do("GET", "/v1/skills/com.acme/open/versions/2.0.0", "", "", nil); got.Status != 404 {
		t.Fatalf("a version published after the skill was restricted is shown to anonymous callers: %d %s", got.Status, got.Body)
	}
	if a, b := artifact("open", "2.0.0", ""), artifact("open", "1.0.0", ""); a != 404 || b != 404 {
		t.Fatalf("restricted skill content to anonymous callers: v2 %d, v1 %d", a, b)
	}
	if a, b := skillStatus("open", ""), artifact("open", "2.0.0", reader); a != 404 || b != 200 {
		t.Fatalf("restricted skill: anonymous %d, authenticated v2 artifact %d", a, b)
	}
	up.Expect(up.Do("PATCH", "/v1/mcp/servers/com.acme/open-mcp", owner, "", map[string]any{"visibility": "internal"}), 200, "restrict upstream mcp")
	mcpPost("open-mcp-next", "")
	up.Expect(up.Do("POST", "/v1/mcp/servers/com.acme", owner, "application/json", serverJSON("com.acme/open-mcp", "2.0.0", "https://open-mcp.acme.example/mcp")), 201, "new mcp version")
	v2 := "/v0.1/servers/" + url.PathEscape("com.acme/open-mcp") + "/versions/2.0.0"
	if a, b := down.Do("GET", v2, "", "", nil).Status, down.Do("GET", v2, reader, "", nil).Status; a != 404 || b != 200 {
		t.Fatalf("new version of a restricted server: anonymous %d, authenticated %d", a, b)
	}
	if a := mcpStatus("open-mcp", ""); a != 404 {
		t.Fatalf("restricted server is still public here: %d", a)
	}

	// 2. At the next sync. "quiet" is unlisted upstream, so no listing shows
	// it: the mirror has to look at every entry it holds.
	up.Expect(up.Do("PATCH", "/v1/skills/com.acme/quiet", owner, "", map[string]any{"visibility": "internal"}), 200, "restrict unlisted skill")
	up.Expect(up.Do("PATCH", "/v1/skills/com.acme/staff", owner, "", map[string]any{"visibility": "private"}), 200, "make skill private")
	up.Expect(up.Do("PATCH", "/v1/mcp/servers/com.acme/staff-mcp", owner, "", map[string]any{"listed": false}), 200, "unlist upstream mcp")
	up.Expect(up.Do("PATCH", "/v1/mcp/servers/com.acme/quiet-mcp", owner, "", map[string]any{"visibility": "private"}), 200, "make unlisted mcp private")
	if s = down.sync("hub"); s["last_status"] != "ok" {
		t.Fatalf("second sync: %v", s)
	}
	if a, b := skillStatus("quiet", ""), skillStatus("quiet", reader); a != 404 || b != 200 {
		t.Errorf("unlisted skill restricted upstream: anonymous %d, authenticated %d", a, b)
	}
	if a := artifact("quiet", "1.0.0", ""); a != 404 {
		t.Errorf("cached bytes of a restricted skill are still public: %d", a)
	}
	// An entry the upstream made private is withdrawn: ordinary callers can
	// read neither its metadata nor its cached content.
	if a, b, c := skillStatus("staff", reader), artifact("staff", "1.0.0", reader), mcpStatus("quiet-mcp", reader); a != 404 || b != 404 || c != 404 {
		t.Errorf("withdrawn entries: skill %d, artifact %d, mcp %d", a, b, c)
	}
	if got := names(down.Do("GET", "/v0.1/servers", reader, "", nil), "servers", "name"); strings.Contains(got, "staff-mcp") {
		t.Errorf("a server unlisted upstream is still listed: %s", got)
	}
	if a := mcpStatus("staff-mcp", reader); a != 200 {
		t.Errorf("an unlisted server stays readable by name: %d", a)
	}
	// Shared again upstream, it comes back.
	up.Expect(up.Do("PATCH", "/v1/skills/com.acme/staff", owner, "", map[string]any{"visibility": "internal"}), 200, "share again")
	if s = down.sync("hub"); s["last_status"] != "ok" {
		t.Fatalf("third sync: %v", s)
	}
	if a, b := skillStatus("staff", reader), artifact("staff", "1.0.0", reader); a != 200 || b != 200 {
		t.Errorf("a skill shared again must return: metadata %d, artifact %d", a, b)
	}

	// 3. A mirror that is not a member upstream sees a restricted entry
	// simply disappear (404); it is withdrawn here as well.
	outsider := up.token([]string{"skills:read"}, nil)
	far := NewEnv(t, newStore(t), Options{Loopback: true, UpstreamTokens: map[string]string{"hub": outsider}})
	far.Expect(far.upstream(map[string]any{"name": "hub", "kind": "openlinker-registry", "base_url": up.Base, "visibility": "public"}), 201, "upstream")
	mcpPost("temp-mcp", "")
	post("temp", "")
	if s := far.sync("hub"); s["last_status"] != "ok" {
		t.Fatalf("outsider sync: %v", s)
	}
	farRead := func(path string) int { return far.Do("GET", path, "", "", nil).Status }
	tempArtifact, tempMCP := "/v1/skills/com.acme/temp/versions/1.0.0/artifact", "/v0.1/servers/com.acme%2Ftemp-mcp/versions/latest"
	if farRead(tempArtifact) != 200 || farRead(tempMCP) != 200 {
		t.Fatal("outsider mirror must serve public entries")
	}
	up.Expect(up.Do("PATCH", "/v1/skills/com.acme/temp", owner, "", map[string]any{"visibility": "private"}), 200, "hide skill")
	up.Expect(up.Do("PATCH", "/v1/mcp/servers/com.acme/temp-mcp", owner, "", map[string]any{"visibility": "private"}), 200, "hide mcp")
	if s := far.sync("hub"); s["last_status"] != "ok" || stat(s, "skills_withdrawn") != 1 || stat(s, "mcp_withdrawn") != 1 {
		t.Fatalf("outsider sync after the upstream hid entries: %v", s)
	}
	farReader := far.token([]string{"skills:read"}, nil)
	for _, path := range []string{"/v1/skills/com.acme/temp", tempArtifact, tempMCP} {
		if a, b := farRead(path), far.Do("GET", path, farReader, "", nil).Status; a != 404 || b != 404 {
			t.Errorf("%s after the upstream hid it: anonymous %d, authenticated %d", path, a, b)
		}
	}
}

// runForwardAudience checks who can make the instance reach out. An upstream
// whose entries are mirrored as internal is not consulted for anonymous
// callers, and running out of forwarding budget answers the same for an
// entry that is hidden from the caller as for one that does not exist.
func runForwardAudience(t *testing.T, newStore func(t *testing.T) store.Store) {
	var hits atomic.Int64
	upstreamSkill := func(visibility string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			body := md("tool", "A tool", "MIT")
			switch r.URL.Path {
			case "/v1/skills/com.staff/tool":
				_ = json.NewEncoder(w).Encode(map[string]any{"namespace": "com.staff", "name": "tool", "status": "active", "mode": "hosted",
					"visibility": visibility, "listed": true, "latest_version": "1", "versions": []map[string]any{{"version": "1", "yanked": false}}})
			case "/v1/skills/com.staff/tool/versions/1":
				_ = json.NewEncoder(w).Encode(map[string]any{"namespace": "com.staff", "name": "tool", "version": "1", "mode": "hosted",
					"frontmatter": map[string]any{"name": "tool", "description": "A tool"}, "license": map[string]any{"kind": "spdx", "id": "MIT"},
					"core_compatible": true, "files": []map[string]any{{"path": "SKILL.md", "size": len(body), "sha256": strings.TrimPrefix(sha(body), "sha256:")}},
					"artifact":       map[string]any{"type": "skill-md", "digest": sha(body), "size": len(body)},
					"content_access": map[string]any{"full_text": true, "artifact": true}})
			default:
				w.WriteHeader(404)
				_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found"}}`))
			}
		})
	}

	t.Run("internal upstream", func(t *testing.T) {
		fake := httptest.NewServer(upstreamSkill("public"))
		t.Cleanup(fake.Close)
		e := NewEnv(t, newStore(t), Options{Loopback: true})
		e.Expect(e.upstream(map[string]any{"name": "staff", "kind": "openlinker-registry", "base_url": fake.URL,
			"proxy_on_miss": true, "visibility": "internal"}), 201, "upstream")
		hits.Store(0)
		e.Expect(e.Do("GET", "/v1/skills/com.staff/tool", "", "", nil), 404, "anonymous read")
		e.Expect(e.Do("GET", "/v0.1/servers/com.staff%2Ftool/versions/latest", "", "", nil), 404, "anonymous mcp read")
		if n := hits.Load(); n != 0 {
			t.Fatalf("an anonymous caller made the instance send %d requests to an internal upstream", n)
		}
		if _, err := e.Store.GetSkill(context.Background(), "com.staff", "tool"); !errors.Is(err, model.ErrNotFound) {
			t.Fatal("an anonymous caller made the instance store an entry it cannot see")
		}
		reader := e.token([]string{"skills:read"}, nil)
		e.Expect(e.Do("GET", "/v1/skills/com.staff/tool", reader, "", nil), 200, "authenticated read")
	})

	t.Run("budget", func(t *testing.T) {
		// The upstream shows the skill only to its members; the mirror may
		// publish what it mirrors, so the skill ends up internal here.
		fake := httptest.NewServer(upstreamSkill("internal"))
		t.Cleanup(fake.Close)
		e := NewEnv(t, newStore(t), Options{Loopback: true, ForwardPerMinute: 2})
		e.Expect(e.upstream(map[string]any{"name": "staff", "kind": "openlinker-registry", "base_url": fake.URL,
			"proxy_on_miss": true, "visibility": "public"}), 201, "upstream")
		reader := e.token([]string{"skills:read"}, nil)
		e.Expect(e.Do("GET", "/v1/skills/com.staff/tool", reader, "", nil), 200, "mirror the skill")
		// For an anonymous caller "tool" is hidden and "nope" is missing. With
		// budget left both are 404; without, both are 429.
		hidden := func() int { return e.Do("GET", "/v1/skills/com.staff/tool", "", "", nil).Status }
		if got := hidden(); got != 404 {
			t.Fatalf("hidden entry with budget: %d", got)
		}
		if got := e.Do("GET", "/v1/skills/com.staff/nope", "", "", nil).Status; got != 404 {
			t.Fatalf("missing entry with budget: %d", got)
		}
		a, b := hidden(), e.Do("GET", "/v1/skills/com.staff/nope-2", "", "", nil).Status
		if a != 429 || b != 429 {
			t.Fatalf("out of budget, a hidden entry answers %d and a missing one %d: they must be the same", a, b)
		}
		// A caller who can see the entry is not affected by anyone's budget.
		e.Expect(e.Do("GET", "/v1/skills/com.staff/tool", reader, "", nil), 200, "visible entry")
	})
}

// runHostileUpstream feeds the instance upstream data built to hurt it.
func runHostileUpstream(t *testing.T, newStore func(t *testing.T) store.Store) {
	// A tar that is valid and matches its digest, but is not the canonical
	// tar of its files (here: a modification time; equally a sparse file or a
	// PAX header). Decoding must be bounded and the bytes refused.
	skillMD := md("odd", "Odd archive", "MIT")
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	_ = tw.WriteHeader(&tar.Header{Name: "SKILL.md", Mode: 0o644, Size: int64(len(skillMD)), ModTime: time.Unix(1700000000, 0), Typeflag: tar.TypeReg})
	_, _ = tw.Write(skillMD)
	_ = tw.Close()
	oddTar := raw.Bytes()
	// A sparse entry declaring gigabytes of data the archive does not hold.
	var sparse bytes.Buffer
	tw = tar.NewWriter(&sparse)
	_ = tw.WriteHeader(&tar.Header{Name: "SKILL.md", Mode: 0o644, Size: int64(len(skillMD)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(skillMD)
	_ = tw.Flush()
	sparse.Write(gnuSparseHeader("huge.bin", 4<<30))
	sparse.Write(make([]byte, 1024))
	sparseTar := sparse.Bytes()
	if hdr := sparseEntry(t, sparseTar); hdr.Size != 4<<30 {
		t.Fatalf("test archive is not sparse as intended: %+v", hdr)
	}

	artifacts := map[string][]byte{"odd": oddTar, "sparse": sparseTar}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/skills/com.odd/"), "/")
		data, ok := artifacts[parts[0]]
		switch {
		case !ok || !strings.HasPrefix(r.URL.Path, "/v1/skills/com.odd/"):
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found"}}`))
		case len(parts) == 1:
			_ = json.NewEncoder(w).Encode(map[string]any{"namespace": "com.odd", "name": parts[0], "status": "active", "mode": "hosted",
				"visibility": "public", "listed": true, "latest_version": "1", "versions": []map[string]any{{"version": "1", "yanked": false}}})
		case len(parts) == 3:
			_ = json.NewEncoder(w).Encode(map[string]any{"namespace": "com.odd", "name": parts[0], "version": "1", "mode": "hosted",
				"frontmatter": map[string]any{"name": parts[0], "description": "d"}, "license": map[string]any{"kind": "spdx", "id": "MIT"},
				"core_compatible": true, "files": []map[string]any{{"path": "SKILL.md", "size": len(skillMD), "sha256": strings.TrimPrefix(sha(skillMD), "sha256:")}},
				"artifact":       map[string]any{"type": "tar", "digest": sha(data), "size": len(data)},
				"content_access": map[string]any{"full_text": true, "artifact": true}})
		default:
			_, _ = w.Write(data)
		}
	}))
	t.Cleanup(fake.Close)
	e := NewEnv(t, newStore(t), Options{Loopback: true})
	e.Expect(e.upstream(map[string]any{"name": "odd", "kind": "openlinker-registry", "base_url": fake.URL, "proxy_on_miss": true, "visibility": "public"}), 201, "upstream")
	for name := range artifacts {
		e.Expect(e.Do("GET", "/v1/skills/com.odd/"+name, "", "", nil), 200, "metadata of "+name)
		if r := e.Do("GET", "/v1/skills/com.odd/"+name+"/versions/1/artifact", "", "", nil); r.Status != 409 || errCode(r) != "source_changed" {
			t.Fatalf("%s: a tar that is not canonical must not be served: %d %s", name, r.Status, r.Body)
		}
	}

	// A discovery index whose entry URLs carry tokens or signatures: such a
	// URL must be neither stored nor republished.
	plain := md("plain", "Plain entry", "MIT")
	signed := md("signed", "Signed entry", "MIT")
	var site *httptest.Server
	site = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/agent-skills/index.json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"$schema": "https://schemas.agentskills.io/discovery/0.2.0/schema.json", "skills": []map[string]any{
				{"name": "plain", "type": "skill-md", "description": "Plain entry", "url": site.URL + "/plain/SKILL.md", "digest": sha(plain)},
				{"name": "signed", "type": "skill-md", "description": "Signed entry", "url": site.URL + "/signed/SKILL.md?token=SECRET-TOKEN", "digest": sha(signed)},
			}})
		case "/plain/SKILL.md":
			_, _ = w.Write(plain)
		case "/signed/SKILL.md":
			_, _ = w.Write(signed)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(site.Close)
	e.Expect(e.upstream(map[string]any{"name": "site", "kind": "agent-skills", "base_url": site.URL, "target_namespace": "mirror.site", "visibility": "public"}), 201, "index upstream")
	s := e.sync("site")
	if s["last_status"] != "ok" || stat(s, "skills_created") != 1 || stat(s, "skills_skipped") != 1 {
		t.Fatalf("index sync: %v", s)
	}
	e.Expect(e.Do("GET", "/v1/skills/mirror.site/plain", "", "", nil), 200, "plain entry")
	e.Expect(e.Do("GET", "/v1/skills/mirror.site/signed", AdminToken, "", nil), 404, "entry with a credential-bearing URL")
	for _, path := range []string{"/ns/mirror.site/.well-known/agent-skills/index.json", "/v1/skills?namespace=mirror.site", "/v1/admin/audit?limit=200"} {
		if r := e.Do("GET", path, AdminToken, "", nil); strings.Contains(string(r.Body), "SECRET-TOKEN") {
			t.Fatalf("%s exposes the token of an upstream URL", path)
		}
	}
}

// runMCPSyncRules covers incremental MCP sync against a registry speaking
// the official API.
func runMCPSyncRules(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Run("latest only sees deletions", func(t *testing.T) {
		up := NewEnv(t, newStore(t), Options{})
		up.namespace("com.acme", "domain")
		pub := up.publisher("com.acme")
		up.Expect(up.publishMCP(pub, "com.acme", serverJSON("com.acme/solo", "1.0.0", "https://solo.acme.example/mcp")), 201, "publish")
		up.Expect(up.publishMCP(pub, "com.acme", serverJSON("com.acme/multi", "1.0.0", "https://multi.acme.example/mcp")), 201, "publish")
		up.Expect(up.publishMCP(pub, "com.acme", serverJSON("com.acme/multi", "2.0.0", "https://multi.acme.example/mcp")), 201, "publish")

		down := NewEnv(t, newStore(t), Options{Loopback: true})
		down.Expect(down.upstream(map[string]any{"name": "official", "kind": "openlinker-registry", "base_url": up.Base, "latest_only": true, "visibility": "public"}), 201, "upstream")
		if s := down.sync("official"); s["last_status"] != "ok" || stat(s, "mcp_created") != 2 {
			t.Fatalf("first sync: %v", s)
		}
		down.Expect(down.Do("GET", "/v0.1/servers/com.acme%2Fmulti/versions/1.0.0", "", "", nil), 404, "only latest versions are mirrored")
		// Deleting the only version leaves the server without a latest
		// version upstream; the deletion must still reach the mirror. A
		// change to a version that was never mirrored must not create it.
		up.Expect(up.Do("POST", "/v1/mcp/servers/com.acme/solo/status", pub, "", map[string]any{"status": "deleted"}), 200, "delete upstream")
		up.Expect(up.Do("POST", "/v1/mcp/servers/com.acme/multi/versions/1.0.0/status", pub, "", map[string]any{"status": "deprecated", "message": "old"}), 200, "deprecate old version")
		if s := down.sync("official"); s["last_status"] != "ok" {
			t.Fatalf("second sync: %v", s)
		}
		r := down.Expect(down.Do("GET", "/v0.1/servers/com.acme%2Fsolo/versions/1.0.0", "", "", nil), 200, "deleted version")
		if official(r.JSON)["status"] != "deleted" {
			t.Fatalf("an upstream deletion did not reach a latest-only mirror: %s", r.Body)
		}
		if got := servers(down.Do("GET", "/v0.1/servers", "", "", nil)); len(got) != 1 {
			t.Fatalf("the deleted server is still listed: %d entries", len(got))
		}
		down.Expect(down.Do("GET", "/v0.1/servers/com.acme%2Fmulti/versions/1.0.0", "", "", nil), 404, "a change to an unmirrored version must not create it")

		// A version that was the latest when it was mirrored is superseded
		// and later deleted. The latest-only listing keeps showing the
		// server (through its new latest version), which must not hide that
		// the older version here is out of date.
		up.Expect(up.publishMCP(pub, "com.acme", serverJSON("com.acme/grow", "1.0.0", "https://grow.acme.example/mcp")), 201, "publish")
		down.sync("official")
		up.Expect(up.publishMCP(pub, "com.acme", serverJSON("com.acme/grow", "2.0.0", "https://grow.acme.example/mcp")), 201, "publish")
		down.sync("official")
		up.Expect(up.Do("POST", "/v1/mcp/servers/com.acme/grow/versions/1.0.0/status", pub, "", map[string]any{"status": "deleted"}), 200, "delete old version")
		if s := down.sync("official"); s["last_status"] != "ok" {
			t.Fatalf("sync: %v", s)
		}
		r = down.Expect(down.Do("GET", "/v0.1/servers/com.acme%2Fgrow/versions/1.0.0", "", "", nil), 200, "superseded version")
		if official(r.JSON)["status"] != "deleted" {
			t.Fatalf("a superseded version deleted upstream is still %v here", official(r.JSON)["status"])
		}
		if r = down.Do("GET", "/v0.1/servers/com.acme%2Fgrow/versions/latest", "", "", nil); v0Version(r.JSON) != "2.0.0" {
			t.Fatalf("latest: %s", r.Body)
		}
	})

	t.Run("watermark", func(t *testing.T) {
		// One entry the mirror rejects claims an update far in the future;
		// one good entry was updated in the past. The watermark may only
		// follow what was stored.
		past := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
		entry := func(name, updated string) map[string]any {
			return map[string]any{"server": map[string]any{"name": name, "description": "d", "version": "1.0.0",
				"remotes": []map[string]any{{"type": "sse", "url": "https://x.example/sse"}}},
				"_meta": map[string]any{metaOfficial: map[string]any{"status": "active", "isLatest": true, "updatedAt": updated, "publishedAt": updated}}}
		}
		fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path != "/v0.1/servers" {
				w.WriteHeader(404)
				_, _ = w.Write([]byte(`{"error":"Not found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"servers": []map[string]any{
				entry("single/rejected", "2999-01-01T00:00:00Z"), // a one-label namespace is refused
				entry("com.good/server", past.Format(time.RFC3339)),
			}, "metadata": map[string]any{"count": 2}})
		}))
		t.Cleanup(fake.Close)
		e := NewEnv(t, newStore(t), Options{Loopback: true})
		e.Expect(e.upstream(map[string]any{"name": "feed", "kind": "mcp-registry", "base_url": fake.URL, "visibility": "public"}), 201, "upstream")
		s := e.sync("feed")
		if s["last_status"] != "ok" || stat(s, "mcp_created") != 1 || stat(s, "mcp_skipped") != 1 {
			t.Fatalf("sync: %v", s)
		}
		cursor, _ := time.Parse(time.RFC3339Nano, s["cursor"].(string))
		if !cursor.Equal(past) {
			t.Fatalf("the watermark is %v; it must be the newest stored update, %v", cursor, past)
		}
	})
}

// runLimitsAndFormats covers rate-limit scope and the error body of the
// official API routes.
func runLimitsAndFormats(t *testing.T, newStore func(t *testing.T) store.Store) {
	e := NewEnv(t, newStore(t), Options{ReadPerMinute: 3})
	// The health probes are exempt; a path that merely ends like one is not.
	for range 10 {
		e.Expect(e.Do("GET", "/healthz", "", "", nil), 200, "healthz")
		e.Expect(e.Do("GET", "/readyz", "", "", nil), 200, "readyz")
	}
	for range 3 {
		e.Expect(e.Do("GET", "/v1/skills/com.x/y/versions/1/files/healthz", "", "", nil), 404, "a file named healthz")
	}
	r := e.Expect(e.Do("GET", "/v1/skills/com.x/y/versions/1/files/readyz", "", "", nil), 429, "read limit")
	if errCode(r) != "rate_limited" || r.Header.Get("Retry-After") == "" {
		t.Fatalf("native rate limit: %s", r.Body)
	}
	// The official API reports every error as {"error": "<message>"}.
	r = e.Expect(e.Do("GET", "/v0.1/servers", "", "", nil), 429, "official API read limit")
	if _, isString := r.JSON["error"].(string); !isString || r.Header.Get("Retry-After") == "" {
		t.Fatalf("official API rate limit: %s %v", r.Body, r.Header)
	}

	private := NewEnv(t, newStore(t), Options{Mode: "private"})
	for what, token := range map[string]string{"anonymous": "", "bad token": "olr_not_a_real_token"} {
		for _, path := range []string{"/v0.1/servers", "/v0/servers/com.x%2Fy/versions/latest"} {
			r := private.Expect(private.Do("GET", path, token, "", nil), 401, what+" "+path)
			if _, isString := r.JSON["error"].(string); !isString {
				t.Fatalf("%s %s: official API error body: %s", what, path, r.Body)
			}
		}
	}
	// Native routes keep their own shape.
	if r := private.Expect(private.Do("GET", "/v1/skills", "", "", nil), 401, "native anonymous"); errCode(r) != "unauthorized" {
		t.Fatalf("native error body: %s", r.Body)
	}

	// Bad tokens are budgeted by address, before any lookup. Anonymous
	// requests are not affected.
	guarded := NewEnv(t, newStore(t), Options{AuthFailuresPerMinute: 3})
	valid := guarded.token([]string{"skills:read"}, nil)
	for range 3 {
		guarded.Expect(guarded.Do("GET", "/v1/skills", "olr_aaaaaaaaaaaaaaaa_"+strings.Repeat("b", 43), "", nil), 401, "bad token")
	}
	r = guarded.Expect(guarded.Do("GET", "/v1/skills", "olr_aaaaaaaaaaaaaaaa_"+strings.Repeat("c", 43), "", nil), 429, "too many bad tokens")
	if errCode(r) != "rate_limited" || r.Header.Get("Retry-After") == "" {
		t.Fatalf("authentication failure limit: %s", r.Body)
	}
	guarded.Expect(guarded.Do("GET", "/v1/skills", valid, "", nil), 429, "credentialed request from a blocked address")
	guarded.Expect(guarded.Do("GET", "/v1/skills", "", "", nil), 200, "anonymous request")
	// Successful authentications never count.
	calm := NewEnv(t, newStore(t), Options{AuthFailuresPerMinute: 3})
	good := calm.token([]string{"skills:read"}, nil)
	for range 10 {
		calm.Expect(calm.Do("GET", "/v1/skills", good, "", nil), 200, "valid token")
	}

	// A forwarding 429 on the official API carries Retry-After too.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"Not found"}`))
	}))
	t.Cleanup(fake.Close)
	f := NewEnv(t, newStore(t), Options{Loopback: true, ForwardPerMinute: 1})
	f.Expect(f.upstream(map[string]any{"name": "feed", "kind": "mcp-registry", "base_url": fake.URL, "proxy_on_miss": true, "visibility": "public"}), 201, "upstream")
	f.Expect(f.Do("GET", "/v0.1/servers/com.x%2Fone/versions/latest", "", "", nil), 404, "forwarded miss")
	r = f.Expect(f.Do("GET", "/v0.1/servers/com.x%2Ftwo/versions/latest", "", "", nil), 429, "forward limit")
	if _, isString := r.JSON["error"].(string); !isString || r.Header.Get("Retry-After") == "" {
		t.Fatalf("official API forward limit: %s %v", r.Body, r.Header)
	}
}

// gnuSparseHeader is an old-format GNU sparse tar header for a file that is
// one hole of realSize bytes: it stores no data at all.
func gnuSparseHeader(name string, realSize int64) []byte {
	b := make([]byte, 512)
	octal := func(off int, v int64) { copy(b[off:], fmt.Sprintf("%011o\x00", v)) }
	copy(b[0:], name)
	copy(b[100:], "0000644\x00")
	copy(b[108:], "0000000\x00")
	copy(b[116:], "0000000\x00")
	octal(124, 0) // stored size
	octal(136, 0) // mtime
	b[156] = 'S'
	copy(b[257:], "ustar  \x00")
	octal(386, realSize) // sparse map entry: data starts at realSize ...
	octal(398, 0)        // ... and is empty
	octal(483, realSize)
	copy(b[148:], "        ")
	var sum int64
	for _, c := range b {
		sum += int64(c)
	}
	copy(b[148:], fmt.Sprintf("%06o\x00 ", sum))
	return b
}

// sparseEntry returns the header of the second entry as Go's reader sees it.
func sparseEntry(t *testing.T, data []byte) *tar.Header {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	if _, err := tr.Next(); err != nil {
		t.Fatal(err)
	}
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	return hdr
}

// runSkillSyncRules covers how skills of another registry are mirrored.
func runSkillSyncRules(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Run("all versions in upstream order", func(t *testing.T) {
		// More versions than fit one page of anything: every one is
		// mirrored, in the upstream's order, and each is addressable.
		up := NewEnv(t, newStore(t), Options{})
		up.namespace("com.acme", "domain")
		pub := up.publisher("com.acme")
		const n = 103
		for i := 1; i <= n; i++ {
			up.Expect(up.Do("POST", fmt.Sprintf("/v1/skills/com.acme/many/versions?version=v%d", i), pub, "text/markdown",
				md("many", fmt.Sprintf("Revision %d", i), "MIT")), 201, "upstream version")
		}
		down := NewEnv(t, newStore(t), Options{Loopback: true})
		down.Expect(down.upstream(map[string]any{"name": "hub", "kind": "openlinker-registry", "base_url": up.Base, "proxy_on_miss": true, "visibility": "public"}), 201, "upstream")
		r := down.Expect(down.Do("GET", "/v1/skills/com.acme/many", "", "", nil), 200, "forwarded skill")
		versions := r.JSON["versions"].([]any)
		if r.JSON["latest_version"] != fmt.Sprintf("v%d", n) || len(versions) != n {
			t.Fatalf("mirrored %d versions, latest %v", len(versions), r.JSON["latest_version"])
		}
		for i, item := range versions { // newest first, like upstream
			if got, want := item.(map[string]any)["version"], fmt.Sprintf("v%d", n-i); got != want {
				t.Fatalf("version %d is %v, want %s", i, got, want)
			}
		}
		down.Expect(down.Do("GET", "/v1/skills/com.acme/many/versions/v1/artifact", "", "", nil), 200, "oldest version")
	})

	t.Run("one bad entry does not stop a sync", func(t *testing.T) {
		good := md("good", "Good skill", "MIT")
		version := func(name string) map[string]any {
			return map[string]any{"namespace": "com.mixed", "name": name, "version": "1", "mode": "hosted",
				"frontmatter": map[string]any{"name": name, "description": "d"}, "license": map[string]any{"kind": "spdx", "id": "MIT"},
				"core_compatible": true, "files": []map[string]any{{"path": "SKILL.md", "size": len(good), "sha256": strings.TrimPrefix(sha(good), "sha256:")}},
				"artifact":       map[string]any{"type": "skill-md", "digest": sha(good), "size": len(good)},
				"content_access": map[string]any{"full_text": true, "artifact": true}}
		}
		detail := func(name string) map[string]any {
			return map[string]any{"namespace": "com.mixed", "name": name, "status": "active", "mode": "hosted", "visibility": "public",
				"listed": true, "latest_version": "1", "versions": []map[string]any{{"version": "1", "yanked": false}}}
		}
		failSkills := false
		fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			enc := json.NewEncoder(w)
			switch r.URL.Path {
			case "/v1/skills":
				if failSkills {
					w.WriteHeader(500)
					return
				}
				// A malformed item, a skill with a malformed detail, a skill
				// with a malformed version, and a good one.
				_, _ = w.Write([]byte(`{"skills":[{"namespace":"com.mixed","name":"stamp","updated_at":"not a time"},` +
					`{"namespace":"com.mixed","name":"baddetail"},{"namespace":"com.mixed","name":"badversion"},{"namespace":"com.mixed","name":"good"}]}`))
			case "/v1/skills/com.mixed/baddetail":
				_, _ = w.Write([]byte(`{"namespace":"com.mixed","name":"baddetail","versions":"none"}`))
			case "/v1/skills/com.mixed/badversion":
				_ = enc.Encode(detail("badversion"))
			case "/v1/skills/com.mixed/badversion/versions/1":
				_, _ = w.Write([]byte(`{"namespace":"com.mixed","name":"badversion","version":"1","files":"none"}`))
			case "/v1/skills/com.mixed/good":
				_ = enc.Encode(detail("good"))
			case "/v1/skills/com.mixed/good/versions/1":
				_ = enc.Encode(version("good"))
			case "/v0.1/servers":
				_ = enc.Encode(map[string]any{"servers": []map[string]any{{"server": map[string]any{"name": "com.mixed/server", "description": "d",
					"version": "1.0.0", "remotes": []map[string]any{{"type": "sse", "url": "https://x.example/sse"}}},
					"_meta": map[string]any{metaOfficial: map[string]any{"status": "active", "isLatest": true},
						metaEntry: map[string]any{"visibility": "public", "listed": true}}}}})
			default:
				w.WriteHeader(404)
				_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found"}}`))
			}
		}))
		t.Cleanup(fake.Close)
		e := NewEnv(t, newStore(t), Options{Loopback: true})
		e.Expect(e.upstream(map[string]any{"name": "mixed", "kind": "openlinker-registry", "base_url": fake.URL, "visibility": "public"}), 201, "upstream")
		s := e.sync("mixed")
		// The malformed detail is skipped; the skill whose version cannot be
		// read waits for a later run.
		if s["last_status"] != "ok" || stat(s, "skills_created") != 1 || stat(s, "mcp_created") != 1 ||
			stat(s, "skills_skipped") != 1 || stat(s, "skills_incomplete") != 1 {
			t.Fatalf("sync with malformed entries: %v", s)
		}
		e.Expect(e.Do("GET", "/v1/skills/com.mixed/good", "", "", nil), 200, "the good skill")
		// When the skill listing itself fails, MCP servers are still synced.
		failSkills = true
		fresh := NewEnv(t, newStore(t), Options{Loopback: true})
		fresh.Expect(fresh.upstream(map[string]any{"name": "mixed", "kind": "openlinker-registry", "base_url": fake.URL, "visibility": "public"}), 201, "upstream")
		r := fresh.Expect(fresh.Do("POST", "/v1/admin/upstreams/mixed/sync?wait=true", AdminToken, "", nil), 200, "sync")
		if s := r.JSON["sync"].(map[string]any); s["last_status"] != "failed" || stat(s, "mcp_created") != 1 {
			t.Fatalf("a failed skill listing must not keep MCP servers from syncing: %v", s)
		}
	})

	t.Run("a version that cannot be read yet", func(t *testing.T) {
		// The upstream lists v1 and v2 but does not serve v1 at first. v2
		// must not be recorded without it: versions are only added after the
		// newest one here, so v1 would be left out for good.
		body := md("late", "Late skill", "MIT")
		ready := false
		fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			enc := json.NewEncoder(w)
			version := func(v string) map[string]any {
				return map[string]any{"namespace": "com.late", "name": "late", "version": v, "mode": "hosted",
					"frontmatter": map[string]any{"name": "late", "description": "d"}, "license": map[string]any{"kind": "spdx", "id": "MIT"},
					"core_compatible": true, "files": []map[string]any{{"path": "SKILL.md", "size": len(body), "sha256": strings.TrimPrefix(sha(body), "sha256:")}},
					"artifact":       map[string]any{"type": "skill-md", "digest": sha(body), "size": len(body)},
					"content_access": map[string]any{"full_text": true, "artifact": true}}
			}
			switch r.URL.Path {
			case "/v1/skills":
				_, _ = w.Write([]byte(`{"skills":[{"namespace":"com.late","name":"late"}]}`))
			case "/v1/skills/com.late/late":
				_ = enc.Encode(map[string]any{"namespace": "com.late", "name": "late", "status": "active", "mode": "hosted", "visibility": "public",
					"listed": true, "latest_version": "v2", "versions": []map[string]any{{"version": "v2"}, {"version": "v1"}}})
			case "/v1/skills/com.late/late/versions/v2":
				_ = enc.Encode(version("v2"))
			case "/v1/skills/com.late/late/versions/v1":
				if ready {
					_ = enc.Encode(version("v1"))
					return
				}
				fallthrough
			default:
				w.WriteHeader(404)
				_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found"}}`))
			}
		}))
		t.Cleanup(fake.Close)
		e := NewEnv(t, newStore(t), Options{Loopback: true})
		e.Expect(e.upstream(map[string]any{"name": "late", "kind": "openlinker-registry", "base_url": fake.URL, "visibility": "public"}), 201, "upstream")
		if s := e.sync("late"); s["last_status"] != "ok" || stat(s, "skills_created") != 0 || stat(s, "skills_incomplete") != 1 {
			t.Fatalf("sync while a version is unreadable: %v", s)
		}
		e.Expect(e.Do("GET", "/v1/skills/com.late/late", "", "", nil), 404, "nothing recorded yet")
		ready = true
		if s := e.sync("late"); s["last_status"] != "ok" || stat(s, "skills_created") != 2 {
			t.Fatalf("sync once the version is readable: %v", s)
		}
		r := e.Expect(e.Do("GET", "/v1/skills/com.late/late", "", "", nil), 200, "complete skill")
		if vs := r.JSON["versions"].([]any); r.JSON["latest_version"] != "v2" || len(vs) != 2 || vs[1].(map[string]any)["version"] != "v1" {
			t.Fatalf("versions out of order or incomplete: %s", r.Body)
		}
	})

	t.Run("a skill yanked upstream with unseen versions", func(t *testing.T) {
		up := NewEnv(t, newStore(t), Options{})
		up.namespace("com.acme", "domain")
		pub := up.publisher("com.acme")
		up.Expect(up.Do("POST", "/v1/skills/com.acme/gone/versions?version=1", pub, "text/markdown", md("gone", "Soon yanked", "MIT")), 201, "v1")
		down := NewEnv(t, newStore(t), Options{Loopback: true})
		down.Expect(down.upstream(map[string]any{"name": "hub", "kind": "openlinker-registry", "base_url": up.Base, "visibility": "public"}), 201, "upstream")
		down.sync("hub")
		// Between two syncs the upstream publishes v2 and then yanks the skill.
		up.Expect(up.Do("POST", "/v1/skills/com.acme/gone/versions?version=2", pub, "text/markdown", md("gone", "Soon yanked", "MIT")), 201, "v2")
		up.Expect(up.Do("POST", "/v1/skills/com.acme/gone/yank", pub, "", nil), 200, "yank")
		for i := range 2 {
			s := down.sync("hub")
			if s["last_status"] != "ok" || stat(s, "skills_conflicts") != 0 || stat(s, "skills_skipped") != 0 {
				t.Fatalf("sync %d of a yanked skill: %v", i, s)
			}
		}
		if r := down.Do("GET", "/v1/skills/com.acme/gone", "", "", nil); r.JSON["status"] != "yanked" {
			t.Fatalf("yank did not arrive: %s", r.Body)
		}
		down.Expect(down.Do("GET", "/v1/skills/com.acme/gone/versions/1/artifact", "", "", nil), 410, "yanked content")
	})

	t.Run("official registry without a usable cursor", func(t *testing.T) {
		// The upstream reports no timestamps, so there is never a cursor and
		// every run is a full latest-only listing. A server whose only
		// version is deleted, and one that is gone, drop out of that listing;
		// the mirror must notice both.
		entry := func(name, status string, latest bool) map[string]any {
			return map[string]any{"server": map[string]any{"name": name, "description": "d", "version": "1.0.0",
				"remotes": []map[string]any{{"type": "sse", "url": "https://x.example/sse"}}},
				"_meta": map[string]any{metaOfficial: map[string]any{"status": status, "isLatest": latest}}}
		}
		later := false
		fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			enc := json.NewEncoder(w)
			switch {
			case r.URL.Path == "/v0.1/servers" && !later:
				_ = enc.Encode(map[string]any{"servers": []map[string]any{entry("com.feed/deleted", "active", true),
					entry("com.feed/gone", "active", true), entry("com.feed/kept", "active", true)}})
			case r.URL.Path == "/v0.1/servers":
				_ = enc.Encode(map[string]any{"servers": []map[string]any{entry("com.feed/kept", "active", true)}})
			case later && r.URL.EscapedPath() == "/v0.1/servers/com.feed%2Fdeleted/versions":
				_ = enc.Encode(map[string]any{"servers": []map[string]any{entry("com.feed/deleted", "deleted", false)}})
			default:
				w.WriteHeader(404)
				_, _ = w.Write([]byte(`{"error":"Server not found"}`))
			}
		}))
		t.Cleanup(fake.Close)
		e := NewEnv(t, newStore(t), Options{Loopback: true})
		e.Expect(e.upstream(map[string]any{"name": "feed", "kind": "mcp-registry", "base_url": fake.URL, "latest_only": true, "visibility": "public"}), 201, "upstream")
		if s := e.sync("feed"); s["last_status"] != "ok" || stat(s, "mcp_created") != 3 || s["cursor"] != nil {
			t.Fatalf("first sync: %v", s)
		}
		later = true
		if s := e.sync("feed"); s["last_status"] != "ok" {
			t.Fatalf("second sync: %v", s)
		}
		if got := servers(e.Do("GET", "/v0.1/servers", "", "", nil)); len(got) != 1 || got[0]["server"].(map[string]any)["name"] != "com.feed/kept" {
			t.Fatalf("servers deleted or gone upstream are still listed: %d", len(got))
		}
		for _, name := range []string{"deleted", "gone"} {
			r := e.Expect(e.Do("GET", "/v0.1/servers/com.feed%2F"+name+"/versions/1.0.0", "", "", nil), 200, name)
			if official(r.JSON)["status"] != "deleted" {
				t.Fatalf("%s: %s", name, r.Body)
			}
		}
	})
}
