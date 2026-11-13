package upstream_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/safehttp"
	"github.com/OpenLinker-ai/openlinker-registry/internal/upstream"
)

// serverAB is deliberately oddly spaced: MCPDoc.Server must be these bytes.
const serverAB = `{ "name":"io.github.a/b",  "version":"1.0.0+x", "description":"Demo <b>",
  "remotes":[{"type":"streamable-http","url":"https://mcp.example.com/mcp"}] }`

const officialAB = `{"io.modelcontextprotocol.registry/official":{"status":"deprecated","statusMessage":"use c/d",
  "statusChangedAt":"2026-11-03T04:05:06Z","publishedAt":"2026-10-01T00:00:00.5Z","updatedAt":"2026-11-03T04:05:06+08:00","isLatest":true}}`

func TestListMCPPaginationAndQuery(t *testing.T) {
	page1 := `{"servers":[
		{"server":` + serverAB + `,"_meta":` + officialAB + `},
		{"server":"not an object","_meta":{}},
		{"server":{"name":"io.github.a/no-version"}},
		{"server":{"name":42,"version":"1.0.0"}},
		{"server":{"name":"io.github.a/num-version","version":1}},
		{"server":{"name":"","version":"1.0.0"}},
		{"server":null},
		"just a string", null, 7, [],
		{"server":{"name":"io.github.c/d","version":"2.0.0"},"_meta":{"io.modelcontextprotocol.registry/official":{"status":"deleted","isLatest":false}}}
	],"metadata":{"nextCursor":"io.github.c/d:2.0.0","count":2}}`
	// Second page: the older flat shape, and the end of the listing.
	page2 := `{"servers":[
		{"name":"io.github.e/f","version":"0.1.0","_meta":{"io.modelcontextprotocol.registry/official":{"status":"active","publishedAt":"2026-09-09T09:09:09Z","isLatest":true}}},
		{"name":"io.github.g/h","version":"3.0.0"},
		{"name":"io.github.g/odd","version":"3.0.0","_meta":{"io.modelcontextprotocol.registry/official":{"status":"archived","statusMessage":7,"publishedAt":"yesterday","updatedAt":12,"isLatest":"yes"}}},
		{"version":"1.0.0","_meta":{}}
	],"metadata":{"nextCursor":null,"count":3}}`
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0.1/servers" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("cursor") == "" {
			writeJSON(w, page1)
		} else {
			writeJSON(w, page2)
		}
	})
	c := newClient(f.URL)
	// A non-UTC time with a fraction: sent in UTC, truncated to the second.
	since := time.Date(2026, 11, 1, 8, 30, 15, 987654321, time.FixedZone("CST", 8*3600))
	opts := upstream.MCPListOptions{Limit: 2, UpdatedSince: &since, LatestOnly: true, Search: "demo tool&x"}

	docs, next, err := c.ListMCP(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || next != "io.github.c/d:2.0.0" {
		t.Fatalf("page 1: %d docs, next %q", len(docs), next)
	}
	d := docs[0]
	if string(d.Server) != serverAB {
		t.Errorf("Server bytes were changed:\n%s", d.Server)
	}
	if d.Name != "io.github.a/b" || d.Version != "1.0.0+x" || d.Status != "deprecated" || d.StatusMessage != "use c/d" || !d.IsLatest {
		t.Errorf("doc = %+v", d)
	}
	if !d.StatusChangedAt.Equal(time.Date(2026, 11, 3, 4, 5, 6, 0, time.UTC)) ||
		!d.PublishedAt.Equal(time.Date(2026, 10, 1, 0, 0, 0, 5e8, time.UTC)) ||
		!d.UpdatedAt.Equal(time.Date(2026, 11, 2, 20, 5, 6, 0, time.UTC)) {
		t.Errorf("times = %v %v %v", d.StatusChangedAt, d.PublishedAt, d.UpdatedAt)
	}
	if d := docs[1]; d.Name != "io.github.c/d" || d.Status != "deleted" || d.IsLatest || !d.PublishedAt.IsZero() {
		t.Errorf("doc = %+v", d)
	}

	opts.Cursor = next
	docs, next, err = c.ListMCP(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 3 || next != "" {
		t.Fatalf("page 2: %d docs, next %q", len(docs), next)
	}
	if d := docs[0]; d.Name != "io.github.e/f" || d.Version != "0.1.0" || d.Status != "active" || !d.IsLatest ||
		!d.PublishedAt.Equal(time.Date(2026, 9, 9, 9, 9, 9, 0, time.UTC)) || !strings.Contains(string(d.Server), `"_meta"`) {
		t.Errorf("flat doc = %+v", d)
	}
	// No registry metadata at all: active, zero times.
	if d := docs[1]; d.Name != "io.github.g/h" || d.Status != "active" || d.IsLatest || !d.UpdatedAt.IsZero() || !d.StatusChangedAt.IsZero() {
		t.Errorf("bare doc = %+v", d)
	}
	// Unknown status and wrongly typed metadata fields do not drop the entry.
	if d := docs[2]; d.Name != "io.github.g/odd" || d.Status != "active" || d.StatusMessage != "" || d.IsLatest ||
		!d.PublishedAt.IsZero() || !d.UpdatedAt.IsZero() {
		t.Errorf("odd doc = %+v", d)
	}

	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	q := reqs[0].URL.Query()
	if q.Get("limit") != "2" || q.Get("updated_since") != "2026-11-01T00:30:15Z" || q.Get("version") != "latest" ||
		q.Get("search") != "demo tool&x" || q.Has("cursor") || len(q) != 4 {
		t.Errorf("page 1 query = %v", reqs[0].URL.RawQuery)
	}
	q = reqs[1].URL.Query()
	if q.Get("cursor") != "io.github.c/d:2.0.0" || q.Get("limit") != "2" || q.Get("version") != "latest" || len(q) != 5 {
		t.Errorf("page 2 query = %v", reqs[1].URL.RawQuery)
	}

	// Zero options send no query string at all.
	if _, _, err := c.ListMCP(ctx, upstream.MCPListOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := f.uris()[2]; got != "/v0.1/servers" {
		t.Errorf("request = %q", got)
	}
}

func TestListMCPResponseShapes(t *testing.T) {
	cases := []struct {
		body string
		docs int
		next string
		code string
	}{
		{`{"servers":[]}`, 0, "", ""},
		{`{"servers":null}`, 0, "", ""},
		{`{"servers":[],"metadata":{}}`, 0, "", ""},
		{`{"servers":[],"metadata":{"nextCursor":""}}`, 0, "", ""},
		{`{"servers":[],"metadata":{"nextCursor":null}}`, 0, "", ""},
		{`{"servers":[],"metadata":{"nextCursor":17}}`, 0, "", ""},
		{`{"servers":[],"metadata":null}`, 0, "", ""},
		{`{"servers":[{"server":{"name":"a/b","version":"1"}}],"metadata":{"nextCursor":"n2"},"extra":{"x":[1,2]}}`, 1, "n2", ""},
		// A page of only malformed entries still carries its cursor.
		{`{"servers":[{"server":7}],"metadata":{"nextCursor":"n3"}}`, 0, "n3", ""},
		{`{"servers":[],"metadata":{"nextCursor":"same"}}`, 0, "", upstream.CodeBadResponse},

		{`{"metadata":{"nextCursor":"x"}}`, 0, "", upstream.CodeBadResponse},
		{`{"servers":{"a":1}}`, 0, "", upstream.CodeBadResponse},
		{`{"servers":"SECRET-BODY-MARKER"}`, 0, "", upstream.CodeBadResponse},
		{`[{"server":{}}]`, 0, "", upstream.CodeBadResponse},
		{`null`, 0, "", upstream.CodeBadResponse},
		{`<html>SECRET-BODY-MARKER</html>`, 0, "", upstream.CodeBadResponse},
		{`{"servers":[]} trailing SECRET-BODY-MARKER`, 0, "", upstream.CodeBadResponse},
		{``, 0, "", upstream.CodeBadResponse},
	}
	for _, tc := range cases {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
			// Be lenient on Content-Type, strict on the parse.
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(tc.body))
		})
		docs, next, err := newClient(f.URL).ListMCP(ctx, upstream.MCPListOptions{Cursor: "same"})
		if tc.code != "" {
			if ucode(err) != tc.code || docs != nil || next != "" {
				t.Errorf("%s: docs=%v next=%q err=%v, want %s", tc.body, docs, next, err, tc.code)
			} else if strings.Contains(err.Error(), "SECRET-BODY-MARKER") {
				t.Errorf("error echoes the body: %v", err)
			}
			continue
		}
		if err != nil || len(docs) != tc.docs || next != tc.next {
			t.Errorf("%s: docs=%v next=%q err=%v", tc.body, docs, next, err)
		}
	}
}

