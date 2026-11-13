package upstream_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/OpenLinker-ai/openlinker-registry/internal/safehttp"
	"github.com/OpenLinker-ai/openlinker-registry/internal/upstream"
)

const token = "olr_test-token-5f2c9a"

var ctx = context.Background()

func policy() safehttp.Policy {
	return safehttp.Policy{AllowHTTP: true, AllowCIDRs: []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128"),
	}}
}

func newClient(base string) *upstream.Client {
	return &upstream.Client{HTTP: safehttp.NewClient(policy()), BaseURL: base, Token: token}
}

// fake is an httptest server that records every request it receives.
type fake struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
}

func newFake(t *testing.T, h http.HandlerFunc) *fake {
	t.Helper()
	f := &fake{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Clone(context.Background()))
		f.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fake) requests() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.reqs...)
}

func (f *fake) uris() []string {
	var out []string
	for _, r := range f.requests() {
		out = append(out, r.RequestURI)
	}
	return out
}

func (f *fake) port() string {
	_, port, _ := net.SplitHostPort(f.Listener.Addr().String())
	return port
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

func ucode(err error) string {
	var ue *upstream.Error
	if errors.As(err, &ue) {
		return ue.Code
	}
	return ""
}

func scode(err error) (string, int) {
	var se *safehttp.Error
	if errors.As(err, &se) {
		return se.Code, se.Status
	}
	return "", 0
}

func wantNotFound(t *testing.T, what string, err error) {
	t.Helper()
	c, status := scode(err)
	if !errors.Is(err, upstream.ErrNotFound) || c != safehttp.CodeStatus || status != http.StatusNotFound {
		t.Fatalf("%s: err = %v, want ErrNotFound wrapping a 404 status error", what, err)
	}
}

func TestBaseURLValidation(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, `{"servers":[]}`) })
	for _, base := range []string{
		"", "example.com", "/registry", "ftp://example.com", "https://", "https:///x",
		"https://example.com/registry?x=1", "https://example.com/registry?", "https://example.com/#f",
		"https://user:" + token + "@example.com/registry", "://bad",
	} {
		c := newClient(base)
		_, _, err := c.ListMCP(ctx, upstream.MCPListOptions{})
		if ucode(err) != upstream.CodeInvalidURL {
			t.Errorf("base %q: err = %v, want invalid_url", base, err)
		}
		if err != nil && strings.Contains(err.Error(), token) {
			t.Errorf("base %q: error echoes the base URL: %v", base, err)
		}
	}
	// Trailing slashes are ignored and a path prefix is kept.
	c := newClient(f.URL + "/registry//")
	if _, _, err := c.ListMCP(ctx, upstream.MCPListOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := f.uris(); len(got) != 1 || got[0] != "/registry/v0.1/servers" {
		t.Fatalf("requests = %v", got)
	}
	// No HTTP client is a programming error reported as an error, not a panic.
	if _, _, err := (&upstream.Client{BaseURL: f.URL}).ListMCP(ctx, upstream.MCPListOptions{}); ucode(err) != upstream.CodeInvalidArgument {
		t.Fatalf("nil HTTP: %v", err)
	}
}

func TestRequestHeaders(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, `{"servers":[]}`) })
	c := newClient(f.URL)
	if _, _, err := c.ListMCP(ctx, upstream.MCPListOptions{}); err != nil {
		t.Fatal(err)
	}
	c.UserAgent = "custom-agent/1"
	c.Token = ""
	if _, _, err := c.ListMCP(ctx, upstream.MCPListOptions{}); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if h := reqs[0].Header; h.Get("Authorization") != "Bearer "+token || h.Get("User-Agent") != "openlinker-registry" || h.Get("Accept") != "application/json" {
		t.Fatalf("headers = %v", h)
	}
	if h := reqs[1].Header; h.Get("User-Agent") != "custom-agent/1" || len(h.Values("Authorization")) != 0 {
		t.Fatalf("headers = %v", h)
	}
	for _, r := range reqs {
		if r.Method != http.MethodGet || strings.Contains(r.RequestURI, token) {
			t.Fatalf("%s %s", r.Method, r.RequestURI)
		}
	}
}

