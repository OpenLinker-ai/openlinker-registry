package apitest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

const (
	bundleType    = "application/vnd.openlinker.skill-bundle.v1+json"
	PlatformTerms = "LicenseRef-openlinker-platform-terms"
)

func coreBundle(name, description, body string) []byte {
	files, _ := json.Marshal(map[string]string{"SKILL.md": "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body + "\n"})
	return []byte(`{"required_commands":[],"name":"` + name + `","description":"` + description + `","files":` + string(files) + `,"capability_ids":[],"providers":["codex"]}`)
}

type projector struct {
	g   gatewayFixture
	sub string
}

func (p projector) send(method, path string, body []byte, ct string, acts ...string) Resp {
	if len(acts) == 0 {
		acts = []string{"read", "write", "project"}
	}
	return p.g.do(method, path, body, ct, p.g.assertion(method, path, body, func(x *Assertion) {
		x.Sub, x.GH, x.Act = p.sub, "", acts
	}))
}

func (p projector) concurrent(method, path string, body []byte) Resp {
	assertion := p.g.assertion(method, path, body, func(x *Assertion) {
		x.Sub, x.GH, x.Act = p.sub, "", []string{"read", "write", "project"}
	})
	req, _ := http.NewRequest(method, p.g.e.Base+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+p.g.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenLinker-User-Assertion", assertion)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return Resp{Status: 0, Body: []byte(err.Error())}
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := Resp{Status: res.StatusCode, Body: raw}
	_ = json.Unmarshal(raw, &out.JSON)
	return out
}

func (p projector) state(name string, doc map[string]any) Resp {
	body, _ := json.Marshal(doc)
	return p.send("PUT", "/v1/projections/skills/"+name, body, "application/json")
}

func (p projector) version(name, version string, bundle []byte, license string) Resp {
	q := ""
	if license != "" {
		q = "?license=" + url.QueryEscape(license)
	}
	return p.send("PUT", "/v1/projections/skills/"+name+"/versions/"+version+q, bundle, bundleType)
}

func generatedNamespace(sub string, n int) string {
	return "ai.openlinker.u" + strings.ReplaceAll(sub, "-", "")[:n]
}

func runProjection(t *testing.T, newStore func(t *testing.T) store.Store) {
	e := NewEnv(t, newStore(t), Options{Review: true})
	g := newGateway(t, e, "core", []string{"skills:read", "skills:write"}, []string{"openlinker"})
	plain := newGateway(t, e, "plain", []string{"skills:read", "skills:write"}, []string{"openlinker"})
	e.Expect(e.Do("PATCH", "/v1/admin/gateways/core", AdminToken, "", map[string]any{"projection": true}), 200, "projection gateway")
	alice := projector{g, coreUser}
	ns := generatedNamespace(coreUser, 12)
	entry := "/v1/skills/" + ns + "/p-tools"
	v1 := coreBundle("pdf-tools", "Read PDFs", "Use pdftotext.")
	v2 := coreBundle("pdf-tools", "Read and split PDFs", "Use qpdf.")

	t.Run("projection needs a projection gateway and the project action", func(t *testing.T) {
		if r := e.Expect(projector{plain, coreUser}.version("p-tools", "1", v1, ""), 403, "plain gateway"); errCode(r) != "projection_not_allowed" {
			t.Fatalf("plain: %s", r.Body)
		}
		e.Expect(alice.send("PUT", "/v1/projections/skills/p-tools/versions/1", v1, bundleType, "read", "write"), 401, "no project action")
		e.Expect(e.Do("PUT", "/v1/projections/skills/p-tools/versions/1", g.token, bundleType, v1), 403, "no user assertion")
		e.Expect(e.Do("PATCH", "/v1/admin/gateways/core", g.token, "", map[string]any{"projection": false}), 403, "only admins grant projection")
	})

	t.Run("versions are ingested hidden and idempotently", func(t *testing.T) {
		r := e.Expect(alice.version("p-tools", "1", v1, "LicenseRef-openlinker-platform-terms"), 201, "ingest v1")
		if r.JSON["namespace"] != ns || r.JSON["digest"] != sha(v1) {
			t.Fatalf("ingest: %s", r.Body)
		}
		e.Expect(alice.version("p-tools", "1", v1, "LicenseRef-openlinker-platform-terms"), 200, "repeat ingest")
		if r := e.Expect(alice.version("p-tools", "1", v2, "LicenseRef-openlinker-platform-terms"), 409, "different bytes"); errCode(r) != "version_exists" {
			t.Fatalf("different bytes: %s", r.Body)
		}
		e.Expect(e.Do("GET", entry, "", "", nil), 404, "hidden until a state arrives")
		r = e.Expect(e.Do("GET", entry+"/versions/1", AdminToken, "", nil), 200, "admin sees the hidden version")
		if lic := r.JSON["license"].(map[string]any); lic["kind"] != "custom" || lic["approved"] != true {
			t.Fatalf("platform terms: %s", r.Body)
		}
	})

	t.Run("states apply in revision order", func(t *testing.T) {
		r := e.Expect(alice.state("p-tools", map[string]any{"op": "11111111-1111-4111-8111-111111111111", "revision": 1, "phase": 1,
			"owner": map[string]any{"sub": coreUser}, "state": "visible", "listed": true, "versions": []string{"1"}, "current_version": "1",
			"display": map[string]any{"name": "PDF tools", "description": "Read PDFs"},
			"legacy":  map[string]any{"core_instance": "c0ffee00", "package_id": "6c3b"}}), 200, "visible")
		if r.JSON["applied"] != true || r.JSON["namespace"] != ns {
			t.Fatalf("applied: %s", r.Body)
		}
		r = e.Expect(e.Do("GET", entry, "", "", nil), 200, "public after the state")
		if r.JSON["review"] != nil || r.JSON["latest_version"] != "1" {
			t.Fatalf("projection is not reviewed: %s", r.Body)
		}
		meta, _ := r.JSON["_meta"].(map[string]any)
		if legacy, _ := meta["ai.openlinker/legacy"].(map[string]any); legacy["package_id"] != "6c3b" {
			t.Fatalf("legacy: %s", r.Body)
		}
		if !listed(t, e, "", "/ns/"+ns+"/.well-known/agent-skills/index.json", "skills", "p-tools") {
			t.Fatal("projected entry not discoverable")
		}
		e.Expect(alice.version("p-tools", "2", v2, "LicenseRef-openlinker-platform-terms"), 201, "ingest v2")
		withdraw := map[string]any{"op": "22222222-2222-4222-8222-222222222222", "revision": 2, "phase": 1, "owner": map[string]any{"sub": coreUser},
			"state": "visible", "listed": true, "versions": []string{"2"}, "current_version": "2", "display": map[string]any{"name": "PDF tools", "description": "Read and split PDFs"}}
		e.Expect(alice.state("p-tools", withdraw), 200, "withdraw v1")
		e.Expect(e.Do("GET", entry+"/versions/1", "", "", nil), 404, "withdrawn version")
		e.Expect(e.Do("GET", entry+"/versions/1/artifact", "", "", nil), 404, "withdrawn artifact")
		e.Expect(e.Do("GET", entry+"/versions/2", "", "", nil), 200, "kept version")
		if strings.Contains(string(e.Do("GET", entry, "", "", nil).Body), `"version":"1"`) {
			t.Fatal("withdrawn version listed")
		}
		r = e.Expect(alice.state("p-tools", map[string]any{"op": "33333333-3333-4333-8333-333333333333", "revision": 1, "phase": 1,
			"owner": map[string]any{"sub": coreUser}, "state": "visible", "listed": true, "versions": []string{"1", "2"}, "current_version": "2"}), 200, "stale state")
		if r.JSON["applied"] != false {
			t.Fatalf("stale applied: %s", r.Body)
		}
		e.Expect(e.Do("GET", entry+"/versions/1", "", "", nil), 404, "stale state changes nothing")
		r = e.Expect(alice.send("GET", "/v1/projections/skills/p-tools", nil, ""), 200, "projection state")
		if r.JSON["revision"] != float64(2) || r.JSON["hidden"] != false || r.JSON["current_version"] != "2" ||
			strings.Join(toStrings(r.JSON["versions"]), ",") != "2" || strings.Join(toStrings(r.JSON["ingested"]), ",") != "1,2" {
			t.Fatalf("projection state: %s", r.Body)
		}
		e.Expect(projector{g, otherUser}.send("GET", "/v1/projections/skills/p-tools", nil, ""), 404, "another owner's state")
		e.Expect(projector{plain, coreUser}.send("GET", "/v1/projections/skills/p-tools", nil, ""), 403, "plain gateway state")
		r = e.Expect(alice.state("p-tools", withdraw), 200, "replayed op")
		if r.JSON["applied"] != true {
			t.Fatalf("replay must return the first result: %s", r.Body)
		}
		missing := map[string]any{"op": "44444444-4444-4444-8444-444444444444", "revision": 3, "phase": 1, "owner": map[string]any{"sub": coreUser},
			"state": "visible", "listed": true, "versions": []string{"9"}, "current_version": "9"}
		if r := e.Expect(alice.state("p-tools", missing), 409, "unknown version"); errCode(r) != "version_missing" {
			t.Fatalf("missing: %s", r.Body)
		}
		bad := map[string]any{"op": "45454545-4545-4545-8545-454545454545", "revision": 3, "phase": 0, "owner": map[string]any{"sub": coreUser},
			"state": "visible", "listed": true, "versions": []string{"2"}, "current_version": "1"}
		e.Expect(alice.state("p-tools", bad), 422, "current version outside the list")
	})

	t.Run("hidden is stronger than any share or membership", func(t *testing.T) {
		shared := newGateway(t, e, "shared", []string{"skills:read"}, []string{"openlinker"})
		e.Expect(e.Do("POST", "/v1/admin/namespaces/"+ns+"/roles", AdminToken, "", map[string]any{"subject": "token:" + shared.tokenID, "role": "reader"}), 200, "share")
		member := e.token([]string{"skills:read"}, []map[string]string{{"namespace": ns, "role": "owner"}})
		e.Expect(alice.state("p-tools", map[string]any{"op": "55555555-5555-4555-8555-555555555555", "revision": 4, "phase": 0,
			"owner": map[string]any{"sub": coreUser}, "state": "hidden", "listed": false, "versions": []string{}}), 200, "hide")
		for _, p := range []string{entry, entry + "/versions/2", entry + "/versions/2/artifact", entry + "/versions/2/files/SKILL.md"} {
			e.Expect(e.Do("GET", p, "", "", nil), 404, "anonymous "+p)
			e.Expect(e.Do("GET", p, member, "", nil), 404, "member "+p)
			e.Expect(shared.do("GET", p, nil, "", ""), 404, "shared gateway "+p)
		}
		e.Expect(shared.do("GET", entry+"/versions/2/artifact?purpose=import", nil, "", ""), 404, "import read")
		e.Expect(e.Do("GET", entry, AdminToken, "", nil), 200, "admin")
		if listed(t, e, member, "/v1/skills?namespace="+ns, "skills", "p-tools") {
			t.Fatal("hidden entry listed")
		}
	})

	t.Run("managed entries accept only their own projection", func(t *testing.T) {
		writer := actor{plain, coreUser, ""}
		e.Expect(e.Do("POST", "/v1/admin/namespaces/"+ns+"/roles", AdminToken, "", map[string]any{"subject": "openlinker:plain:" + coreUser, "role": "owner"}), 200, "owner role")
		if r := e.Expect(writer.send("POST", entry+"/versions?version=9", md("p-tools", "Shadow", "MIT"), "text/markdown"), 409, "ordinary upload"); errCode(r) != "managed_by_core" {
			t.Fatalf("upload: %s", r.Body)
		}
		if r := e.Expect(writer.json("PATCH", entry, `{"listed":false}`), 409, "ordinary patch"); errCode(r) != "managed_by_core" {
			t.Fatalf("patch: %s", r.Body)
		}
		bob := projector{g, otherUser}
		if r := e.Expect(bob.version("p-tools", "3", v1, "LicenseRef-openlinker-platform-terms"), 201, "same name, other owner"); r.JSON["namespace"] == ns {
			t.Fatalf("other owner landed in alice's namespace: %s", r.Body)
		}
	})

	t.Run("tombstones keep late requests out", func(t *testing.T) {
		tomb := "/v1/skills/" + ns + "/p-tomb"
		e.Expect(alice.state("p-tomb", map[string]any{"op": "66666666-6666-4666-8666-666666666666", "revision": 5, "phase": 0,
			"owner": map[string]any{"sub": coreUser}, "state": "hidden", "listed": false, "versions": []string{}}), 200, "tombstone")
		e.Expect(alice.version("p-tomb", "1", v1, "LicenseRef-openlinker-platform-terms"), 201, "late ingest")
		r := e.Expect(alice.state("p-tomb", map[string]any{"op": "77777777-7777-4777-8777-777777777777", "revision": 4, "phase": 1,
			"owner": map[string]any{"sub": coreUser}, "state": "visible", "listed": true, "versions": []string{"1"}, "current_version": "1"}), 200, "late visible state")
		if r.JSON["applied"] != false {
			t.Fatalf("late state applied: %s", r.Body)
		}
		e.Expect(e.Do("GET", tomb, "", "", nil), 404, "tombstone stays hidden")
	})

	t.Run("projection never clears moderation", func(t *testing.T) {
		visible := func(rev int, op string) map[string]any {
			return map[string]any{"op": op, "revision": rev, "phase": 1, "owner": map[string]any{"sub": coreUser},
				"state": "visible", "listed": true, "versions": []string{"2"}, "current_version": "2"}
		}
		e.Expect(alice.state("p-tools", visible(6, "88888888-8888-4888-8888-888888888888")), 200, "visible again")
		e.Expect(e.Do("GET", entry, "", "", nil), 200, "visible")
		e.Expect(e.decide("skills", ns+"/p-tools", map[string]any{"decision": "reject", "reason": "malware"}), 200, "reject")
		e.Expect(alice.state("p-tools", visible(7, "99999999-9999-4999-8999-999999999999")), 200, "later projection")
		e.Expect(e.Do("GET", entry, "", "", nil), 404, "rejection holds")
	})

	t.Run("mcp states", func(t *testing.T) {
		doc := map[string]any{"op": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "revision": 1, "phase": 1, "owner": map[string]any{"sub": coreUser},
			"state": "visible", "listed": true, "version": "0.1.1",
			"server":   map[string]any{"description": "Weather", "remotes": []map[string]any{{"type": "streamable-http", "url": "https://openlinker.example/mcp/agents/1"}}},
			"metadata": map[string]any{"homepage": "https://example.com"}, "metadata_revision": 1, "legacy": map[string]any{"agent_id": "1"}}
		body, _ := json.Marshal(doc)
		e.Expect(alice.send("PUT", "/v1/projections/mcp/m-weather", body, "application/json"), 200, "mcp visible")
		name := url.PathEscape(ns + "/m-weather")
		e.Expect(e.Do("GET", "/v0.1/servers/"+name+"/versions/latest", "", "", nil), 200, "official read")
		doc["op"], doc["revision"], doc["phase"], doc["state"] = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", 2, 0, "hidden"
		body, _ = json.Marshal(doc)
		e.Expect(alice.send("PUT", "/v1/projections/mcp/m-weather", body, "application/json"), 200, "mcp hidden")
		e.Expect(e.Do("GET", "/v0.1/servers/"+name+"/versions/latest", "", "", nil), 404, "hidden official read")
		e.Expect(e.Do("GET", "/v1/mcp/servers/"+ns+"/m-weather", "", "", nil), 404, "hidden native read")
		r := e.Expect(alice.send("GET", "/v1/projections/mcp/m-weather", nil, ""), 200, "mcp projection state")
		if r.JSON["revision"] != float64(2) || r.JSON["hidden"] != true || r.JSON["version"] != "0.1.1" {
			t.Fatalf("mcp projection state: %s", r.Body)
		}
	})

	t.Run("a later handle claim does not move projections", func(t *testing.T) {
		e.Expect(actor{g, coreUser, ""}.json("POST", "/v1/namespaces", `{"name":"ai.openlinker.alicehandle"}`), 201, "alice handle")
		r := e.Expect(alice.state("p-tools", map[string]any{"op": "c1c1c1c1-c1c1-4c1c-8c1c-c1c1c1c1c1c1", "revision": 8, "phase": 0,
			"owner": map[string]any{"sub": coreUser}, "state": "hidden", "listed": false, "versions": []string{}}), 200, "withdraw after the claim")
		if r.JSON["namespace"] != ns || r.JSON["applied"] != true {
			t.Fatalf("projection moved: %s", r.Body)
		}
		if r := e.Expect(alice.version("p-after", "1", v1, "LicenseRef-openlinker-platform-terms"), 201, "new entry after the claim"); r.JSON["namespace"] != ns {
			t.Fatalf("new entry moved: %s", r.Body)
		}
	})

	t.Run("a visible state for a missing entry leaves a tombstone", func(t *testing.T) {
		state := func(rev int, op string) map[string]any {
			return map[string]any{"op": op, "revision": rev, "phase": 1, "owner": map[string]any{"sub": coreUser},
				"state": "visible", "listed": true, "versions": []string{"1"}, "current_version": "1"}
		}
		if r := e.Expect(alice.state("p-tomb2", state(5, "c2c2c2c2-c2c2-4c2c-8c2c-c2c2c2c2c2c2")), 409, "versions not ingested"); errCode(r) != "version_missing" {
			t.Fatalf("missing: %s", r.Body)
		}
		e.Expect(alice.version("p-tomb2", "1", v1, "LicenseRef-openlinker-platform-terms"), 201, "late ingest")
		if r := e.Expect(alice.state("p-tomb2", state(4, "c3c3c3c3-c3c3-4c3c-8c3c-c3c3c3c3c3c3")), 200, "older state"); r.JSON["applied"] != false {
			t.Fatalf("older state applied: %s", r.Body)
		}
		e.Expect(e.Do("GET", "/v1/skills/"+ns+"/p-tomb2", "", "", nil), 404, "still hidden")
		e.Expect(e.Do("GET", "/v1/skills/"+ns+"/p-tomb2", AdminToken, "", nil), 200, "tombstone exists")
	})

	t.Run("concurrent replays of one op agree", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{"op": "c4c4c4c4-c4c4-4c4c-8c4c-c4c4c4c4c4c4", "revision": 1, "phase": 0,
			"owner": map[string]any{"sub": coreUser}, "state": "hidden", "listed": false, "versions": []string{}})
		mcpBody, _ := json.Marshal(map[string]any{"op": "c5c5c5c5-c5c5-4c5c-8c5c-c5c5c5c5c5c5", "revision": 1, "phase": 0,
			"owner": map[string]any{"sub": coreUser}, "state": "hidden", "listed": false})
		for _, c := range []struct {
			path string
			body []byte
		}{{"/v1/projections/skills/p-race", body}, {"/v1/projections/mcp/m-race", mcpBody}} {
			var wg sync.WaitGroup
			results := make([]Resp, 8)
			for i := range results {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					results[i] = alice.concurrent("PUT", c.path, c.body)
				}(i)
			}
			wg.Wait()
			for _, r := range results {
				if r.Status != 200 || r.JSON["applied"] != true {
					t.Fatalf("%s replay: %d %s", c.path, r.Status, r.Body)
				}
			}
		}
	})

	t.Run("limits follow bundle admission and the contract", func(t *testing.T) {
		state := func(op, description string, legacy map[string]any) map[string]any {
			return map[string]any{"op": op, "revision": 9, "phase": 0, "owner": map[string]any{"sub": coreUser}, "state": "hidden",
				"listed": false, "versions": []string{}, "display": map[string]any{"name": "Long", "description": description}, "legacy": legacy}
		}
		e.Expect(alice.state("p-long", state("c6c6c6c6-c6c6-4c6c-8c6c-c6c6c6c6c6c6", strings.Repeat("é", 2000), nil)), 200, "2000 characters")
		e.Expect(alice.state("p-long", state("c7c7c7c7-c7c7-4c7c-8c7c-c7c7c7c7c7c7", strings.Repeat("é", 2001), nil)), 422, "2001 characters")
		e.Expect(alice.state("p-long", state("c8c8c8c8-c8c8-4c8c-8c8c-c8c8c8c8c8c8", "ok", map[string]any{"x": strings.Repeat("a", 4100)})), 422, "legacy over 4 KiB")
		e.Expect(alice.state("p-long", state("cbcbcbcb-cbcb-4bcb-8bcb-cbcbcbcbcbcb", "ok", map[string]any{"x": strings.Repeat("a", 4088)})), 200, "legacy of exactly 4 KiB")
		long := []byte(`{"required_commands":[],"name":"pdf-tools","description":"Read PDFs","files":{"SKILL.md":"---\nname: pdf-tools\ndescription: Read PDFs\n---\nUse it.\n","` + strings.Repeat("a", 101) + `":"x"},"capability_ids":[],"providers":["codex"]}`)
		if r := e.Expect(alice.send("POST", "/v1/projections/validate", long, bundleType), 200, "long path"); r.JSON["ok"] != false {
			t.Fatalf("long path validated: %s", r.Body)
		}
		mismatch := []byte(`{"required_commands":[],"name":"pdf-tools","description":"Something else","files":{"SKILL.md":"---\nname: pdf-tools\ndescription: Read PDFs\n---\nUse it.\n"},"capability_ids":[],"providers":["codex"]}`)
		r := e.Expect(alice.send("POST", "/v1/projections/validate", mismatch, bundleType), 200, "mismatched bundle")
		if r.JSON["ok"] != false || r.JSON["code"] != "bundle_invalid" {
			t.Fatalf("mismatch: %s", r.Body)
		}
		if r := e.Expect(alice.version("p-mismatch", "1", mismatch, "LicenseRef-openlinker-platform-terms"), 422, "ingest refuses the same bundle"); errCode(r) != "bundle_invalid" {
			t.Fatalf("ingest mismatch: %s", r.Body)
		}
	})

	t.Run("managed entries refuse ordinary writes, admins included", func(t *testing.T) {
		vis := "/v1/skills/" + ns + "/p-vis"
		e.Expect(alice.version("p-vis", "1", v1, "LicenseRef-openlinker-platform-terms"), 201, "ingest")
		e.Expect(alice.state("p-vis", map[string]any{"op": "c9c9c9c9-c9c9-4c9c-8c9c-c9c9c9c9c9c9", "revision": 1, "phase": 1,
			"owner": map[string]any{"sub": coreUser}, "state": "visible", "listed": true, "versions": []string{"1"}, "current_version": "1",
			"display": map[string]any{"name": "PDF tools", "description": "Read PDFs"}, "legacy": map[string]any{"package_id": "vis"}}), 200, "visible")
		if r := e.Expect(e.Do("PATCH", vis, AdminToken, "", map[string]any{"listed": false}), 409, "admin patch"); errCode(r) != "managed_by_core" {
			t.Fatalf("admin patch: %s", r.Body)
		}
		claimer := actor{plain, thirdUser, ""}
		if r := e.Expect(claimer.json("POST", vis+"/claim", `{}`), 409, "claim"); errCode(r) != "managed_by_core" {
			t.Fatalf("claim: %s", r.Body)
		}
		writer := actor{plain, coreUser, ""}
		if r := e.Expect(writer.json("POST", "/v1/skills/"+ns+"/indexes", `{"url":"https://github.com/acme/skills/tree/main/skills/demo","commit":"`+C1+`","name":"p-vis"}`), 409, "index"); errCode(r) != "managed_by_core" {
			t.Fatalf("index: %s", r.Body)
		}
		r := e.Expect(e.Do("GET", "/v1/skills?namespace="+ns, AdminToken, "", nil), 200, "listing")
		if strings.Contains(string(r.Body), "ai.openlinker/legacy") {
			t.Fatalf("legacy in a listing: %s", r.Body)
		}
		if r := e.Expect(e.Do("PUT", "/v1/admin/skills/"+ns+"/p-vis/alias", AdminToken, "", map[string]any{"alias": "pdf"}), 409, "admin alias"); errCode(r) != "managed_by_core" {
			t.Fatalf("alias: %s", r.Body)
		}
		r = e.Expect(e.Do("GET", vis, "", "", nil), 200, "detail")
		if r.JSON["display_name"] != "PDF tools" {
			t.Fatalf("display name: %s", r.Body)
		}
		if !strings.Contains(string(r.Body), "ai.openlinker/legacy") {
			t.Fatalf("legacy missing from the detail: %s", r.Body)
		}
		e.Expect(e.decide("skills", ns+"/p-vis", map[string]any{"decision": "reject", "reason": "moderation"}), 200, "moderation stays possible")
	})

	t.Run("mcp state is atomic and keeps its metadata", func(t *testing.T) {
		name := url.PathEscape(ns + "/m-weather")
		stale, _ := json.Marshal(map[string]any{"op": "cacacaca-caca-4aca-8aca-cacacacacaca", "revision": 1, "phase": 1, "owner": map[string]any{"sub": coreUser},
			"state": "visible", "listed": true, "version": "0.1.9",
			"server": map[string]any{"description": "Weather", "remotes": []map[string]any{{"type": "streamable-http", "url": "https://openlinker.example/mcp/agents/1"}}}})
		if r := e.Expect(alice.send("PUT", "/v1/projections/mcp/m-weather", stale, "application/json"), 200, "stale visible"); r.JSON["applied"] != false {
			t.Fatalf("stale applied: %s", r.Body)
		}
		e.Expect(e.Do("GET", "/v1/mcp/servers/"+ns+"/m-weather/versions/0.1.9", AdminToken, "", nil), 404, "stale version not written")
		e.Expect(e.Do("GET", "/v0.1/servers/"+name+"/versions/0.1.9", AdminToken, "", nil), 404, "stale version not written, official API")
		r := e.Expect(e.Do("GET", "/v1/mcp/servers/"+ns+"/m-weather", AdminToken, "", nil), 200, "detail")
		meta, _ := r.JSON["_meta"].(map[string]any)
		if md, _ := meta["ai.openlinker/metadata"].(map[string]any); md["homepage"] != "https://example.com" || meta["ai.openlinker/metadata_revision"] != float64(1) {
			t.Fatalf("metadata: %s", r.Body)
		}
		if legacy, _ := meta["ai.openlinker/legacy"].(map[string]any); legacy["agent_id"] != "1" {
			t.Fatalf("legacy: %s", r.Body)
		}
		nullServer, _ := json.Marshal(map[string]any{"op": "cececece-cece-4ece-8ece-cececececece", "revision": 9, "phase": 1, "owner": map[string]any{"sub": coreUser},
			"state": "visible", "listed": true, "version": "0.1.9", "server": nil})
		if r := e.Expect(alice.send("PUT", "/v1/projections/mcp/m-weather", nullServer, "application/json"), 422, "null server"); errCode(r) != "server_invalid" {
			t.Fatalf("null server: %s", r.Body)
		}
		e.Expect(e.decide("mcp/servers", ns+"/m-weather", map[string]any{"decision": "reject", "reason": "check"}), 200, "reject")
		before := e.Expect(e.Do("GET", "/v1/mcp/servers/"+ns+"/m-weather", AdminToken, "", nil), 200, "rejected detail").JSON["review"].(map[string]any)["revision"]
		next, _ := json.Marshal(map[string]any{"op": "cdcdcdcd-cdcd-4dcd-8dcd-cdcdcdcdcdcd", "revision": 10, "phase": 1, "owner": map[string]any{"sub": coreUser},
			"state": "visible", "listed": true, "version": "0.2.0",
			"server": map[string]any{"description": "Weather v2", "remotes": []map[string]any{{"type": "streamable-http", "url": "https://openlinker.example/mcp/agents/1"}}}})
		e.Expect(alice.send("PUT", "/v1/projections/mcp/m-weather", next, "application/json"), 200, "new version")
		after := e.Expect(e.Do("GET", "/v1/mcp/servers/"+ns+"/m-weather", AdminToken, "", nil), 200, "detail after").JSON["review"].(map[string]any)
		if after["revision"] != before.(float64)+1 || after["state"] != "rejected" {
			t.Fatalf("review after a projected version: before %v after %v", before, after)
		}
	})

	t.Run("validate and namespace preflight", func(t *testing.T) {
		r := e.Expect(alice.send("POST", "/v1/projections/validate", v1, bundleType), 200, "valid")
		if r.JSON["ok"] != true {
			t.Fatalf("valid: %s", r.Body)
		}
		bad := []byte(`{"required_commands":[],"name":"x","description":"x","files":{"SKILL.md":"no frontmatter","a.txt":"1","A.txt":"2"},"capability_ids":[],"providers":["codex"]}`)
		r = e.Expect(alice.send("POST", "/v1/projections/validate", bad, bundleType), 200, "invalid")
		if r.JSON["ok"] != false || r.JSON["code"] == "" {
			t.Fatalf("invalid: %s", r.Body)
		}
		bobSub, dave := otherUser, "d4d4d4d4-1234-4abc-8def-0123456789ab"
		e.Expect(actor{g, bobSub, ""}.json("POST", "/v1/namespaces", `{"name":"ai.openlinker.bobby"}`), 201, "bob handle")
		e.Expect(e.Do("POST", "/v1/admin/namespaces", AdminToken, "", map[string]any{"name": generatedNamespace(dave, 12), "kind": "openlinker"}), 201, "pre-existing name")
		subjects, _ := json.Marshal(map[string]any{"subjects": []string{coreUser, bobSub, dave}})
		r = e.Expect(alice.send("POST", "/v1/projections/namespaces/preflight", subjects, "application/json"), 200, "preflight")
		got := map[string]string{}
		for _, it := range r.JSON["namespaces"].([]any) {
			m := it.(map[string]any)
			got[m["sub"].(string)] = m["namespace"].(string)
		}
		if got[coreUser] != ns || got[bobSub] != generatedNamespace(bobSub, 12) || got[dave] != generatedNamespace(dave, 16) {
			t.Fatalf("preflight: %s", r.Body)
		}
		carol := actor{g, thirdUser, ""}
		if r := e.Expect(carol.json("POST", "/v1/namespaces", `{"name":"`+generatedNamespace(coreUser, 12)+`"}`), 422, "generated names are reserved"); errCode(r) != "namespace_not_claimable" {
			t.Fatalf("reserved: %s", r.Body)
		}
	})
}

