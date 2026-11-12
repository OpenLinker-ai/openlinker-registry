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

func TestCredentialsByVariableNameAndQueryParameter(t *testing.T) {
	const secret = "live-secret-0123456789"
	docs := map[string]string{
		// In a variables object the key is the input's name.
		"variable key": `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"remotes":[{"type":"sse","url":"https://{tenant}.example.com/sse","variables":{"API_TOKEN":{"default":"` + secret + `"}}}]}`,
		"nested variable key": `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"remotes":[{"type":"sse","url":"https://mcp.example.com/sse","headers":[{"name":"X-Trace","value":"{t}",
			"variables":{"t":{"variables":{"db_password":{"value":"` + secret + `"}}}}}]}]}`,
		// What is substituted into a credential is a credential.
		"variable of a secret header": `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"remotes":[{"type":"sse","url":"https://mcp.example.com/sse","headers":[{"name":"Authorization","isSecret":true,
			"value":"Bearer {key}","variables":{"key":{"default":"` + secret + `"}}}]}]}`,
		"nested variable of a credential by name": `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"packages":[{"registryType":"npm","identifier":"w","transport":{"type":"stdio"},"environmentVariables":[{"name":"API_KEY",
			"value":"{a}","variables":{"a":{"value":"{b}","variables":{"b":{"value":"` + secret + `"}}}}}]}]}`,
		"variable of a credential parameter": `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"remotes":[{"type":"sse","url":"https://mcp.example.com/sse?api_key={key}","variables":{"key":{"default":"` + secret + `"}}}]}`,
		"variable of a fragment credential": `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"x-app":{"url":"https://app.example.com/start#access_token={t}","variables":{"t":{"value":"{u}","variables":{"u":{"default":"` + secret + `"}}}}},
			"remotes":[{"type":"sse","url":"https://mcp.example.com/sse"}]}`,
		"variable in the userinfo": `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"packages":[{"registryType":"npm","identifier":"w","transport":{"type":"sse","url":"http://localhost:{port}/sse",
			"variables":{"port":{"default":"3000"}}}}],
			"x-source":{"url":"https://{who}@git.example/w","variables":{"who":{"default":"` + secret + `"}}},
			"remotes":[{"type":"sse","url":"https://mcp.example.com/sse"}]}`,
		// An argument is named by its flag.
		"flag named like a credential": `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"packages":[{"registryType":"npm","identifier":"w","transport":{"type":"stdio"},
			"packageArguments":[{"type":"named","name":"--cookie","value":"sid=` + secret + `"}]}]}`,
		"authorization flag with a variable": `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"packages":[{"registryType":"npm","identifier":"w","transport":{"type":"stdio"},
			"runtimeArguments":[{"type":"named","name":"--authorization","value":"Bearer {k}","variables":{"k":{"default":"` + secret + `"}}}]}]}`,
		"query parameter": `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"remotes":[{"type":"sse","url":"https://mcp.example.com/sse?region=eu&api_key=` + secret + `"}]}`,
		// A package identifier is a download URL for some package types.
		"package identifier": `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"packages":[{"registryType":"mcpb","identifier":"https://downloads.example/tool.mcpb?api_key=` + secret + `","transport":{"type":"stdio"}}]}`,
		"url in an unknown member": `{"name":"io.github.alice/w","description":"d","version":"1.0.0","x-mirrors":["https://u:` + secret + `@mirror.example/w"],
			"remotes":[{"type":"sse","url":"https://mcp.example.com/sse"}]}`,
		"user name with an at sign": `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"packages":[{"registryType":"mcpb","identifier":"https://alice@example.org:` + secret + `@downloads.example/w.mcpb","transport":{"type":"stdio"}}]}`,
		"scheme-relative": `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"packages":[{"registryType":"mcpb","identifier":"//user:` + secret + `@downloads.example/w.mcpb","transport":{"type":"stdio"}}]}`,
		"scheme-relative in an unknown member": `{"name":"io.github.alice/w","description":"d","version":"1.0.0","x-mirror":"//u:` + secret + `@mirror.example/w",
			"remotes":[{"type":"sse","url":"https://mcp.example.com/sse"}]}`,
		"compound scheme": `{"name":"io.github.alice/w","description":"d","version":"1.0.0","x-db":"jdbc:postgresql://app:` + secret + `@db.example/w",
			"remotes":[{"type":"sse","url":"https://mcp.example.com/sse"}]}`,
		"fragment parameter": `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"websiteUrl":"https://example.com/app#access_token=` + secret + `&state=1",
			"remotes":[{"type":"sse","url":"https://mcp.example.com/sse"}]}`,
		"template scheme": `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"remotes":[{"type":"sse","url":"{scheme}://user:` + secret + `@mcp.example.com/sse","variables":{"scheme":{"default":"https"}}}]}`,
		// A remote URL may be a template; it is still a URL.
		"template url": `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"remotes":[{"type":"sse","url":"{base}/mcp?api_key=` + secret + `","variables":{"base":{"default":"https://mcp.example.com"}}}]}`,
		"template in an unknown member": `{"name":"io.github.alice/w","description":"d","version":"1.0.0","x-alt":"{base}/mcp?access_token=` + secret + `",
			"remotes":[{"type":"sse","url":"https://mcp.example.com/sse"}]}`,
		"encoded query name": `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"remotes":[{"type":"sse","url":"https://mcp.example.com/sse?access%5Ftoken=` + secret + `"}]}`,
	}
	for name, doc := range docs {
		if _, err := Parse([]byte(doc), Options{Strict: true}); err == nil || err.(*Error).Code != codeSecretValue {
			t.Errorf("%s: strict must refuse the credential, got %v", name, err)
		} else if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the error repeats the credential", name)
		}
		p, err := Parse([]byte(doc), Options{})
		if err != nil {
			t.Errorf("%s: lenient: %v", name, err)
			continue
		}
		if strings.Contains(string(p.Canonical), secret) {
			t.Errorf("%s: lenient must remove the credential: %s", name, p.Canonical)
		}
		if _, err := Parse(p.Canonical, Options{Strict: true}); err != nil {
			t.Errorf("%s: what lenient stores must pass strict: %v", name, err)
		}
	}
	// Fragments and parameters that are not credentials survive unchanged.
	plain := `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
		"websiteUrl":"https://example.com/docs?lang=en#install",
		"remotes":[{"type":"sse","url":"https://mcp.example.com/sse#"}]}`
	if p, err := Parse([]byte(plain), Options{}); err != nil || !strings.Contains(string(p.Canonical), "docs?lang=en#install") ||
		!strings.Contains(string(p.Canonical), `"https://mcp.example.com/sse#"`) {
		t.Fatalf("ordinary URLs must not be rewritten: %v", err)
	}
	// Variables used elsewhere in a URL are not credentials.
	host := `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
		"remotes":[{"type":"sse","url":"https://{tenant}.example.com/sse?region={region}&api_key={key}",
		"variables":{"tenant":{"default":"acme"},"region":{"default":"eu"},"key":{"description":"Your key","isSecret":true}}}]}`
	if p, err := Parse([]byte(host), Options{Strict: true}); err != nil || !strings.Contains(string(p.Canonical), `"default":"acme"`) ||
		!strings.Contains(string(p.Canonical), `"default":"eu"`) {
		t.Fatalf("variables outside credential positions must keep their defaults: %v", err)
	}
	// A variable of an input that is not a credential keeps its default.
	region := `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
		"remotes":[{"type":"sse","url":"https://mcp.example.com/sse","headers":[{"name":"X-Region","value":"{region}",
		"variables":{"region":{"default":"eu-west-1"}}}]}]}`
	if p, err := Parse([]byte(region), Options{Strict: true}); err != nil || !strings.Contains(string(p.Canonical), "eu-west-1") {
		t.Fatalf("variables of ordinary inputs must pass: %v", err)
	}
	// Placeholders and ordinary parameters are untouched.
	ok := `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
		"remotes":[{"type":"sse","url":"https://mcp.example.com/sse?api_key={key}&max_tokens=4096&region=eu",
		"variables":{"key":{"description":"Your key","isSecret":true},"region":{"default":"eu"}}}]}`
	p, err := Parse([]byte(ok), Options{Strict: true})
	if err != nil || !strings.Contains(string(p.Canonical), "api_key={key}&max_tokens=4096&region=eu") {
		t.Fatalf("placeholders and ordinary parameters must pass: %v", err)
	}
}
