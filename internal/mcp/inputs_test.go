package mcp

import (
	"strings"
	"testing"
)

// A published document is immutable and is served to clients that decode
// inputs with the types of the official schema. Strict publishing refuses a
// member of another type; mirroring keeps what the upstream has.
func TestInputMemberTypes(t *testing.T) {
	doc := func(remoteExtra, packageExtra string) string {
		return `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"remotes":[{"type":"sse","url":"https://mcp.example.com/sse"` + remoteExtra + `}],
			"packages":[{"registryType":"npm","identifier":"w","transport":{"type":"stdio"}` + packageExtra + `}]}`
	}
	withTransport := func(transport string) string {
		return `{"name":"io.github.alice/w","description":"d","version":"1.0.0",
			"packages":[{"registryType":"npm","identifier":"w","transport":` + transport + `}]}`
	}
	bad := map[string]string{
		"header value":            doc(`,"headers":[{"name":"X-Region","value":123}]`, ``),
		"header default":          doc(`,"headers":[{"name":"X-Region","default":true}]`, ``),
		"header description":      doc(`,"headers":[{"name":"X-Region","description":["eu"]}]`, ``),
		"header placeholder":      doc(`,"headers":[{"name":"X-Region","placeholder":1}]`, ``),
		"isRequired":              doc(`,"headers":[{"name":"X-Region","isRequired":"yes"}]`, ``),
		"isSecret":                doc(`,"headers":[{"name":"X-Region","isSecret":1}]`, ``),
		"format":                  doc(`,"headers":[{"name":"X-Region","format":"integer"}]`, ``),
		"format type":             doc(`,"headers":[{"name":"X-Region","format":7}]`, ``),
		"choices":                 doc(`,"headers":[{"name":"X-Region","choices":"eu"}]`, ``),
		"choices item":            doc(`,"headers":[{"name":"X-Region","choices":["eu",2]}]`, ``),
		"null value":              doc(`,"headers":[{"name":"X-Region","value":null}]`, ``),
		"variables":               doc(`,"headers":[{"name":"X-Region","value":"{r}","variables":["r"]}]`, ``),
		"variable":                doc(`,"headers":[{"name":"X-Region","value":"{r}","variables":{"r":"eu"}}]`, ``),
		"variable default":        doc(`,"headers":[{"name":"X-Region","value":"{r}","variables":{"r":{"default":5}}}]`, ``),
		"nested variable":         doc(`,"headers":[{"name":"X-Region","value":"{r}","variables":{"r":{"value":"{s}","variables":{"s":{"value":false}}}}}]`, ``),
		"remote variable":         doc(`,"variables":{"tenant":{"default":42}}`, ``),
		"remote variables":        doc(`,"variables":"tenant"`, ``),
		"environment variable":    doc(``, `,"environmentVariables":[{"name":"REGION","value":3}]`),
		"runtime argument":        doc(``, `,"runtimeArguments":[{"type":"named","name":"--region","value":3}]`),
		"package argument hint":   doc(``, `,"packageArguments":[{"type":"positional","value":"x","valueHint":9}]`),
		"argument isRepeated":     doc(``, `,"packageArguments":[{"type":"named","name":"--tag","isRepeated":"true"}]`),
		"argument variable":       doc(``, `,"packageArguments":[{"type":"named","name":"--tag","value":"{t}","variables":{"t":{"choices":[1]}}}]`),
		"package header":          withTransport(`{"type":"sse","url":"http://localhost:3000/sse","headers":[{"name":"X-Region","value":1}]}`),
		"package transport value": withTransport(`{"type":"sse","url":"http://localhost:{port}/sse","variables":{"port":{"default":3000}}}`),
	}
	for name, data := range bad {
		_, err := Parse([]byte(data), Options{Strict: true})
		if err == nil {
			t.Errorf("%s: strict accepted a member of the wrong type", name)
		} else if code := err.(*Error).Code; code == codeSecretValue {
			t.Errorf("%s: refused as a secret (%v), want a malformed document", name, err)
		}
		if _, err := Parse([]byte(data), Options{}); err != nil {
			t.Errorf("%s: lenient must keep what an upstream serves: %v", name, err)
		}
	}
	good := doc(`,"headers":[{"name":"X-Region","description":"Region","isRequired":true,"isSecret":false,"format":"string",
		"value":"{r}","placeholder":"eu","choices":["eu","us"],"variables":{"r":{"default":"eu","format":"string","choices":["eu","us"],
		"variables":{}}},"x-note":7}],"variables":{"tenant":{"description":"Tenant","default":"acme","isRequired":false}}`,
		`,"environmentVariables":[{"name":"REGION","value":"eu","format":"string"},{"name":"PORT","default":"3000","format":"number"}],
		"runtimeArguments":[{"type":"named","name":"--region","value":"eu","isRepeated":false}],
		"packageArguments":[{"type":"positional","valueHint":"path","format":"filepath","isRequired":true}]`)
	p, err := Parse([]byte(good), Options{Strict: true})
	if err != nil {
		t.Fatalf("a document with every input member of the right type: %v", err)
	}
	if !strings.Contains(string(p.Canonical), `"x-note":7`) {
		t.Fatalf("members the schema does not define are left alone: %s", p.Canonical)
	}
}
