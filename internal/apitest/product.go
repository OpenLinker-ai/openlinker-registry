package apitest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

const (
	metaOfficial = "io.modelcontextprotocol.registry/official"
	metaEntry    = "ai.openlinker.registry/entry"
)

func serverJSON(name, version, remote string) map[string]any {
	return map[string]any{"name": name, "description": "Weather tools", "version": version,
		"remotes": []map[string]any{{"type": "streamable-http", "url": remote}}}
}

func (e *Env) namespace(name, kind string) {
	e.T.Helper()
	e.Expect(e.Do("POST", "/v1/admin/namespaces", AdminToken, "", map[string]any{"name": name, "kind": kind}), 201, "namespace "+name)
}

func (e *Env) publisher(ns string) string {
	return e.token([]string{"skills:read", "skills:write"}, []map[string]string{{"namespace": ns, "role": "owner"}})
}

func (e *Env) publishMCP(token, ns string, doc map[string]any) Resp {
	return e.Do("POST", "/v1/mcp/servers/"+ns, token, "application/json", doc)
}

func servers(r Resp) []map[string]any {
	var out []map[string]any
	list, _ := r.JSON["servers"].([]any)
	for _, s := range list {
		out = append(out, s.(map[string]any))
	}
	return out
}

func official(entry map[string]any) map[string]any {
	meta, _ := entry["_meta"].(map[string]any)
	o, _ := meta[metaOfficial].(map[string]any)
	return o
}

func v0Version(entry map[string]any) string {
	s, _ := entry["server"].(map[string]any)
	v, _ := s["version"].(string)
	return v
}