// The token goes to BaseURL's scheme+host+port and nowhere else.
func TestTokenOnlyToBaseOrigin(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	other := newFake(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("other")) })
	var base *fake
	base = newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/agent-skills/index.json" {
			writeJSON(w, fmt.Sprintf(`{"skills":[
				{"name":"local","type":"skill-md","description":"","url":"/files/local/SKILL.md","digest":%q},
				{"name":"remote","type":"archive","description":"","url":"%s/remote.tar.gz","digest":%q}]}`, digest, other.URL, digest))
			return
		}
		_, _ = w.Write([]byte("base"))
	})
	c := newClient(base.URL)
	index, err := c.WellKnownIndex(ctx)
	entries, dropped := index.Entries, index.Dropped
	if err != nil || dropped != 0 || len(entries) != 2 || !index.Complete {
		t.Fatalf("%+v %v", index, err)
	}
	for i, want := range []string{"base", "other"} {
		body, err := c.Fetch(ctx, entries[i].URL, 1<<10)
		if err != nil || string(body) != want {
			t.Fatalf("fetch %s: %q %v", entries[i].URL, body, err)
		}
	}
	// Same host, different port; and the same port under another host name.
	if _, err := c.Fetch(ctx, other.URL+"/again?x=1", 1<<10); err != nil {
		t.Fatal(err)
	}
	byName := "http://localhost:" + base.port() + "/by-name"
	if _, err := net.LookupHost("localhost"); err == nil {
		if _, err := c.Fetch(ctx, byName, 1<<10); err != nil {
			t.Fatal(err)
		}
	}
	// Spelling differences of the same origin still get the token.
	if _, err := c.Fetch(ctx, strings.Replace(base.URL, "http://", "HTTP://", 1)+"/same-origin", 1<<10); err != nil {
		t.Fatal(err)
	}

	for _, r := range other.requests() {
		if len(r.Header.Values("Authorization")) != 0 {
			t.Fatalf("another origin received Authorization on %s", r.RequestURI)
		}
		if strings.Contains(r.RequestURI, token) {
			t.Fatalf("token in URL: %s", r.RequestURI)
		}
	}
	if n := len(other.requests()); n != 2 {
		t.Fatalf("other origin received %d requests, want 2", n)
	}
	for _, r := range base.requests() {
		got := r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/by-name":
			if got != "" {
				t.Fatalf("a different host name on the same port received the token")
			}
		default:
			if got != "Bearer "+token {
				t.Fatalf("%s: Authorization = %q", r.RequestURI, got)
			}
		}
	}
}

func TestFetchValidation(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("0123456789")) })
	c := newClient(f.URL)
	for _, u := range []string{
		"", "relative/path", "/abs/path", "//host/path", "ftp://example.com/x", "file:///etc/passwd",
		"http://user:pw@127.0.0.1/x", f.URL + "/x#frag", "http://", "https:opaque",
	} {
		if _, err := c.Fetch(ctx, u, 100); ucode(err) != upstream.CodeInvalidURL {
			t.Errorf("Fetch(%q) = %v, want invalid_url", u, err)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("%d requests were sent for invalid URLs", n)
	}
	if body, err := c.Fetch(ctx, f.URL+"/ok", 10); err != nil || string(body) != "0123456789" {
		t.Fatalf("%q %v", body, err)
	}
	_, err := c.Fetch(ctx, f.URL+"/ok", 9)
	if code, _ := scode(err); code != safehttp.CodeTooLarge {
		t.Fatalf("err = %v, want too_large", err)
	}
	// The address policy of the HTTP client applies to Fetch targets.
	strict := &upstream.Client{HTTP: safehttp.NewClient(safehttp.Policy{AllowHTTP: true}), BaseURL: "http://registry.example", Token: token}
	_, err = strict.Fetch(ctx, f.URL+"/blocked", 10)
	if code, _ := scode(err); code != safehttp.CodeAddressNotAllowed {
		t.Fatalf("err = %v, want address_not_allowed", err)
	}
	_, err = newClient("https://registry.example").Fetch(ctx, "http://169.254.169.254/latest/meta-data", 10)
	if code, _ := scode(err); code != safehttp.CodeAddressNotAllowed {
		t.Fatalf("err = %v, want address_not_allowed", err)
	}
}