func TestMCPPrefixFallbackIsRemembered(t *testing.T) {
	// An upstream that only serves /v0, under a path prefix.
	old := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v0/servers":
			writeJSON(w, `{"servers":[{"name":"io.github.a/b","version":"1.0.0"}]}`)
		case "/api/v0/servers/io.github.a%2Fb/versions/1.0.0":
			writeJSON(w, `{"name":"io.github.a/b","version":"1.0.0"}`)
		default:
			http.NotFound(w, r)
		}
	})
	c := newClient(old.URL + "/api")
	// A miss before any prefix has answered probes both and settles nothing.
	_, err := c.GetMCP(ctx, "io.github.x/missing", "1.0.0")
	wantNotFound(t, "missing server", err)
	if docs, _, err := c.ListMCP(ctx, upstream.MCPListOptions{}); err != nil || len(docs) != 1 {
		t.Fatalf("%v %v", docs, err)
	}
	// From here on only /v0 is used, for hits and misses alike.
	if d, err := c.GetMCP(ctx, "io.github.a/b", "1.0.0"); err != nil || d.Name != "io.github.a/b" {
		t.Fatalf("%+v %v", d, err)
	}
	_, err = c.GetMCP(ctx, "io.github.x/missing", "1.0.0")
	wantNotFound(t, "missing server after fallback", err)
	want := []string{
		"/api/v0.1/servers/io.github.x%2Fmissing/versions/1.0.0",
		"/api/v0/servers/io.github.x%2Fmissing/versions/1.0.0",
		"/api/v0.1/servers",
		"/api/v0/servers",
		"/api/v0/servers/io.github.a%2Fb/versions/1.0.0",
		"/api/v0/servers/io.github.x%2Fmissing/versions/1.0.0",
	}
	if got := old.uris(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// An upstream serving /v0.1: once it answered, a 404 is final and /v0
	// is never tried.
	cur := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0.1/servers" {
			writeJSON(w, `{"servers":[]}`)
			return
		}
		http.NotFound(w, r)
	})
	c = newClient(cur.URL)
	if _, _, err := c.ListMCP(ctx, upstream.MCPListOptions{}); err != nil {
		t.Fatal(err)
	}
	_, _, err = c.ListMCPVersions(ctx, "io.github.x/missing")
	wantNotFound(t, "versions of a missing server", err)
	if got := cur.uris(); len(got) != 2 || got[1] != "/v0.1/servers/io.github.x%2Fmissing/versions" {
		t.Fatalf("requests = %v", got)
	}

	// A fallback that fails for another reason is reported as that failure,
	// not as "not found", and settles nothing.
	flaky := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v0.1/") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	})
	c = newClient(flaky.URL)
	_, err = c.GetMCP(ctx, "io.github.a/b", "1.0.0")
	if code, status := scode(err); code != safehttp.CodeStatus || status != http.StatusBadGateway || errors.Is(err, upstream.ErrNotFound) {
		t.Fatalf("err = %v, want a 502 status error", err)
	}
	// A failure of /v0.1 that is not a 404 does not fall back at all.
	down := newFake(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	c = newClient(down.URL)
	_, _, err = c.ListMCP(ctx, upstream.MCPListOptions{})
	if _, status := scode(err); status != http.StatusServiceUnavailable || len(down.requests()) != 1 {
		t.Fatalf("err = %v after %d requests", err, len(down.requests()))
	}
}