func runMCP(t *testing.T, e *Env) {
	e.namespace("com.acme", "domain")
	e.namespace("com.secret", "domain")
	pub := e.publisher("com.acme")
	secret := e.publisher("com.secret")
	const name = "com.acme/weather"
	escaped := url.PathEscape(name)

	// --- publishing ---
	r := e.Expect(e.publishMCP(pub, "com.acme", serverJSON(name, "1.0.0", "https://mcp.acme.example/mcp")), 201, "publish")
	if r.JSON["mode"] != "registered" || r.JSON["is_latest"] != true || !digestRe.MatchString(r.JSON["digest"].(string)) {
		t.Fatalf("publish response: %s", r.Body)
	}
	e.Expect(e.publishMCP("", "com.acme", serverJSON(name, "9.9.9", "https://x.example/mcp")), 401, "anonymous publish")
	e.Expect(e.publishMCP(secret, "com.acme", serverJSON(name, "9.9.9", "https://x.example/mcp")), 403, "publish without a role")
	for code, doc := range map[string]map[string]any{
		"namespace_mismatch": serverJSON("com.other/weather", "1.0.0", "https://x.example/mcp"),
		"version_invalid":    serverJSON(name, "latest", "https://x.example/mcp"),
		"remote_invalid":     serverJSON(name, "2.0.0", "http://x.example/mcp"),
		"no_transport":       {"name": name, "description": "d", "version": "2.0.0"},
		"secret_value": {"name": name, "description": "d", "version": "2.0.0", "remotes": []map[string]any{{"type": "sse",
			"url": "https://x.example/sse", "headers": []map[string]any{{"name": "Authorization", "isSecret": true, "value": "Bearer hunter2"}}}}},
	} {
		if r := e.Expect(e.publishMCP(pub, "com.acme", doc), 422, code); errCode(r) != code {
			t.Fatalf("want %s, got %s", code, r.Body)
		}
	}
	// A credential is refused whether or not it is flagged as a secret.
	unflagged := map[string]any{"name": name, "description": "d", "version": "2.0.0", "remotes": []map[string]any{{"type": "sse",
		"url": "https://x.example/sse", "headers": []map[string]any{{"name": "Authorization", "value": "Bearer hunter2"}}}}}
	if r := e.Expect(e.publishMCP(pub, "com.acme", unflagged), 422, "unflagged credential"); errCode(r) != "secret_value" || strings.Contains(string(r.Body), "hunter2") {
		t.Fatalf("unflagged credential: %s", r.Body)
	}
	// So is a literal behind a placeholder in a credential position.
	indirect := map[string]any{"name": name, "description": "d", "version": "2.0.0", "remotes": []map[string]any{{"type": "sse",
		"url": "https://x.example/sse?api_key={key}", "variables": map[string]any{"key": map[string]any{"default": "hunter2"}}}}}
	if r := e.Expect(e.publishMCP(pub, "com.acme", indirect), 422, "credential behind a placeholder"); errCode(r) != "secret_value" || strings.Contains(string(r.Body), "hunter2") {
		t.Fatalf("credential behind a placeholder: %s", r.Body)
	}
	if r := e.Expect(e.publishMCP(pub, "com.acme", serverJSON(name, "1.0.0", "https://changed.example/mcp")), 409, "republish"); errCode(r) != "version_exists" {
		t.Fatalf("got %s", r.Body)
	}
	// Semantic versions decide what is latest, not publication order.
	for _, v := range []string{"1.2.0", "1.10.0", "1.3.0"} {
		e.Expect(e.publishMCP(pub, "com.acme", serverJSON(name, v, "https://mcp.acme.example/mcp")), 201, "publish "+v)
	}

	// --- native read API ---
	r = e.Expect(e.Do("GET", "/v1/mcp/servers", "", "", nil), 200, "list")
	if got := servers(r); len(got) != 1 || got[0]["version"] != "1.10.0" || got[0]["name"] != name {
		t.Fatalf("native list shows one row per server at its latest version: %s", r.Body)
	}
	if got := servers(e.Do("GET", "/v1/mcp/servers?version=all", "", "", nil)); len(got) != 4 {
		t.Fatalf("version=all: %d rows", len(got))
	}
	if got := servers(e.Do("GET", "/v1/mcp/servers?q=WEATHER+tools", "", "", nil)); len(got) != 1 {
		t.Fatal("search is a case-insensitive substring over name, title and description")
	}
	if got := servers(e.Do("GET", "/v1/mcp/servers?q=nomatch", "", "", nil)); len(got) != 0 {
		t.Fatal("search matched nothing expected")
	}
	r = e.Expect(e.Do("GET", "/v1/mcp/servers/com.acme/weather", "", "", nil), 200, "detail")
	if r.JSON["latest_version"] != "1.10.0" || len(r.JSON["versions"].([]any)) != 4 {
		t.Fatalf("detail: %s", r.Body)
	}
	e.Expect(e.Do("GET", "/v1/mcp/servers/com.acme/weather/versions/1.2.0", "", "", nil), 200, "version")
	e.Expect(e.Do("GET", "/v1/mcp/servers/com.acme/weather/versions/7.7.7", "", "", nil), 404, "missing version")

	// --- official registry API ---
	for _, prefix := range []string{"/v0.1", "/v0"} {
		r = e.Expect(e.Do("GET", prefix+"/servers", "", "", nil), 200, prefix+" list")
		got := servers(r)
		// Every version, ordered by name then version as text.
		var order []string
		for _, s := range got {
			order = append(order, v0Version(s))
		}
		if strings.Join(order, ",") != "1.0.0,1.10.0,1.2.0,1.3.0" {
			t.Fatalf("%s order: %v", prefix, order)
		}
		if meta := r.JSON["metadata"].(map[string]any); meta["count"] != float64(4) || meta["nextCursor"] != nil {
			t.Fatalf("metadata: %v", meta)
		}
		r = e.Expect(e.Do("GET", prefix+"/servers/"+escaped+"/versions/latest", "", "", nil), 200, "latest")
		if v0Version(r.JSON) != "1.10.0" || official(r.JSON)["isLatest"] != true || official(r.JSON)["status"] != "active" {
			t.Fatalf("latest: %s", r.Body)
		}
		// A proxy that decodes %2F delivers the name as two segments.
		e.Expect(e.Do("GET", prefix+"/servers/com.acme/weather/versions/1.0.0", "", "", nil), 200, "decoded name")
		if got := servers(e.Expect(e.Do("GET", prefix+"/servers/"+escaped+"/versions", "", "", nil), 200, "versions")); len(got) != 4 {
			t.Fatalf("versions: %d", len(got))
		}
	}
	if got := servers(e.Do("GET", "/v0.1/servers?version=latest", "", "", nil)); len(got) != 1 || v0Version(got[0]) != "1.10.0" {
		t.Fatal("version=latest must return only the latest version")
	}
	// The stored document carries no registry metadata of its own.
	r = e.Do("GET", "/v0.1/servers/"+escaped+"/versions/1.0.0", "", "", nil)
	if s := r.JSON["server"].(map[string]any); s["_meta"] != nil || s["name"] != name {
		t.Fatalf("server document: %s", r.Body)
	}
	if entry := r.JSON["_meta"].(map[string]any)[metaEntry].(map[string]any); entry["mode"] != "registered" {
		t.Fatalf("registry metadata: %s", r.Body)
	}
	// Pagination returns every row exactly once.
	seen, cursor := map[string]bool{}, ""
	for range 4 {
		r = e.Expect(e.Do("GET", "/v0.1/servers?limit=3&cursor="+cursor, "", "", nil), 200, "page")
		for _, s := range servers(r) {
			if seen[v0Version(s)] {
				t.Fatalf("duplicate %s", v0Version(s))
			}
			seen[v0Version(s)] = true
		}
		cursor, _ = r.JSON["metadata"].(map[string]any)["nextCursor"].(string)
		if cursor == "" {
			break
		}
	}
	if len(seen) != 4 {
		t.Fatalf("pagination saw %d rows", len(seen))
	}
	// Errors use the official body: {"error": "<message>"}.
	r = e.Expect(e.Do("GET", "/v0.1/servers/com.acme%2Fnope/versions/latest", "", "", nil), 404, "unknown server")
	if _, isString := r.JSON["error"].(string); !isString {
		t.Fatalf("error body: %s", r.Body)
	}
	e.Expect(e.Do("GET", "/v0.1/servers/com.acme%2Fweather", "", "", nil), 404, "unknown sub-path")
	e.Expect(e.Do("GET", "/v0.1/servers?limit=0", "", "", nil), 400, "bad limit")
	e.Expect(e.Do("GET", "/v0.1/servers?updated_since=yesterday", "", "", nil), 400, "bad timestamp")
	e.Expect(e.Do("GET", "/v0.1/servers?cursor=!!!", "", "", nil), 400, "bad cursor")

	// --- status ---
	status := func(path string, body map[string]any, token string) Resp {
		return e.Do("POST", "/v1/mcp/servers/com.acme/weather"+path, token, "", body)
	}
	e.Expect(status("/status", map[string]any{"status": "deprecated"}, ""), 401, "anonymous status")
	e.Expect(status("/status", map[string]any{"status": "deprecated"}, secret), 403, "status without a role")
	e.Expect(status("/versions/1.10.0/status", map[string]any{"status": "active", "message": "x"}, pub), 422, "message on active")
	e.Expect(status("/versions/1.10.0/status", map[string]any{"status": "gone"}, pub), 422, "unknown status")
	e.Expect(status("/versions/4.0.0/status", map[string]any{"status": "deprecated"}, pub), 404, "unknown version")
	r = e.Expect(status("/versions/1.10.0/status", map[string]any{"status": "deprecated", "message": "use 2.x"}, pub), 200, "deprecate")
	if r.JSON["updated_count"] != float64(1) {
		t.Fatalf("deprecate: %s", r.Body)
	}
	r = e.Do("GET", "/v0.1/servers/"+escaped+"/versions/latest", "", "", nil)
	if o := official(r.JSON); o["status"] != "deprecated" || o["statusMessage"] != "use 2.x" || v0Version(r.JSON) != "1.10.0" {
		t.Fatalf("a deprecated version stays latest: %s", r.Body)
	}
	e.Expect(status("/versions/1.10.0/status", map[string]any{"status": "deleted"}, pub), 200, "delete")
	if r = e.Do("GET", "/v0.1/servers/"+escaped+"/versions/latest", "", "", nil); v0Version(r.JSON) != "1.3.0" {
		t.Fatalf("latest must move off a deleted version: %s", r.Body)
	}
	if got := servers(e.Do("GET", "/v0.1/servers", "", "", nil)); len(got) != 3 {
		t.Fatalf("deleted versions are hidden by default: %d", len(got))
	}
	if got := servers(e.Do("GET", "/v0.1/servers?include_deleted=true", "", "", nil)); len(got) != 4 {
		t.Fatalf("include_deleted: %d", len(got))
	}
	// Incremental sync sees deletions.
	if got := servers(e.Do("GET", "/v0.1/servers?updated_since=2000-01-01T00:00:00Z", "", "", nil)); len(got) != 4 {
		t.Fatalf("updated_since implies include_deleted: %d", len(got))
	}
	if got := servers(e.Do("GET", "/v0.1/servers?updated_since=2999-01-01T00:00:00Z", "", "", nil)); len(got) != 0 {
		t.Fatal("updated_since in the future must return nothing")
	}
	e.Expect(status("/versions/1.10.0/status", map[string]any{"status": "active"}, pub), 200, "restore")
	if r = e.Do("GET", "/v0.1/servers/"+escaped+"/versions/latest", "", "", nil); v0Version(r.JSON) != "1.10.0" {
		t.Fatalf("latest returns to the best version: %s", r.Body)
	}
	if r = e.Expect(status("/status", map[string]any{"status": "deprecated", "message": "retired"}, pub), 200, "deprecate all"); r.JSON["updated_count"] != float64(4) {
		t.Fatalf("all versions: %s", r.Body)
	}

	// --- visibility ---
	e.Expect(e.Do("POST", "/v1/mcp/servers/com.secret?visibility=private", secret, "application/json",
		serverJSON("com.secret/internal-tools", "1.0.0", "https://mcp.secret.example/mcp")), 201, "private publish")
	hidden := "/servers/" + url.PathEscape("com.secret/internal-tools")
	for _, token := range []string{"", pub} {
		e.Expect(e.Do("GET", "/v0.1"+hidden+"/versions/latest", token, "", nil), 404, "private server hidden")
		e.Expect(e.Do("GET", "/v0.1"+hidden+"/versions", token, "", nil), 404, "private versions hidden")
		e.Expect(e.Do("GET", "/v1/mcp/servers/com.secret/internal-tools", token, "", nil), 404, "private detail hidden")
		for _, s := range servers(e.Do("GET", "/v0.1/servers", token, "", nil)) {
			if strings.HasPrefix(s["server"].(map[string]any)["name"].(string), "com.secret/") {
				t.Fatal("private server listed to a non-member")
			}
		}
	}
	e.Expect(e.Do("GET", "/v0.1"+hidden+"/versions/latest", secret, "", nil), 200, "member read")
	if got := servers(e.Do("GET", "/v0.1/servers?search=internal", secret, "", nil)); len(got) != 1 {
		t.Fatal("member must see the private server")
	}
	// Unlisted entries are readable by name but not listed.
	e.Expect(e.Do("PATCH", "/v1/mcp/servers/com.secret/internal-tools", secret, "", map[string]any{"visibility": "public", "listed": false}), 200, "unlist")
	e.Expect(e.Do("GET", "/v0.1"+hidden+"/versions/latest", "", "", nil), 200, "unlisted direct read")
	if got := servers(e.Do("GET", "/v0.1/servers?search=internal", "", "", nil)); len(got) != 0 {
		t.Fatal("unlisted server appeared in a listing")
	}

	// --- indexed entries and official-style publish ---
	r = e.Expect(e.Do("POST", "/v1/mcp/servers/com.acme?indexed=true", pub, "application/json",
		serverJSON("com.acme/found-elsewhere", "0.1.0", "https://third.example/sse")), 201, "index")
	if r.JSON["mode"] != "indexed" {
		t.Fatalf("indexed mode: %s", r.Body)
	}
	r = e.Expect(e.Do("POST", "/v0.1/publish", pub, "application/json", serverJSON("com.acme/published", "1.0.0", "https://p.example/mcp")), 200, "official publish")
	if v0Version(r.JSON) != "1.0.0" || official(r.JSON)["isLatest"] != true {
		t.Fatalf("publish response: %s", r.Body)
	}
	// A server may be called "servers" or "versions"; both path forms still
	// address it.
	for _, odd := range []string{"servers", "versions"} {
		e.Expect(e.publishMCP(pub, "com.acme", serverJSON("com.acme/"+odd, "1.0.0", "https://odd.example/"+odd)), 201, "publish "+odd)
		for _, path := range []string{"/v0.1/servers/com.acme%2F" + odd + "/versions/1.0.0", "/v0.1/servers/com.acme/" + odd + "/versions/1.0.0",
			"/v0.1/servers/com.acme%2F" + odd + "/versions", "/v0.1/servers/com.acme/" + odd + "/versions"} {
			e.Expect(e.Do("GET", path, "", "", nil), 200, path)
		}
	}
	e.Expect(e.Do("POST", "/v0.1/publish", secret, "application/json", serverJSON("com.acme/stolen", "1.0.0", "https://p.example/mcp")), 403, "publish into a foreign namespace")
	e.Expect(e.Do("POST", "/v0.1/publish", "", "application/json", serverJSON("com.acme/anon", "1.0.0", "https://p.example/mcp")), 401, "anonymous official publish")

	// --- identity and admin listings ---
	r = e.Expect(e.Do("GET", "/v1/whoami", pub, "", nil), 200, "whoami")
	if r.JSON["authenticated"] != true || r.JSON["roles"].(map[string]any)["com.acme"] != "owner" {
		t.Fatalf("whoami: %s", r.Body)
	}
	if r = e.Do("GET", "/v1/whoami", "", "", nil); r.JSON["authenticated"] != false {
		t.Fatalf("anonymous whoami: %s", r.Body)
	}
	e.Expect(e.Do("GET", "/v1/admin/tokens", pub, "", nil), 403, "tokens need admin")
	r = e.Expect(e.Do("GET", "/v1/admin/tokens", AdminToken, "", nil), 200, "tokens")
	if strings.Contains(string(r.Body), "secret_hash") || strings.Contains(string(r.Body), pub) {
		t.Fatal("token listing leaked secret material")
	}
	if r = e.Expect(e.Do("GET", "/v1/admin/namespaces", AdminToken, "", nil), 200, "namespaces"); len(r.JSON["namespaces"].([]any)) != 2 {
		t.Fatalf("namespaces: %s", r.Body)
	}
}

