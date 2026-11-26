package apitest

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

type Assertion struct {
	Aud string
	Cid string
	Sub string
	GH  string
	Act []string
	M   string
	T   string
	BH  string
	Iat time.Time
	Exp time.Time
	Jti string
	Kid string
	Alg string
	Typ string
}

func CanonicalTarget(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	q := u.Query()
	type kv struct{ k, v string }
	var pairs []kv
	for k, vs := range q {
		for _, v := range vs {
			pairs = append(pairs, kv{k, v})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, url.QueryEscape(p.k)+"="+url.QueryEscape(p.v))
	}
	t := u.EscapedPath()
	if len(parts) > 0 {
		t += "?" + strings.Join(parts, "&")
	}
	return t
}

func BodyHash(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

func NewJTI() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func SignAssertion(key ed25519.PrivateKey, a Assertion) string {
	if a.Alg == "" {
		a.Alg = "EdDSA"
	}
	if a.Typ == "" {
		a.Typ = "openlinker-user-assertion+jwt"
	}
	header, _ := json.Marshal(map[string]string{"alg": a.Alg, "typ": a.Typ, "kid": a.Kid})
	claims := map[string]any{"aud": a.Aud, "cid": a.Cid, "sub": a.Sub, "act": a.Act,
		"req": map[string]string{"m": a.M, "t": a.T, "bh": a.BH},
		"iat": a.Iat.Unix(), "exp": a.Exp.Unix(), "jti": a.Jti}
	if a.GH != "" {
		claims["gh"] = a.GH
	}
	payload, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	sig := ed25519.Sign(key, []byte(signing))
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

type gatewayFixture struct {
	e        *Env
	token    string
	tokenID  string
	key      ed25519.PrivateKey
	kid      string
	consumer string
}

const coreUser = "8f14e45f-ceea-467a-a866-051b2ad0d4a1"
const otherUser = "45c48cce-2e2d-4fbd-9a1e-3c7a0d2e6a11"

func (g gatewayFixture) assertion(method, path string, body []byte, mutate func(*Assertion)) string {
	now := time.Now()
	a := Assertion{Aud: PublicURL, Cid: g.consumer, Sub: coreUser, GH: "1", Act: []string{"read"},
		M: method, T: CanonicalTarget(g.e.Base + path), BH: BodyHash(body),
		Iat: now, Exp: now.Add(60 * time.Second), Jti: NewJTI(), Kid: g.kid}
	if mutate != nil {
		mutate(&a)
	}
	return SignAssertion(g.key, a)
}

func (g gatewayFixture) do(method, path string, body []byte, ct string, assertion string) Resp {
	g.e.Headers = map[string]string{}
	if assertion != "" {
		g.e.Headers["OpenLinker-User-Assertion"] = assertion
	}
	defer func() { g.e.Headers = nil }()
	var b any
	if body != nil {
		b = body
	}
	return g.e.Do(method, path, g.token, ct, b)
}

func (g gatewayFixture) read(path string, mutate func(*Assertion)) Resp {
	return g.do("GET", path, nil, "", g.assertion("GET", path, nil, mutate))
}

func skillNames(t *testing.T, r Resp) string {
	t.Helper()
	var out []string
	for _, s := range r.JSON["skills"].([]any) {
		m := s.(map[string]any)
		out = append(out, m["namespace"].(string)+"/"+m["name"].(string))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func newGateway(t *testing.T, e *Env, consumer string, scopes []string, identities []string) gatewayFixture {
	t.Helper()
	r := e.Expect(e.Do("POST", "/v1/admin/tokens", AdminToken, "", map[string]any{"name": consumer, "scopes": scopes}), 201, "gateway token")
	tok, id := r.JSON["token"].(string), r.JSON["id"].(string)
	e.Expect(e.Do("POST", "/v1/admin/gateways", AdminToken, "", map[string]any{"consumer": consumer, "token_id": id, "identities": identities}), 201, "gateway")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	e.Expect(e.Do("POST", "/v1/admin/gateways/"+consumer+"/keys", AdminToken, "", map[string]any{"kid": "k1", "public_key": base64.StdEncoding.EncodeToString(pub)}), 201, "gateway key")
	return gatewayFixture{e: e, token: tok, tokenID: id, key: priv, kid: "k1", consumer: consumer}
}

func runGateway(t *testing.T, newStore func(t *testing.T) store.Store) {
	e := NewEnv(t, newStore(t), Options{})
	for _, ns := range []map[string]any{
		{"name": "io.github.acme", "kind": "github", "github_id": 1},
		{"name": "io.github.bob", "kind": "github", "github_id": 7},
		{"name": "io.github.shared", "kind": "github", "github_id": 9},
	} {
		e.Expect(e.Do("POST", "/v1/admin/namespaces", AdminToken, "", ns), 201, "ns")
	}
	pub := e.token([]string{"skills:read", "skills:write"}, []map[string]string{
		{"namespace": "io.github.acme", "role": "publisher"}, {"namespace": "io.github.bob", "role": "publisher"}, {"namespace": "io.github.shared", "role": "publisher"}})
	up := func(ns, name, q string, body []byte) {
		e.Expect(e.Do("POST", "/v1/skills/"+ns+"/"+name+"/versions?"+q, pub, "text/markdown", body), 201, "upload "+ns+"/"+name)
	}
	up("io.github.acme", "pub", "version=1", md("pub", "Public", "MIT"))
	up("io.github.acme", "int", "version=1&visibility=internal", md("int", "Internal", "MIT"))
	up("io.github.acme", "priv", "version=1&visibility=private", md("priv", "Private", "MIT"))
	up("io.github.bob", "priv", "version=1&visibility=private", md("priv", "Bob private", "MIT"))
	up("io.github.shared", "doc", "version=1&visibility=private", md("doc", "Shared", "MIT"))
	demo := "https://github.com/acme/skills/tree/main/skills/demo"
	p := e.Expect(e.Do("POST", "/v1/skills/io.github.acme/indexes/preview", pub, "", map[string]any{"url": demo}), 200, "preview demo")
	e.Expect(e.Do("POST", "/v1/skills/io.github.acme/indexes", pub, "", map[string]any{"url": demo, "commit": p.JSON["commit"],
		"expected_tree_sha": p.JSON["tree_sha"], "version": "1"}), 201, "index demo")
	nolic := "https://github.com/acme/skills/tree/main/skills/nolic"
	p = e.Expect(e.Do("POST", "/v1/skills/io.github.acme/indexes/preview", pub, "", map[string]any{"url": nolic}), 200, "preview nolic")
	e.Expect(e.Do("POST", "/v1/skills/io.github.acme/indexes", pub, "", map[string]any{"url": nolic, "commit": p.JSON["commit"],
		"expected_tree_sha": p.JSON["tree_sha"], "version": "1"}), 201, "index nolic")

	t.Run("admin contract", func(t *testing.T) {
		r := e.Expect(e.Do("POST", "/v1/admin/tokens", AdminToken, "", map[string]any{"name": "adm", "scopes": []string{"skills:read", "admin"}}), 201, "admin token")
		e.Expect(e.Do("POST", "/v1/admin/gateways", AdminToken, "", map[string]any{"consumer": "bad-admin", "token_id": r.JSON["id"], "identities": []string{"openlinker"}}), 422, "admin token cannot be a gateway")
		r = e.Expect(e.Do("POST", "/v1/admin/tokens", AdminToken, "", map[string]any{"name": "w", "scopes": []string{"skills:write"}}), 201, "write token")
		e.Expect(e.Do("POST", "/v1/admin/gateways", AdminToken, "", map[string]any{"consumer": "bad-scope", "token_id": r.JSON["id"], "identities": []string{"openlinker"}}), 422, "gateway token needs skills:read")
		r = e.Expect(e.Do("POST", "/v1/admin/tokens", AdminToken, "", map[string]any{"name": "ok", "scopes": []string{"skills:read"}}), 201, "read token")
		e.Expect(e.Do("POST", "/v1/admin/gateways", AdminToken, "", map[string]any{"consumer": "bad-identity", "token_id": r.JSON["id"], "identities": []string{"github-org"}}), 422, "unknown identity kind")
		e.Expect(e.Do("POST", "/v1/admin/gateways", AdminToken, "", map[string]any{"consumer": "Bad Name", "token_id": r.JSON["id"], "identities": []string{"openlinker"}}), 422, "consumer name")
		e.Expect(e.Do("POST", "/v1/admin/gateways", AdminToken, "", map[string]any{"consumer": "nope", "token_id": "aaaaaaaaaaaaaaaa", "identities": []string{"openlinker"}}), 422, "unknown token")
		e.Expect(e.Do("POST", "/v1/admin/gateways", "", "", map[string]any{"consumer": "anon", "token_id": r.JSON["id"], "identities": []string{"openlinker"}}), 401, "anon admin")
		e.Expect(e.Do("POST", "/v1/admin/gateways/missing/keys", AdminToken, "", map[string]any{"kid": "k1", "public_key": base64.StdEncoding.EncodeToString(make([]byte, 32))}), 404, "key for unknown gateway")
		e.Expect(e.Do("POST", "/v1/admin/namespaces/io.github.bob/roles", AdminToken, "", map[string]any{"subject": "openlinker:core-a:not-a-uuid", "role": "reader"}), 422, "bad openlinker subject")
		e.Expect(e.Do("POST", "/v1/admin/namespaces/io.github.bob/roles", AdminToken, "", map[string]any{"subject": "github:abc", "role": "reader"}), 422, "bad github subject")
		e.Expect(e.Do("POST", "/v1/admin/namespaces/io.github.bob/roles", AdminToken, "", map[string]any{"subject": "github:0", "role": "reader"}), 422, "zero github subject")
	})

	g := newGateway(t, e, "core-a", []string{"skills:read", "skills:write"}, []string{"openlinker", "github"})
	e.Expect(e.Do("POST", "/v1/admin/namespaces/io.github.shared/roles", AdminToken, "", map[string]any{"subject": "token:" + g.tokenID, "role": "publisher"}), 200, "share to consumer")

	t.Run("gateway listing and keys", func(t *testing.T) {
		r := e.Expect(e.Do("GET", "/v1/admin/gateways", AdminToken, "", nil), 200, "list gateways")
		body := string(r.Body)
		if !strings.Contains(body, `"core-a"`) || !strings.Contains(body, `"k1"`) || strings.Contains(body, g.token) {
			t.Fatalf("gateway listing %s", body)
		}
		e.Expect(e.Do("POST", "/v1/admin/gateways/core-a/keys", AdminToken, "", map[string]any{"kid": "k1", "public_key": base64.StdEncoding.EncodeToString(make([]byte, 32))}), 409, "duplicate kid")
		e.Expect(e.Do("POST", "/v1/admin/gateways/core-a/keys", AdminToken, "", map[string]any{"kid": "k9", "public_key": base64.StdEncoding.EncodeToString(make([]byte, 31))}), 422, "short key")
		e.Expect(e.Do("POST", "/v1/admin/tokens", AdminToken, "", map[string]any{"name": "dup", "scopes": []string{"skills:read"}}), 201, "dup token")
		e.Expect(e.Do("POST", "/v1/admin/gateways", AdminToken, "", map[string]any{"consumer": "core-b", "token_id": g.tokenID, "identities": []string{"openlinker"}}), 409, "token already a gateway")
	})

	t.Run("consumer token alone sees public and shared only", func(t *testing.T) {
		got := skillNames(t, e.Expect(g.do("GET", "/v1/skills", nil, "", ""), 200, "consumer list"))
		if got != "io.github.acme/demo,io.github.acme/nolic,io.github.acme/pub,io.github.shared/doc" {
			t.Fatalf("consumer list = %s", got)
		}
		e.Expect(g.do("GET", "/v1/skills/io.github.acme/int", nil, "", ""), 404, "consumer cannot see internal")
		e.Expect(g.do("GET", "/v1/skills/io.github.acme/priv", nil, "", ""), 404, "consumer cannot see private")
	})

	t.Run("assertion adds the user's own visibility", func(t *testing.T) {
		got := skillNames(t, e.Expect(g.read("/v1/skills", nil), 200, "user list"))
		if got != "io.github.acme/demo,io.github.acme/int,io.github.acme/nolic,io.github.acme/priv,io.github.acme/pub,io.github.shared/doc" {
			t.Fatalf("user list = %s", got)
		}
		e.Expect(g.read("/v1/skills/io.github.bob/priv", nil), 404, "other user's private")
		e.Expect(g.read("/v1/skills/io.github.acme/priv/versions/1/artifact", nil), 200, "own private artifact")
		r := e.Expect(g.read("/v1/whoami", nil), 200, "whoami")
		user, _ := r.JSON["user"].(map[string]any)
		if user == nil || user["sub"] != coreUser || user["consumer"] != "core-a" || user["github_id"] != "1" {
			t.Fatalf("whoami %s", r.Body)
		}
	})

	t.Run("explicit role grant to a Core user", func(t *testing.T) {
		e.Expect(e.Do("POST", "/v1/admin/namespaces/io.github.bob/roles", AdminToken, "", map[string]any{"subject": "openlinker:core-a:" + otherUser, "role": "reader"}), 200, "grant core user")
		e.Expect(g.read("/v1/skills/io.github.bob/priv", func(a *Assertion) { a.Sub = otherUser; a.GH = "" }), 200, "granted core user")
		e.Expect(g.read("/v1/skills/io.github.acme/priv", func(a *Assertion) { a.Sub = otherUser; a.GH = "" }), 404, "no github id, no acme membership")
		e.Expect(e.Do("POST", "/v1/admin/namespaces/io.github.bob/roles", AdminToken, "", map[string]any{"subject": "github:42", "role": "reader"}), 200, "grant github user")
		e.Expect(g.read("/v1/skills/io.github.bob/priv", func(a *Assertion) { a.Sub = otherUser; a.GH = "42" }), 200, "granted github user")
	})

	t.Run("identity kinds limit what an assertion proves", func(t *testing.T) {
		o := newGateway(t, e, "core-ol", []string{"skills:read"}, []string{"openlinker"})
		e.Expect(o.read("/v1/skills/io.github.acme/priv", nil), 404, "github identity not allowed for this gateway")
		e.Expect(o.read("/v1/skills/io.github.bob/priv", func(a *Assertion) { a.Sub = otherUser; a.GH = "42" }), 404, "grant is scoped to its consumer")
		e.Expect(o.read("/v1/skills/io.github.acme/int", nil), 200, "authenticated user sees internal")
	})

	t.Run("invalid assertions are rejected", func(t *testing.T) {
		path := "/v1/skills"
		_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
		bad := map[string]func(*Assertion){
			"audience":    func(a *Assertion) { a.Aud = "https://other.example/registry" },
			"consumer":    func(a *Assertion) { a.Cid = "core-ol" },
			"unknown kid": func(a *Assertion) { a.Kid = "k404" },
			"expired": func(a *Assertion) {
				a.Iat = time.Now().Add(-100 * time.Second)
				a.Exp = time.Now().Add(-40 * time.Second)
			},
			"future":          func(a *Assertion) { a.Iat = time.Now().Add(40 * time.Second); a.Exp = time.Now().Add(90 * time.Second) },
			"too long":        func(a *Assertion) { a.Exp = a.Iat.Add(61 * time.Second) },
			"exp before iat":  func(a *Assertion) { a.Exp = a.Iat },
			"method":          func(a *Assertion) { a.M = "POST" },
			"target":          func(a *Assertion) { a.T = "/registry/v1/skills?q=x" },
			"body hash":       func(a *Assertion) { a.BH = BodyHash([]byte("x")) },
			"no read act":     func(a *Assertion) { a.Act = []string{"import"} },
			"no subject":      func(a *Assertion) { a.Sub = "" },
			"bad subject":     func(a *Assertion) { a.Sub = "root" },
			"bad github id":   func(a *Assertion) { a.GH = "0x1" },
			"github overflow": func(a *Assertion) { a.GH = "9999999999999999999" },
			"short jti":       func(a *Assertion) { a.Jti = "abc" },
			"alg":             func(a *Assertion) { a.Alg = "none" },
			"typ":             func(a *Assertion) { a.Typ = "JWT" },
		}
		for name, m := range bad {
			r := e.Expect(g.read(path, m), 401, "assertion "+name)
			if errCode(r) != "assertion_invalid" {
				t.Fatalf("%s: code %s", name, errCode(r))
			}
		}
		wrongKey := g
		wrongKey.key = otherKey
		e.Expect(wrongKey.read(path, nil), 401, "wrong signature")
		e.Expect(g.read(path, func(a *Assertion) {
			a.Iat = time.Now().Add(-80 * time.Second)
			a.Exp = time.Now().Add(-20 * time.Second)
		}), 200, "within skew after expiry")
		e.Expect(g.read(path, func(a *Assertion) { a.Iat = time.Now().Add(20 * time.Second); a.Exp = time.Now().Add(60 * time.Second) }), 200, "within skew before issue")
		e.Expect(g.do("GET", path, nil, "", "not.a.jws"), 401, "malformed")
		e.Expect(g.do("GET", path, nil, "", strings.Repeat("a", 9000)), 401, "oversized")
		e.Headers = map[string]string{"OpenLinker-User-Assertion": g.assertion("GET", path, nil, nil)}
		e.Expect(e.Do("GET", path, "", "", nil), 401, "assertion without consumer token")
		e.Headers = map[string]string{"OpenLinker-User-Assertion": g.assertion("GET", path, nil, nil)}
		e.Expect(e.Do("GET", path, pub, "", nil), 401, "assertion with ordinary token")
		e.Headers = map[string]string{"OpenLinker-User-Assertion": g.assertion("GET", path, nil, nil)}
		e.Expect(e.Do("GET", path, AdminToken, "", nil), 401, "assertion with admin token")
		e.Headers = nil
	})

	t.Run("request binding uses the canonical query", func(t *testing.T) {
		signed := "/v1/skills?q=priv&limit=5"
		reordered := "/v1/skills?limit=5&q=priv"
		e.Expect(g.do("GET", reordered, nil, "", g.assertion("GET", signed, nil, nil)), 200, "reordered query")
		e.Expect(g.do("GET", "/v1/skills?limit=6&q=priv", nil, "", g.assertion("GET", signed, nil, nil)), 401, "changed query")
		dup := "/v1/skills?q=a&q=b"
		e.Expect(g.do("GET", dup, nil, "", g.assertion("GET", dup, nil, nil)), 401, "duplicate query keys")
		e.Expect(g.do("GET", "/v1/skills?q=b&q=a", nil, "", g.assertion("GET", dup, nil, nil)), 401, "reordered duplicate values")
		bad := "/v1/skills?q=%zz"
		e.Expect(g.do("GET", bad, nil, "", g.assertion("GET", "/v1/skills", nil, func(a *Assertion) { a.T = "/registry/v1/skills?q=%25zz" })), 401, "malformed query")
	})

	t.Run("replay is refused once, also across restarts and in parallel", func(t *testing.T) {
		a := g.assertion("GET", "/v1/skills", nil, nil)
		e.Expect(g.do("GET", "/v1/skills", nil, "", a), 200, "first use")
		r := e.Expect(g.do("GET", "/v1/skills", nil, "", a), 401, "replay")
		if errCode(r) != "assertion_invalid" {
			t.Fatalf("replay code %s", errCode(r))
		}
		restarted := NewEnv(t, e.Store, Options{})
		h := g
		h.e = restarted
		b := h.assertion("GET", "/v1/skills", nil, nil)
		e.Expect(g.do("GET", "/v1/skills", nil, "", b), 200, "use on first instance")
		restarted.Expect(h.do("GET", "/v1/skills", nil, "", b), 401, "replay on restarted instance")
		late := g.assertion("GET", "/v1/skills", nil, func(a *Assertion) {
			a.Iat = time.Now().Add(-70 * time.Second)
			a.Exp = time.Now().Add(-10 * time.Second)
		})
		e.Expect(g.do("GET", "/v1/skills", nil, "", late), 200, "accepted within skew after nominal expiry")
		restarted.Expect(h.do("GET", "/v1/skills", nil, "", late), 401, "replay after nominal expiry on another instance")
		c := g.assertion("GET", "/v1/skills", nil, nil)
		var wg sync.WaitGroup
		codes := make([]int, 8)
		for i := range codes {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				req, _ := http.NewRequest("GET", e.Base+"/v1/skills", nil)
				req.Header.Set("Authorization", "Bearer "+g.token)
				req.Header.Set("OpenLinker-User-Assertion", c)
				res, err := http.DefaultClient.Do(req)
				if err == nil {
					codes[i] = res.StatusCode
					res.Body.Close()
				}
			}(i)
		}
		wg.Wait()
		ok := 0
		for _, c := range codes {
			if c == 200 {
				ok++
			}
		}
		if ok != 1 {
			t.Fatalf("parallel replay accepted %d times: %v", ok, codes)
		}
	})

	t.Run("writes come only from the user's own roles", func(t *testing.T) {
		body := md("viaGateway", "x", "MIT")
		path := "/v1/skills/io.github.shared/via/versions?version=1"
		e.Expect(g.do("POST", path, body, "text/markdown", g.assertion("POST", path, body, func(a *Assertion) { a.Act = []string{"read"} })), 401, "write needs write act")
		r := g.do("POST", path, body, "text/markdown", g.assertion("POST", path, body, func(a *Assertion) { a.Act = []string{"read", "write"} }))
		e.Expect(r, 403, "consumer share grants no write")
		e.Expect(g.do("POST", path, body, "text/markdown", g.assertion("POST", path, []byte("other"), func(a *Assertion) { a.Act = []string{"write"} })), 401, "body must match")
		own := "/v1/skills/io.github.acme/via/versions?version=1"
		viaBody := md("via", "Via gateway", "MIT")
		e.Expect(g.do("POST", own, viaBody, "text/markdown", g.assertion("POST", own, viaBody, func(a *Assertion) { a.Act = []string{"write"} })), 201, "user's own github namespace")
	})

	t.Run("import read bypasses only the license restriction", func(t *testing.T) {
		art := "/v1/skills/io.github.acme/nolic/versions/1/artifact"
		r := e.Expect(g.read(art, nil), 403, "license withholds without import purpose")
		if errCode(r) != "license_unknown" {
			t.Fatalf("code %s", errCode(r))
		}
		r = e.Expect(g.read(art+"?purpose=import", nil), 403, "license bypass needs the import act")
		if errCode(r) != "license_unknown" {
			t.Fatalf("code %s", errCode(r))
		}
		r = e.Expect(g.do("GET", art+"?purpose=import", nil, "", ""), 403, "license bypass needs an assertion")
		if errCode(r) != "license_unknown" {
			t.Fatalf("code %s", errCode(r))
		}
		r = e.Expect(e.Do("GET", art+"?purpose=import", pub, "", nil), 403, "import purpose needs a gateway")
		if errCode(r) != "import_not_allowed" {
			t.Fatalf("code %s", errCode(r))
		}
		demoArt := "/v1/skills/io.github.acme/demo/versions/1/artifact"
		e.Expect(g.do("GET", demoArt, nil, "", ""), 200, "cached redistributable read")
		e.Expect(g.do("GET", demoArt+"?purpose=import", nil, "", ""), 200, "gateway import read of redistributable content")
		e.Fake.Tamper(C1, "skills/demo/SKILL.md", md("demo", "Tampered", "MIT"))
		e.Expect(g.do("GET", demoArt, nil, "", ""), 200, "ordinary read still served from the recently verified cache")
		r = e.Expect(g.do("GET", demoArt+"?purpose=import", nil, "", ""), 409, "import read re-verifies the source")
		if errCode(r) != "source_changed" {
			t.Fatalf("code %s", errCode(r))
		}
		e.Expect(e.Do("GET", art+"?purpose=other", "", "", nil), 400, "unknown purpose")
		r = e.Expect(g.read(art+"?purpose=import", func(a *Assertion) { a.Act = []string{"read", "import"} }), 200, "import read")
		v := e.Expect(e.Do("GET", "/v1/skills/io.github.acme/nolic/versions/1", "", "", nil), 200, "version")
		if sha(r.Body) != v.JSON["artifact"].(map[string]any)["digest"] {
			t.Fatal("import read must return the recorded artifact")
		}
		if r.Header.Get("Cache-Control") != "private, no-store" {
			t.Fatalf("import read cache %q", r.Header.Get("Cache-Control"))
		}
		e.Expect(e.Do("POST", "/v1/skills/io.github.acme/nolic/versions/1/yank", pub, "", map[string]any{"reason": "x"}), 200, "yank")
		e.Expect(g.read(art+"?purpose=import", func(a *Assertion) { a.Act = []string{"read", "import"} }), 410, "yank still applies")
	})

	t.Run("invalid assertions are throttled per gateway", func(t *testing.T) {
		te := NewEnv(t, newStore(t), Options{AuthFailuresPerMinute: 3})
		tg := newGateway(t, te, "core-t", []string{"skills:read"}, []string{"openlinker"})
		for i := 0; i < 3; i++ {
			te.Expect(tg.read("/v1/skills", func(a *Assertion) { a.Aud = "https://wrong.example" }), 401, "bad assertion")
		}
		r := te.Expect(tg.read("/v1/skills", func(a *Assertion) { a.Aud = "https://wrong.example" }), 429, "throttled")
		if r.Header.Get("Retry-After") == "" {
			t.Fatal("throttled response needs Retry-After")
		}
		body := []byte(strings.Repeat("x", 1<<20))
		path := "/v1/skills/io.github.x/y/versions?version=1"
		te.Expect(tg.do("POST", path, body, "text/markdown", tg.assertion("POST", path, body, nil)), 429, "throttled before the body is read")
		te.Expect(tg.do("GET", "/v1/skills", nil, "", ""), 200, "the gateway token itself keeps working")
		te2 := NewEnv(t, newStore(t), Options{AuthFailuresPerMinute: 2})
		tg2 := newGateway(t, te2, "core-u", []string{"skills:read"}, []string{"openlinker"})
		huge := []byte(strings.Repeat("x", 5<<20))
		for i := 0; i < 2; i++ {
			te2.Expect(tg2.do("POST", path, huge, "text/markdown", "not.a.jws"), 413, "oversized body")
		}
		te2.Expect(tg2.do("POST", path, huge, "text/markdown", "not.a.jws"), 429, "oversized bodies count against the budget")
	})

	t.Run("key revocation and rotation", func(t *testing.T) {
		pub2, priv2, _ := ed25519.GenerateKey(rand.Reader)
		e.Expect(e.Do("POST", "/v1/admin/gateways/core-a/keys", AdminToken, "", map[string]any{"kid": "k2", "public_key": base64.StdEncoding.EncodeToString(pub2)}), 201, "second key")
		k2 := g
		k2.key, k2.kid = priv2, "k2"
		e.Expect(k2.read("/v1/skills", nil), 200, "new key")
		e.Expect(g.read("/v1/skills", nil), 200, "old key still valid")
		e.Expect(e.Do("POST", "/v1/admin/gateways/core-a/keys/k1/revoke", AdminToken, "", nil), 200, "revoke k1")
		e.Expect(g.read("/v1/skills", nil), 401, "revoked key")
		e.Expect(k2.read("/v1/skills", nil), 200, "other key unaffected")
		e.Expect(e.Do("POST", "/v1/admin/tokens/"+g.tokenID+"/revoke", AdminToken, "", nil), 200, "revoke token")
		e.Expect(k2.read("/v1/skills", nil), 401, "revoked consumer token")
	})
}
