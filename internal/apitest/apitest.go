// Package apitest is an end-to-end HTTP suite run against any store.Store
// implementation (in-memory always; Postgres when a DSN is configured). It
// uses a fake GitHub server and never touches the real network.
package apitest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/api"
	"github.com/OpenLinker-ai/openlinker-registry/internal/auth"
	"github.com/OpenLinker-ai/openlinker-registry/internal/blobstore"
	"github.com/OpenLinker-ai/openlinker-registry/internal/github"
	"github.com/OpenLinker-ai/openlinker-registry/internal/github/githubtest"
	"github.com/OpenLinker-ai/openlinker-registry/internal/ratelimit"
	"github.com/OpenLinker-ai/openlinker-registry/internal/registry"
	"github.com/OpenLinker-ai/openlinker-registry/internal/safehttp"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

const (
	AdminToken = "admin-bootstrap-token-0123456789abcdef"
	PublicURL  = "https://reg.example/registry"
	C1         = "1111111111111111111111111111111111111111"
)

func md(name, desc, license string) []byte {
	s := "---\nname: " + name + "\ndescription: " + desc + "\n"
	if license != "" {
		s += "license: " + license + "\n"
	}
	return []byte(s + "---\nInstructions for " + name + ".\n")
}

// Env is a running registry for one test.
type Env struct {
	T       *testing.T
	Srv     *httptest.Server
	Fake    *githubtest.Fake
	Store   store.Store
	BlobDir string
	Base    string
	Svc     *registry.Service
	// Headers are added to every request sent with Do.
	Headers map[string]string
}

// noRedirect reports redirects instead of following them.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// Options tweak the environment.
type Options struct {
	Mode          string
	AllowAnonRead bool
	// UI serves the built-in pages.
	UI bool
	// Loopback lets the instance reach upstreams on this machine over http
	// (other test registries). Without it the production outbound policy
	// applies.
	Loopback bool
	// UpstreamTokens are bearer tokens by upstream name.
	UpstreamTokens map[string]string
	// ForwardPerMinute limits forwarded misses per client (0 = unlimited).
	ForwardPerMinute int
	// RealPublicURL makes the instance publish URLs that point at the test
	// server instead of the fixed PublicURL, so other instances can fetch them.
	RealPublicURL bool
	// VerifyInterval overrides how long verified indexed content is served
	// from the cache (default one hour; negative means verify every read).
	VerifyInterval time.Duration
}

// NewEnv starts a registry over st.
func NewEnv(t *testing.T, st store.Store, o Options) *Env {
	t.Helper()
	if o.Mode == "" {
		o.Mode, o.AllowAnonRead = "public", true
	}
	f := githubtest.New("acme", "skills")
	t.Cleanup(f.Close)
	f.AddCommit(C1, map[string]githubtest.Entry{
		"skills/demo/SKILL.md":       {Data: md("demo", "Indexed demo", "MIT")},
		"skills/demo/scripts/run.sh": {Data: []byte("#!/bin/sh\r\necho é\n"), Mode: "100755"},
		"skills/nolic/SKILL.md":      {Data: md("nolic", "No license", "")},
		"skills/nolic/extra.md":      {Data: []byte("extra")},
		"skills/single/SKILL.md":     {Data: md("single", "Single file no license", "")},
		"skills/custom/SKILL.md":     {Data: md("custom", "Custom license", "Proprietary")},
		"skills/custom/LICENSE":      {Data: []byte("All rights reserved, redistribution permitted.")},
	}, "main")
	gh, err := github.New(github.Config{APIBase: f.API.URL, RawBase: f.Raw.URL})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	blobs, err := blobstore.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	var policy safehttp.Policy
	if o.Loopback {
		policy = safehttp.Policy{AllowHTTP: true, AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}}
	}
	verify := time.Hour
	if o.VerifyInterval != 0 {
		verify = max(o.VerifyInterval, 0)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := registry.New(st, blobs, gh, registry.Config{Mode: o.Mode, AllowPublic: true, PublicURL: PublicURL, VerifyInterval: verify,
		UpstreamPolicy: policy, UpstreamToken: func(name string) string { return o.UpstreamTokens[name] }})
	svc.Log = quiet
	srv := &api.Server{Svc: svc, Auth: auth.NewAuthenticator(st, AdminToken), BasePath: "/registry",
		AllowAnonRead: o.AllowAnonRead, MaxUploadBytes: 4 << 20, Log: quiet, UI: o.UI,
		ForwardLimit: ratelimit.New(o.ForwardPerMinute)}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	if o.RealPublicURL {
		svc.Cfg.PublicURL = hs.URL + "/registry"
	}
	return &Env{T: t, Srv: hs, Fake: f, Store: st, BlobDir: dir, Base: hs.URL + "/registry", Svc: svc}
}

// Resp is a decoded response.
type Resp struct {
	Status int
	Header http.Header
	Body   []byte
	JSON   map[string]any
}

