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

	// The same holds when the upstream is configured by protocol only.
	plain := NewEnv(t, newStore(t), Options{Loopback: true, UpstreamTokens: map[string]string{"hub": owner}})
	plain.Expect(plain.upstream(map[string]any{"name": "hub", "kind": "mcp-registry", "base_url": up.Base, "visibility": "public"}), 201, "upstream")
	if s := plain.sync("hub"); s["last_status"] != "ok" {
		t.Fatalf("sync: %v", s)
	}
	if got := names(plain.Do("GET", "/v0.1/servers", "", "", nil), "servers", "name"); got != "com.acme/open-mcp" {
		t.Errorf("anonymous mcp listing through an mcp-registry upstream: %s", got)
	}
	plain.Expect(plain.Do("GET", "/v0.1/servers/com.acme%2Fsecret-mcp/versions/latest", AdminToken, "", nil), 404, "private entry through an mcp-registry upstream")

	// Restrictions applied upstream later follow at the next sync.
	up.Expect(up.Do("PATCH", "/v1/skills/com.acme/open", owner, "", map[string]any{"visibility": "internal"}), 200, "restrict upstream skill")
	up.Expect(up.Do("PATCH", "/v1/mcp/servers/com.acme/open-mcp", owner, "", map[string]any{"visibility": "private"}), 200, "hide upstream mcp")
	if s = down.sync("hub"); s["last_status"] != "ok" {
		t.Fatalf("second sync: %v", s)
	}
	if a, b := skillStatus("open", ""), skillStatus("open", reader); a != 404 || b != 200 {
		t.Errorf("skill restricted upstream: anonymous %d, authenticated %d", a, b)
	}
	if a := mcpStatus("open-mcp", ""); a != 404 {
		t.Errorf("mcp server hidden upstream is still public here: %d", a)
	}
	if got := names(down.Do("GET", "/v0.1/servers", reader, "", nil), "servers", "name"); strings.Contains(got, "open-mcp") {
		t.Errorf("mcp server hidden upstream is still listed: %s", got)
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
		down.Expect(down.upstream(map[string]any{"name": "official", "kind": "mcp-registry", "base_url": up.Base, "latest_only": true, "visibility": "public"}), 201, "upstream")
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