// --- upstreams: forwarding and mirroring between two registries ---

func (e *Env) upstream(body map[string]any) Resp {
	return e.Do("POST", "/v1/admin/upstreams", AdminToken, "", body)
}

func (e *Env) sync(name string) map[string]any {
	e.T.Helper()
	r := e.Expect(e.Do("POST", "/v1/admin/upstreams/"+name+"/sync?wait=true", AdminToken, "", nil), 200, "sync "+name)
	return r.JSON["sync"].(map[string]any)
}

func stat(sync map[string]any, key string) float64 {
	n, _ := sync["last_stats"].(map[string]any)[key].(float64)
	return n
}

func runUpstreams(t *testing.T, newStore func(t *testing.T) store.Store) {
	// up is a public registry with content; down is a private registry that
	// forwards to it and mirrors it.
	up := NewEnv(t, newStore(t), Options{})
	down := NewEnv(t, newStore(t), Options{Mode: "private", Loopback: true})
	up.namespace("com.acme", "domain")
	up.namespace("com.local", "domain")
	pub := up.publisher("com.acme")
	localUp := up.publisher("com.local")

	skillMD := md("hello", "Says hello", "MIT")
	up.Expect(up.Do("POST", "/v1/skills/com.acme/hello/versions?version=1.0.0", pub, "text/markdown", skillMD), 201, "upstream skill")
	bundle := tarGz(t, map[string][]byte{"SKILL.md": md("kit", "A tool kit", "Apache-2.0"), "scripts/run.sh": []byte("#!/bin/sh\necho kit\n")})
	up.Expect(up.Do("POST", "/v1/skills/com.acme/kit/versions?version=2.0.0", pub, "application/gzip", bundle), 201, "upstream archive skill")
	up.Expect(up.Do("POST", "/v1/skills/com.local/shadow/versions?version=1.0.0", localUp, "text/markdown", md("shadow", "Upstream copy", "MIT")), 201, "shadowed skill")
	up.Expect(up.publishMCP(pub, "com.acme", serverJSON("com.acme/weather", "1.0.0", "https://mcp.acme.example/mcp")), 201, "upstream mcp")

	// --- configuration ---
	if r := NewEnv(t, newStore(t), Options{Mode: "private"}).upstream(map[string]any{"name": "hub", "kind": "openlinker-registry", "base_url": up.Base}); r.Status != 422 || errCode(r) != "base_url_invalid" {
		t.Fatalf("the default outbound policy must refuse loopback and http upstreams: %d %s", r.Status, r.Body)
	}
	for what, body := range map[string]map[string]any{
		"kind":             {"name": "x", "kind": "npm", "base_url": up.Base},
		"scheme":           {"name": "x", "kind": "mcp-registry", "base_url": "ftp://example.com"},
		"credentials":      {"name": "x", "kind": "mcp-registry", "base_url": "http://user:pw@127.0.0.1:1/"},
		"name":             {"name": "Bad Name", "kind": "mcp-registry", "base_url": up.Base},
		"missing target":   {"name": "x", "kind": "agent-skills", "base_url": up.Base},
		"unwanted target":  {"name": "x", "kind": "mcp-registry", "base_url": up.Base, "target_namespace": "mirror.x"},
		"pattern":          {"name": "x", "kind": "mcp-registry", "base_url": up.Base, "namespace_allow": []string{"IO.*"}},
		"interval":         {"name": "x", "kind": "mcp-registry", "base_url": up.Base, "sync_interval_seconds": 5},
		"private mirror":   {"name": "x", "kind": "mcp-registry", "base_url": up.Base, "visibility": "private"},
		"forward on index": {"name": "x", "kind": "agent-skills", "base_url": up.Base, "target_namespace": "mirror.x", "proxy_on_miss": true},
	} {
		down.Expect(down.upstream(body), 422, "invalid upstream: "+what)
	}
	down.Expect(down.Do("POST", "/v1/admin/upstreams", "", "", map[string]any{"name": "hub"}), 401, "anonymous upstream admin")
	r := down.Expect(down.upstream(map[string]any{"name": "hub", "kind": "openlinker-registry", "base_url": up.Base + "/", "proxy_on_miss": true}), 201, "create upstream")
	if r.JSON["base_url"] != up.Base || r.JSON["visibility"] != "internal" || r.JSON["token_configured"] != false {
		t.Fatalf("upstream: %s", r.Body)
	}
	down.Expect(down.upstream(map[string]any{"name": "hub", "kind": "mcp-registry", "base_url": up.Base}), 409, "duplicate upstream")

	down.namespace("com.local", "domain")
	reader := down.token([]string{"skills:read"}, nil)

	// --- forwarding a miss ---
	down.Expect(down.Do("GET", "/v1/skills/com.acme/hello", "", "", nil), 401, "private instance needs a token")
	r = down.Expect(down.Do("GET", "/v1/skills/com.acme/hello", reader, "", nil), 200, "forwarded skill")
	if r.JSON["upstream"] != "hub" || r.JSON["mode"] != "indexed" || r.JSON["visibility"] != "internal" || r.JSON["latest_version"] != "1.0.0" {
		t.Fatalf("mirrored skill: %s", r.Body)
	}
	// Bytes are pulled through on first read and verified against the digest.
	r = down.Expect(down.Do("GET", "/v1/skills/com.acme/hello/versions/1.0.0/artifact", reader, "", nil), 200, "pull-through artifact")
	if string(r.Body) != string(skillMD) || r.Header.Get("ETag") != `"`+sha(skillMD)+`"` {
		t.Fatal("mirrored artifact differs from the upstream bytes")
	}
	r = down.Expect(down.Do("GET", "/v1/skills/com.acme/kit/versions/2.0.0", reader, "", nil), 200, "forwarded archive skill")
	if loc := r.JSON["locator"].(map[string]any); loc["kind"] != "registry" || loc["upstream"] != "hub" {
		t.Fatalf("locator: %s", r.Body)
	}
	archiveDigest := r.JSON["archive"].(map[string]any)["digest"].(string)
	r = down.Expect(down.Do("GET", "/v1/skills/com.acme/kit/versions/2.0.0/archive.tar.gz", reader, "", nil), 200, "mirrored archive")
	if sha(r.Body) != archiveDigest {
		t.Fatal("the mirror must reproduce the upstream's archive digest")
	}
	r = down.Expect(down.Do("GET", "/v1/skills/com.acme/kit/versions/2.0.0/files/scripts/run.sh", reader, "", nil), 200, "mirrored file")
	if string(r.Body) != "#!/bin/sh\necho kit\n" {
		t.Fatalf("file: %q", r.Body)
	}
	down.Expect(down.Do("GET", "/v1/skills/com.acme/missing", reader, "", nil), 404, "missing everywhere")
	// A namespace that exists locally is never forwarded: local ownership wins.
	down.Expect(down.Do("GET", "/v1/skills/com.local/shadow", reader, "", nil), 404, "local namespace not forwarded")
	r = down.Expect(down.Do("GET", "/v0.1/servers/"+url.PathEscape("com.acme/weather")+"/versions/latest", reader, "", nil), 200, "forwarded mcp")
	if entry := r.JSON["_meta"].(map[string]any)[metaEntry].(map[string]any); entry["mode"] != "mirrored" || entry["upstream"] != "hub" {
		t.Fatalf("mirrored mcp: %s", r.Body)
	}
	down.Expect(down.Do("GET", "/v0.1/servers/com.acme%2Fnope/versions/latest", reader, "", nil), 404, "missing mcp")

	// --- mirrored namespaces are read-only here ---
	admin := AdminToken
	if r = down.Do("POST", "/v1/skills/com.acme/mine/versions?version=1", admin, "text/markdown", md("mine", "Local", "MIT")); r.Status != 409 || errCode(r) != "namespace_upstream_managed" {
		t.Fatalf("local publish into a mirrored namespace: %d %s", r.Status, r.Body)
	}
	if r = down.publishMCP(admin, "com.acme", serverJSON("com.acme/mine", "1.0.0", "https://x.example/mcp")); r.Status != 409 || errCode(r) != "namespace_upstream_managed" {
		t.Fatalf("local mcp publish into a mirrored namespace: %d %s", r.Status, r.Body)
	}
	if r = down.Do("POST", "/v1/mcp/servers/com.acme/weather/status", admin, "", map[string]any{"status": "deleted"}); r.Status != 409 || errCode(r) != "mirrored_read_only" {
		t.Fatalf("status change on a mirrored server: %d %s", r.Status, r.Body)
	}
	// A mirrored skill follows its upstream: even an administrator cannot
	// yank or re-publish it here (a local yank would be final).
	for what, r := range map[string]Resp{
		"yank":         down.Do("POST", "/v1/skills/com.acme/hello/yank", admin, "", nil),
		"deprecate":    down.Do("POST", "/v1/skills/com.acme/hello/deprecate", admin, "", nil),
		"version yank": down.Do("POST", "/v1/skills/com.acme/hello/versions/1.0.0/yank", admin, "", nil),
		"settings":     down.Do("PATCH", "/v1/skills/com.acme/hello", admin, "", map[string]any{"visibility": "public"}),
	} {
		if r.Status != 409 || errCode(r) != "mirrored_read_only" {
			t.Fatalf("%s of a mirrored skill: %d %s", what, r.Status, r.Body)
		}
	}
	// A mirrored namespace has no members: nobody can be given a role in it.
	if r = down.Do("POST", "/v1/admin/namespaces/com.acme/roles", admin, "", map[string]any{"subject": "token:aaaaaaaaaaaaaaaa", "role": "reader"}); r.Status != 409 || errCode(r) != "namespace_upstream_managed" {
		t.Fatalf("role in a mirrored namespace: %d %s", r.Status, r.Body)
	}
	if r = down.Do("POST", "/v1/admin/tokens", admin, "", map[string]any{"name": "t", "scopes": []string{"skills:read"},
		"roles": []map[string]string{{"namespace": "com.acme", "role": "reader"}}}); r.Status != 409 || errCode(r) != "namespace_upstream_managed" {
		t.Fatalf("token with a role in a mirrored namespace: %d %s", r.Status, r.Body)
	}
	down.Expect(down.Do("DELETE", "/v1/admin/upstreams/hub", admin, "", nil), 409, "upstream in use")

	// --- sync ---
	up.Expect(up.Do("POST", "/v1/skills/com.acme/hello/versions?version=1.1.0", pub, "text/markdown", md("hello", "Says hello again", "MIT")), 201, "new upstream version")
	// A version published upstream after the skill was mirrored is fetched
	// when it is asked for; it does not have to wait for a sync.
	down.Expect(down.Do("GET", "/v1/skills/com.acme/hello/versions/1.1.0", reader, "", nil), 200, "forwarded new version")
	down.Expect(down.Do("GET", "/v1/skills/com.acme/hello/versions/9.9.9", reader, "", nil), 404, "version missing everywhere")
	if r = down.Do("GET", "/v1/skills/com.acme/hello", reader, "", nil); r.JSON["latest_version"] != "1.1.0" {
		t.Fatalf("latest after version forward: %s", r.Body)
	}
	up.Expect(up.Do("POST", "/v1/skills/com.acme/later/versions?version=0.1.0", pub, "text/markdown", md("later", "Published later", "MIT")), 201, "new upstream skill")
	up.Expect(up.publishMCP(pub, "com.acme", serverJSON("com.acme/weather", "1.1.0", "https://mcp.acme.example/mcp")), 201, "new upstream mcp version")
	up.Expect(up.publishMCP(pub, "com.acme", serverJSON("com.acme/maps", "0.5.0", "https://maps.acme.example/sse")), 201, "new upstream mcp")
	s1 := down.sync("hub")
	if s1["last_status"] != "ok" || stat(s1, "skills_created") != 1 || stat(s1, "mcp_created") != 2 || stat(s1, "skills_conflicts") != 1 {
		t.Fatalf("first sync: %v", s1)
	}
	r = down.Expect(down.Do("GET", "/v1/skills?namespace=com.acme", reader, "", nil), 200, "mirrored list")
	if got := r.JSON["skills"].([]any); len(got) != 3 {
		t.Fatalf("mirrored skills: %s", r.Body)
	}
	if r = down.Do("GET", "/v1/skills/com.acme/hello", reader, "", nil); r.JSON["latest_version"] != "1.1.0" {
		t.Fatalf("latest after sync: %s", r.Body)
	}
	if got := servers(down.Do("GET", "/v0.1/servers?version=latest", reader, "", nil)); len(got) != 2 {
		t.Fatalf("mirrored mcp servers: %d", len(got))
	}
	// The shadowed upstream namespace stayed local.
	down.Expect(down.Do("GET", "/v1/skills/com.local/shadow", reader, "", nil), 404, "conflicting namespace not mirrored")
	s2 := down.sync("hub")
	if stat(s2, "skills_created") != 0 || stat(s2, "mcp_created") != 0 || stat(s2, "skills_unchanged") != 3 {
		t.Fatalf("a second sync must change nothing: %v", s2)
	}
	// Status changes upstream propagate.
	up.Expect(up.Do("POST", "/v1/skills/com.acme/later/yank", pub, "", map[string]any{"reason": "withdrawn"}), 200, "upstream yank")
	up.Expect(up.Do("POST", "/v1/mcp/servers/com.acme/maps/status", pub, "", map[string]any{"status": "deleted"}), 200, "upstream mcp delete")
	// Yanking an old version changes neither the latest version nor the
	// status of the skill upstream; it must still arrive.
	up.Expect(up.Do("POST", "/v1/skills/com.acme/hello/versions/1.0.0/yank", pub, "", map[string]any{"reason": "broken"}), 200, "upstream yank of an old version")
	s3 := down.sync("hub")
	if s3["last_status"] != "ok" {
		t.Fatalf("third sync: %v", s3)
	}
	if r = down.Do("GET", "/v1/skills/com.acme/later", reader, "", nil); r.JSON["status"] != "yanked" {
		t.Fatalf("yank must propagate: %s", r.Body)
	}
	down.Expect(down.Do("GET", "/v1/skills/com.acme/later/versions/0.1.0/artifact", reader, "", nil), 410, "yanked mirror content")
	down.Expect(down.Do("GET", "/v1/skills/com.acme/hello/versions/1.0.0/artifact", reader, "", nil), 410, "yanked old version")
	down.Expect(down.Do("GET", "/v1/skills/com.acme/hello/versions/1.1.0/artifact", reader, "", nil), 200, "latest version unaffected")
	down.Expect(down.Do("GET", "/v0.1/servers/"+url.PathEscape("com.acme/maps")+"/versions/0.5.0", reader, "", nil), 200, "deleted version still addressable")
	if got := servers(down.Do("GET", "/v0.1/servers?version=latest", reader, "", nil)); len(got) != 1 {
		t.Fatalf("an upstream deletion must hide the server: %d", len(got))
	}

	// --- settings ---
	r = down.Expect(down.Do("PATCH", "/v1/admin/upstreams/hub", admin, "", map[string]any{"enabled": false}), 200, "disable")
	if r.JSON["enabled"] != false {
		t.Fatalf("patch: %s", r.Body)
	}
	down.Expect(down.Do("POST", "/v1/admin/upstreams/hub/sync?wait=true", admin, "", nil), 409, "sync of a disabled upstream")
	down.Expect(down.Do("GET", "/v1/skills/com.acme/never-seen", reader, "", nil), 404, "a disabled upstream is not consulted")
	if r = down.Expect(down.Do("GET", "/v1/admin/upstreams", admin, "", nil), 200, "list upstreams"); len(r.JSON["upstreams"].([]any)) != 1 {
		t.Fatalf("upstreams: %s", r.Body)
	}

	t.Run("verification", func(t *testing.T) { runMirrorVerification(t, newStore) })
	t.Run("discovery index", func(t *testing.T) { runIndexUpstream(t, newStore) })
	t.Run("private upstream", func(t *testing.T) { runPrivateUpstream(t, newStore) })
	t.Run("forward limit", func(t *testing.T) { runForwardLimit(t, newStore, up) })
}