// Do sends a request; body may be nil, []byte or any JSON value.
func (e *Env) Do(method, path, token, contentType string, body any) Resp {
	e.T.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
		if contentType == "" {
			contentType = "application/json"
		}
	}
	req, _ := http.NewRequest(method, e.Base+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range e.Headers {
		req.Header.Set(k, v)
	}
	res, err := noRedirect.Do(req)
	if err != nil {
		e.T.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	r := Resp{Status: res.StatusCode, Header: res.Header, Body: data}
	if strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(data, &r.JSON)
	}
	return r
}

// Expect asserts the status.
func (e *Env) Expect(r Resp, status int, what string) Resp {
	e.T.Helper()
	if r.Status != status {
		e.T.Fatalf("%s: status %d, want %d: %s", what, r.Status, status, r.Body)
	}
	return r
}

func errCode(r Resp) string {
	if m, ok := r.JSON["error"].(map[string]any); ok {
		c, _ := m["code"].(string)
		return c
	}
	return ""
}

func (e *Env) token(scopes []string, roles []map[string]string) string {
	r := e.Expect(e.Do("POST", "/v1/admin/tokens", AdminToken, "", map[string]any{"name": "t", "scopes": scopes, "roles": roles}), 201, "create token")
	return r.JSON["token"].(string)
}

func sha(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }

var digestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// Run executes the end-to-end suite against stores produced by newStore.
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Run("public instance", func(t *testing.T) { runPublic(t, NewEnv(t, newStore(t), Options{})) })
	t.Run("private instance", func(t *testing.T) { runPrivate(t, NewEnv(t, newStore(t), Options{Mode: "private"})) })
	t.Run("mcp servers", func(t *testing.T) { runMCP(t, NewEnv(t, newStore(t), Options{})) })
	t.Run("upstreams", func(t *testing.T) { runUpstreams(t, newStore) })
	t.Run("built-in pages", func(t *testing.T) { runUI(t, newStore) })
}

