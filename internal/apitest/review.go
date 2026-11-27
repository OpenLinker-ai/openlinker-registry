package apitest

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

const thirdUser = "c9f0f895-fb98-4b91-a0b4-6d0e1f2a3b44"

type actor struct {
	g   gatewayFixture
	sub string
	gh  string
}

func (a actor) send(method, path string, body []byte, ct string) Resp {
	return a.g.do(method, path, body, ct, a.g.assertion(method, path, body, func(x *Assertion) {
		x.Sub, x.GH, x.Act = a.sub, a.gh, []string{"read", "write"}
	}))
}

func (a actor) json(method, path string, v string) Resp {
	return a.send(method, path, []byte(v), "application/json")
}

func (a actor) get(path string) Resp {
	return a.g.read(path, func(x *Assertion) { x.Sub, x.GH = a.sub, a.gh })
}

func (e *Env) decide(kind, entry string, body map[string]any) Resp {
	e.T.Helper()
	read := "/v1/skills/" + entry
	if kind == "mcp/servers" {
		read = "/v1/mcp/servers/" + entry
	}
	r := e.Expect(e.Do("GET", read, AdminToken, "", nil), 200, "admin reads "+entry)
	if review, ok := r.JSON["review"].(map[string]any); ok {
		body["revision"] = review["revision"]
	}
	return e.Do("POST", "/v1/admin/review/"+kind+"/"+entry, AdminToken, "", body)
}

func reviewState(r Resp) string {
	m, _ := r.JSON["review"].(map[string]any)
	if m == nil {
		return "approved"
	}
	s, _ := m["state"].(string)
	return s
}

func listed(t *testing.T, e *Env, token, path, _, want string) bool {
	t.Helper()
	r := e.Expect(e.Do("GET", path, token, "", nil), 200, "list "+path)
	return strings.Contains(string(r.Body), want)
}