// runMirrorVerification points a registry at a lying upstream: metadata that
// does not describe the bytes it serves. Nothing unverified may be served.
func runMirrorVerification(t *testing.T, newStore func(t *testing.T) store.Store) {
	good := md("honest", "Honest skill", "MIT")
	evil := md("honest", "Tampered", "MIT")
	version := func(name string, digest string, fileHash string) map[string]any {
		return map[string]any{"namespace": "com.liar", "name": name, "version": "1.0.0", "mode": "hosted",
			"frontmatter": map[string]any{"name": name, "description": "d"}, "license": map[string]any{"kind": "spdx", "id": "MIT"},
			"core_compatible": true, "files": []map[string]any{{"path": "SKILL.md", "size": len(good), "sha256": fileHash}},
			"artifact":       map[string]any{"type": "skill-md", "digest": digest, "size": len(good)},
			"content_access": map[string]any{"full_text": true, "artifact": true}}
	}
	goodHash := strings.TrimPrefix(sha(good), "sha256:")
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		detail := func(name string) {
			_ = json.NewEncoder(w).Encode(map[string]any{"namespace": "com.liar", "name": name, "status": "active", "mode": "hosted",
				"visibility": "public", "listed": true, "latest_version": "1.0.0",
				"versions": []map[string]any{{"version": "1.0.0", "yanked": false}}})
		}
		switch r.URL.Path {
		case "/v1/skills/com.liar/swapped":
			detail("swapped")
		case "/v1/skills/com.liar/swapped/versions/1.0.0":
			// The digest is that of the good bytes; the artifact served is not.
			_ = json.NewEncoder(w).Encode(version("swapped", sha(good), goodHash))
		case "/v1/skills/com.liar/swapped/versions/1.0.0/artifact":
			_, _ = w.Write(evil)
		case "/v1/skills/com.liar/manifest":
			detail("manifest")
		case "/v1/skills/com.liar/manifest/versions/1.0.0":
			// The digest matches the bytes, but the manifest describes other content.
			_ = json.NewEncoder(w).Encode(version("manifest", sha(good), strings.Repeat("0", 64)))
		case "/v1/skills/com.liar/manifest/versions/1.0.0/artifact":
			_, _ = w.Write(good)
		case "/v1/skills/com.liar/traversal":
			detail("traversal")
		case "/v1/skills/com.liar/traversal/versions/1.0.0":
			v := version("traversal", sha(good), goodHash)
			v["files"] = []map[string]any{{"path": "../../etc/passwd", "size": 1, "sha256": goodHash}}
			_ = json.NewEncoder(w).Encode(v)
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found"}}`))
		}
	}))
	t.Cleanup(fake.Close)
	e := NewEnv(t, newStore(t), Options{Loopback: true})
	e.Expect(e.upstream(map[string]any{"name": "liar", "kind": "openlinker-registry", "base_url": fake.URL, "proxy_on_miss": true, "visibility": "public"}), 201, "upstream")
	for _, name := range []string{"swapped", "manifest"} {
		e.Expect(e.Do("GET", "/v1/skills/com.liar/"+name, "", "", nil), 200, "metadata of "+name)
		for _, sub := range []string{"artifact", "files/SKILL.md"} {
			r := e.Do("GET", "/v1/skills/com.liar/"+name+"/versions/1.0.0/"+sub, "", "", nil)
			if r.Status != 409 || errCode(r) != "source_changed" || strings.Contains(string(r.Body), "Tampered") {
				t.Fatalf("%s %s: unverified content must not be served: %d %s", name, sub, r.Status, r.Body)
			}
		}
		r := e.Do("GET", "/v1/skills/com.liar/"+name+"/versions/1.0.0", "", "", nil)
		if check := r.JSON["check"].(map[string]any); check["result"] != "source_changed" {
			t.Fatalf("the failed check must be recorded: %s", r.Body)
		}
	}
	// A manifest with an unsafe path is refused outright.
	e.Expect(e.Do("GET", "/v1/skills/com.liar/traversal", "", "", nil), 404, "unsafe manifest")
}