func runPublic(t *testing.T, e *Env) {
	// --- infrastructure ---
	r := e.Expect(e.Do("GET", "/healthz", "", "", nil), 200, "healthz")
	for _, h := range []string{"X-Content-Type-Options", "Content-Security-Policy"} {
		if r.Header.Get(h) == "" {
			t.Fatalf("missing security header %s", h)
		}
	}
	if res, _ := http.Get(e.Srv.URL + "/v1/skills"); res.StatusCode != 404 {
		t.Fatal("routes must only be served under the base path")
	}
	e.Expect(e.Do("GET", "/v1/skills", "olr_bogus_token", "", nil), 401, "invalid token")
	e.Expect(e.Do("POST", "/v1/admin/namespaces", "", "", map[string]any{"name": "io.github.acme"}), 401, "anon admin")

	// --- namespaces and tokens ---
	e.Expect(e.Do("POST", "/v1/admin/namespaces", AdminToken, "", map[string]any{"name": "io.github.acme", "kind": "github", "github_id": 1}), 201, "ns")
	e.Expect(e.Do("POST", "/v1/admin/namespaces", AdminToken, "", map[string]any{"name": "io.github.other", "kind": "github", "github_id": 2}), 201, "ns2")
	r = e.Expect(e.Do("POST", "/v1/admin/namespaces", AdminToken, "", map[string]any{"name": "io.github.acme-renamed", "kind": "github", "github_id": 1}), 409, "github id reuse")
	if errCode(r) != "github_id_bound" {
		t.Fatalf("got %s", errCode(r))
	}
	e.Expect(e.Do("POST", "/v1/admin/namespaces", AdminToken, "", map[string]any{"name": "internal.team", "kind": "internal"}), 422, "internal on public")
	e.Expect(e.Do("POST", "/v1/admin/namespaces", AdminToken, "", map[string]any{"name": "com.example", "kind": "domain", "bogus": 1}), 400, "strict json")

	pub := e.token([]string{"skills:read", "skills:write"}, []map[string]string{{"namespace": "io.github.acme", "role": "publisher"}})
	owner := e.token([]string{"skills:read", "skills:write"}, []map[string]string{{"namespace": "io.github.acme", "role": "owner"}})
	outsider := e.token([]string{"skills:read", "skills:write"}, []map[string]string{{"namespace": "io.github.other", "role": "owner"}})
	reader := e.token([]string{"skills:read"}, nil)

	// --- hosted upload ---
	up := func(name, query, token, ct string, body []byte) Resp {
		return e.Do("POST", "/v1/skills/io.github.acme/"+name+"/versions?"+query, token, ct, body)
	}
	e.Expect(up("hello", "version=1.0.0", "", "text/markdown", md("hello", "Says hello", "MIT")), 401, "anon upload")
	e.Expect(up("hello", "version=1.0.0", outsider, "text/markdown", md("hello", "Says hello", "MIT")), 403, "outsider upload")
	e.Expect(up("hello", "version=1.0.0", reader, "text/markdown", md("hello", "Says hello", "MIT")), 403, "reader upload")
	e.Expect(up("Hello", "version=1.0.0", pub, "text/markdown", md("hello", "Says hello", "MIT")), 422, "bad slug")
	e.Expect(up("hello", "version=1.0.0&x=1", pub, "text/markdown", md("hello", "Says hello", "MIT")), 400, "unknown query")
	e.Expect(up("hello", "version=1.0.0", pub, "text/markdown", []byte("no frontmatter")), 422, "no frontmatter")
	e.Expect(up("hello", "version=1.0.0", pub, "application/octet-stream", md("hello", "Says hello", "MIT")), 415, "bad content type")
	helloMD := md("hello", "Says hello", "MIT")
	r = e.Expect(up("hello", "version=1.0.0", pub, "text/markdown", helloMD), 201, "upload hello")
	if r.JSON["artifact"].(map[string]any)["digest"] != sha(helloMD) {
		t.Fatal("skill-md artifact digest must be sha256 of the raw bytes")
	}
	r = e.Expect(up("hello", "version=1.0.0", pub, "text/markdown", md("hello", "Changed", "MIT")), 409, "immutable version")
	if errCode(r) != "version_exists" {
		t.Fatalf("got %s", errCode(r))
	}
	e.Expect(up("nolicense", "version=1", pub, "text/markdown", md("nolicense", "x", "")), 422, "public needs license")
	e.Expect(up("nolicense", "version=1&visibility=internal", pub, "text/markdown", md("nolicense", "Internal one", "")), 201, "internal unknown license")
	e.Expect(e.Do("PATCH", "/v1/skills/io.github.acme/nolicense", owner, "", map[string]any{"visibility": "public"}), 422, "cannot publish unknown license")

	var tgz bytes.Buffer
	zw := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(zw)
	for _, f := range []struct{ n, b string }{{"multi/SKILL.md", string(md("multi", "Multi file", "Apache-2.0"))}, {"multi/ref/notes.md", "notes"}} {
		tw.WriteHeader(&tar.Header{Name: f.n, Mode: 0o600, Size: int64(len(f.b)), Typeflag: tar.TypeReg})
		tw.Write([]byte(f.b))
	}
	tw.Close()
	zw.Close()
	r = e.Expect(up("multi", "version=2.0.0", pub, "application/gzip", tgz.Bytes()), 201, "upload tgz")
	multiArchive := r.JSON["archive"].(map[string]any)["digest"].(string)

	e.Expect(up("secret", "version=1&visibility=private", pub, "text/markdown", md("secret", "Private", "MIT")), 201, "private")
	e.Expect(up("hidden", "version=1&listed=false", pub, "text/markdown", md("hidden", "Unlisted", "MIT")), 201, "unlisted")

	// --- visibility on every read exit ---
	names := func(r Resp) string {
		var out []string
		for _, s := range r.JSON["skills"].([]any) {
			out = append(out, s.(map[string]any)["name"].(string))
		}
		return strings.Join(out, ",")
	}
	if got := names(e.Expect(e.Do("GET", "/v1/skills", "", "", nil), 200, "anon list")); got != "hello,multi" {
		t.Fatalf("anonymous list = %s", got)
	}
	if got := names(e.Expect(e.Do("GET", "/v1/skills", reader, "", nil), 200, "reader list")); got != "hello,multi,nolicense" {
		t.Fatalf("authenticated list = %s", got)
	}
	if got := names(e.Expect(e.Do("GET", "/v1/skills", owner, "", nil), 200, "owner list")); got != "hello,multi,nolicense,secret" {
		t.Fatalf("member list = %s", got)
	}
	if got := names(e.Expect(e.Do("GET", "/v1/skills?q=MULTI", "", "", nil), 200, "search")); got != "multi" {
		t.Fatalf("search = %s", got)
	}
	r = e.Expect(e.Do("GET", "/v1/skills?limit=1", "", "", nil), 200, "page 1")
	cur, _ := r.JSON["next_cursor"].(string)
	if names(r) != "hello" || cur == "" {
		t.Fatal("pagination page 1")
	}
	if got := names(e.Expect(e.Do("GET", "/v1/skills?limit=1&cursor="+cur, "", "", nil), 200, "page 2")); got != "multi" {
		t.Fatalf("page 2 = %s", got)
	}
	for _, p := range []string{"", "/versions/1", "/versions/1/artifact", "/versions/1/files/SKILL.md"} {
		e.Expect(e.Do("GET", "/v1/skills/io.github.acme/secret"+p, "", "", nil), 404, "anon private "+p)
		e.Expect(e.Do("GET", "/v1/skills/io.github.acme/secret"+p, reader, "", nil), 404, "reader private "+p)
		e.Expect(e.Do("GET", "/v1/skills/io.github.acme/secret"+p, owner, "", nil), 200, "owner private "+p)
		e.Expect(e.Do("GET", "/v1/skills/io.github.acme/nolicense"+p, "", "", nil), 404, "anon internal "+p)
		e.Expect(e.Do("GET", "/v1/skills/io.github.acme/nolicense"+p, reader, "", nil), 200, "reader internal "+p)
	}
	e.Expect(e.Do("POST", "/v1/skills/io.github.acme/secret/yank", outsider, "", nil), 403, "outsider yank")
	// Unlisted: readable by name, excluded from list and indexes.
	e.Expect(e.Do("GET", "/v1/skills/io.github.acme/hidden", "", "", nil), 200, "unlisted by name")

	// --- artifacts ---
	r = e.Expect(e.Do("GET", "/v1/skills/io.github.acme/hello/versions/1.0.0/artifact", "", "", nil), 200, "artifact")
	if !bytes.Equal(r.Body, helloMD) || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/plain") || r.Header.Get("Content-Disposition") == "" {
		t.Fatal("skill-md artifact response")
	}
	r = e.Expect(e.Do("GET", "/v1/skills/io.github.acme/multi/versions/2.0.0/archive.tar.gz", "", "", nil), 200, "archive")
	if sha(r.Body) != multiArchive || r.Header.Get("Content-Type") != "application/gzip" {
		t.Fatal("archive digest mismatch")
	}
	r = e.Expect(e.Do("GET", "/v1/skills/io.github.acme/multi/versions/2.0.0/files/ref/notes.md", "", "", nil), 200, "file")
	if string(r.Body) != "notes" {
		t.Fatal("file content")
	}
	e.Expect(e.Do("GET", "/v1/skills/io.github.acme/multi/versions/2.0.0/files/nope.md", "", "", nil), 404, "missing file")
	// Dot segments are cleaned (and redirected) by the router; other unsafe
	// paths are rejected before any lookup.
	e.Expect(e.Do("GET", "/v1/skills/io.github.acme/multi/versions/2.0.0/files/..%5C..%5Cetc", "", "", nil), 400, "unsafe path")

	// --- GitHub preview / index / import ---
	ghURL := "https://github.com/acme/skills/tree/main/skills/demo"
	auditBefore := len(e.Expect(e.Do("GET", "/v1/admin/audit?limit=1000", AdminToken, "", nil), 200, "audit").JSON["events"].([]any))
	e.Expect(e.Do("POST", "/v1/skills/io.github.acme/indexes/preview", "", "", map[string]any{"url": ghURL}), 401, "anon preview")
	r = e.Expect(e.Do("POST", "/v1/skills/io.github.acme/indexes/preview", pub, "", map[string]any{"url": ghURL}), 200, "preview")
	commit, tree := r.JSON["commit"].(string), r.JSON["tree_sha"].(string)
	if commit != C1 || len(tree) != 40 || r.JSON["suggested_name"] != "demo" {
		t.Fatalf("preview %v", r.JSON)
	}
	e.Expect(e.Do("POST", "/v1/skills/io.github.acme/indexes/preview", pub, "", map[string]any{"url": ghURL, "ref": "feat/x"}), 400, "slash ref")
	if n := len(e.Expect(e.Do("GET", "/v1/admin/audit?limit=1000", AdminToken, "", nil), 200, "audit").JSON["events"].([]any)); n != auditBefore {
		t.Fatal("preview must not write")
	}
	e.Expect(e.Do("GET", "/v1/skills/io.github.acme/demo", "", "", nil), 404, "preview created nothing")
	e.Expect(e.Do("POST", "/v1/skills/io.github.acme/indexes", pub, "", map[string]any{"url": ghURL}), 422, "index needs preview lock")
	r = e.Expect(e.Do("POST", "/v1/skills/io.github.acme/indexes", pub, "", map[string]any{"url": ghURL, "commit": commit,
		"expected_tree_sha": strings.Repeat("0", 40)}), 409, "tree mismatch")
	if errCode(r) != "tree_sha_mismatch" {
		t.Fatalf("got %s", errCode(r))
	}
	e.Expect(e.Do("POST", "/v1/skills/io.github.acme/indexes", pub, "", map[string]any{"url": ghURL, "commit": commit,
		"expected_tree_sha": tree, "license": "MIT"}), 422, "indexer cannot assert license")
	r = e.Expect(e.Do("POST", "/v1/skills/io.github.acme/indexes", pub, "", map[string]any{"url": ghURL, "commit": commit,
		"expected_tree_sha": tree, "version": "1"}), 201, "index")
	if r.JSON["mode"] != "indexed" || r.JSON["locator"].(map[string]any)["tree_sha"] != tree {
		t.Fatalf("index %v", r.JSON)
	}
	demoArchive := r.JSON["archive"].(map[string]any)["digest"].(string)
	r = e.Expect(e.Do("POST", "/v1/skills/io.github.acme/indexes", pub, "", map[string]any{"url": ghURL, "commit": commit,
		"expected_tree_sha": tree, "name": "demo-again"}), 409, "duplicate source")
	if errCode(r) != "source_already_indexed" {
		t.Fatalf("got %s", errCode(r))
	}
	// A conflict from another namespace must not reveal which entry holds
	// the source.
	r = e.Expect(e.Do("POST", "/v1/skills/io.github.other/indexes", outsider, "", map[string]any{"url": ghURL, "commit": commit,
		"expected_tree_sha": tree, "name": "copy"}), 409, "duplicate source elsewhere")
	if strings.Contains(string(r.Body), "acme") || strings.Contains(string(r.Body), "demo") {
		t.Fatalf("conflict leaks the existing entry: %s", r.Body)
	}

	// Indexed read: cache hit, then regenerate after eviction with identical bytes.
	first := e.Expect(e.Do("GET", "/v1/skills/io.github.acme/demo/versions/1/archive.tar.gz", "", "", nil), 200, "indexed archive").Body
	if sha(first) != demoArchive {
		t.Fatal("indexed archive digest")
	}
	hits := e.Fake.RawHits.Load()
	e.Expect(e.Do("GET", "/v1/skills/io.github.acme/demo/versions/1/files/scripts/run.sh", "", "", nil), 200, "cached file")
	if e.Fake.RawHits.Load() != hits {
		t.Fatal("cache hit must not refetch")
	}
	evict := func() {
		os.RemoveAll(filepath.Join(e.BlobDir, "cache"))
		os.MkdirAll(filepath.Join(e.BlobDir, "cache", "sha256"), 0o750)
	}
	evict()
	again := e.Expect(e.Do("GET", "/v1/skills/io.github.acme/demo/versions/1/archive.tar.gz", "", "", nil), 200, "regenerated").Body
	if !bytes.Equal(first, again) || e.Fake.RawHits.Load() == hits {
		t.Fatal("regenerated archive must refetch and be byte-identical")
	}
	// Cached content is only trusted within the verification interval: the
	// source changes while bytes are cached, a read inside the interval is
	// served from cache, and the first read after it re-verifies and fails.
	e.Fake.Tamper(C1, "skills/demo/SKILL.md", md("demo", "Tampered", "MIT"))
	e.Expect(e.Do("GET", "/v1/skills/io.github.acme/demo/versions/1/artifact", "", "", nil), 200, "within verify interval")
	realNow := e.Svc.Now
	e.Svc.Now = func() time.Time { return realNow().Add(2 * time.Hour) }
	r = e.Expect(e.Do("GET", "/v1/skills/io.github.acme/demo/versions/1/artifact", "", "", nil), 409, "cached but stale verification")
	if errCode(r) != "source_changed" {
		t.Fatalf("got %s", errCode(r))
	}
	// Source changed: tampered raw bytes after eviction.
	evict()
	e.Fake.Tamper(C1, "skills/demo/SKILL.md", md("demo", "Tampered", "MIT"))
	r = e.Expect(e.Do("GET", "/v1/skills/io.github.acme/demo/versions/1/artifact", "", "", nil), 409, "source changed")
	if errCode(r) != "source_changed" {
		t.Fatalf("got %s", errCode(r))
	}
	r = e.Expect(e.Do("GET", "/v1/skills/io.github.acme/demo/versions/1", "", "", nil), 200, "version")
	if r.JSON["check"].(map[string]any)["result"] != "source_changed" {
		t.Fatal("check result not recorded")
	}

	// Unknown license: metadata only.
	nolic := "https://github.com/acme/skills/tree/main/skills/nolic"
	p := e.Expect(e.Do("POST", "/v1/skills/io.github.acme/indexes/preview", pub, "", map[string]any{"url": nolic}), 200, "preview nolic")
	e.Expect(e.Do("POST", "/v1/skills/io.github.acme/indexes", pub, "", map[string]any{"url": nolic, "commit": p.JSON["commit"],
		"expected_tree_sha": p.JSON["tree_sha"], "version": "1"}), 201, "index nolic")
	for _, path := range []string{"/artifact", "/archive.tar.gz", "/files/SKILL.md"} {
		r = e.Expect(e.Do("GET", "/v1/skills/io.github.acme/nolic/versions/1"+path, "", "", nil), 403, "unknown license "+path)
		if errCode(r) != "license_unknown" || r.JSON["error"].(map[string]any)["source_url"] == nil {
			t.Fatalf("unknown license response %v", r.JSON)
		}
	}
	r = e.Expect(e.Do("GET", "/v1/skills/io.github.acme/nolic/versions/1", "", "", nil), 200, "metadata ok")
	if len(r.JSON["files"].([]any)) != 2 {
		t.Fatal("manifest must still be visible")
	}
	// Single public file with unknown license: indexed via raw URL.
	single := "https://github.com/acme/skills/blob/main/skills/single/SKILL.md"
	p = e.Expect(e.Do("POST", "/v1/skills/io.github.acme/indexes/preview", pub, "", map[string]any{"url": single}), 200, "preview single")
	e.Expect(e.Do("POST", "/v1/skills/io.github.acme/indexes", pub, "", map[string]any{"url": single, "commit": p.JSON["commit"],
		"expected_tree_sha": p.JSON["tree_sha"], "version": "1"}), 201, "index single")
	// Custom license: pending review until approved.
	custom := "https://github.com/acme/skills/tree/main/skills/custom"
	p = e.Expect(e.Do("POST", "/v1/skills/io.github.acme/indexes/preview", pub, "", map[string]any{"url": custom}), 200, "preview custom")
	e.Expect(e.Do("POST", "/v1/skills/io.github.acme/indexes", pub, "", map[string]any{"url": custom, "commit": p.JSON["commit"],
		"expected_tree_sha": p.JSON["tree_sha"], "version": "1"}), 201, "index custom")
	r = e.Expect(e.Do("GET", "/v1/skills/io.github.acme/custom/versions/1/files/LICENSE", "", "", nil), 403, "custom pending")
	if errCode(r) != "license_pending_review" {
		t.Fatalf("got %s", errCode(r))
	}
	e.Expect(e.Do("POST", "/v1/admin/skills/io.github.acme/custom/versions/1/approve-license", pub, "", nil), 403, "non-admin approve")
	e.Expect(e.Do("POST", "/v1/admin/skills/io.github.acme/custom/versions/1/approve-license", AdminToken, "", nil), 200, "approve")
	e.Expect(e.Do("GET", "/v1/skills/io.github.acme/custom/versions/1/files/LICENSE", "", "", nil), 200, "custom approved")

	// Hosted GitHub import keeps bytes permanently.
	p = e.Expect(e.Do("POST", "/v1/skills/io.github.acme/github-imports/preview", pub, "", map[string]any{"url": nolic}), 200, "import preview")
	e.Expect(e.Do("POST", "/v1/skills/io.github.acme/github-imports", pub, "", map[string]any{"url": nolic, "commit": p.JSON["commit"],
		"expected_tree_sha": p.JSON["tree_sha"], "name": "imported"}), 422, "public import needs license")
	r = e.Expect(e.Do("POST", "/v1/skills/io.github.acme/github-imports", pub, "", map[string]any{"url": nolic, "commit": p.JSON["commit"],
		"expected_tree_sha": p.JSON["tree_sha"], "name": "imported", "license": "MIT"}), 201, "import")
	if r.JSON["source"].(map[string]any)["commit"] != C1 || r.JSON["version"] != "git-111111111111" {
		t.Fatalf("import %v", r.JSON)
	}
	e.Fake.Down.Store(true)
	e.Expect(e.Do("GET", "/v1/skills/io.github.acme/imported/versions/git-111111111111/files/extra.md", "", "", nil), 200, "hosted survives source outage")
	evict()
	r = e.Expect(e.Do("GET", "/v1/skills/io.github.acme/custom/versions/1/artifact", "", "", nil), 502, "source unavailable")
	if errCode(r) != "source_unavailable" {
		t.Fatalf("got %s", errCode(r))
	}
	e.Fake.Down.Store(false)
	// A later successful re-verification restores the check result.
	e.Expect(e.Do("GET", "/v1/skills/io.github.acme/custom/versions/1/artifact", "", "", nil), 200, "source back")

	// --- well-known discovery ---
	wk := func(path string) map[string]any {
		r := e.Expect(e.Do("GET", path, "", "", nil), 200, path)
		if r.JSON["$schema"] != registry.DiscoverySchema {
			t.Fatalf("schema %v", r.JSON["$schema"])
		}
		out := map[string]any{}
		for _, s := range r.JSON["skills"].([]any) {
			m := s.(map[string]any)
			if len(m) != 5 {
				t.Fatalf("entry must have exactly name,type,description,url,digest: %v", m)
			}
			u := m["url"].(string)
			if !digestRe.MatchString(m["digest"].(string)) || (m["type"] != "skill-md" && m["type"] != "archive") ||
				!skill.ValidSlug(m["name"].(string)) || strings.ContainsAny(u, "?#@") || !strings.HasPrefix(u, "http") {
				t.Fatalf("invalid entry %v", m)
			}
			out[m["name"].(string)] = m
		}
		return out
	}
	ns := wk("/ns/io.github.acme/.well-known/agent-skills/index.json")
	keys := func(m map[string]any) string {
		var k []string
		for n := range m {
			k = append(k, n)
		}
		slices.Sort(k)
		return strings.Join(k, ",")
	}
	// demo: source_changed; nolic: unknown license multi-file; hidden: unlisted;
	// nolicense: internal; secret: private.
	if got := keys(ns); got != "custom,hello,imported,multi,single" {
		t.Fatalf("namespace index = %s", got)
	}
	if ns["single"].(map[string]any)["url"] != e.Fake.Raw.URL+"/acme/skills/"+C1+"/skills/single/SKILL.md" {
		t.Fatalf("single-file indexed entry must point at the commit raw URL: %v", ns["single"])
	}
	if ns["multi"].(map[string]any)["digest"] != multiArchive || ns["hello"].(map[string]any)["type"] != "skill-md" {
		t.Fatal("hosted entries")
	}
	// Every advertised URL serves bytes matching the digest.
	for name, v := range ns {
		m := v.(map[string]any)
		u := m["url"].(string)
		if strings.HasPrefix(u, PublicURL) {
			r := e.Expect(e.Do("GET", strings.TrimPrefix(u, PublicURL), "", "", nil), 200, "fetch "+name)
			if sha(r.Body) != m["digest"] {
				t.Fatalf("%s: digest mismatch", name)
			}
		}
	}
	if got := keys(wk("/.well-known/agent-skills/index.json")); got != "" {
		t.Fatalf("site index must only contain aliased entries, got %s", got)
	}
	e.Expect(e.Do("PUT", "/v1/admin/skills/io.github.acme/hello/alias", AdminToken, "", map[string]any{"alias": "hello-world"}), 200, "alias")
	e.Expect(e.Do("PUT", "/v1/admin/skills/io.github.acme/multi/alias", AdminToken, "", map[string]any{"alias": "hello-world"}), 409, "alias taken")
	e.Expect(e.Do("PUT", "/v1/admin/skills/io.github.acme/hidden/alias", AdminToken, "", map[string]any{"alias": "hidden"}), 200, "alias unlisted")
	if got := keys(wk("/.well-known/agent-skills/index.json")); got != "hello-world" {
		t.Fatalf("site index = %s", got)
	}
	// Unknown namespaces and namespaces without public entries look the same.
	for _, n := range []string{"io.github.nobody", "io.github.other"} {
		r := e.Expect(e.Do("GET", "/ns/"+n+"/.well-known/agent-skills/index.json", "", "", nil), 200, "empty ns index "+n)
		if string(r.Body) != `{"$schema":"`+registry.DiscoverySchema+`","skills":[]}`+"\n" {
			t.Fatalf("%s index = %s", n, r.Body)
		}
	}

	// --- status changes ---
	e.Expect(e.Do("PATCH", "/v1/skills/io.github.acme/hello", pub, "", map[string]any{"listed": false}), 403, "publisher cannot patch")
	e.Expect(e.Do("POST", "/v1/skills/io.github.acme/hello/deprecate", pub, "", map[string]any{"reason": "use v2"}), 200, "deprecate")
	if got := names(e.Expect(e.Do("GET", "/v1/skills", "", "", nil), 200, "list")); !strings.Contains(got, "hello") {
		t.Fatal("deprecated skills stay listed")
	}
	e.Expect(e.Do("GET", "/v1/skills/io.github.acme/hello/versions/1.0.0/artifact", "", "", nil), 200, "deprecated downloadable")
	if got := keys(wk("/.well-known/agent-skills/index.json")); got != "" {
		t.Fatalf("deprecated must leave the index: %s", got)
	}
	e.Expect(e.Do("POST", "/v1/skills/io.github.acme/multi/versions/2.0.0/yank", pub, "", map[string]any{"reason": "bad"}), 200, "yank version")
	e.Expect(e.Do("GET", "/v1/skills/io.github.acme/multi/versions/2.0.0/archive.tar.gz", "", "", nil), 410, "yanked version")
	e.Expect(e.Do("POST", "/v1/skills/io.github.acme/single/yank", pub, "", nil), 200, "yank skill")
	e.Expect(e.Do("GET", "/v1/skills/io.github.acme/single/versions/1/artifact", owner, "", nil), 410, "yanked skill")
	if got := keys(wk("/ns/io.github.acme/.well-known/agent-skills/index.json")); got != "custom,imported" {
		t.Fatalf("index after yank = %s", got)
	}
	e.Expect(e.Do("PATCH", "/v1/skills/io.github.acme/secret", owner, "", map[string]any{"visibility": "public"}), 200, "owner patch")
	e.Expect(e.Do("GET", "/v1/skills/io.github.acme/secret", "", "", nil), 200, "now public")

	// Revoked tokens stop working.
	r = e.Expect(e.Do("POST", "/v1/admin/tokens", AdminToken, "", map[string]any{"name": "tmp", "scopes": []string{"skills:read"}}), 201, "tmp token")
	e.Expect(e.Do("GET", "/v1/skills", r.JSON["token"].(string), "", nil), 200, "tmp works")
	e.Expect(e.Do("POST", "/v1/admin/tokens/"+r.JSON["id"].(string)+"/revoke", AdminToken, "", nil), 200, "revoke")
	e.Expect(e.Do("GET", "/v1/skills", r.JSON["token"].(string), "", nil), 401, "revoked")

	// Cache headers: shared caching only for anonymous reads of public content.
	r = e.Expect(e.Do("GET", "/v1/skills/io.github.acme/custom/versions/1/files/LICENSE", "", "", nil), 200, "anon public file")
	if r.Header.Get("Cache-Control") != "public, max-age=60" || !strings.Contains(r.Header.Get("Vary"), "Authorization") {
		t.Fatalf("anonymous public caching headers: %v", r.Header)
	}
	r = e.Expect(e.Do("GET", "/v1/skills/io.github.acme/custom/versions/1/files/LICENSE", reader, "", nil), 200, "auth public file")
	if r.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("authenticated response must not be shared-cacheable: %v", r.Header.Get("Cache-Control"))
	}
	r = e.Expect(e.Do("GET", "/v1/skills/io.github.acme/nolicense/versions/1/artifact", reader, "", nil), 200, "internal artifact")
	if r.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatal("internal artifact must not be cacheable")
	}
	r = e.Expect(e.Do("GET", "/.well-known/agent-skills/index.json", "", "", nil), 200, "index headers")
	if r.Header.Get("Cache-Control") != "public, max-age=60" || !strings.Contains(r.Header.Get("Vary"), "Authorization") {
		t.Fatalf("index cache headers: %v", r.Header)
	}

	// Custom licenses need non-empty text, which is kept and exposed.
	tarOf := func(files map[string]string) []byte {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		for _, n := range []string{"SKILL.md", "LICENSE"} {
			tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg})
			tw.Write([]byte(files[n]))
		}
		tw.Close()
		return b.Bytes()
	}
	cmd := string(md("lic", "Custom licensed", "custom"))
	r = e.Expect(up("lic", "version=1", pub, "application/x-tar", tarOf(map[string]string{"SKILL.md": cmd, "LICENSE": "  \n"})), 422, "empty LICENSE")
	if errCode(r) != "license_text_required" {
		t.Fatalf("got %s", errCode(r))
	}
	r = e.Expect(up("lic", "version=1", pub, "application/x-tar", tarOf(map[string]string{"SKILL.md": cmd, "LICENSE": "Use freely, keep this notice.\n"})), 201, "custom LICENSE")
	if r.JSON["license"].(map[string]any)["text"] != "Use freely, keep this notice." {
		t.Fatalf("license text not exposed: %v", r.JSON["license"])
	}

	// Failed publishes can leave orphan artifacts; GC removes only those.
	orphan := sha(md("hello", "Changed", "MIT"))
	if _, err := e.Svc.Blobs.Get(context.Background(), blobstore.Artifacts, orphan); err != nil {
		t.Fatalf("expected the conflicting publish to leave an orphan blob: %v", err)
	}
	n, err := e.Svc.GCArtifacts(context.Background(), time.Now().Add(time.Hour))
	if err != nil || n < 1 {
		t.Fatalf("gc removed %d: %v", n, err)
	}
	if _, err := e.Svc.Blobs.Get(context.Background(), blobstore.Artifacts, orphan); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("orphan survived gc: %v", err)
	}
	e.Expect(e.Do("GET", "/v1/skills/io.github.acme/hello/versions/1.0.0/artifact", "", "", nil), 200, "referenced artifact kept")
	if n, _ := e.Svc.GCArtifacts(context.Background(), time.Now().Add(-time.Hour)); n != 0 {
		t.Fatal("gc must respect the grace period")
	}

	// Audit never contains token plaintext.
	audit := e.Expect(e.Do("GET", "/v1/admin/audit?limit=1000", AdminToken, "", nil), 200, "audit")
	if bytes.Contains(audit.Body, []byte(pub)) || bytes.Contains(audit.Body, []byte(AdminToken)) {
		t.Fatal("audit leaked a token")
	}
}