func TestGetMCP(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/v0.1/servers/io.github.a%2Fb/versions/1.0.0%2Bx":
			writeJSON(w, `{"server":`+serverAB+`,"_meta":`+officialAB+`}`+"\n")
		case "/v0.1/servers/io.github.a%2Fb/versions/latest":
			writeJSON(w, `{"server":{"name":"io.github.a/b","version":"2.0.0"},"_meta":{"io.modelcontextprotocol.registry/official":{"isLatest":true}}}`)
		case "/v0.1/servers/io.github.flat%2Fone/versions/0.9.0":
			writeJSON(w, "\n  "+`{"name":"io.github.flat/one","version":"0.9.0","_meta":{"io.modelcontextprotocol.registry/official":{"status":"deleted"}}}`+"\n")
		case "/v0.1/servers/io.github.a%2Fother/versions/1.0.0":
			writeJSON(w, `{"server":`+serverAB+`}`)
		case "/v0.1/servers/io.github.a%2Fb/versions/9.9.9":
			writeJSON(w, `{"server":`+serverAB+`}`)
		case "/v0.1/servers/io.github.a%2Fnover/versions/1.0.0":
			writeJSON(w, `{"server":{"name":"io.github.a/nover"}}`)
		case "/v0.1/servers/io.github.a%2Fstr/versions/1.0.0":
			writeJSON(w, `{"server":"io.github.a/str"}`)
		case "/v0.1/servers/io.github.a%2Fjunk/versions/1.0.0":
			writeJSON(w, `not json SECRET-BODY-MARKER`)
		case "/v0.1/servers/a%20b%3Fc%23d%25e/versions/v%201":
			writeJSON(w, `{"server":{"name":"a b?c#d%e","version":"v 1"}}`)
		default:
			http.NotFound(w, r)
		}
	})
	c := newClient(f.URL)

	d, err := c.GetMCP(ctx, "io.github.a/b", "1.0.0+x")
	if err != nil {
		t.Fatal(err)
	}
	if string(d.Server) != serverAB || d.Name != "io.github.a/b" || d.Version != "1.0.0+x" || d.Status != "deprecated" || !d.IsLatest {
		t.Fatalf("doc = %+v", d)
	}
	// Both the slash in the name and the plus in the version stay escaped.
	r := f.requests()[0]
	if r.RequestURI != "/v0.1/servers/io.github.a%2Fb/versions/1.0.0%2Bx" || r.URL.EscapedPath() != r.RequestURI {
		t.Fatalf("RequestURI = %q, EscapedPath = %q", r.RequestURI, r.URL.EscapedPath())
	}

	if d, err = c.GetMCP(ctx, "io.github.a/b", "latest"); err != nil || d.Version != "2.0.0" || !d.IsLatest || d.Status != "active" {
		t.Fatalf("latest: %+v %v", d, err)
	}
	d, err = c.GetMCP(ctx, "io.github.flat/one", "0.9.0")
	if err != nil || d.Status != "deleted" || d.Version != "0.9.0" ||
		string(d.Server) != `{"name":"io.github.flat/one","version":"0.9.0","_meta":{"io.modelcontextprotocol.registry/official":{"status":"deleted"}}}` {
		t.Fatalf("flat: %+v %v", d, err)
	}
	if d, err = c.GetMCP(ctx, "a b?c#d%e", "v 1"); err != nil || d.Name != "a b?c#d%e" {
		t.Fatalf("reserved characters: %+v %v", d, err)
	}

	for _, name := range []string{"io.github.a/other", "io.github.a/nover", "io.github.a/str", "io.github.a/junk"} {
		_, err := c.GetMCP(ctx, name, "1.0.0")
		if ucode(err) != upstream.CodeBadResponse || strings.Contains(err.Error(), "SECRET-BODY-MARKER") {
			t.Errorf("%s: err = %v, want bad_response", name, err)
		}
	}
	if _, err := c.GetMCP(ctx, "io.github.a/b", "9.9.9"); ucode(err) != upstream.CodeBadResponse {
		t.Errorf("another version than requested: %v", err)
	}
	_, err = c.GetMCP(ctx, "io.github.a/missing", "1.0.0")
	wantNotFound(t, "GetMCP", err)
}