// runIndexUpstream mirrors an agentskills.io discovery index (here: the
// namespace index of another registry) into a local namespace.
func runIndexUpstream(t *testing.T, newStore func(t *testing.T) store.Store) {
	up := NewEnv(t, newStore(t), Options{RealPublicURL: true})
	up.namespace("com.acme", "domain")
	pub := up.publisher("com.acme")
	up.Expect(up.Do("POST", "/v1/skills/com.acme/hello/versions?version=1.0.0", pub, "text/markdown", md("hello", "Says hello", "MIT")), 201, "skill")
	kit := tarGz(t, map[string][]byte{"SKILL.md": md("kit", "A tool kit", "Apache-2.0"), "scripts/run.sh": []byte("#!/bin/sh\necho kit\n")})
	up.Expect(up.Do("POST", "/v1/skills/com.acme/kit/versions?version=2.0.0", pub, "application/gzip", kit), 201, "archive skill")
	up.Expect(up.Do("POST", "/v1/skills/com.acme/gone/versions?version=1.0.0", pub, "text/markdown", md("gone", "Yanked", "MIT")), 201, "yanked skill")
	up.Expect(up.Do("POST", "/v1/skills/com.acme/gone/yank", pub, "", nil), 200, "yank")

	e := NewEnv(t, newStore(t), Options{Loopback: true})
	e.namespace("com.taken", "domain")
	if r := e.upstream(map[string]any{"name": "site", "kind": "agent-skills", "base_url": up.Base + "/ns/com.acme", "target_namespace": "com.taken"}); r.Status != 409 || errCode(r) != "namespace_upstream_managed" {
		t.Fatalf("a local namespace cannot become a mirror target: %d %s", r.Status, r.Body)
	}
	if r := e.Expect(e.Do("GET", "/v1/admin/upstreams", AdminToken, "", nil), 200, "list"); len(r.JSON["upstreams"].([]any)) != 0 {
		t.Fatal("a rejected upstream must not be left behind")
	}
	e.Expect(e.upstream(map[string]any{"name": "site", "kind": "agent-skills", "base_url": up.Base + "/ns/com.acme",
		"target_namespace": "mirror.acme", "visibility": "public"}), 201, "index upstream")
	s := e.sync("site")
	// hello (single file) and kit (archive) are in the upstream namespace
	// index; the yanked skill is not.
	if s["last_status"] != "ok" || stat(s, "skills_created") != 2 {
		t.Fatalf("index sync: %v", s)
	}
	r := e.Expect(e.Do("GET", "/v1/skills/mirror.acme/kit", "", "", nil), 200, "indexed skill")
	version, _ := r.JSON["latest_version"].(string)
	if !strings.HasPrefix(version, "sha-") || r.JSON["upstream"] != "site" {
		t.Fatalf("indexed skill: %s", r.Body)
	}
	r = e.Expect(e.Do("GET", "/v1/skills/mirror.acme/kit/versions/"+version+"/files/scripts/run.sh", "", "", nil), 200, "indexed file")
	if string(r.Body) != "#!/bin/sh\necho kit\n" {
		t.Fatalf("file: %q", r.Body)
	}
	if s = e.sync("site"); stat(s, "skills_created") != 0 || stat(s, "skills_unchanged") != 2 {
		t.Fatalf("second index sync: %v", s)
	}
	// The mirror republishes the entries in its own discovery index.
	r = e.Expect(e.Do("GET", "/ns/mirror.acme/.well-known/agent-skills/index.json", "", "", nil), 200, "downstream index")
	if got := r.JSON["skills"].([]any); len(got) != 2 {
		t.Fatalf("downstream index: %s", r.Body)
	}
}