func runReview(t *testing.T, newStore func(t *testing.T) store.Store) {
	e := NewEnv(t, newStore(t), Options{Review: true, ReportThreshold: 2})
	g := newGateway(t, e, "core", []string{"skills:read", "skills:write"}, []string{"openlinker", "github"})
	alice := actor{g, coreUser, ""}
	bob := actor{g, otherUser, ""}
	carol := actor{g, thirdUser, ""}

	t.Run("namespace self-service", func(t *testing.T) {
		r := e.Expect(alice.json("POST", "/v1/namespaces", `{"name":"ai.openlinker.alice"}`), 201, "claim handle")
		if r.JSON["name"] != "ai.openlinker.alice" || r.JSON["kind"] != "openlinker" {
			t.Fatalf("claim: %s", r.Body)
		}
		e.Expect(alice.json("POST", "/v1/namespaces", `{"name":"ai.openlinker.alice"}`), 200, "claim again")
		if r := e.Expect(alice.json("POST", "/v1/namespaces", `{"name":"ai.openlinker.alice2"}`), 409, "second handle"); errCode(r) != "handle_already_chosen" {
			t.Fatalf("second handle: %s", r.Body)
		}
		if r := e.Expect(bob.json("POST", "/v1/namespaces", `{"name":"ai.openlinker.alice"}`), 409, "taken"); errCode(r) != "namespace_exists" {
			t.Fatalf("taken: %s", r.Body)
		}
		for _, bad := range []string{"ai.openlinker.admin", "ai.openlinker.openlinker", "ai.openlinker.ab", "ai.openlinker.-bob", "ai.openlinker.Bob", "com.example", "internal.team"} {
			if r := e.Expect(bob.json("POST", "/v1/namespaces", `{"name":"`+bad+`"}`), 422, bad); errCode(r) != "namespace_not_claimable" {
				t.Fatalf("%s: %s", bad, r.Body)
			}
		}
		read := bob.g.do("POST", "/v1/namespaces", []byte(`{"name":"ai.openlinker.bob"}`), "application/json",
			bob.g.assertion("POST", "/v1/namespaces", []byte(`{"name":"ai.openlinker.bob"}`), func(x *Assertion) { x.Sub = otherUser }))
		e.Expect(read, 401, "assertion without write")
		if r := e.Expect(g.do("POST", "/v1/namespaces", []byte(`{"name":"ai.openlinker.gw"}`), "application/json", ""), 403, "gateway alone"); errCode(r) != "user_required" {
			t.Fatalf("gateway alone: %s", r.Body)
		}
		e.Expect(e.Do("POST", "/v1/namespaces", "", "", map[string]any{"name": "ai.openlinker.anon"}), 401, "anonymous")
		e.Expect(bob.json("POST", "/v1/namespaces", `{"name":"ai.openlinker.bob"}`), 201, "bob handle")
		e.Expect(carol.json("POST", "/v1/namespaces", `{"name":"ai.openlinker.carol"}`), 201, "carol handle")
		r = e.Expect(alice.get("/v1/whoami"), 200, "whoami")
		if !strings.Contains(string(r.Body), "ai.openlinker.alice") || !strings.Contains(string(r.Body), coreUser) {
			t.Fatalf("whoami: %s", r.Body)
		}
		onlyOL := newGateway(t, e, "ghonly", []string{"skills:read", "skills:write"}, []string{"github"})
		dave := actor{onlyOL, thirdUser, "5"}
		if r := e.Expect(dave.json("POST", "/v1/namespaces", `{"name":"ai.openlinker.dave"}`), 403, "gateway without openlinker identity"); errCode(r) != "identity_not_allowed" {
			t.Fatalf("identity: %s", r.Body)
		}
	})

	first := "/v1/skills/ai.openlinker.alice/first"
	t.Run("new namespace waits for review", func(t *testing.T) {
		r := e.Expect(alice.send("POST", first+"/versions?version=1.0.0", md("first", "First", "MIT"), "text/markdown"), 201, "publish")
		if reviewState(r) != "pending" {
			t.Fatalf("publish review: %s", r.Body)
		}
		hidden := []string{first, first + "/versions/1.0.0", first + "/versions/1.0.0/artifact",
			first + "/versions/1.0.0/artifact?purpose=import", first + "/versions/1.0.0/files/SKILL.md"}
		for _, p := range hidden {
			anon := 404
			if strings.Contains(p, "purpose=import") {
				anon = 403
			}
			e.Expect(e.Do("GET", p, "", "", nil), anon, "anonymous "+p)
			e.Expect(g.do("GET", p, nil, "", ""), 404, "gateway "+p)
			e.Expect(bob.get(p), 404, "other user "+p)
		}
		published := r.JSON["review"].(map[string]any)["revision"]
		r = e.Expect(alice.get(first), 200, "member reads")
		if r.JSON["review"].(map[string]any)["revision"] != published {
			t.Fatalf("publish answered a stale revision %v, entry has %s", published, r.Body)
		}
		if reviewState(r) != "pending" || r.JSON["visibility"] != "private" {
			t.Fatalf("member view: %s", r.Body)
		}
		if review := r.JSON["review"].(map[string]any); review["visibility"] != "public" {
			t.Fatalf("requested visibility: %s", r.Body)
		}
		if listed(t, e, "", "/v1/skills", "skills", "ai.openlinker.alice") || listed(t, e, "", "/ns/ai.openlinker.alice/.well-known/agent-skills/index.json", "skills", "first") {
			t.Fatal("pending entry listed")
		}
		r = e.Expect(e.Do("GET", "/v1/admin/review?state=pending", AdminToken, "", nil), 200, "queue")
		if !strings.Contains(string(r.Body), `"ai.openlinker.alice"`) {
			t.Fatalf("queue: %s", r.Body)
		}
		e.Expect(e.Do("GET", "/v1/admin/review", "", "", nil), 401, "anonymous queue")
		e.Expect(alice.json("POST", "/v1/admin/review/skills/ai.openlinker.alice/first", `{"decision":"approve"}`), 403, "user cannot approve")
		e.Expect(e.Do("POST", "/v1/admin/review/skills/ai.openlinker.alice/first", AdminToken, "", map[string]any{"decision": "maybe", "revision": 1}), 422, "bad decision")
		if r := e.Expect(e.Do("POST", "/v1/admin/review/skills/ai.openlinker.alice/first", AdminToken, "", map[string]any{"decision": "approve", "revision": 99}), 409, "stale revision"); errCode(r) != "review_stale" {
			t.Fatalf("stale: %s", r.Body)
		}
		e.Expect(e.decide("skills", "ai.openlinker.alice/first", map[string]any{"decision": "approve", "trust_namespace": true}), 200, "approve")
		for _, p := range hidden[:4] {
			if strings.Contains(p, "purpose=import") {
				e.Expect(g.do("GET", p, nil, "", ""), 200, "gateway import after approval")
				continue
			}
			e.Expect(e.Do("GET", p, "", "", nil), 200, "anonymous after approval "+p)
		}
		if !listed(t, e, "", "/v1/skills", "skills", "ai.openlinker.alice") || !listed(t, e, "", "/ns/ai.openlinker.alice/.well-known/agent-skills/index.json", "skills", "first") {
			t.Fatal("approved entry not listed")
		}
	})

	t.Run("shares never reveal entries under review", func(t *testing.T) {
		e.Expect(carol.send("POST", "/v1/skills/ai.openlinker.carol/draft/versions?version=1", md("draft", "Draft", "MIT"), "text/markdown"), 201, "draft")
		shared := newGateway(t, e, "shared", []string{"skills:read"}, []string{"openlinker"})
		e.Expect(e.Do("POST", "/v1/admin/namespaces/ai.openlinker.carol/roles", AdminToken, "", map[string]any{"subject": "token:" + shared.tokenID, "role": "reader"}), 200, "share namespace")
		member := e.token([]string{"skills:read"}, []map[string]string{{"namespace": "ai.openlinker.carol", "role": "reader"}})
		draft := "/v1/skills/ai.openlinker.carol/draft"
		for _, p := range []string{draft, draft + "/versions/1", draft + "/versions/1/artifact", draft + "/versions/1/artifact?purpose=import", draft + "/versions/1/files/SKILL.md"} {
			e.Expect(shared.do("GET", p, nil, "", ""), 404, "shared gateway "+p)
			e.Expect(actor{shared, otherUser, ""}.get(p), 404, "unrelated user through shared gateway "+p)
		}
		e.Expect(e.Do("GET", draft, member, "", nil), 200, "namespace member token")
		if listed(t, e, shared.token, "/v1/skills?namespace=ai.openlinker.carol", "skills", "draft") {
			t.Fatal("shared gateway lists a draft")
		}
		if !listed(t, e, member, "/v1/skills?namespace=ai.openlinker.carol", "skills", "draft") {
			t.Fatal("member cannot list a draft")
		}
	})

	t.Run("trusted namespace publishes directly, indexing still waits", func(t *testing.T) {
		r := e.Expect(alice.send("POST", "/v1/skills/ai.openlinker.alice/second/versions?version=1", md("second", "Second", "MIT"), "text/markdown"), 201, "second")
		if reviewState(r) != "approved" {
			t.Fatalf("trusted: %s", r.Body)
		}
		r = e.Expect(alice.send("POST", first+"/versions?version=1.1.0", md("first", "First", "MIT"), "text/markdown"), 201, "new version")
		if reviewState(r) != "approved" {
			t.Fatalf("new version: %s", r.Body)
		}
		demo := "https://github.com/acme/skills/tree/main/skills/demo"
		body := `{"url":"` + demo + `"}`
		p := e.Expect(alice.json("POST", "/v1/skills/ai.openlinker.alice/indexes/preview", body), 200, "preview")
		r = e.Expect(alice.json("POST", "/v1/skills/ai.openlinker.alice/indexes", `{"url":"`+demo+`","commit":"`+p.JSON["commit"].(string)+
			`","expected_tree_sha":"`+p.JSON["tree_sha"].(string)+`","version":"1"}`), 201, "index")
		if reviewState(r) != "pending" {
			t.Fatalf("indexed: %s", r.Body)
		}
		e.Expect(e.Do("GET", "/v1/skills/ai.openlinker.alice/demo", "", "", nil), 404, "indexed hidden")
		e.Expect(e.decide("skills", "ai.openlinker.alice/demo", map[string]any{"decision": "approve"}), 200, "approve indexed")
		e.Expect(e.Do("GET", "/v1/skills/ai.openlinker.alice/demo", "", "", nil), 200, "indexed visible")
		p = e.Expect(alice.json("POST", "/v1/skills/ai.openlinker.alice/indexes/preview", body), 200, "preview again")
		r = e.Expect(alice.json("POST", "/v1/skills/ai.openlinker.alice/indexes", `{"url":"`+demo+`","commit":"`+p.JSON["commit"].(string)+
			`","expected_tree_sha":"`+p.JSON["tree_sha"].(string)+`","version":"2"}`), 201, "index new version")
		if reviewState(r) != "pending" {
			t.Fatalf("new indexed version: %s", r.Body)
		}
		e.Expect(e.Do("GET", "/v1/skills/ai.openlinker.alice/demo", "", "", nil), 404, "indexed hidden again")
		seen := e.Expect(e.Do("GET", "/v1/skills/ai.openlinker.alice/demo", AdminToken, "", nil), 200, "reviewer looks")
		revision := seen.JSON["review"].(map[string]any)["revision"]
		p = e.Expect(alice.json("POST", "/v1/skills/ai.openlinker.alice/indexes/preview", body), 200, "preview third")
		e.Expect(alice.json("POST", "/v1/skills/ai.openlinker.alice/indexes", `{"url":"`+demo+`","commit":"`+p.JSON["commit"].(string)+
			`","expected_tree_sha":"`+p.JSON["tree_sha"].(string)+`","version":"3"}`), 201, "submitted after the reviewer looked")
		if r := e.Expect(e.Do("POST", "/v1/admin/review/skills/ai.openlinker.alice/demo", AdminToken, "", map[string]any{"decision": "approve", "revision": revision}), 409, "approval of what was seen"); errCode(r) != "review_stale" {
			t.Fatalf("stale approval: %s", r.Body)
		}
		e.Expect(e.Do("GET", "/v1/skills/ai.openlinker.alice/demo", "", "", nil), 404, "still hidden")
	})

	bobSkill := "/v1/skills/ai.openlinker.bob/spam"
	t.Run("rejection and resubmission", func(t *testing.T) {
		e.Expect(bob.send("POST", bobSkill+"/versions?version=1", md("spam", "Spam", "MIT"), "text/markdown"), 201, "bob publish")
		e.Expect(e.decide("skills", "ai.openlinker.bob/spam", map[string]any{"decision": "reject", "reason": "spam"}), 200, "reject")
		r := e.Expect(bob.get(bobSkill), 200, "bob reads rejected")
		review := r.JSON["review"].(map[string]any)
		if review["state"] != "rejected" || review["reason"] != "spam" {
			t.Fatalf("rejected: %s", r.Body)
		}
		e.Expect(e.Do("GET", bobSkill, "", "", nil), 404, "rejected hidden")
		r = e.Expect(bob.send("POST", bobSkill+"/versions?version=2", md("spam", "Not spam", "MIT"), "text/markdown"), 201, "resubmit")
		if reviewState(r) != "pending" || r.JSON["review"].(map[string]any)["reason"] != "" {
			t.Fatalf("resubmit: %s", r.Body)
		}
	})

	t.Run("reports reopen review", func(t *testing.T) {
		report := `{"kind":"skill","namespace":"ai.openlinker.alice","name":"first","category":"malware","details":"runs curl | sh"}`
		e.Expect(e.Do("POST", "/v1/reports", "", "", []byte(report)), 401, "anonymous report")
		if r := e.Expect(bob.json("POST", "/v1/reports", `{"kind":"skill","namespace":"ai.openlinker.alice","name":"demo","category":"spam","details":""}`), 404, "report hidden"); errCode(r) != "not_found" {
			t.Fatalf("hidden: %s", r.Body)
		}
		e.Expect(bob.json("POST", "/v1/reports", `{"kind":"skill","namespace":"ai.openlinker.alice","name":"first","category":"nonsense","details":""}`), 422, "bad category")
		r := e.Expect(bob.json("POST", "/v1/reports", report), 201, "first report")
		id := r.JSON["id"]
		r = e.Expect(bob.json("POST", "/v1/reports", report), 200, "duplicate report")
		if r.JSON["id"] != id {
			t.Fatalf("duplicate: %s", r.Body)
		}
		e.Expect(e.Do("GET", first, "", "", nil), 200, "still visible below threshold")
		e.Expect(carol.json("POST", "/v1/reports", report), 201, "second reporter")
		e.Expect(e.Do("GET", first, "", "", nil), 404, "hidden at threshold")
		if r := e.Expect(alice.get(first), 200, "owner sees reopened"); reviewState(r) != "pending" {
			t.Fatalf("reopened: %s", r.Body)
		}
		r = e.Expect(e.Do("GET", "/v1/admin/reports?state=open", AdminToken, "", nil), 200, "reports")
		reports, _ := r.JSON["reports"].([]any)
		if len(reports) != 2 || strings.Contains(string(e.Do("GET", first, "", "", nil).Body), "curl") {
			t.Fatalf("reports: %s", r.Body)
		}
		e.Expect(e.Do("GET", "/v1/admin/reports", bob.g.token, "", nil), 403, "gateway cannot list reports")
		writeOnly := e.token([]string{"skills:write"}, nil)
		if r := e.Expect(e.Do("POST", "/v1/reports", writeOnly, "", []byte(report)), 403, "write-only token reports"); errCode(r) != "forbidden" {
			t.Fatalf("write-only: %s", r.Body)
		}
		rid := url.PathEscape(jsonNumber(reports[0].(map[string]any)["id"]))
		e.Expect(e.Do("POST", "/v1/admin/reports/"+rid+"/resolve", AdminToken, "", map[string]any{"resolution": "dismissed"}), 200, "resolve")
		e.Expect(e.Do("POST", "/v1/admin/reports/"+rid+"/resolve", AdminToken, "", map[string]any{"resolution": "dismissed"}), 409, "resolve twice")
		e.Expect(e.decide("skills", "ai.openlinker.alice/first", map[string]any{"decision": "approve"}), 200, "approve again")
		e.Expect(e.Do("GET", first, "", "", nil), 200, "visible again")
		e.Expect(e.Do("POST", "/v1/reports", AdminToken, "", []byte(`{"kind":"skill","namespace":"ai.openlinker.alice","name":"second","category":"other","details":""}`)), 201, "admin report")
		e.Expect(e.Do("GET", "/v1/skills/ai.openlinker.alice/second", "", "", nil), 404, "admin report reopens at once")
	})

	t.Run("mcp servers", func(t *testing.T) {
		name := "ai.openlinker.bob/weather"
		doc := `{"name":"` + name + `","description":"Weather","version":"1.0.0","remotes":[{"type":"streamable-http","url":"https://mcp.bob.example/mcp"}]}`
		r := e.Expect(bob.json("POST", "/v1/mcp/servers/ai.openlinker.bob", doc), 201, "mcp publish")
		if reviewState(r) != "pending" {
			t.Fatalf("mcp review: %s", r.Body)
		}
		if g := e.Expect(bob.get("/v1/mcp/servers/ai.openlinker.bob/weather"), 200, "owner reads"); g.JSON["review"].(map[string]any)["revision"] != r.JSON["review"].(map[string]any)["revision"] {
			t.Fatalf("mcp publish answered a stale revision: %s vs %s", r.Body, g.Body)
		}
		page := e.Expect(e.Do("GET", "/v1/admin/review?state=pending&limit=1", AdminToken, "", nil), 200, "first page")
		cursor, _ := page.JSON["next_cursor"].(string)
		if items, _ := page.JSON["items"].([]any); len(items) != 1 || cursor == "" {
			t.Fatalf("first page: %s", page.Body)
		}
		next := e.Expect(e.Do("GET", "/v1/admin/review?state=pending&limit=1&cursor="+url.QueryEscape(cursor), AdminToken, "", nil), 200, "second page")
		if items, _ := next.JSON["items"].([]any); len(items) != 1 || string(next.Body) == string(page.Body) {
			t.Fatalf("second page: %s", next.Body)
		}
		since := time.Now().UTC().Add(-time.Millisecond).Format(time.RFC3339Nano)
		time.Sleep(5 * time.Millisecond)
		esc := url.PathEscape(name)
		for _, p := range []string{"/v1/mcp/servers/ai.openlinker.bob/weather", "/v1/mcp/servers/ai.openlinker.bob/weather/versions/latest",
			"/v0.1/servers/" + esc + "/versions/latest", "/v0.1/servers/" + esc + "/versions", "/v0/servers/" + esc + "/versions/1.0.0"} {
			e.Expect(e.Do("GET", p, "", "", nil), 404, "anonymous "+p)
		}
		for _, p := range []string{"/v1/mcp/servers", "/v0.1/servers", "/v0/servers?version=latest"} {
			if listed(t, e, "", p, "servers", "ai.openlinker.bob/weather") {
				t.Fatalf("pending listed in %s", p)
			}
		}
		e.Expect(e.decide("mcp/servers", "ai.openlinker.bob/weather", map[string]any{"decision": "approve"}), 200, "approve mcp")
		e.Expect(e.Do("GET", "/v0.1/servers/"+esc+"/versions/latest", "", "", nil), 200, "approved mcp")
		if !listed(t, e, "", "/v0.1/servers?updated_since="+url.QueryEscape(since), "servers", name) {
			t.Fatal("an approval must reach incremental readers")
		}
		indexed := `{"name":"ai.openlinker.bob/other","description":"Other","version":"1","remotes":[{"type":"sse","url":"https://mcp.other.example/sse"}]}`
		r = e.Expect(bob.json("POST", "/v1/mcp/servers/ai.openlinker.bob?indexed=true", indexed), 201, "mcp indexed")
		if reviewState(r) != "pending" {
			t.Fatalf("indexed mcp in a trusted-less namespace: %s", r.Body)
		}
	})

	t.Run("concurrent reports reach the threshold", func(t *testing.T) {
		ce := NewEnv(t, newStore(t), Options{Review: true, ReportThreshold: 3})
		cg := newGateway(t, ce, "core", []string{"skills:read", "skills:write"}, []string{"openlinker"})
		owner := actor{cg, coreUser, ""}
		ce.Expect(owner.json("POST", "/v1/namespaces", `{"name":"ai.openlinker.owner"}`), 201, "handle")
		ce.Expect(owner.send("POST", "/v1/skills/ai.openlinker.owner/x/versions?version=1", md("x", "X", "MIT"), "text/markdown"), 201, "publish")
		ce.Expect(ce.decide("skills", "ai.openlinker.owner/x", map[string]any{"decision": "approve"}), 200, "approve")
		subs := []string{"0d5c3c1e-0000-4000-8000-000000000001", "0d5c3c1e-0000-4000-8000-000000000002", "0d5c3c1e-0000-4000-8000-000000000003"}
		assertions := make([]string, len(subs))
		body := []byte(`{"kind":"skill","namespace":"ai.openlinker.owner","name":"x","category":"spam","details":""}`)
		for i, sub := range subs {
			assertions[i] = cg.assertion("POST", "/v1/reports", body, func(x *Assertion) { x.Sub, x.Act = sub, []string{"read", "write"} })
		}
		statuses := make(chan int, len(subs))
		var wg sync.WaitGroup
		for _, a := range assertions {
			wg.Add(1)
			go func(a string) {
				defer wg.Done()
				req, _ := http.NewRequest("POST", ce.Base+"/v1/reports", bytes.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+cg.token)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("OpenLinker-User-Assertion", a)
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					statuses <- 0
					return
				}
				res.Body.Close()
				statuses <- res.StatusCode
			}(a)
		}
		wg.Wait()
		close(statuses)
		for st := range statuses {
			if st != 201 {
				t.Fatalf("report status %d", st)
			}
		}
		ce.Expect(ce.Do("GET", "/v1/skills/ai.openlinker.owner/x", "", "", nil), 404, "threshold reached under concurrency")
	})

	t.Run("a downstream mirror never copies entries under review", func(t *testing.T) {
		up := NewEnv(t, newStore(t), Options{Review: true})
		ug := newGateway(t, up, "core", []string{"skills:read", "skills:write"}, []string{"openlinker"})
		author := actor{ug, coreUser, ""}
		up.Expect(author.json("POST", "/v1/namespaces", `{"name":"ai.openlinker.author"}`), 201, "handle")
		up.Expect(author.send("POST", "/v1/skills/ai.openlinker.author/tool/versions?version=1", md("tool", "Tool", "MIT"), "text/markdown"), 201, "publish")
		member := up.token([]string{"skills:read"}, []map[string]string{{"namespace": "ai.openlinker.author", "role": "reader"}})
		if r := up.Expect(up.Do("GET", "/v1/skills/ai.openlinker.author/tool", member, "", nil), 200, "member reads"); r.JSON["visibility"] != "private" {
			t.Fatalf("presented visibility: %s", r.Body)
		}
		down := NewEnv(t, newStore(t), Options{Mode: "private", Loopback: true, UpstreamTokens: map[string]string{"hub": member}})
		down.Expect(down.upstream(map[string]any{"name": "hub", "kind": "openlinker-registry", "base_url": up.Base}), 201, "upstream")
		reader := down.token([]string{"skills:read"}, nil)
		path := "/v1/skills/ai.openlinker.author/tool"
		down.sync("hub")
		down.Expect(down.Do("GET", path, reader, "", nil), 404, "pending is not mirrored")
		up.Expect(up.decide("skills", "ai.openlinker.author/tool", map[string]any{"decision": "approve"}), 200, "approve")
		down.sync("hub")
		down.Expect(down.Do("GET", path, reader, "", nil), 200, "approved is mirrored")
		up.Expect(up.Do("POST", "/v1/reports", AdminToken, "", []byte(`{"kind":"skill","namespace":"ai.openlinker.author","name":"tool","category":"malware","details":""}`)), 201, "admin report")
		down.sync("hub")
		down.Expect(down.Do("GET", path, reader, "", nil), 404, "reopened entry is withdrawn downstream")
		up.Expect(up.decide("skills", "ai.openlinker.author/tool", map[string]any{"decision": "reject", "reason": "malware"}), 200, "reject")
		down.sync("hub")
		down.Expect(down.Do("GET", path, reader, "", nil), 404, "rejected stays withdrawn")
	})

	t.Run("writes are limited per user", func(t *testing.T) {
		le := NewEnv(t, newStore(t), Options{Review: true, WritePerMinute: 3})
		lg := newGateway(t, le, "core", []string{"skills:read", "skills:write"}, []string{"openlinker"})
		a, b := actor{lg, coreUser, ""}, actor{lg, otherUser, ""}
		le.Expect(a.json("POST", "/v1/namespaces", `{"name":"ai.openlinker.alice"}`), 201, "a1")
		le.Expect(a.json("POST", "/v1/namespaces", `{"name":"ai.openlinker.alice"}`), 200, "a2")
		le.Expect(a.json("POST", "/v1/namespaces", `{"name":"ai.openlinker.alice"}`), 200, "a3")
		le.Expect(a.json("POST", "/v1/namespaces", `{"name":"ai.openlinker.alice"}`), 429, "a4")
		le.Expect(b.json("POST", "/v1/namespaces", `{"name":"ai.openlinker.bob"}`), 201, "b1")
	})

	t.Run("review off approves everything", func(t *testing.T) {
		oe := NewEnv(t, newStore(t), Options{})
		og := newGateway(t, oe, "core", []string{"skills:read", "skills:write"}, []string{"openlinker"})
		a := actor{og, coreUser, ""}
		oe.Expect(a.json("POST", "/v1/namespaces", `{"name":"ai.openlinker.alice"}`), 201, "handle")
		r := oe.Expect(a.send("POST", "/v1/skills/ai.openlinker.alice/x/versions?version=1", md("x", "X", "MIT"), "text/markdown"), 201, "publish")
		if reviewState(r) != "approved" {
			t.Fatalf("review off: %s", r.Body)
		}
		oe.Expect(oe.Do("GET", "/v1/skills/ai.openlinker.alice/x", "", "", nil), 200, "visible")
	})
}