func toStrings(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, x := range list {
		out = append(out, x.(string))
	}
	return out
}

func projectionEnv(t *testing.T, st store.Store, o Options) (*Env, projector) {
	e := NewEnv(t, st, o)
	g := newGateway(t, e, "core", []string{"skills:read", "skills:write"}, []string{"openlinker"})
	e.Expect(e.Do("PATCH", "/v1/admin/gateways/core", AdminToken, "", map[string]any{"projection": true}), 200, "projection gateway")
	return e, projector{g, coreUser}
}

func visibleState(op string, rev int, versions ...string) map[string]any {
	return map[string]any{"op": op, "revision": rev, "phase": 1, "owner": map[string]any{"sub": coreUser}, "state": "visible",
		"listed": true, "versions": versions, "current_version": versions[len(versions)-1]}
}

func runProjectionReads(t *testing.T, newStore func(t *testing.T) store.Store) {
	const marker = "withdrawn-0123456789"
	t.Run("a page that loads a version withdrawn meanwhile shows nothing of it", func(t *testing.T) {
		h := &hookStore{Store: newStore(t), hooks: map[string]func(){}}
		e, alice := projectionEnv(t, h, Options{UI: true})
		e.Expect(alice.version("p-split", "1", coreBundle("pdf-tools", "Read PDFs", "Use it."), PlatformTerms), 201, "v1")
		e.Expect(alice.version("p-split", "2", coreBundle("pdf-tools", marker, "Use it."), PlatformTerms), 201, "v2")
		e.Expect(alice.state("p-split", visibleState("d1d1d1d1-d1d1-4d1d-8d1d-d1d1d1d1d1d1", 1, "1", "2")), 200, "visible")
		h.before("GetVersion", func() {
			e.Expect(alice.state("p-split", visibleState("d2d2d2d2-d2d2-4d2d-8d2d-d2d2d2d2d2d2", 2, "1")), 200, "withdraw v2")
		})
		path := "/ui/skills/" + generatedNamespace(coreUser, 12) + "/p-split/versions/2"
		r := e.Do("GET", path, "", "", nil)
		if h.waiting("GetVersion") {
			t.Fatal("the page never read the version; the case tests nothing")
		}
		if r.Status != 404 || strings.Contains(string(r.Body), marker) {
			t.Fatalf("withdrawn version rendered: %d %s", r.Status, r.Body)
		}
	})

	t.Run("mirrors follow withdrawn and restored versions", func(t *testing.T) {
		up, alice := projectionEnv(t, newStore(t), Options{})
		down := NewEnv(t, newStore(t), Options{Loopback: true})
		ns := generatedNamespace(coreUser, 12)
		up.Expect(alice.version("p-mirror", "1", coreBundle("pdf-tools", "Read PDFs", "Use it."), PlatformTerms), 201, "v1")
		up.Expect(alice.version("p-mirror", "2", coreBundle("pdf-tools", "Read and split PDFs", "Use it."), PlatformTerms), 201, "v2")
		up.Expect(alice.state("p-mirror", visibleState("d3d3d3d3-d3d3-4d3d-8d3d-d3d3d3d3d3d3", 1, "1", "2")), 200, "visible")
		down.Expect(down.upstream(map[string]any{"name": "hub", "kind": "openlinker-registry", "base_url": up.Base}), 201, "upstream")
		v1 := "/v1/skills/" + ns + "/p-mirror/versions/1"
		down.sync("hub")
		reader := ""
		down.Expect(down.Do("GET", v1, reader, "", nil), 200, "mirrored v1")
		up.Expect(alice.state("p-mirror", visibleState("d4d4d4d4-d4d4-4d4d-8d4d-d4d4d4d4d4d4", 2, "2")), 200, "withdraw v1")
		down.sync("hub")
		down.Expect(down.Do("GET", v1, reader, "", nil), 404, "withdrawn upstream")
		down.Expect(down.Do("GET", v1+"/artifact", reader, "", nil), 404, "withdrawn artifact")
		down.Expect(down.Do("GET", "/v1/skills/"+ns+"/p-mirror/versions/2", reader, "", nil), 200, "kept version")
		up.Expect(alice.state("p-mirror", visibleState("d5d5d5d5-d5d5-4d5d-8d5d-d5d5d5d5d5d5", 3, "1", "2")), 200, "restore v1")
		down.sync("hub")
		down.Expect(down.Do("GET", v1, reader, "", nil), 200, "restored upstream")
	})

	t.Run("a mirror backfills an older version restored upstream", func(t *testing.T) {
		up, alice := projectionEnv(t, newStore(t), Options{})
		down := NewEnv(t, newStore(t), Options{Loopback: true})
		ns := generatedNamespace(coreUser, 12)
		up.Expect(alice.version("p-back", "1", coreBundle("pdf-tools", "Read PDFs", "Use it."), PlatformTerms), 201, "v1")
		up.Expect(alice.version("p-back", "2", coreBundle("pdf-tools", "Read and split PDFs", "Use it."), PlatformTerms), 201, "v2")
		up.Expect(alice.state("p-back", visibleState("e1e1e1e1-e1e1-4e1e-8e1e-e1e1e1e1e1e1", 1, "2")), 200, "only v2")
		down.Expect(down.upstream(map[string]any{"name": "hub", "kind": "openlinker-registry", "base_url": up.Base}), 201, "upstream")
		down.sync("hub")
		entry := "/v1/skills/" + ns + "/p-back"
		down.Expect(down.Do("GET", entry+"/versions/1", "", "", nil), 404, "v1 not mirrored yet")
		up.Expect(alice.state("p-back", visibleState("e2e2e2e2-e2e2-4e2e-8e2e-e2e2e2e2e2e2", 2, "1", "2")), 200, "restore v1")
		down.sync("hub")
		down.Expect(down.Do("GET", entry+"/versions/1", "", "", nil), 200, "v1 backfilled")
		if r := down.Expect(down.Do("GET", entry, "", "", nil), 200, "detail"); r.JSON["latest_version"] != "2" {
			t.Fatalf("latest after backfill: %s", r.Body)
		}
		down.sync("hub")
		down.Expect(down.Do("GET", entry+"/versions/1", "", "", nil), 200, "stable after another sync")
	})

	t.Run("successive backfills keep the upstream's latest version", func(t *testing.T) {
		up, alice := projectionEnv(t, newStore(t), Options{})
		down := NewEnv(t, newStore(t), Options{Loopback: true})
		ns := generatedNamespace(coreUser, 12)
		for _, v := range []string{"1", "2", "3"} {
			up.Expect(alice.version("p-steps", v, coreBundle("pdf-tools", "Read PDFs v"+v, "Use it."), PlatformTerms), 201, "v"+v)
		}
		up.Expect(alice.state("p-steps", visibleState("f1f1f1f1-f1f1-4f1f-8f1f-f1f1f1f1f1f1", 1, "3")), 200, "only v3")
		down.Expect(down.upstream(map[string]any{"name": "hub", "kind": "openlinker-registry", "base_url": up.Base}), 201, "upstream")
		down.sync("hub")
		up.Expect(alice.state("p-steps", visibleState("f2f2f2f2-f2f2-4f2f-8f2f-f2f2f2f2f2f2", 2, "2", "3")), 200, "restore v2")
		down.sync("hub")
		up.Expect(alice.state("p-steps", visibleState("f3f3f3f3-f3f3-4f3f-8f3f-f3f3f3f3f3f3", 3, "1", "2", "3")), 200, "restore v1")
		down.sync("hub")
		entry := "/v1/skills/" + ns + "/p-steps"
		down.Expect(down.Do("GET", entry+"/versions/1", "", "", nil), 200, "v1 backfilled")
		if r := down.Expect(down.Do("GET", entry, "", "", nil), 200, "detail"); r.JSON["latest_version"] != "3" {
			t.Fatalf("latest after successive backfills: %s", r.Body)
		}
	})

	t.Run("validation needs a public instance", func(t *testing.T) {
		e := NewEnv(t, newStore(t), Options{Mode: "private"})
		g := newGateway(t, e, "core", []string{"skills:read", "skills:write"}, []string{"openlinker"})
		e.Do("PATCH", "/v1/admin/gateways/core", AdminToken, "", map[string]any{"projection": true})
		r := projector{g, coreUser}.send("POST", "/v1/projections/validate", coreBundle("pdf-tools", "Read PDFs", "Use it."), bundleType)
		if r.Status != 422 || errCode(r) != "projection_public_only" {
			t.Fatalf("private validate: %d %s", r.Status, r.Body)
		}
	})
}