func runPrivate(t *testing.T, e *Env) {
	e.Expect(e.Do("GET", "/v1/skills", "", "", nil), 401, "anon list on private")
	e.Expect(e.Do("GET", "/.well-known/agent-skills/index.json", "", "", nil), 401, "anon index on private")
	e.Expect(e.Do("POST", "/v1/admin/namespaces", AdminToken, "", map[string]any{"name": "internal.team", "kind": "internal"}), 201, "internal ns")
	e.Expect(e.Do("POST", "/v1/admin/namespaces", AdminToken, "", map[string]any{"name": "ai.openlinker.bob", "kind": "openlinker"}), 422, "openlinker ns on private")
	pub := e.token([]string{"skills:read", "skills:write"}, []map[string]string{{"namespace": "internal.team", "role": "publisher"}})
	r := e.Expect(e.Do("POST", "/v1/skills/internal.team/tool/versions?version=1", pub, "text/markdown", md("tool", "Team tool", "")), 201, "default internal")
	_ = r
	r = e.Expect(e.Do("GET", "/v1/skills/internal.team/tool", pub, "", nil), 200, "read")
	if r.JSON["visibility"] != "internal" {
		t.Fatalf("private instance default visibility = %v", r.JSON["visibility"])
	}
	other := e.token([]string{"skills:read"}, nil)
	e.Expect(e.Do("GET", "/v1/skills/internal.team/tool/versions/1/artifact", other, "", nil), 200, "internal readable by any user")
}