func jsonNumber(v any) string {
	switch n := v.(type) {
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	case string:
		return n
	}
	return ""
}

func runClaims(t *testing.T, newStore func(t *testing.T) store.Store) {
	e := NewEnv(t, newStore(t), Options{Review: true, UI: true})
	e.Fake.SetOwner(1, "User")
	e.Fake.AddUser(1, "acme", "User")
	e.Fake.AddUser(7, "bob", "User")
	e.Fake.AddUser(9, "acme-org", "Organization")
	g := newGateway(t, e, "core", []string{"skills:read", "skills:write"}, []string{"openlinker", "github"})
	author := actor{g, coreUser, "1"}
	bob := actor{g, otherUser, "7"}
	impostor := actor{g, thirdUser, "9"}

	t.Run("github namespaces", func(t *testing.T) {
		if r := e.Expect(bob.json("POST", "/v1/namespaces", `{"name":"io.github.acme"}`), 403, "login mismatch"); errCode(r) != "github_login_mismatch" {
			t.Fatalf("mismatch: %s", r.Body)
		}
		if r := e.Expect(impostor.json("POST", "/v1/namespaces", `{"name":"io.github.acme-org"}`), 403, "organization"); errCode(r) != "github_org_unsupported" {
			t.Fatalf("org: %s", r.Body)
		}
		noGH := actor{g, otherUser, ""}
		if r := e.Expect(noGH.json("POST", "/v1/namespaces", `{"name":"io.github.bob"}`), 403, "no github id"); errCode(r) != "github_identity_required" {
			t.Fatalf("no gh: %s", r.Body)
		}
		e.Expect(bob.json("POST", "/v1/namespaces", `{"name":"io.github.bob"}`), 201, "bob github ns")
		e.Expect(bob.json("POST", "/v1/namespaces", `{"name":"io.github.bob"}`), 200, "idempotent")
		e.Expect(bob.json("POST", "/v1/namespaces", `{"name":"ai.openlinker.bob"}`), 201, "bob handle")
	})

	demo := "https://github.com/acme/skills/tree/main/skills/demo"
	single := "https://github.com/acme/skills/tree/main/skills/single"
	nolic := "https://github.com/acme/skills/tree/main/skills/nolic"
	index := func(url string) {
		p := e.Expect(bob.json("POST", "/v1/skills/ai.openlinker.bob/indexes/preview", `{"url":"`+url+`"}`), 200, "preview")
		e.Expect(bob.json("POST", "/v1/skills/ai.openlinker.bob/indexes", `{"url":"`+url+`","commit":"`+p.JSON["commit"].(string)+
			`","expected_tree_sha":"`+p.JSON["tree_sha"].(string)+`","version":"1"}`), 201, "index "+url)
	}
	index(demo)
	index(single)
	e.Fake.SetRepoID(0)
	p := e.Expect(bob.json("POST", "/v1/skills/ai.openlinker.bob/indexes/preview", `{"url":"`+nolic+`"}`), 200, "preview without repository id")
	if r := e.Expect(bob.json("POST", "/v1/skills/ai.openlinker.bob/indexes", `{"url":"`+nolic+`","commit":"`+p.JSON["commit"].(string)+
		`","expected_tree_sha":"`+p.JSON["tree_sha"].(string)+`","version":"1"}`), 502, "index without a repository id"); errCode(r) != "github_unavailable" {
		t.Fatalf("no repository id: %s", r.Body)
	}
	e.Fake.SetRepoID(4242)
	e.Fake.RecycleOnLookup(2, 777, 9, "User")
	p = e.Expect(bob.json("POST", "/v1/skills/ai.openlinker.bob/indexes/preview", `{"url":"`+nolic+`"}`), 200, "preview before recycle")
	if r := e.Expect(bob.json("POST", "/v1/skills/ai.openlinker.bob/indexes", `{"url":"`+nolic+`","commit":"`+p.JSON["commit"].(string)+
		`","expected_tree_sha":"`+p.JSON["tree_sha"].(string)+`","version":"1"}`), 502, "repository replaced during indexing"); errCode(r) != "github_unavailable" {
		t.Fatalf("recycled during indexing: %s", r.Body)
	}
	e.Fake.Recycle(4242, 1, "User")
	legacy := model.Version{Version: "1", FrontmatterName: "nolic", FrontmatterDescription: "Legacy", LicenseKind: "spdx", LicenseID: "MIT",
		Files: []skill.ManifestEntry{}, ArtifactDigest: sha([]byte("legacy")), ArtifactType: "skill-md", ArtifactSize: 6,
		Locator:   &model.Locator{Kind: "github", Owner: "acme", Repo: "skills", Commit: C1, Path: "skills/nolic", TreeSHA: C1},
		IndexedBy: "legacy", CreatedBy: "legacy"}
	if _, _, err := e.Store.PublishVersion(context.Background(), model.PublishInput{Namespace: "ai.openlinker.bob", Name: "nolic",
		Mode: "indexed", SourceKey: "github:acme/skills:/skills/nolic", Visibility: "public", Listed: true, Version: legacy},
		model.AuditEvent{Actor: "test", Action: "test"}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"demo", "single"} {
		e.Expect(e.decide("skills", "ai.openlinker.bob/"+n, map[string]any{"decision": "approve"}), 200, "approve "+n)
	}
	before := e.Expect(e.Do("GET", "/v1/skills/ai.openlinker.bob/demo/versions/1", "", "", nil), 200, "before claim")

	t.Run("claim refusals", func(t *testing.T) {
		if r := e.Expect(bob.json("POST", "/v1/skills/ai.openlinker.bob/demo/claim", `{}`), 403, "not the author"); errCode(r) != "claim_not_owner" {
			t.Fatalf("not owner: %s", r.Body)
		}
		e.Expect(bob.send("POST", "/v1/skills/ai.openlinker.bob/hosted/versions?version=1", md("hosted", "Hosted", "MIT"), "text/markdown"), 201, "hosted")
		e.Expect(e.decide("skills", "ai.openlinker.bob/hosted", map[string]any{"decision": "approve"}), 200, "approve hosted")
		if r := e.Expect(author.json("POST", "/v1/skills/ai.openlinker.bob/hosted/claim", `{}`), 422, "hosted"); errCode(r) != "claim_not_indexed" {
			t.Fatalf("hosted: %s", r.Body)
		}
		if r := e.Expect(author.json("POST", "/v1/skills/ai.openlinker.bob/nolic/claim", `{}`), 403, "no repository id"); errCode(r) != "claim_unverifiable" {
			t.Fatalf("legacy: %s", r.Body)
		}
		readOnly := author.g.do("POST", "/v1/skills/ai.openlinker.bob/demo/claim", []byte(`{}`), "application/json",
			author.g.assertion("POST", "/v1/skills/ai.openlinker.bob/demo/claim", []byte(`{}`), func(x *Assertion) { x.Sub, x.GH = coreUser, "1" }))
		e.Expect(readOnly, 401, "assertion without write")
		e.Fake.SetOwner(9, "Organization")
		if r := e.Expect(impostor.json("POST", "/v1/skills/ai.openlinker.bob/demo/claim", `{}`), 403, "org repo"); errCode(r) != "github_org_unsupported" {
			t.Fatalf("org repo: %s", r.Body)
		}
		e.Fake.SetOwner(1, "User")
		e.Fake.Down.Store(true)
		if r := e.Expect(author.json("POST", "/v1/skills/ai.openlinker.bob/demo/claim", `{}`), 502, "github down"); errCode(r) != "github_unavailable" {
			t.Fatalf("down: %s", r.Body)
		}
		e.Fake.Down.Store(false)
		ro := newGateway(t, e, "readonly", []string{"skills:read"}, []string{"openlinker", "github"})
		if r := e.Expect(actor{ro, coreUser, "1"}.json("POST", "/v1/skills/ai.openlinker.bob/demo/claim", `{}`), 403, "read-only gateway"); errCode(r) != "forbidden" {
			t.Fatalf("read-only gateway: %s", r.Body)
		}
	})

	t.Run("claim moves the entry", func(t *testing.T) {
		path := "/v1/skills/ai.openlinker.bob/demo/claim"
		requests := make([]*http.Request, 3)
		for i := range requests {
			req, _ := http.NewRequest("POST", e.Base+path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+g.token)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("OpenLinker-User-Assertion", g.assertion("POST", path, []byte(`{}`), func(x *Assertion) {
				x.Sub, x.GH, x.Act = coreUser, "1", []string{"read", "write"}
			}))
			requests[i] = req
		}
		statuses := make(chan int, len(requests))
		var wg sync.WaitGroup
		for _, req := range requests {
			wg.Add(1)
			go func(req *http.Request) {
				defer wg.Done()
				res, err := noRedirect.Do(req)
				if err != nil {
					statuses <- 0
					return
				}
				res.Body.Close()
				statuses <- res.StatusCode
			}(req)
		}
		wg.Wait()
		close(statuses)
		won := 0
		for st := range statuses {
			switch st {
			case 200:
				won++
			case 404, 409:
			default:
				t.Fatalf("concurrent claim status %d", st)
			}
		}
		if won == 0 {
			t.Fatal("no concurrent claim succeeded")
		}
		r := e.Expect(author.json("POST", "/v1/skills/io.github.acme/demo/claim", `{}`), 200, "claim")
		if r.JSON["namespace"] != "io.github.acme" || r.JSON["name"] != "demo" || r.JSON["claimed"] != true {
			t.Fatalf("claim: %s", r.Body)
		}
		after := e.Expect(e.Do("GET", "/v1/skills/io.github.acme/demo/versions/1", "", "", nil), 200, "new path")
		if after.JSON["artifact"].(map[string]any)["digest"] != before.JSON["artifact"].(map[string]any)["digest"] {
			t.Fatalf("digest changed: %s vs %s", before.Body, after.Body)
		}
		for _, suffix := range []string{"", "/versions/1", "/versions/1/artifact", "/versions/1/files/SKILL.md"} {
			r := e.Expect(e.Do("GET", "/v1/skills/ai.openlinker.bob/demo"+suffix, "", "", nil), 308, "redirect "+suffix)
			if r.Header.Get("Location") != "/registry/v1/skills/io.github.acme/demo"+suffix {
				t.Fatalf("location %q", r.Header.Get("Location"))
			}
		}
		r = e.Expect(g.do("GET", "/v1/skills/ai.openlinker.bob/demo/versions/1/artifact?purpose=import", nil, "", ""), 308, "import redirect")
		if r.Header.Get("Location") != "/registry/v1/skills/io.github.acme/demo/versions/1/artifact?purpose=import" {
			t.Fatalf("import location %q", r.Header.Get("Location"))
		}
		e.Expect(author.json("POST", "/v1/skills/io.github.acme/demo/claim", `{}`), 200, "claim again is idempotent")
		for from, to := range map[string]string{
			"/ui/skills/ai.openlinker.bob/demo":                     "/registry/ui/skills/io.github.acme/demo",
			"/ui/skills/ai.openlinker.bob/demo/versions/1/artifact": "/registry/ui/skills/io.github.acme/demo/versions/1/artifact",
		} {
			if r := e.Expect(e.Do("GET", from, "", "", nil), 308, "ui redirect "+from); r.Header.Get("Location") != to {
				t.Fatalf("ui location %q", r.Header.Get("Location"))
			}
		}
		e.Expect(bob.json("POST", "/v1/skills/io.github.acme/demo/yank", `{}`), 403, "indexer lost write access")
		if r := e.Expect(bob.json("POST", "/v1/skills/ai.openlinker.bob/demo/yank", `{}`), 404, "old coordinate write"); errCode(r) != "not_found" {
			t.Fatalf("old write: %s", r.Body)
		}
		if r := e.Expect(bob.send("POST", "/v1/skills/ai.openlinker.bob/demo/versions?version=9", md("demo", "Shadow", "MIT"), "text/markdown"), 409, "old name reserved"); errCode(r) != "name_reserved" {
			t.Fatalf("reserved: %s", r.Body)
		}
		e.Expect(author.json("POST", "/v1/skills/io.github.acme/demo/deprecate", `{"reason":"moved"}`), 200, "author manages")
		audit := e.Expect(e.Do("GET", "/v1/admin/audit", AdminToken, "", nil), 200, "audit")
		if !strings.Contains(string(audit.Body), "skill.claim") {
			t.Fatalf("audit: %s", audit.Body)
		}
	})

	t.Run("name taken in the author's namespace", func(t *testing.T) {
		e.Expect(author.send("POST", "/v1/skills/io.github.acme/single/versions?version=1&visibility=private", md("single", "Author copy", "MIT"), "text/markdown"), 201, "author entry")
		r := e.Expect(author.json("POST", "/v1/skills/ai.openlinker.bob/single/claim", `{}`), 409, "conflict")
		if errCode(r) != "claim_name_taken" || strings.Contains(string(r.Body), "Author copy") {
			t.Fatalf("conflict: %s", r.Body)
		}
		e.Expect(e.Do("GET", "/v1/skills/ai.openlinker.bob/single", "", "", nil), 200, "unchanged after conflict")
		r = e.Expect(author.json("POST", "/v1/skills/ai.openlinker.bob/single/claim", `{"name":"single-indexed"}`), 200, "claim under another name")
		if r.JSON["namespace"] != "io.github.acme" || r.JSON["name"] != "single-indexed" {
			t.Fatalf("renamed claim: %s", r.Body)
		}
		e.Expect(e.Do("GET", "/v1/skills/ai.openlinker.bob/single", "", "", nil), 308, "renamed redirect")
	})

	t.Run("a recycled repository path cannot claim", func(t *testing.T) {
		index("https://github.com/acme/skills/tree/main/skills/custom")
		e.Expect(e.decide("skills", "ai.openlinker.bob/custom", map[string]any{"decision": "approve"}), 200, "approve custom")
		e.Fake.Recycle(999, 9, "User")
		if r := e.Expect(impostor.json("POST", "/v1/skills/ai.openlinker.bob/custom/claim", `{}`), 403, "new owner of the path"); errCode(r) != "claim_not_owner" {
			t.Fatalf("recycled: %s", r.Body)
		}
	})
}