// runPrivateUpstream reads from an upstream that requires a token.
func runPrivateUpstream(t *testing.T, newStore func(t *testing.T) store.Store) {
	up := NewEnv(t, newStore(t), Options{Mode: "private"})
	up.namespace("internal.team", "internal")
	pub := up.publisher("internal.team")
	up.Expect(up.Do("POST", "/v1/skills/internal.team/runbook/versions?version=1", pub, "text/markdown", md("runbook", "Team runbook", "")), 201, "private upstream skill")
	upReader := up.token([]string{"skills:read"}, nil)

	without := NewEnv(t, newStore(t), Options{Mode: "private", Loopback: true})
	without.Expect(without.upstream(map[string]any{"name": "team", "kind": "openlinker-registry", "base_url": up.Base, "proxy_on_miss": true}), 201, "upstream")
	reader := without.token([]string{"skills:read"}, nil)
	if r := without.Do("GET", "/v1/skills/internal.team/runbook", reader, "", nil); r.Status != 502 || errCode(r) != "upstream_unavailable" {
		t.Fatalf("an upstream that refuses us is unavailable, not a miss: %d %s", r.Status, r.Body)
	}
	if s := without.Do("POST", "/v1/admin/upstreams/team/sync?wait=true", AdminToken, "", nil); s.JSON["sync"].(map[string]any)["last_status"] != "failed" {
		t.Fatalf("sync without credentials: %s", s.Body)
	}

	with := NewEnv(t, newStore(t), Options{Mode: "private", Loopback: true, UpstreamTokens: map[string]string{"team": upReader}})
	r := with.Expect(with.upstream(map[string]any{"name": "team", "kind": "openlinker-registry", "base_url": up.Base, "proxy_on_miss": true}), 201, "upstream")
	if r.JSON["token_configured"] != true || strings.Contains(string(r.Body), upReader) {
		t.Fatalf("token state: %s", r.Body)
	}
	reader = with.token([]string{"skills:read"}, nil)
	with.Expect(with.Do("GET", "/v1/skills/internal.team/runbook", reader, "", nil), 200, "authenticated forward")
	// The upstream hosts this unlicensed skill internally and serves it; the
	// mirror serves exactly what its source serves.
	r = with.Expect(with.Do("GET", "/v1/skills/internal.team/runbook/versions/1/artifact", reader, "", nil), 200, "internal content through the mirror")
	if !strings.Contains(string(r.Body), "Team runbook") {
		t.Fatalf("artifact: %s", r.Body)
	}
	if r = with.Do("GET", "/v1/admin/audit?limit=50", AdminToken, "", nil); strings.Contains(string(r.Body), upReader) {
		t.Fatal("the upstream token leaked into the audit log")
	}
}

