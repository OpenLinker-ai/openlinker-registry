package apitest

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

const bundleType = "application/vnd.openlinker.skill-bundle.v1+json"

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
		if r.JSON["applied"] != true {
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
		if got[coreUser] != ns || got[bobSub] != "ai.openlinker.bobby" || got[dave] != generatedNamespace(dave, 16) {
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