func TestRedirectIsAnErrorAndNotFollowed(t *testing.T) {
	target := newFake(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, `{"servers":[]}`) })
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	})
	c := newClient(f.URL)
	calls := map[string]func() error{
		"ListMCP":          func() error { _, _, err := c.ListMCP(ctx, upstream.MCPListOptions{}); return err },
		"GetMCP":           func() error { _, err := c.GetMCP(ctx, "io.github.a/b", "1.0.0"); return err },
		"ListMCPVersions":  func() error { _, _, err := c.ListMCPVersions(ctx, "io.github.a/b"); return err },
		"ListSkills":       func() error { _, _, err := c.ListSkills(ctx, "", 0); return err },
		"GetSkill":         func() error { _, err := c.GetSkill(ctx, "io.github.alice", "demo"); return err },
		"GetSkillVersion":  func() error { _, err := c.GetSkillVersion(ctx, "io.github.alice", "demo", "1.0.0"); return err },
		"GetSkillArtifact": func() error { _, err := c.GetSkillArtifact(ctx, "io.github.alice", "demo", "1.0.0", 100); return err },
		"WellKnownIndex":   func() error { _, err := c.WellKnownIndex(ctx); return err },
		"Fetch":            func() error { _, err := c.Fetch(ctx, f.URL+"/file", 100); return err },
	}
	for name, call := range calls {
		err := call()
		if code, status := scode(err); code != safehttp.CodeRedirectNotFollowed || status != http.StatusFound {
			t.Errorf("%s: err = %v, want redirect_not_followed", name, err)
		}
		if errors.Is(err, upstream.ErrNotFound) {
			t.Errorf("%s: a redirect must not look like not-found", name)
		}
	}
	if n := len(target.requests()); n != 0 {
		t.Fatalf("redirect target received %d requests", n)
	}
	if n := len(f.requests()); n != len(calls) {
		t.Fatalf("redirecting server received %d requests, want %d (no retries, no prefix fallback)", n, len(calls))
	}
}

func TestInvalidArguments(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, `{}`) })
	c := newClient(f.URL)
	calls := map[string]func() error{
		"GetMCP empty name":       func() error { _, err := c.GetMCP(ctx, "", "1.0.0"); return err },
		"GetMCP empty version":    func() error { _, err := c.GetMCP(ctx, "a/b", ""); return err },
		"GetMCP dot-dot":          func() error { _, err := c.GetMCP(ctx, "..", "1.0.0"); return err },
		"ListMCPVersions empty":   func() error { _, _, err := c.ListMCPVersions(ctx, ""); return err },
		"ListMCPVersions dot":     func() error { _, _, err := c.ListMCPVersions(ctx, "."); return err },
		"GetSkill empty ns":       func() error { _, err := c.GetSkill(ctx, "", "demo"); return err },
		"GetSkill dot-dot name":   func() error { _, err := c.GetSkill(ctx, "io.github.alice", ".."); return err },
		"GetSkillVersion empty":   func() error { _, err := c.GetSkillVersion(ctx, "io.github.alice", "demo", ""); return err },
		"GetSkillArtifact dot ns": func() error { _, err := c.GetSkillArtifact(ctx, ".", "demo", "1.0.0", 10); return err },
	}
	for name, call := range calls {
		if err := call(); ucode(err) != upstream.CodeInvalidArgument {
			t.Errorf("%s: err = %v, want invalid_argument", name, err)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("%d requests were sent for invalid arguments", n)
	}
}

