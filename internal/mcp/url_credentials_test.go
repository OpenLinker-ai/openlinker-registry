package mcp

import (
	"strings"
	"testing"
)

func TestURLsMustNotEmbedCredentials(t *testing.T) {
	const secret = "ghp_0123456789abcdef"
	docs := map[string]string{
		"remote": `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"remotes":[{"type":"sse","url":"https://user:` + secret + `@mcp.example.com/sse"}]}`,
		"remote template": `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"remotes":[{"type":"sse","url":"https://` + secret + `@{tenant}.example.com/sse"}]}`,
		"package transport": `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"packages":[{"registryType":"npm","identifier":"w","transport":{"type":"sse","url":"http://` + secret + `@localhost:3000/sse"}}]}`,
		"repository": `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"repository":{"url":"https://` + secret + `@github.com/alice/w","source":"github"},
			"remotes":[{"type":"sse","url":"https://mcp.example.com/sse"}]}`,
	}
	for name, doc := range docs {
		// Known URL fields report it under their own code; the rest as a secret.
		if _, err := Parse([]byte(doc), Options{Strict: true}); err == nil {
			t.Errorf("%s: strict must refuse URL credentials", name)
		} else if name == "package transport" && err.(*Error).Code != codeSecretValue {
			t.Errorf("%s: code %s, want %s", name, err.(*Error).Code, codeSecretValue)
		} else if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the error repeats the credential", name)
		}
		p, err := Parse([]byte(doc), Options{})
		if err != nil {
			t.Errorf("%s: lenient: %v", name, err)
			continue
		}
		if strings.Contains(string(p.Canonical), secret) || strings.Contains(string(p.Canonical), "@") {
			t.Errorf("%s: lenient must remove URL credentials: %s", name, p.Canonical)
		}
		// What lenient stores passes strict's credential rule.
		if _, err := Parse(p.Canonical, Options{Strict: true}); err != nil && err.(*Error).Code == codeSecretValue {
			t.Errorf("%s: redacted document still fails: %v", name, err)
		}
	}
	// An "@" outside the authority is not userinfo.
	ok := `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
		"remotes":[{"type":"sse","url":"https://mcp.example.com/users/@alice/sse?to=a@b.example"}]}`
	p, err := Parse([]byte(ok), Options{Strict: true})
	if err != nil || !strings.Contains(string(p.Canonical), "/users/@alice/sse?to=a@b.example") {
		t.Fatalf("path and query may contain @: %v", err)
	}
}
