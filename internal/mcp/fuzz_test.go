package mcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
)

// secretLiteral walks a document decoded by encoding/json (not by this
// package) and reports whether any secret input still holds a literal.
func secretLiteral(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		if secret, _ := t["isSecret"].(bool); secret {
			for _, key := range []string{"value", "default"} {
				switch val := t[key].(type) {
				case nil:
				case string:
					if val != "" && !placeholderRe.MatchString(val) {
						return true
					}
				default:
					return true
				}
			}
		}
		for _, child := range t {
			if secretLiteral(child) {
				return true
			}
		}
	case []any:
		for _, child := range t {
			if secretLiteral(child) {
				return true
			}
		}
	}
	return false
}

func FuzzParse(f *testing.F) {
	seeds := []string{
		officialDocument,
		`{"name":"io.github.alice/weather","description":"Weather lookups","version":"1.0.0","remotes":[{"type":"streamable-http","url":"https://mcp.example.com/mcp"}]}`,
		`{"name":"io.github.alice/weather","version":"1","remotes":[{"type":"sse","url":"https://a.example/","headers":[{"name":"X","isSecret":true,"value":"abc","default":"def"}]}]}`,
		`{"name":"IO.GitHub.Alice/w","version":"^1.x","_meta":{"io.modelcontextprotocol.registry/official":{"status":"active"},"ai.openlinker.registry/entry":1}}`,
		`{"name":"io.github.alice/weather","version":"1","description":"d","packages":[{"registryType":"npm","identifier":"x","transport":{"type":"sse","url":"{base}/sse"},"environmentVariables":[{"name":"K","isSecret":true,"default":"{k}","variables":{"k":{"isSecret":true,"value":12}}}],"runtimeArguments":[{"type":"positional","value":"a"}]}]}`,
		`{"name":"a.b/c","version":"1","n":[9007199254740993,1.10,-0,1E400],"s":"\ud83d\ude00\u0001<>&\u2028😀","_meta":{}}`,
		`{"name":"a.b/c","version":"1","version":"2"}`,
		`{"name":"a.b/c","version":"1","x":"\ud800"}`,
		`[{"isSecret":true,"value":"x"}]`,
		`{"a":{"a":{"a":[[[[{"isSecret":true,"value":"x","default":null}]]]]}}}`,
		``, `null`, `{`, "\xff",
	}
	for _, seed := range seeds {
		for _, isStrict := range []bool{true, false} {
			f.Add([]byte(seed), isStrict, false)
		}
	}
	f.Add([]byte(`{"name":"a.b/c","description":"d","version":"1","remotes":[{"type":"sse","url":"http://{h}:{p}/x"}]}`), true, true)

	f.Fuzz(func(t *testing.T, data []byte, isStrict, allowHTTP bool) {
		o := Options{Strict: isStrict, AllowHTTPRemotes: allowHTTP}

		// Canonicalize: a fixed point, and never more than JSON.
		canonical, digest, err := Canonicalize(data)
		if err != nil {
			var e *Error
			if !errors.As(err, &e) || e.Code != codeInvalidJSON || e.Msg == "" {
				t.Fatalf("Canonicalize error is not a coded *Error: %#v", err)
			}
		} else {
			if !json.Valid(data) || !json.Valid(canonical) {
				t.Fatalf("Canonicalize accepted or produced invalid JSON: %q -> %q", data, canonical)
			}
			if len(canonical) > len(data) {
				t.Fatalf("canonical form is longer than its input: %q -> %q", data, canonical)
			}
			again, digest2, err := Canonicalize(canonical)
			if err != nil || !bytes.Equal(again, canonical) || digest2 != digest {
				t.Fatalf("Canonicalize is not idempotent: %q -> %q -> %q (%v)", data, canonical, again, err)
			}
		}

		p, err := Parse(data, o)
		if err != nil {
			var e *Error
			if !errors.As(err, &e) || e.Code == "" || e.Msg == "" {
				t.Fatalf("Parse error is not a coded *Error: %#v", err)
			}
			if p != nil {
				t.Fatal("Parse returned a result with an error")
			}
			return
		}
		if canonical == nil {
			t.Fatalf("Parse accepted what Canonicalize refused: %q", data)
		}
		sum := sha256.Sum256(p.Canonical)
		if p.Digest != "sha256:"+hex.EncodeToString(sum[:]) {
			t.Fatalf("digest is not sha256 of Canonical")
		}
		if len(p.Canonical) > len(data) {
			t.Fatalf("Canonical is longer than its input")
		}
		namespace, server, ok := SplitName(p.Name)
		if !ok || namespace != p.Namespace || server != p.Server {
			t.Fatalf("SplitName disagrees with Parse for %q", p.Name)
		}
		var generic map[string]any
		dec := json.NewDecoder(bytes.NewReader(p.Canonical))
		dec.UseNumber() // 1E400 is valid JSON but not a float64
		if err := dec.Decode(&generic); err != nil {
			t.Fatalf("Canonical is not a JSON object: %v", err)
		}
		if secretLiteral(generic) {
			t.Fatalf("a secret literal survived: %q -> %q", data, p.Canonical)
		}
		if meta, ok := generic["_meta"].(map[string]any); ok {
			if _, found := meta[MetaOfficial]; found {
				t.Fatalf("%s survived", MetaOfficial)
			}
			if _, found := meta[MetaRegistry]; found {
				t.Fatalf("%s survived", MetaRegistry)
			}
		}

		again, err := Parse(p.Canonical, o)
		if err != nil {
			t.Fatalf("Canonical does not parse again: %v\n%q -> %q", err, data, p.Canonical)
		}
		if !bytes.Equal(again.Canonical, p.Canonical) || again.Digest != p.Digest {
			t.Fatalf("Parse is not idempotent:\n%q\n%q\n%q", data, p.Canonical, again.Canonical)
		}
		if again.Name != p.Name || again.Version != p.Version || again.Title != p.Title ||
			again.Description != p.Description || again.HasPackages != p.HasPackages || len(again.Remotes) != len(p.Remotes) {
			t.Fatalf("parsed fields changed on the second pass: %+v vs %+v", p, again)
		}
		if isStrict {
			// Whatever publishes also mirrors, with the same digest.
			m, err := Parse(p.Canonical, Options{AllowHTTPRemotes: allowHTTP})
			if err != nil || m.Digest != p.Digest {
				t.Fatalf("a published document changes under lenient rules: %v", err)
			}
		}
	})
}