func runForwardLimit(t *testing.T, newStore func(t *testing.T) store.Store, up *Env) {
	e := NewEnv(t, newStore(t), Options{Loopback: true, ForwardPerMinute: 2})
	// Without a forwarding upstream, misses are plain misses and cost nothing.
	for range 5 {
		e.Expect(e.Do("GET", "/v1/skills/com.acme/nothing", "", "", nil), 404, "miss without forwarding")
	}
	e.Expect(e.upstream(map[string]any{"name": "hub", "kind": "openlinker-registry", "base_url": up.Base, "proxy_on_miss": true, "visibility": "public"}), 201, "upstream")
	e.Expect(e.Do("GET", "/v1/skills/com.acme/nothing-1", "", "", nil), 404, "first forwarded miss")
	e.Expect(e.Do("GET", "/v1/skills/com.acme/nothing-2", "", "", nil), 404, "second forwarded miss")
	r := e.Expect(e.Do("GET", "/v1/skills/com.acme/nothing-3", "", "", nil), 429, "third forwarded miss")
	if errCode(r) != "rate_limited" || r.Header.Get("Retry-After") == "" {
		t.Fatalf("rate limit response: %s %v", r.Body, r.Header)
	}
	// Entries that are already here are not limited.
	e.Expect(e.Do("GET", "/v1/skills", "", "", nil), 200, "local read")
}

// --- built-in pages ---

