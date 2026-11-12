package upstream_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/OpenLinker-ai/openlinker-registry/internal/upstream"
)

func TestWellKnownIndex(t *testing.T) {
	d1 := "sha256:" + strings.Repeat("1a", 32)
	d2 := "sha256:" + strings.Repeat("2b", 32)
	index := `{"$schema":"https://schemas.agentskills.io/discovery/0.2.0/schema.json","skills":[
		{"name":"relative","type":"skill-md","description":"Relative to the index","url":"relative/SKILL.md","digest":"` + d1 + `"},
		{"name":"rooted","type":"archive","description":"Rooted path","url":"/files/rooted.tar.gz?v=2","digest":"` + d2 + `"},
		{"name":"absolute","type":"archive","description":"","url":"https://cdn.example.net/skills/absolute.tar.gz","digest":"` + d1 + `","extra":{"ignored":true}},
		{"name":"parent","type":"skill-md","url":"../up/SKILL.md","digest":"` + d1 + `"},
		{"name":"other-host","type":"skill-md","url":"//files.example.org/x/SKILL.md","digest":"` + d2 + `"},

		{"type":"skill-md","description":"no name","url":"a/SKILL.md","digest":"` + d1 + `"},
		{"name":"","type":"skill-md","url":"a/SKILL.md","digest":"` + d1 + `"},
		{"name":"no-url","type":"skill-md","digest":"` + d1 + `"},
		{"name":"empty-url","type":"skill-md","url":"","digest":"` + d1 + `"},
		{"name":"bad-type","type":"zip","url":"a.zip","digest":"` + d1 + `"},
		{"name":"no-type","url":"a/SKILL.md","digest":"` + d1 + `"},
		{"name":"no-digest","type":"skill-md","url":"a/SKILL.md"},
		{"name":"upper-digest","type":"skill-md","url":"a/SKILL.md","digest":"sha256:` + strings.Repeat("AB", 32) + `"},
		{"name":"short-digest","type":"skill-md","url":"a/SKILL.md","digest":"sha256:abcd"},
		{"name":"md5-digest","type":"skill-md","url":"a/SKILL.md","digest":"md5:` + strings.Repeat("ab", 16) + `"},
		{"name":"bare-digest","type":"skill-md","url":"a/SKILL.md","digest":"` + strings.Repeat("ab", 32) + `"},
		{"name":"padded-digest","type":"skill-md","url":"a/SKILL.md","digest":" ` + d1 + `"},
		{"name":"ftp","type":"archive","url":"ftp://example.com/a.tar.gz","digest":"` + d1 + `"},
		{"name":"file","type":"archive","url":"file:///etc/passwd","digest":"` + d1 + `"},
		{"name":"javascript","type":"archive","url":"javascript:alert(1)","digest":"` + d1 + `"},
		{"name":"userinfo","type":"archive","url":"https://user:pw@example.com/a.tar.gz","digest":"` + d1 + `"},
		{"name":"fragment","type":"archive","url":"https://example.com/a.tar.gz#x","digest":"` + d1 + `"},
		{"name":"unparseable","type":"archive","url":"https://exa mple.com/%zz","digest":"` + d1 + `"},
		{"name":42,"type":"skill-md","url":"a/SKILL.md","digest":"` + d1 + `"},
		{"name":"typed-url","type":"skill-md","url":["a"],"digest":"` + d1 + `"},
		"a string", null, 12, ["x"]
	]}`
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/site/.well-known/agent-skills/index.json" {
			writeJSON(w, index)
			return
		}
		http.NotFound(w, r)
	})
	c := newClient(f.URL + "/site")
	entries, dropped, err := c.WellKnownIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []upstream.IndexEntry{
		{Name: "relative", Type: "skill-md", Description: "Relative to the index", URL: f.URL + "/site/.well-known/agent-skills/relative/SKILL.md", Digest: d1},
		{Name: "rooted", Type: "archive", Description: "Rooted path", URL: f.URL + "/files/rooted.tar.gz?v=2", Digest: d2},
		{Name: "absolute", Type: "archive", URL: "https://cdn.example.net/skills/absolute.tar.gz", Digest: d1},
		{Name: "parent", Type: "skill-md", URL: f.URL + "/site/.well-known/up/SKILL.md", Digest: d1},
		{Name: "other-host", Type: "skill-md", URL: "http://files.example.org/x/SKILL.md", Digest: d2},
	}
	if len(entries) != len(want) {
		t.Fatalf("kept %d entries, want %d: %+v", len(entries), len(want), entries)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entry %d = %+v\nwant      %+v", i, entries[i], want[i])
		}
	}
	if dropped != 24 {
		t.Errorf("dropped = %d, want 24", dropped)
	}
	if got := f.uris(); len(got) != 1 || got[0] != "/site/.well-known/agent-skills/index.json" {
		t.Fatalf("requests = %v", got)
	}

	// A site without an index.
	_, _, err = newClient(f.URL).WellKnownIndex(ctx)
	wantNotFound(t, "WellKnownIndex", err)
}

func TestWellKnownIndexShapes(t *testing.T) {
	cases := map[string]string{
		`{"$schema":"x","skills":[]}`: "",
		`{"skills":null}`:             "",
		`{"$schema":"x"}`:             upstream.CodeBadResponse,
		`{"skills":{"a":{}}}`:         upstream.CodeBadResponse,
		`{"skills":"SECRET-BODY"}`:    upstream.CodeBadResponse,
		`[]`:                          upstream.CodeBadResponse,
		`<!doctype html>SECRET-BODY`:  upstream.CodeBadResponse,
		``:                            upstream.CodeBadResponse,
	}
	for body, code := range cases {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
		entries, dropped, err := newClient(f.URL).WellKnownIndex(ctx)
		if ucode(err) != code || len(entries) != 0 || dropped != 0 {
			t.Errorf("%q: %v %d %v, want code %q", body, entries, dropped, err, code)
		}
		if err != nil && strings.Contains(err.Error(), "SECRET-BODY") {
			t.Errorf("error echoes the body: %v", err)
		}
	}
}