func TestListMCPVersions(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/v0.1/servers/io.github.a%2Fb/versions":
			writeJSON(w, `{"servers":[
				{"server":{"name":"io.github.a/b","version":"2.0.0"},"_meta":{"io.modelcontextprotocol.registry/official":{"isLatest":true,"status":"active"}}},
				{"server":{"name":"io.github.a/b"}},
				{"server":{"name":"io.github.someone/else","version":"6.6.6"}},
				{"name":"io.github.a/b","version":"1.0.0","_meta":{"io.modelcontextprotocol.registry/official":{"status":"deprecated"}}}
			],"metadata":{"count":4}}`)
		case "/v0.1/servers/io.github.a%2Fempty/versions":
			writeJSON(w, `{"servers":[]}`)
		case "/v0.1/servers/io.github.a%2Fbad/versions":
			writeJSON(w, `{"versions":[]}`)
		default:
			http.NotFound(w, r)
		}
	})
	c := newClient(f.URL)
	docs, complete, err := c.ListMCPVersions(ctx, "io.github.a/b")
	if err != nil || len(docs) != 2 || complete {
		t.Fatalf("%+v complete=%v %v (the list has entries that cannot be read)", docs, complete, err)
	}
	if docs[0].Version != "2.0.0" || !docs[0].IsLatest || docs[1].Version != "1.0.0" || docs[1].Status != "deprecated" {
		t.Fatalf("docs = %+v", docs)
	}
	if got := f.uris()[0]; got != "/v0.1/servers/io.github.a%2Fb/versions" {
		t.Fatalf("request = %q", got)
	}
	if docs, complete, err = c.ListMCPVersions(ctx, "io.github.a/empty"); err != nil || len(docs) != 0 || !complete {
		t.Fatalf("%+v complete=%v %v", docs, complete, err)
	}
	if _, _, err = c.ListMCPVersions(ctx, "io.github.a/bad"); ucode(err) != upstream.CodeBadResponse {
		t.Fatalf("err = %v, want bad_response", err)
	}
	_, _, err = c.ListMCPVersions(ctx, "io.github.a/missing")
	wantNotFound(t, "ListMCPVersions", err)
}