// Whatever fails, the token stays out of the error text, including when the
// upstream reflects it.
func TestErrorsNeverContainToken(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.Header().Set("X-Reflected", auth)
		switch {
		case strings.HasSuffix(r.URL.Path, "/servers"):
			writeJSON(w, `{"servers": "`+auth+`"}`)
		case strings.Contains(r.URL.Path, "/v1/skills/io.github.alice/broken"):
			writeJSON(w, `{"namespace": "`+auth+`", "name": 12}`)
		case strings.Contains(r.URL.Path, "/v1/skills/io.github.alice/gone"):
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte("gone: " + auth))
		case strings.Contains(r.URL.Path, "/v1/skills/io.github.alice/moved"):
			w.Header().Set("Location", "/elsewhere?token="+auth)
			w.WriteHeader(http.StatusFound)
		case strings.Contains(r.URL.Path, "/v1/skills"):
			writeJSON(w, strings.Repeat(auth, 1000))
		case strings.Contains(r.URL.Path, "/.well-known/"):
			_, _ = w.Write([]byte("<html>" + auth + "</html>"))
		default:
			http.Error(w, "no such thing for "+auth, http.StatusNotFound)
		}
	})
	c := newClient(f.URL)
	c.MaxBody = 256
	blocked := &upstream.Client{HTTP: safehttp.NewClient(safehttp.Policy{AllowHTTP: true}), BaseURL: f.URL, Token: token}
	badBase := newClient("https://user:" + token + "@example.com")
	var errs []error
	add := func(err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected an error")
		}
		errs = append(errs, err)
	}
	_, _, err := c.ListMCP(ctx, upstream.MCPListOptions{})
	add(err) // bad_response
	_, err = c.GetMCP(ctx, "io.github.a/b", "1.0.0")
	add(err) // 404 on both prefixes
	_, err = c.GetSkill(ctx, "io.github.alice", "broken")
	add(err) // wrong types
	_, err = c.GetSkill(ctx, "io.github.alice", "gone")
	add(err) // 410
	_, err = c.GetSkill(ctx, "io.github.alice", "moved")
	add(err) // 302
	_, _, err = c.ListSkills(ctx, "", 0)
	add(err) // too large
	_, err = c.WellKnownIndex(ctx)
	add(err) // not JSON
	_, err = c.Fetch(ctx, f.URL+"/missing", 10)
	add(err)
	_, _, err = blocked.ListMCP(ctx, upstream.MCPListOptions{})
	add(err) // address_not_allowed
	_, err = blocked.Fetch(ctx, f.URL+"/x", 10)
	add(err)
	_, _, err = badBase.ListMCP(ctx, upstream.MCPListOptions{})
	add(err) // invalid_url
	_, err = badBase.Fetch(ctx, f.URL+"/x", 10)
	add(err)
	_, err = c.Fetch(ctx, "http://user:"+token+"@127.0.0.1/x", 10)
	add(err)
	for i, err := range errs {
		for _, text := range []string{err.Error(), fmt.Sprintf("%v|%+v|%#v", err, err, err)} {
			if strings.Contains(text, token) || strings.Contains(text, "Bearer") {
				t.Errorf("error %d leaks the token: %s", i, text)
			}
		}
		for e := errors.Unwrap(err); e != nil; e = errors.Unwrap(e) {
			if strings.Contains(e.Error(), token) {
				t.Errorf("error %d leaks the token in its chain: %v", i, e)
			}
		}
	}
	for _, r := range f.requests() {
		if strings.Contains(r.RequestURI, token) {
			t.Errorf("token in a request URL: %s", r.RequestURI)
		}
	}
}

// Concurrent first calls may both probe the prefix; the client must stay
// consistent (run with -race).
func TestConcurrentUse(t *testing.T) {
	var v01 atomic.Int64
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v0.1/") {
			v01.Add(1)
			http.NotFound(w, r)
			return
		}
		writeJSON(w, `{"servers":[{"server":{"name":"io.github.a/b","version":"1.0.0"}}]}`)
	})
	c := newClient(f.URL)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			docs, _, err := c.ListMCP(ctx, upstream.MCPListOptions{})
			if err != nil || len(docs) != 1 {
				t.Errorf("%v %v", docs, err)
			}
		}()
	}
	wg.Wait()
	before := v01.Load()
	if _, _, err := c.ListMCP(ctx, upstream.MCPListOptions{}); err != nil {
		t.Fatal(err)
	}
	if v01.Load() != before {
		t.Fatal("the remembered prefix was not used")
	}
}