func runUI(t *testing.T, newStore func(t *testing.T) store.Store) {
	off := NewEnv(t, newStore(t), Options{Mode: "private"})
	off.Expect(off.Do("GET", "/ui/", "", "", nil), 404, "pages disabled")

	e := NewEnv(t, newStore(t), Options{Mode: "private", UI: true})
	e.namespace("internal.team", "internal")
	pub := e.publisher("internal.team")
	e.Expect(e.Do("POST", "/v1/skills/internal.team/xss/versions?version=1", pub, "text/markdown",
		md("xss", `<script>alert(1)</script> & "quotes"`, "")), 201, "skill")
	doc := serverJSON("internal.team/tools", "1.0.0", "http://tools.internal.example/mcp")
	doc["websiteUrl"] = "https://tools.internal.example/"
	e.Expect(e.publishMCP(pub, "internal.team", doc), 201, "mcp")

	r := e.Expect(e.Do("GET", "/", "", "", nil), 303, "root redirects to the pages")
	if r.Header.Get("Location") != "/registry/ui/" {
		t.Fatalf("location %q", r.Header.Get("Location"))
	}
	r = e.Expect(e.Do("GET", "/ui/", "", "", nil), 303, "anonymous visitor")
	if r.Header.Get("Location") != "/registry/ui/login" {
		t.Fatalf("location %q", r.Header.Get("Location"))
	}
	r = e.Expect(e.Do("GET", "/ui/login", "", "", nil), 200, "login form")
	if csp := r.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script") || strings.Contains(csp, "unsafe") {
		t.Fatalf("page CSP: %q", csp)
	}
	e.Expect(e.Do("GET", "/ui/static/style.css", "", "", nil), 200, "stylesheet")

	form := func(token string, headers map[string]string) Resp {
		e.Headers = headers
		defer func() { e.Headers = nil }()
		return e.Do("POST", "/ui/login", "", "application/x-www-form-urlencoded", "token="+url.QueryEscape(token))
	}
	same := map[string]string{"Sec-Fetch-Site": "same-origin"}
	e.Expect(form(pub, map[string]string{"Sec-Fetch-Site": "cross-site"}), 403, "cross-site login")
	e.Expect(form(pub, nil), 403, "login without an origin")
	e.Expect(form("olr_wrong_token", same), 401, "bad token")
	r = e.Expect(form(pub, same), 303, "login")
	cookie := r.Header.Get("Set-Cookie")
	for _, attr := range []string{"olr_session=", "Path=/registry/ui/", "HttpOnly", "SameSite=Strict"} {
		if !strings.Contains(cookie, attr) {
			t.Fatalf("session cookie lacks %s: %s", attr, cookie)
		}
	}
	session := map[string]string{"Cookie": strings.SplitN(cookie, ";", 2)[0], "Accept-Language": "zh-CN,zh;q=0.9"}
	page := func(path string) Resp {
		e.Headers = session
		defer func() { e.Headers = nil }()
		return e.Do("GET", path, "", "", nil)
	}
	r = e.Expect(page("/ui/"), 200, "skills page")
	body := string(r.Body)
	if !strings.Contains(body, "internal.team/") || !strings.Contains(body, "私有实例") {
		t.Fatalf("skills page: %s", body)
	}
	if strings.Contains(body, "<script>") || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal("entry text must be escaped")
	}
	r = e.Expect(page("/ui/skills/internal.team/xss"), 200, "skill page")
	if strings.Contains(string(r.Body), "<script>") || !strings.Contains(string(r.Body), "SKILL.md") {
		t.Fatal("skill page must show escaped content")
	}
	// Downloads work with the page session: the links stay under /ui/.
	if !strings.Contains(string(r.Body), `href="/registry/ui/skills/internal.team/xss/versions/1/artifact"`) {
		t.Fatalf("download link must point at the page copy of the route: %s", r.Body)
	}
	if dl := e.Expect(page("/ui/skills/internal.team/xss/versions/1/artifact"), 200, "download with the session"); !strings.Contains(string(dl.Body), "name: xss") {
		t.Fatalf("download: %s", dl.Body)
	}
	e.Expect(e.Do("GET", "/ui/skills/internal.team/xss/versions/1/artifact", "", "", nil), 303, "download without a session")
	e.Expect(page("/ui/skills/internal.team/nope"), 404, "missing skill page")
	e.Expect(page("/ui/mcp"), 200, "mcp list page")
	r = e.Expect(page("/ui/mcp/internal.team/tools"), 200, "mcp page")
	if !strings.Contains(string(r.Body), "tools.internal.example/mcp") || !strings.Contains(string(r.Body), "mcpServers") {
		t.Fatalf("mcp page: %s", r.Body)
	}
	e.Expect(page("/ui/upstreams"), 403, "upstreams page needs admin")
	// More entries than one page: the "next" link carries a usable cursor.
	for i := range 35 {
		e.Expect(e.publishMCP(pub, "internal.team", serverJSON(fmt.Sprintf("internal.team/bulk-%02d", i), "1.0.0", "http://bulk.internal.example/mcp")), 201, "bulk publish")
	}
	r = e.Expect(page("/ui/mcp"), 200, "first mcp page")
	next := regexp.MustCompile(`href="(/registry/ui/mcp\?cursor=[A-Za-z0-9_-]+)"`).FindStringSubmatch(string(r.Body))
	if next == nil || strings.Contains(string(r.Body), "internal.team/tools") {
		t.Fatalf("first page must link to the next one: %s", r.Body)
	}
	if r = e.Expect(page(strings.TrimPrefix(next[1], "/registry")), 200, "second mcp page"); !strings.Contains(string(r.Body), "internal.team/tools") ||
		strings.Contains(string(r.Body), "bulk-00") {
		t.Fatalf("second page: %s", r.Body)
	}

	// The session cookie works for pages only: the JSON API ignores it, so a
	// cross-site request cannot ride on it.
	e.Headers = session
	e.Expect(e.Do("GET", "/v1/skills", "", "", nil), 401, "API ignores the session cookie")
	e.Expect(e.Do("POST", "/ui/logout", "", "", nil), 403, "logout without an origin")
	e.Headers = map[string]string{"Cookie": session["Cookie"], "Sec-Fetch-Site": "same-origin"}
	r = e.Expect(e.Do("POST", "/ui/logout", "", "", nil), 303, "logout")
	e.Headers = nil
	if !strings.Contains(r.Header.Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout must clear the cookie: %s", r.Header.Get("Set-Cookie"))
	}
	// A stale cookie is "signed out", not an error.
	e.Headers = map[string]string{"Cookie": "olr_session=olr_stale_cookie_value"}
	e.Expect(e.Do("GET", "/ui/", "", "", nil), 303, "stale session")
	e.Headers = nil

	admin := form(AdminToken, same)
	e.Headers = map[string]string{"Cookie": strings.SplitN(admin.Header.Get("Set-Cookie"), ";", 2)[0]}
	e.Expect(e.Do("GET", "/ui/upstreams", "", "", nil), 200, "admin upstreams page")
	e.Headers = nil
}

// tarGz builds a gzip-compressed tar of the given files.
func tarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
