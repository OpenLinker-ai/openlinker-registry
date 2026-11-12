package mcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type obj = map[string]any
type arr = []any

var (
	strict       = Options{Strict: true}
	strictHTTP   = Options{Strict: true, AllowHTTPRemotes: true}
	lenient      = Options{}
	exampleURL   = "https://mcp.example.com/mcp"
	exampleStdio = obj{"registryType": "npm", "identifier": "@alice/weather", "transport": obj{"type": "stdio"}}
)

// minimal is the smallest document that passes the publishing rules.
func minimal() obj {
	return obj{
		"name":        "io.github.alice/weather",
		"description": "Weather lookups",
		"version":     "1.0.0",
		"remotes":     arr{obj{"type": "streamable-http", "url": exampleURL}},
	}
}

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// with returns minimal() after applying edits; a nil value deletes the key.
func with(edits obj) obj {
	doc := minimal()
	for k, v := range edits {
		if v == nil {
			delete(doc, k)
		} else {
			doc[k] = v
		}
	}
	return doc
}

func remote(edits obj) obj {
	r := obj{"type": "streamable-http", "url": exampleURL}
	for k, v := range edits {
		if v == nil {
			delete(r, k)
		} else {
			r[k] = v
		}
	}
	return obj{"remotes": arr{r}}
}

func pkg(edits obj) obj {
	p := obj{"registryType": "npm", "identifier": "@alice/weather", "transport": obj{"type": "stdio"}}
	for k, v := range edits {
		if v == nil {
			delete(p, k)
		} else {
			p[k] = v
		}
	}
	return obj{"packages": arr{p}}
}

func repeat(v any, n int) arr {
	out := make(arr, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func distinctRemotes(n int) arr {
	out := make(arr, n)
	for i := range out {
		out[i] = obj{"type": "sse", "url": fmt.Sprintf("https://mcp.example.com/%d", i)}
	}
	return out
}

type parseCase struct {
	name  string
	edits obj
	opts  Options
	want  string // error code; "" means the document must pass
}

func runCases(t *testing.T, cases []parseCase) {
	t.Helper()
	for _, tc := range cases {
		_, err := Parse(mustJSON(t, with(tc.edits)), tc.opts)
		if got := codeOf(err); got != tc.want {
			t.Errorf("%s: code %q (%v), want %q", tc.name, got, err, tc.want)
		}
	}
}

func TestMinimalDocumentPasses(t *testing.T) {
	for _, o := range []Options{strict, strictHTTP, lenient} {
		p, err := Parse(mustJSON(t, minimal()), o)
		if err != nil {
			t.Fatalf("%+v: %v", o, err)
		}
		want := &Parsed{
			Name: "io.github.alice/weather", Namespace: "io.github.alice", Server: "weather",
			Description: "Weather lookups", Version: "1.0.0",
			Remotes:   []Remote{{Type: "streamable-http", URL: exampleURL}},
			Canonical: []byte(`{"description":"Weather lookups","name":"io.github.alice/weather","remotes":[{"type":"streamable-http","url":"https://mcp.example.com/mcp"}],"version":"1.0.0"}`),
		}
		sum := sha256.Sum256(want.Canonical)
		want.Digest = "sha256:" + hex.EncodeToString(sum[:])
		if !reflect.DeepEqual(p, want) {
			t.Errorf("%+v:\n got %+v\nwant %+v", o, p, want)
		}
	}
}

func TestSizeLimit(t *testing.T) {
	doc := mustJSON(t, minimal())
	pad := func(n int) []byte { return append(bytes.Repeat([]byte(" "), n-len(doc)), doc...) }
	cases := []struct {
		name string
		data []byte
		opts Options
		want string
	}{
		{"strict default, at limit", pad(64 << 10), strict, ""},
		{"strict default, over", pad(64<<10 + 1), strict, codeTooLarge},
		{"lenient default takes what strict refuses", pad(64<<10 + 1), lenient, ""},
		{"lenient default, at limit", pad(256 << 10), lenient, ""},
		{"lenient default, over", pad(256<<10 + 1), lenient, codeTooLarge},
		{"explicit limit, at", doc, Options{Strict: true, MaxBytes: len(doc)}, ""},
		{"explicit limit, over", doc, Options{Strict: true, MaxBytes: len(doc) - 1}, codeTooLarge},
		{"explicit limit above default", pad(64<<10 + 1), Options{Strict: true, MaxBytes: 128 << 10}, ""},
		{"explicit limit below lenient default", pad(1025), Options{MaxBytes: 1024}, codeTooLarge},
		// The size is checked before anything is parsed.
		{"oversized garbage", bytes.Repeat([]byte("x"), 64<<10+1), strict, codeTooLarge},
	}
	for _, tc := range cases {
		if _, err := Parse(tc.data, tc.opts); codeOf(err) != tc.want {
			t.Errorf("%s: code %q, want %q", tc.name, codeOf(err), tc.want)
		}
	}
}

func TestInvalidJSON(t *testing.T) {
	deep := func(n int) string { // the document object plus n arrays
		return `{"name":"io.github.alice/weather","version":"1","x":` + nest("[", "]", n, "") + `}`
	}
	cases := []struct{ name, data, want string }{
		{"array document", `[]`, codeInvalidJSON},
		{"string document", `"io.github.alice/weather"`, codeInvalidJSON},
		{"null document", `null`, codeInvalidJSON},
		{"empty input", ``, codeInvalidJSON},
		{"trailing data", `{"name":"io.github.alice/weather","version":"1"}{}`, codeInvalidJSON},
		{"invalid UTF-8", "{\"name\":\"io.github.alice/weather\",\"version\":\"1\",\"x\":\"\xc3\x28\"}", codeInvalidJSON},
		{"duplicate top-level key", `{"name":"io.github.alice/weather","version":"1","version":"2"}`, codeInvalidJSON},
		{"duplicate nested key", `{"name":"io.github.alice/weather","version":"1","remotes":[{"type":"sse","url":"https://a.example/","headers":[{"name":"X","value":"{k}","value":"real"}]}]}`, codeInvalidJSON},
		{"nearest: no duplicate", `{"name":"io.github.alice/weather","version":"1","remotes":[{"type":"sse","url":"https://a.example/","headers":[{"name":"X","value":"{k}"}]}]}`, ""},
		{"depth 33", deep(32), codeInvalidJSON},
		{"depth 32", deep(31), ""},
		{"lone surrogate", `{"name":"io.github.alice/weather","version":"1","x":"\ud800"}`, codeInvalidJSON},
		{"escaped NUL", `{"name":"io.github.alice/weather","version":"1","x":"\u0000"}`, codeInvalidJSON},
	}
	for _, tc := range cases {
		for _, o := range []Options{strict, lenient} {
			want := tc.want
			if want == "" && o.Strict {
				continue // the passing neighbours are lenient-only documents
			}
			if _, err := Parse([]byte(tc.data), o); codeOf(err) != want {
				t.Errorf("%s (strict=%v): code %q, want %q", tc.name, o.Strict, codeOf(err), want)
			}
		}
	}
}

func TestNameAndNamespace(t *testing.T) {
	label63 := strings.Repeat("a", 63)
	cases := []struct{ name, value, want string }{
		{"plain", "io.github.alice/weather", ""},
		{"all allowed characters", "IO.GitHub.Al-1ce/We_ather.v2-x", ""},
		{"shortest valid", "a.b/c", ""},
		{"200 characters", "io.github.alice/" + strings.Repeat("s", 184), ""},
		{"201 characters", "io.github.alice/" + strings.Repeat("s", 185), codeNameInvalid},
		{"too short", "ab", codeNameInvalid},
		{"empty", "", codeNameInvalid},
		{"no slash", "io.github.alice", codeNameInvalid},
		{"two slashes", "io.github.alice/a/b", codeNameInvalid},
		{"empty server", "io.github.alice/", codeNameInvalid},
		{"empty namespace", "/weather", codeNameInvalid},
		{"underscore in namespace", "io.git_hub.alice/weather", codeNameInvalid},
		{"space", "io.github.alice/wea ther", codeNameInvalid},
		{"non-ASCII", "io.github.alice/wéather", codeNameInvalid},
		{"trailing newline", "io.github.alice/weather\n", codeNameInvalid},
		{"dot server", "io.github.alice/.", codeNameInvalid},
		{"dot-dot server", "io.github.alice/..", codeNameInvalid},
		{"dots inside server", "io.github.alice/a..b", ""},
		{"single label", "alice/weather", codeNamespaceInvalid},
		{"single label, minimum length", "a/b", codeNamespaceInvalid},
		{"empty label", "io..alice/weather", codeNamespaceInvalid},
		{"leading dot", ".io.alice/weather", codeNamespaceInvalid},
		{"trailing dot", "io.alice./weather", codeNamespaceInvalid},
		{"label starts with hyphen", "io.github.-alice/weather", codeNamespaceInvalid},
		{"label ends with hyphen", "io.github.alice-/weather", codeNamespaceInvalid},
		{"63 character label", "io." + label63 + "/w", ""},
		{"64 character label", "io." + label63 + "a/w", codeNamespaceInvalid},
		{"namespace of 127", label63 + "." + label63 + "/w", ""},
		{"namespace of 128", label63 + "." + label63[:62] + ".a/w", ""},              // 63+1+62+1+1
		{"namespace of 129", label63 + "." + label63 + ".a/w", codeNamespaceInvalid}, // 63+1+63+1+1, name still under 200
		{"uppercase label over 63", "IO." + strings.ToUpper(label63) + "A/w", codeNamespaceInvalid},
	}
	for _, tc := range cases {
		for _, o := range []Options{strict, lenient} {
			_, err := Parse(mustJSON(t, with(obj{"name": tc.value})), o)
			if codeOf(err) != tc.want {
				t.Errorf("%s (strict=%v): code %q, want %q", tc.name, o.Strict, codeOf(err), tc.want)
			}
		}
		_, _, ok := SplitName(tc.value)
		if ok != (tc.want == "") {
			t.Errorf("%s: SplitName ok = %v", tc.name, ok)
		}
	}
	runCases(t, []parseCase{
		{"name missing", obj{"name": nil}, strict, codeNameInvalid},
		{"name missing, lenient", obj{"name": nil}, lenient, codeNameInvalid},
		{"name not a string", obj{"name": 7}, lenient, codeNameInvalid},
		{"name is an array", obj{"name": arr{"io.github.alice/weather"}}, lenient, codeNameInvalid},
	})
}

func TestSplitName(t *testing.T) {
	cases := []struct {
		in, namespace, server string
		ok                    bool
	}{
		{"io.github.alice/weather", "io.github.alice", "weather", true},
		{"io.github.Alice/Weather_API", "io.github.alice", "Weather_API", true},
		{"COM.EXAMPLE/Srv", "com.example", "Srv", true},
		{"io.github.alice/a/b", "", "", false},
		{"alice/weather", "", "", false},
		{"io.github.alice", "", "", false},
		{"", "", "", false},
		{"io.github.alice/..", "", "", false},
	}
	for _, tc := range cases {
		namespace, server, ok := SplitName(tc.in)
		if namespace != tc.namespace || server != tc.server || ok != tc.ok {
			t.Errorf("SplitName(%q) = %q, %q, %v", tc.in, namespace, server, ok)
		}
	}

	p, err := Parse(mustJSON(t, with(obj{"name": "io.github.Alice/Weather"})), strict)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "io.github.Alice/Weather" || p.Namespace != "io.github.alice" || p.Server != "Weather" {
		t.Errorf("got %q %q %q", p.Name, p.Namespace, p.Server)
	}
	if !bytes.Contains(p.Canonical, []byte(`"name":"io.github.Alice/Weather"`)) {
		t.Errorf("name was rewritten in %s", p.Canonical)
	}
}

func TestVersion(t *testing.T) {
	both := []parseCase{
		{"missing", obj{"version": nil}, strict, codeVersionInvalid},
		{"empty", obj{"version": ""}, strict, codeVersionInvalid},
		{"latest", obj{"version": "latest"}, strict, codeVersionInvalid},
		{"number", obj{"version": 1}, strict, codeVersionInvalid},
		{"256 characters", obj{"version": strings.Repeat("1", 256)}, strict, codeVersionInvalid},
		{"255 characters", obj{"version": strings.Repeat("1", 255)}, strict, ""},
		{"255 multi-byte characters", obj{"version": strings.Repeat("é", 255)}, strict, ""},
		{"one character", obj{"version": "1"}, strict, ""},
		{"latest is case-sensitive", obj{"version": "Latest"}, strict, ""},
		{"latest as a pre-release tag", obj{"version": "1.0.0-latest"}, strict, ""},
	}
	for _, tc := range both {
		runCases(t, []parseCase{tc})
		tc.opts = lenient
		tc.name += " (lenient)"
		runCases(t, []parseCase{tc})
	}

	ranges := []string{"^1.2.3", "~1.2.3", ">=1.2.3", ">1", "<2.0.0", "<=2", "=1.2.3",
		"1.0.0 || 2.0.0", "1||2", "1.0.0 - 2.0.0", "*", "1.*", "1.2.*", "x", "1.x", "1.X", "1.2.x", "1.x.x", "X.1"}
	for _, v := range ranges {
		runCases(t, []parseCase{
			{"range " + v, obj{"version": v}, strict, codeVersionInvalid},
			// Mirrored entries are not held to the publishing rule.
			{"range " + v + " (lenient)", obj{"version": v}, lenient, ""},
		})
	}
	specific := []string{"1.0.0", "1.0.0+build.1", "2025.11.12-beta", "v1.2.3", "1.0.0-rc.1", "1.0.0-x86", "1.0.0-alpha-1",
		"0.0.1-next", "1.2.3-linux.x64", "2.0", "git-0123abc", "1.0.0-beta+exp.sha.5114f85", "1-2", "extra"}
	for _, v := range specific {
		runCases(t, []parseCase{{"version " + v, obj{"version": v}, strict, ""}})
	}
}

func TestDescriptionTitleSchema(t *testing.T) {
	runCases(t, []parseCase{
		{"description missing", obj{"description": nil}, strict, codeDescriptionInvalid},
		{"description empty", obj{"description": ""}, strict, codeDescriptionInvalid},
		{"description number", obj{"description": 5}, strict, codeDescriptionInvalid},
		{"description 101", obj{"description": strings.Repeat("d", 101)}, strict, codeDescriptionInvalid},
		{"description 100", obj{"description": strings.Repeat("d", 100)}, strict, ""},
		{"description 1", obj{"description": "d"}, strict, ""},
		{"description 100 code points, 400 bytes", obj{"description": strings.Repeat("😀", 100)}, strict, ""},
		{"description 101 code points", obj{"description": strings.Repeat("中", 101)}, strict, codeDescriptionInvalid},
		{"description missing (lenient)", obj{"description": nil}, lenient, ""},
		{"description 500 (lenient)", obj{"description": strings.Repeat("d", 500)}, lenient, ""},

		{"title absent", obj{}, strict, ""},
		{"title empty", obj{"title": ""}, strict, codeTitleInvalid},
		{"title number", obj{"title": 5}, strict, codeTitleInvalid},
		{"title 101", obj{"title": strings.Repeat("t", 101)}, strict, codeTitleInvalid},
		{"title 100", obj{"title": strings.Repeat("é", 100)}, strict, ""},
		{"title 1", obj{"title": "W"}, strict, ""},
		{"title empty (lenient)", obj{"title": ""}, lenient, ""},

		{"$schema number", obj{"$schema": 5}, strict, codeSchemaInvalid},
		{"$schema object", obj{"$schema": obj{}}, strict, codeSchemaInvalid},
		{"$schema official", obj{"$schema": SchemaURL}, strict, ""},
		{"$schema any string", obj{"$schema": "not even a URL"}, strict, ""},
		{"$schema number (lenient)", obj{"$schema": 5}, lenient, ""},
	})
	// JSON null is a present value of the wrong type, not an absent field.
	for _, c := range []struct{ field, want string }{{"title", codeTitleInvalid}, {"$schema", codeSchemaInvalid}} {
		data := bytes.Replace(mustJSON(t, minimal()), []byte(`{`), []byte(`{"`+c.field+`":null,`), 1)
		if _, err := Parse(data, strict); codeOf(err) != c.want {
			t.Errorf("%s null: code %q, want %q", c.field, codeOf(err), c.want)
		}
	}
}

func TestRepositoryWebsiteIcons(t *testing.T) {
	repo := func(edits obj) obj {
		r := obj{"url": "https://github.com/alice/weather", "source": "github"}
		for k, v := range edits {
			if v == nil {
				delete(r, k)
			} else {
				r[k] = v
			}
		}
		return obj{"repository": r}
	}
	icon := func(edits obj) obj {
		i := obj{"src": "https://example.com/icon.png"}
		for k, v := range edits {
			if v == nil {
				delete(i, k)
			} else {
				i[k] = v
			}
		}
		return obj{"icons": arr{i}}
	}
	longSrc := func(n int) string { return "https://example.com/" + strings.Repeat("i", n-len("https://example.com/")) }
	runCases(t, []parseCase{
		{"repository minimal", repo(nil), strict, ""},
		{"repository full", repo(obj{"id": "12345", "subfolder": "servers/weather"}), strict, ""},
		{"repository http url", repo(obj{"url": "http://git.internal.example/alice/weather"}), strict, ""},
		{"repository string", obj{"repository": "https://github.com/alice/weather"}, strict, codeRepositoryInvalid},
		{"repository empty object", obj{"repository": obj{}}, strict, codeRepositoryInvalid},
		{"repository url missing", repo(obj{"url": nil}), strict, codeRepositoryInvalid},
		{"repository url empty", repo(obj{"url": ""}), strict, codeRepositoryInvalid},
		{"repository url relative", repo(obj{"url": "alice/weather"}), strict, codeRepositoryInvalid},
		{"repository url scheme-relative", repo(obj{"url": "//github.com/alice/weather"}), strict, codeRepositoryInvalid},
		{"repository url ssh", repo(obj{"url": "ssh://git@github.com/alice/weather"}), strict, codeRepositoryInvalid},
		{"repository url without host", repo(obj{"url": "https:///alice/weather"}), strict, codeRepositoryInvalid},
		{"repository url with space", repo(obj{"url": "https://github.com/alice/wea ther"}), strict, codeRepositoryInvalid},
		{"repository url number", repo(obj{"url": 5}), strict, codeRepositoryInvalid},
		{"repository url with a token", repo(obj{"url": "https://ghp_0123456789@github.com/alice/weather"}), strict, codeRepositoryInvalid},
		{"repository source missing", repo(obj{"source": nil}), strict, codeRepositoryInvalid},
		{"repository source empty", repo(obj{"source": ""}), strict, codeRepositoryInvalid},
		{"repository id number", repo(obj{"id": 12345}), strict, codeRepositoryInvalid},
		{"repository subfolder absolute", repo(obj{"subfolder": "/servers"}), strict, codeRepositoryInvalid},
		{"repository subfolder parent", repo(obj{"subfolder": "servers/../../etc"}), strict, codeRepositoryInvalid},
		{"repository subfolder only parent", repo(obj{"subfolder": ".."}), strict, codeRepositoryInvalid},
		{"repository subfolder backslash", repo(obj{"subfolder": `servers\weather`}), strict, codeRepositoryInvalid},
		{"repository subfolder number", repo(obj{"subfolder": 1}), strict, codeRepositoryInvalid},
		{"repository subfolder with dots in a name", repo(obj{"subfolder": "servers/..weather../v1"}), strict, ""},
		{"repository string (lenient)", obj{"repository": "x"}, lenient, ""},

		{"website https", obj{"websiteUrl": "https://example.com/weather?x=1"}, strict, ""},
		{"website http", obj{"websiteUrl": "http://example.com"}, strict, codeWebsiteInvalid},
		{"website relative", obj{"websiteUrl": "/weather"}, strict, codeWebsiteInvalid},
		{"website bare host", obj{"websiteUrl": "example.com"}, strict, codeWebsiteInvalid},
		{"website empty", obj{"websiteUrl": ""}, strict, codeWebsiteInvalid},
		{"website number", obj{"websiteUrl": 1}, strict, codeWebsiteInvalid},
		{"website javascript", obj{"websiteUrl": "javascript:alert(1)"}, strict, codeWebsiteInvalid},
		{"website with userinfo", obj{"websiteUrl": "https://alice:pw@example.com/"}, strict, codeWebsiteInvalid},
		{"website http (lenient)", obj{"websiteUrl": "http://example.com"}, lenient, ""},

		{"icon minimal", icon(nil), strict, ""},
		{"icon full", icon(obj{"mimeType": "image/svg+xml", "sizes": arr{"48x48", "any"}, "theme": "dark"}), strict, ""},
		{"icons empty array", obj{"icons": arr{}}, strict, ""},
		{"icons 16", obj{"icons": repeat(obj{"src": "https://example.com/i.png"}, 16)}, strict, ""},
		{"icons 17", obj{"icons": repeat(obj{"src": "https://example.com/i.png"}, 17)}, strict, codeIconInvalid},
		{"icons object", obj{"icons": obj{}}, strict, codeIconInvalid},
		{"icon string item", obj{"icons": arr{"https://example.com/i.png"}}, strict, codeIconInvalid},
		{"icon src missing", icon(obj{"src": nil}), strict, codeIconInvalid},
		{"icon src http", icon(obj{"src": "http://example.com/i.png"}), strict, codeIconInvalid},
		{"icon src data URI", icon(obj{"src": "data:image/png;base64,AAAA"}), strict, codeIconInvalid},
		{"icon src with userinfo", icon(obj{"src": "https://alice@example.com/i.png"}), strict, codeIconInvalid},
		{"icon src 255", icon(obj{"src": longSrc(255)}), strict, ""},
		{"icon src 256", icon(obj{"src": longSrc(256)}), strict, codeIconInvalid},
		{"icon mimeType gif", icon(obj{"mimeType": "image/gif"}), strict, codeIconInvalid},
		{"icon mimeType jpg", icon(obj{"mimeType": "image/jpg"}), strict, ""},
		{"icon mimeType number", icon(obj{"mimeType": 1}), strict, codeIconInvalid},
		{"icon sizes string", icon(obj{"sizes": "48x48"}), strict, codeIconInvalid},
		{"icon sizes bad item", icon(obj{"sizes": arr{"48x48", "48"}}), strict, codeIconInvalid},
		{"icon sizes uppercase", icon(obj{"sizes": arr{"48X48"}}), strict, codeIconInvalid},
		{"icon sizes number item", icon(obj{"sizes": arr{48}}), strict, codeIconInvalid},
		{"icon theme blue", icon(obj{"theme": "blue"}), strict, codeIconInvalid},
		{"icon theme light", icon(obj{"theme": "light"}), strict, ""},
		{"icons 17 (lenient)", obj{"icons": repeat(obj{"src": "x"}, 17)}, lenient, ""},
	})
}

func TestPackages(t *testing.T) {
	httpTransport := func(url any) obj {
		t := obj{"type": "streamable-http"}
		if url != nil {
			t["url"] = url
		}
		return pkg(obj{"transport": t})
	}
	onlyPackages := func(edits obj) obj {
		edits["remotes"] = nil
		return edits
	}
	runCases(t, []parseCase{
		{"stdio package only", onlyPackages(pkg(nil)), strict, ""},
		{"all optional fields", pkg(obj{
			"version": "1.4.2", "fileSha256": strings.Repeat("0a", 32), "registryBaseUrl": "https://registry.npmjs.org",
			"runtimeHint":          "npx",
			"environmentVariables": arr{obj{"name": "UNITS", "default": "metric"}},
			"runtimeArguments":     arr{obj{"type": "named", "name": "--yes"}},
			"packageArguments":     arr{obj{"type": "positional", "valueHint": "city"}, obj{"type": "positional", "value": "--stdio"}},
		}), strict, ""},
		{"packages object", obj{"packages": obj{}}, strict, codePackageInvalid},
		{"packages 32", obj{"packages": repeat(exampleStdio, 32)}, strict, ""},
		{"packages 33", obj{"packages": repeat(exampleStdio, 33)}, strict, codePackageInvalid},
		{"package string item", obj{"packages": arr{"npm:@alice/weather"}}, strict, codePackageInvalid},
		{"registryType missing", pkg(obj{"registryType": nil}), strict, codePackageInvalid},
		{"registryType empty", pkg(obj{"registryType": ""}), strict, codePackageInvalid},
		{"identifier missing", pkg(obj{"identifier": nil}), strict, codePackageInvalid},
		{"identifier number", pkg(obj{"identifier": 5}), strict, codePackageInvalid},
		{"transport missing", pkg(obj{"transport": nil}), strict, codePackageInvalid},
		{"transport string", pkg(obj{"transport": "stdio"}), strict, codePackageInvalid},
		{"transport type missing", pkg(obj{"transport": obj{}}), strict, codePackageInvalid},
		{"transport type http", pkg(obj{"transport": obj{"type": "http"}}), strict, codePackageInvalid},
		{"version latest", pkg(obj{"version": "latest"}), strict, codePackageInvalid},
		{"version empty", pkg(obj{"version": ""}), strict, codePackageInvalid},
		{"version number", pkg(obj{"version": 1}), strict, codePackageInvalid},
		{"version 256", pkg(obj{"version": strings.Repeat("1", 256)}), strict, codePackageInvalid},
		{"version 255", pkg(obj{"version": strings.Repeat("1", 255)}), strict, ""},
		{"fileSha256 uppercase", pkg(obj{"fileSha256": strings.Repeat("0A", 32)}), strict, codePackageInvalid},
		{"fileSha256 short", pkg(obj{"fileSha256": strings.Repeat("0", 63)}), strict, codePackageInvalid},
		{"fileSha256 prefixed", pkg(obj{"fileSha256": "sha256:" + strings.Repeat("0", 64)}), strict, codePackageInvalid},
		{"registryBaseUrl relative", pkg(obj{"registryBaseUrl": "registry.npmjs.org"}), strict, codePackageInvalid},
		{"registryBaseUrl http", pkg(obj{"registryBaseUrl": "http://npm.internal.example"}), strict, ""},
		{"registryBaseUrl with a token", pkg(obj{"registryBaseUrl": "https://ci:npm_0123456789@npm.internal.example"}), strict, codePackageInvalid},
		{"runtimeHint number", pkg(obj{"runtimeHint": 1}), strict, codePackageInvalid},

		{"http transport, localhost", httpTransport("http://localhost:3000/mcp"), strict, ""},
		{"http transport, https", httpTransport("https://example.com/mcp"), strict, ""},
		{"http transport, template start", httpTransport("{base_url}/mcp"), strict, ""},
		{"http transport, template inside", httpTransport("http://localhost:{port}/mcp"), strict, ""},
		{"sse transport", pkg(obj{"transport": obj{"type": "sse", "url": "http://127.0.0.1:8080/sse"}}), strict, ""},
		{"http transport, url missing", httpTransport(nil), strict, codePackageInvalid},
		{"http transport, url number", httpTransport(8080), strict, codePackageInvalid},
		{"http transport, ftp", httpTransport("ftp://example.com/mcp"), strict, codePackageInvalid},
		{"http transport, relative", httpTransport("/mcp"), strict, codePackageInvalid},
		{"http transport, space", httpTransport("http://localhost:3000/m cp"), strict, codePackageInvalid},
		{"http transport, no-break space", httpTransport("http://localhost:3000/m\u00a0cp"), strict, codePackageInvalid},
		{"http transport, bad template name", httpTransport("{1port}/mcp"), strict, codePackageInvalid},
		{"sse transport, url missing", pkg(obj{"transport": obj{"type": "sse"}}), strict, codePackageInvalid},
		{"stdio ignores url", pkg(obj{"transport": obj{"type": "stdio", "url": 5}}), strict, ""},

		{"transport headers", pkg(obj{"transport": obj{"type": "sse", "url": "http://localhost/sse", "headers": arr{obj{"name": "X-Region"}}}}), strict, ""},
		{"transport headers object", pkg(obj{"transport": obj{"type": "stdio", "headers": obj{}}}), strict, codePackageInvalid},
		{"transport header without name", pkg(obj{"transport": obj{"type": "stdio", "headers": arr{obj{"value": "v"}}}}), strict, codePackageInvalid},
		{"environmentVariables string", pkg(obj{"environmentVariables": "A=b"}), strict, codePackageInvalid},
		{"environmentVariables string item", pkg(obj{"environmentVariables": arr{"A=b"}}), strict, codePackageInvalid},
		{"environmentVariables empty name", pkg(obj{"environmentVariables": arr{obj{"name": ""}}}), strict, codePackageInvalid},
		{"environmentVariables numeric name", pkg(obj{"environmentVariables": arr{obj{"name": 1}}}), strict, codePackageInvalid},
		{"environmentVariables empty array", pkg(obj{"environmentVariables": arr{}}), strict, ""},

		{"runtimeArguments object", pkg(obj{"runtimeArguments": obj{}}), strict, codePackageInvalid},
		{"runtimeArguments string item", pkg(obj{"runtimeArguments": arr{"--yes"}}), strict, codePackageInvalid},
		{"runtimeArguments unknown type", pkg(obj{"runtimeArguments": arr{obj{"type": "flag", "name": "--yes"}}}), strict, codePackageInvalid},
		{"runtimeArguments type missing", pkg(obj{"runtimeArguments": arr{obj{"name": "--yes"}}}), strict, codePackageInvalid},
		{"runtimeArguments named without name", pkg(obj{"runtimeArguments": arr{obj{"type": "named", "value": "1"}}}), strict, codePackageInvalid},
		{"runtimeArguments named with empty name", pkg(obj{"runtimeArguments": arr{obj{"type": "named", "name": ""}}}), strict, codePackageInvalid},
		{"packageArguments positional without value or hint", pkg(obj{"packageArguments": arr{obj{"type": "positional", "description": "city"}}}), strict, codePackageInvalid},
		{"packageArguments positional with numeric value", pkg(obj{"packageArguments": arr{obj{"type": "positional", "value": 1}}}), strict, codePackageInvalid},
		{"packageArguments positional with hint", pkg(obj{"packageArguments": arr{obj{"type": "positional", "valueHint": "city"}}}), strict, ""},
		{"packageArguments positional with value", pkg(obj{"packageArguments": arr{obj{"type": "positional", "value": "serve"}}}), strict, ""},
		{"second package is checked", obj{"packages": arr{exampleStdio, obj{"registryType": "pypi"}}}, strict, codePackageInvalid},

		{"packages object (lenient)", obj{"packages": obj{}}, lenient, ""},
		{"package without transport (lenient)", pkg(obj{"transport": nil}), lenient, ""},
	})
}

func TestRemotes(t *testing.T) {
	two := func(a, b string) obj {
		return obj{"remotes": arr{obj{"type": "sse", "url": a}, obj{"type": "streamable-http", "url": b}}}
	}
	runCases(t, []parseCase{
		{"sse", remote(obj{"type": "sse"}), strict, ""},
		{"query string", remote(obj{"url": "https://mcp.example.com/mcp?region=eu&v=2"}), strict, ""},
		{"port", remote(obj{"url": "https://mcp.example.com:8443/mcp"}), strict, ""},
		{"IPv6 host", remote(obj{"url": "https://[2001:db8::1]:8443/mcp"}), strict, ""},
		{"template start", remote(obj{"url": "{base_url}/mcp"}), strict, ""},
		{"template start is not parsed further", remote(obj{"url": "{base_url}"}), strict, ""},
		{"template in host", remote(obj{"url": "https://{tenant}.example.com/mcp"}), strict, ""},
		{"template as host", remote(obj{"url": "https://{host}/mcp"}), strict, ""},
		{"template in port", remote(obj{"url": "https://mcp.example.com:{port}/mcp"}), strict, ""},
		{"template in path and query", remote(obj{"url": "https://mcp.example.com/{team}/mcp?key={key}"}), strict, ""},
		{"headers", remote(obj{"headers": arr{obj{"name": "Authorization", "value": "Bearer {token}", "isSecret": true}}}), strict, ""},
		{"32 remotes", obj{"remotes": distinctRemotes(32)}, strict, ""},

		{"remotes object", obj{"remotes": obj{}}, strict, codeRemoteInvalid},
		{"33 remotes", obj{"remotes": distinctRemotes(33)}, strict, codeRemoteInvalid},
		{"string item", obj{"remotes": arr{exampleURL}}, strict, codeRemoteInvalid},
		{"type missing", remote(obj{"type": nil}), strict, codeRemoteInvalid},
		{"type stdio", remote(obj{"type": "stdio"}), strict, codeRemoteInvalid},
		{"type uppercase", remote(obj{"type": "SSE"}), strict, codeRemoteInvalid},
		{"url missing", remote(obj{"url": nil}), strict, codeRemoteInvalid},
		{"url number", remote(obj{"url": 5}), strict, codeRemoteInvalid},
		{"url empty", remote(obj{"url": ""}), strict, codeRemoteInvalid},
		{"url ftp", remote(obj{"url": "ftp://mcp.example.com/mcp"}), strict, codeRemoteInvalid},
		{"url websocket", remote(obj{"url": "wss://mcp.example.com/mcp"}), strict, codeRemoteInvalid},
		{"url relative", remote(obj{"url": "/mcp"}), strict, codeRemoteInvalid},
		{"url uppercase scheme", remote(obj{"url": "HTTPS://mcp.example.com/mcp"}), strict, codeRemoteInvalid},
		{"url with space", remote(obj{"url": "https://mcp.example.com/m cp"}), strict, codeRemoteInvalid},
		{"url with newline", remote(obj{"url": "https://mcp.example.com/mcp\n"}), strict, codeRemoteInvalid},
		{"url with ideographic space", remote(obj{"url": "https://mcp.example.com/m\u3000cp"}), strict, codeRemoteInvalid},
		{"url without host", remote(obj{"url": "https:///mcp"}), strict, codeRemoteInvalid},
		{"url with only a port", remote(obj{"url": "https://:8443/mcp"}), strict, codeRemoteInvalid},
		{"url with bad port", remote(obj{"url": "https://mcp.example.com:port/mcp"}), strict, codeRemoteInvalid},
		{"url with userinfo", remote(obj{"url": "https://alice:s3cret@mcp.example.com/mcp"}), strict, codeRemoteInvalid},
		{"url with user only", remote(obj{"url": "https://alice@mcp.example.com/mcp"}), strict, codeRemoteInvalid},
		{"url with templated userinfo", remote(obj{"url": "https://{user}:{password}@mcp.example.com/mcp"}), strict, codeRemoteInvalid},
		{"url with fragment", remote(obj{"url": "https://mcp.example.com/mcp#tools"}), strict, codeRemoteInvalid},
		{"url with empty fragment", remote(obj{"url": "https://mcp.example.com/mcp#"}), strict, codeRemoteInvalid},
		{"http", remote(obj{"url": "http://mcp.example.com/mcp"}), strict, codeRemoteInvalid},
		{"http on a private instance", remote(obj{"url": "http://mcp.internal.example/mcp"}), strictHTTP, ""},
		{"https on a private instance", remote(nil), strictHTTP, ""},
		{"userinfo on a private instance", remote(obj{"url": "http://alice@mcp.internal.example/mcp"}), strictHTTP, codeRemoteInvalid},
		{"duplicate url", two(exampleURL, exampleURL), strict, codeRemoteInvalid},
		{"distinct urls", two(exampleURL, exampleURL+"/v2"), strict, ""},
		{"duplicate template url", two("{base}/mcp", "{base}/mcp"), strict, codeRemoteInvalid},
		{"headers object", remote(obj{"headers": obj{}}), strict, codeRemoteInvalid},
		{"header without name", remote(obj{"headers": arr{obj{"value": "{token}"}}}), strict, codeRemoteInvalid},
		{"header string item", remote(obj{"headers": arr{"Authorization"}}), strict, codeRemoteInvalid},
		{"second remote is checked", obj{"remotes": arr{obj{"type": "sse", "url": exampleURL}, obj{"type": "sse"}}}, strict, codeRemoteInvalid},

		{"remotes object (lenient)", obj{"remotes": obj{}}, lenient, ""},
		{"duplicate url (lenient)", two(exampleURL, exampleURL), lenient, ""},
		{"http (lenient)", remote(obj{"url": "http://mcp.example.com/mcp"}), lenient, ""},
	})
}

func TestNoTransport(t *testing.T) {
	runCases(t, []parseCase{
		{"neither", obj{"remotes": nil}, strict, codeNoTransport},
		{"both empty", obj{"remotes": arr{}, "packages": arr{}}, strict, codeNoTransport},
		{"empty remotes", obj{"remotes": arr{}}, strict, codeNoTransport},
		{"empty packages", obj{"remotes": nil, "packages": arr{}}, strict, codeNoTransport},
		{"only a remote", obj{}, strict, ""},
		{"only a package", obj{"remotes": nil, "packages": arr{exampleStdio}}, strict, ""},
		{"package and empty remotes", obj{"remotes": arr{}, "packages": arr{exampleStdio}}, strict, ""},
		{"both", obj{"packages": arr{exampleStdio}}, strict, ""},
		{"neither (lenient)", obj{"remotes": nil}, lenient, ""},
	})
}

func TestMeta(t *testing.T) {
	big := func(n int) obj { // publisher object whose compact JSON is exactly n bytes
		return obj{"k": strings.Repeat("v", n-len(`{"k":""}`))}
	}
	runCases(t, []parseCase{
		{"publisher metadata", obj{"_meta": obj{MetaPublisher: obj{"tool": "publisher-cli", "build": obj{"n": 7}}}}, strict, ""},
		{"empty publisher metadata", obj{"_meta": obj{MetaPublisher: obj{}}}, strict, ""},
		{"empty _meta", obj{"_meta": obj{}}, strict, ""},
		{"publisher 4096 bytes", obj{"_meta": obj{MetaPublisher: big(4096)}}, strict, ""},
		{"publisher 4097 bytes", obj{"_meta": obj{MetaPublisher: big(4097)}}, strict, codeMetaInvalid},
		{"_meta string", obj{"_meta": "x"}, strict, codeMetaInvalid},
		{"_meta array", obj{"_meta": arr{}}, strict, codeMetaInvalid},
		{"unknown key", obj{"_meta": obj{"com.example/x": obj{}}}, strict, codeMetaInvalid},
		{"unknown key beside publisher", obj{"_meta": obj{MetaPublisher: obj{}, "com.example/x": 1}}, strict, codeMetaInvalid},
		{"publisher string", obj{"_meta": obj{MetaPublisher: "x"}}, strict, codeMetaInvalid},
		{"publisher array", obj{"_meta": obj{MetaPublisher: arr{}}}, strict, codeMetaInvalid},
		// Registry-managed keys are removed before the check, so a document
		// copied from a registry response can be published again.
		{"registry keys only", obj{"_meta": obj{MetaOfficial: obj{"status": "active"}, MetaRegistry: obj{"mode": "mirrored"}}}, strict, ""},
		{"registry keys beside publisher", obj{"_meta": obj{MetaOfficial: obj{}, MetaPublisher: obj{"a": 1}}}, strict, ""},
		{"registry keys beside unknown", obj{"_meta": obj{MetaOfficial: obj{}, "com.example/x": 1}}, strict, codeMetaInvalid},

		{"_meta string (lenient)", obj{"_meta": "x"}, lenient, ""},
		{"unknown key (lenient)", obj{"_meta": obj{"com.example/x": obj{}}}, lenient, ""},
		{"publisher 4097 bytes (lenient)", obj{"_meta": obj{MetaPublisher: big(4097)}}, lenient, ""},
	})
	if n := len(mustJSON(t, big(4096))) - 1; n != 4096 { // Encode appends a newline
		t.Fatalf("test helper builds %d bytes, want 4096", n)
	}
}

func TestRegistryMetaIsStripped(t *testing.T) {
	official := obj{"status": "active", "publishedAt": "2025-11-12T00:00:00Z", "isLatest": true}
	entry := obj{"mode": "registered", "listed": true}
	publisher := obj{"tool": "publisher-cli"}
	cases := []struct {
		name       string
		meta, same any // parsing with meta must equal parsing with same (nil: no _meta at all)
	}{
		{"official only", obj{MetaOfficial: official}, nil},
		{"registry only", obj{MetaRegistry: entry}, nil},
		{"both", obj{MetaOfficial: official, MetaRegistry: entry}, nil},
		{"both beside publisher", obj{MetaOfficial: official, MetaRegistry: entry, MetaPublisher: publisher}, obj{MetaPublisher: publisher}},
		{"scalar values are stripped too", obj{MetaOfficial: "forged", MetaRegistry: 1, MetaPublisher: publisher}, obj{MetaPublisher: publisher}},
	}
	for _, o := range []Options{strict, lenient} {
		plain, err := Parse(mustJSON(t, minimal()), o)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range cases {
			got, err := Parse(mustJSON(t, with(obj{"_meta": tc.meta})), o)
			if err != nil {
				t.Errorf("%s (strict=%v): %v", tc.name, o.Strict, err)
				continue
			}
			want := plain
			if tc.same != nil {
				if want, err = Parse(mustJSON(t, with(obj{"_meta": tc.same})), o); err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(got.Canonical, want.Canonical) || got.Digest != want.Digest {
				t.Errorf("%s (strict=%v):\n got %s\nwant %s", tc.name, o.Strict, got.Canonical, want.Canonical)
			}
			for _, key := range []string{MetaOfficial, MetaRegistry} {
				if bytes.Contains(got.Canonical, []byte(key)) {
					t.Errorf("%s (strict=%v): %s survived", tc.name, o.Strict, key)
				}
			}
			// The digest moved only because the keys were there.
			_, raw, err := Canonicalize(mustJSON(t, with(obj{"_meta": tc.meta})))
			if err != nil || raw == got.Digest {
				t.Errorf("%s: stripping did not change the digest (%v)", tc.name, err)
			}
		}

		// Nothing to strip: Canonical is exactly the canonical input.
		untouched := []any{nil, obj{}, obj{MetaPublisher: publisher}}
		if !o.Strict {
			untouched = append(untouched, obj{"com.example/x": 1}, "not an object", arr{MetaOfficial},
				obj{"nested": obj{MetaOfficial: official}}) // only top-level _meta keys are registry-managed
		}
		for _, meta := range untouched {
			data := mustJSON(t, with(obj{"_meta": meta}))
			got, err := Parse(data, o)
			if err != nil {
				t.Errorf("_meta %v (strict=%v): %v", meta, o.Strict, err)
				continue
			}
			canonical, digest, err := Canonicalize(data)
			if err != nil || !bytes.Equal(got.Canonical, canonical) || got.Digest != digest {
				t.Errorf("_meta %v (strict=%v): document without registry keys was changed: %s", meta, o.Strict, got.Canonical)
			}
		}
	}

	// The same key elsewhere in the document is not registry metadata.
	data := mustJSON(t, with(obj{"remotes": arr{obj{"type": "sse", "url": exampleURL, MetaOfficial: 1}}}))
	p, err := Parse(data, strict)
	if err != nil || !bytes.Contains(p.Canonical, []byte(MetaOfficial)) {
		t.Errorf("nested key was removed or rejected: %v", err)
	}
}

// secretDoc places one input at every position the official schema defines.
func secretDoc(input obj) map[string]obj {
	variables := obj{"type": "sse", "url": "https://{tenant}.example.com/sse", "variables": obj{"tenant": input}}
	nested := obj{"name": "X-Trace", "value": "trace-{id}", "variables": obj{"id": obj{"variables": obj{"inner": input}}}}
	stdio := func(key string, in obj) obj {
		return obj{"registryType": "npm", "identifier": "@alice/weather", "transport": obj{"type": "stdio"}, key: arr{in}}
	}
	argument := obj{"type": "named", "name": "--token"}
	for k, v := range input {
		argument[k] = v
	}
	return map[string]obj{
		"remote header":          with(obj{"remotes": arr{obj{"type": "sse", "url": exampleURL, "headers": arr{input}}}}),
		"remote variable":        with(obj{"remotes": arr{variables}}),
		"nested variable":        with(obj{"remotes": arr{obj{"type": "sse", "url": exampleURL, "headers": arr{nested}}}}),
		"environment variable":   with(obj{"packages": arr{stdio("environmentVariables", input)}}),
		"runtime argument":       with(obj{"packages": arr{stdio("runtimeArguments", argument)}}),
		"package argument":       with(obj{"packages": arr{stdio("packageArguments", argument)}}),
		"package header":         with(obj{"packages": arr{obj{"registryType": "npm", "identifier": "x", "transport": obj{"type": "sse", "url": "http://localhost/sse", "headers": arr{input}}}}}),
		"second header":          with(obj{"remotes": arr{obj{"type": "sse", "url": exampleURL, "headers": arr{obj{"name": "X-Region", "value": "eu"}, input}}}}),
		"position not in schema": with(obj{"remotes": arr{obj{"type": "sse", "url": exampleURL, "queryParameters": arr{input}}}}),
	}
}

func TestSecretValues(t *testing.T) {
	const literal = "sk-live-0123456789"
	inputs := []struct {
		name    string
		input   obj
		literal bool
	}{
		{"literal value", obj{"name": "API_KEY", "isSecret": true, "value": literal}, true},
		{"literal default", obj{"name": "API_KEY", "isSecret": true, "default": literal}, true},
		{"literal value and default", obj{"name": "API_KEY", "isSecret": true, "value": literal, "default": literal}, true},
		{"placeholder value, literal default", obj{"name": "API_KEY", "isSecret": true, "value": "{api_key}", "default": literal}, true},
		{"literal with braces that are not a variable", obj{"name": "API_KEY", "isSecret": true, "value": "{}" + literal + "{1x}{a-b}"}, true},
		{"numeric value", obj{"name": "API_KEY", "isSecret": true, "value": 1234567890}, true},
		{"placeholder value", obj{"name": "API_KEY", "isSecret": true, "value": "{api_key}"}, false},
		{"placeholder inside value", obj{"name": "API_KEY", "isSecret": true, "value": "Bearer {token}"}, false},
		{"placeholder default", obj{"name": "API_KEY", "isSecret": true, "default": "{api_key}"}, false},
		{"empty value", obj{"name": "API_KEY", "isSecret": true, "value": ""}, false},
		{"no value", obj{"name": "API_KEY", "isSecret": true, "description": "Your key", "isRequired": true}, false},
		{"not secret", obj{"name": "REGION", "isSecret": false, "value": literal, "default": literal}, false},
		{"isSecret absent", obj{"name": "REGION", "value": literal}, false},
		{"isSecret is not a boolean", obj{"name": "REGION", "isSecret": "true", "value": literal}, false},
		// A credential by name is secret whatever the flag says.
		{"credential name without the flag", obj{"name": "API_KEY", "value": literal}, true},
		{"credential name flagged not secret", obj{"name": "GITHUB_TOKEN", "isSecret": false, "value": literal}, true},
		{"authorization header without the flag", obj{"name": "Authorization", "value": "Bearer " + literal}, true},
		{"credential default without the flag", obj{"name": "X-Api-Key", "default": literal}, true},
		{"credential name with a placeholder", obj{"name": "DB_PASSWORD", "value": "{password}"}, false},
		{"plural is not a credential", obj{"name": "MAX_TOKENS", "value": "4096"}, false},
	}
	for _, in := range inputs {
		for position, doc := range secretDoc(in.input) {
			name := in.name + " in " + position
			data := mustJSON(t, doc)
			want := ""
			if in.literal {
				want = codeSecretValue
			}

			_, err := Parse(data, strict)
			if codeOf(err) != want {
				t.Errorf("%s: strict code %q (%v), want %q", name, codeOf(err), err, want)
			}
			if err != nil && strings.Contains(err.Error(), literal) {
				t.Errorf("%s: error message repeats the secret: %v", name, err)
			}

			p, err := Parse(data, lenient)
			if err != nil {
				t.Errorf("%s: lenient: %v", name, err)
				continue
			}
			canonical, digest, err := Canonicalize(data)
			if err != nil {
				t.Fatal(err)
			}
			if in.literal {
				if bytes.Contains(p.Canonical, []byte(literal)) || bytes.Contains(p.Canonical, []byte("1234567890")) {
					t.Errorf("%s: literal survived redaction: %s", name, p.Canonical)
				}
				if p.Digest == digest {
					t.Errorf("%s: redaction did not change the digest", name)
				}
				// Only the literal goes: the input, its name and its flag stay.
				inputName, _ := in.input["name"].(string)
				flagged, _ := in.input["isSecret"].(bool)
				if !bytes.Contains(p.Canonical, bytes.TrimSpace(mustJSON(t, inputName))) || (flagged && !bytes.Contains(p.Canonical, []byte(`"isSecret":true`))) {
					t.Errorf("%s: redaction removed more than the literal: %s", name, p.Canonical)
				}
			} else if !bytes.Equal(p.Canonical, canonical) {
				t.Errorf("%s: document without a secret literal was changed:\n got %s\nwant %s", name, p.Canonical, canonical)
			}
		}
	}
}

func TestLenientRedactionDetail(t *testing.T) {
	data := []byte(`{
	  "name": "io.github.alice/weather", "version": "1.0.0",
	  "remotes": [{"type": "sse", "url": "https://mcp.example.com/sse", "headers": [
	    {"name": "Authorization", "isSecret": true, "value": "Bearer abc123", "default": "Bearer {token}", "description": "token"},
	    {"name": "X-Api-Key", "isSecret": true, "value": "{api_key}", "default": "fallback-literal"},
	    {"name": "X-Tenant", "isSecret": true, "value": "{tenant}", "default": "{tenant}"},
	    {"name": "X-Region", "value": "eu-west-1", "default": "eu-west-1"}
	  ]}]
	}`)
	if _, err := Parse(data, strict); codeOf(err) != codeSecretValue {
		t.Fatalf("strict code %q", codeOf(err))
	} else if want := "remotes[0].headers[0].value: "; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("message %q does not start with %q", err.Error(), want)
	}
	p, err := Parse(data, lenient)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"name":"io.github.alice/weather","remotes":[{"headers":[` +
		// A literal value takes the default with it, placeholder or not.
		`{"description":"token","isSecret":true,"name":"Authorization"},` +
		`{"isSecret":true,"name":"X-Api-Key","value":"{api_key}"},` +
		`{"default":"{tenant}","isSecret":true,"name":"X-Tenant","value":"{tenant}"},` +
		`{"default":"eu-west-1","name":"X-Region","value":"eu-west-1"}` +
		`],"type":"sse","url":"https://mcp.example.com/sse"}],"version":"1.0.0"}`
	if string(p.Canonical) != want {
		t.Errorf("\n got %s\nwant %s", p.Canonical, want)
	}
	again, err := Parse(p.Canonical, lenient)
	if err != nil || !bytes.Equal(again.Canonical, p.Canonical) || again.Digest != p.Digest {
		t.Errorf("redacted document is not stable: %v", err)
	}
	// What lenient mode produced is publishable as far as secrets go.
	if _, err := Parse(p.Canonical, strict); codeOf(err) != codeDescriptionInvalid {
		t.Errorf("redacted document under strict: code %q", codeOf(err))
	}
}

func TestSecretPathInMessage(t *testing.T) {
	secret := obj{"isSecret": true, "value": "literal"}
	long := strings.Repeat("k", 150)
	cases := []struct {
		name   string
		edits  obj
		prefix string
	}{
		{"array in array", obj{"x": arr{arr{1, secret}}}, "x[0][1].value: "},
		{"default", obj{"x": obj{"y": obj{"isSecret": true, "default": "literal"}}}, "x.y.default: "},
		{"first in key order", obj{"b": secret, "a": arr{secret}}, "a[0].value: "},
		{"top level", secret, "value: "},
		{"long keys are cut", obj{long: obj{long: secret}}, long + "." + strings.Repeat("k", 49) + "...: "},
	}
	for _, tc := range cases {
		_, err := Parse(mustJSON(t, with(tc.edits)), strict)
		if codeOf(err) != codeSecretValue || !strings.HasPrefix(err.Error(), tc.prefix) {
			t.Errorf("%s: got %v, want prefix %q", tc.name, err, tc.prefix)
		}
		if err != nil && strings.Contains(err.Error(), "literal") {
			t.Errorf("%s: message repeats the secret", tc.name)
		}
	}
	if looksLikeRange("") {
		t.Error("an empty version is not a range")
	}
}

func TestLenientBestEffortFields(t *testing.T) {
	data := []byte(`{
	  "name": "io.github.Alice/weather", "version": "^1.x",
	  "title": 7, "description": ["not", "a", "string"],
	  "repository": "nope", "websiteUrl": 1, "icons": {}, "$schema": false,
	  "packages": [1, "two", {"registryType": null}],
	  "remotes": [
	    "https://a.example/mcp",
	    {"type": "sse", "url": "https://one.example/sse"},
	    {"type": "stdio", "url": "https://two.example/"},
	    {"type": "streamable-http"},
	    {"type": "streamable-http", "url": 5},
	    {"type": "streamable-http", "url": "ftp://three.example/"},
	    {"type": "streamable-http", "url": "https:///no-host"},
	    {"type": "streamable-http", "url": "http://four.example/mcp", "headers": "malformed"},
	    {"type": "sse", "url": "https://one.example/sse"},
	    {"type": "streamable-http", "url": "{base}/mcp"},
	    {"type": "streamable-http", "url": "https://{tenant}.five.example/mcp#frag"},
	    null
	  ],
	  "futureField": {"anything": [1, 2, 3]}
	}`)
	p, err := Parse(data, lenient)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "" || p.Description != "" || p.Namespace != "io.github.alice" || p.Version != "^1.x" || !p.HasPackages {
		t.Errorf("fields: %+v", p)
	}
	want := []Remote{
		{"sse", "https://one.example/sse"},
		{"streamable-http", "http://four.example/mcp"},
		{"sse", "https://one.example/sse"},
		{"streamable-http", "{base}/mcp"},
		{"streamable-http", "https://{tenant}.five.example/mcp#frag"},
	}
	if !reflect.DeepEqual(p.Remotes, want) {
		t.Errorf("remotes:\n got %v\nwant %v", p.Remotes, want)
	}
	canonical, digest, err := Canonicalize(data)
	if err != nil || !bytes.Equal(p.Canonical, canonical) || p.Digest != digest {
		t.Errorf("lenient mode rewrote a document it had no reason to touch: %v", err)
	}

	p, err = Parse([]byte(`{"name":"io.github.alice/weather","version":"1","title":"Weather","description":"`+strings.Repeat("d", 300)+`","packages":[],"remotes":7}`), lenient)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Weather" || len(p.Description) != 300 || p.HasPackages || p.Remotes != nil {
		t.Errorf("fields: %+v", p)
	}
}

func TestRemotesAndPackagesReported(t *testing.T) {
	doc := with(obj{
		"title": "Weather",
		"remotes": arr{
			obj{"type": "sse", "url": "https://b.example/sse"},
			obj{"type": "streamable-http", "url": "https://a.example/mcp"},
			obj{"type": "streamable-http", "url": "{base_url}/mcp"},
		},
		"packages": arr{exampleStdio},
	})
	p, err := Parse(mustJSON(t, doc), strict)
	if err != nil {
		t.Fatal(err)
	}
	want := []Remote{{"sse", "https://b.example/sse"}, {"streamable-http", "https://a.example/mcp"}, {"streamable-http", "{base_url}/mcp"}}
	if !reflect.DeepEqual(p.Remotes, want) || !p.HasPackages || p.Title != "Weather" {
		t.Errorf("got %+v", p)
	}
	p, err = Parse(mustJSON(t, with(obj{"remotes": nil, "packages": arr{exampleStdio}})), strict)
	if err != nil || p.Remotes != nil || !p.HasPackages {
		t.Errorf("package-only document: %+v %v", p, err)
	}
}

// officialDocument follows the shape of entries served by
// registry.modelcontextprotocol.io: a hosted remote plus an npm package.
const officialDocument = `{
  "$schema": "https://static.modelcontextprotocol.io/schemas/2025-12-11/server.schema.json",
  "name": "io.github.Alice/weather-mcp",
  "title": "Weather",
  "description": "Current conditions, forecasts and severe weather alerts for any city <worldwide> & beyond.",
  "version": "1.4.2",
  "websiteUrl": "https://weather.example.com/mcp",
  "repository": {
    "url": "https://github.com/Alice/weather-mcp",
    "source": "github",
    "id": "987654321",
    "subfolder": "packages/server"
  },
  "icons": [
    {"src": "https://weather.example.com/icon-48.png", "mimeType": "image/png", "sizes": ["48x48"], "theme": "light"},
    {"src": "https://weather.example.com/icon.svg", "mimeType": "image/svg+xml", "sizes": ["any"]}
  ],
  "remotes": [
    {
      "type": "streamable-http",
      "url": "https://{region}.weather.example.com/mcp",
      "variables": {
        "region": {"description": "Deployment region", "default": "eu", "choices": ["eu", "us"]}
      },
      "headers": [
        {
          "name": "Authorization",
          "description": "API key issued at weather.example.com",
          "value": "Bearer {api_key}",
          "isRequired": true,
          "isSecret": true,
          "variables": {
            "api_key": {"description": "Your API key", "isRequired": true, "isSecret": true, "format": "string"}
          }
        }
      ]
    },
    {"type": "sse", "url": "https://weather.example.com/sse"}
  ],
  "packages": [
    {
      "registryType": "npm",
      "registryBaseUrl": "https://registry.npmjs.org",
      "identifier": "@alice/weather-mcp",
      "version": "1.4.2",
      "runtimeHint": "npx",
      "transport": {"type": "stdio"},
      "runtimeArguments": [
        {"type": "named", "name": "--yes", "description": "Skip the install prompt"}
      ],
      "packageArguments": [
        {"type": "positional", "valueHint": "units", "default": "metric", "choices": ["metric", "imperial"]},
        {"type": "named", "name": "--cache-ttl", "value": "{ttl}", "variables": {"ttl": {"format": "number", "default": "300"}}}
      ],
      "environmentVariables": [
        {"name": "WEATHER_API_KEY", "description": "Your API key", "isRequired": true, "isSecret": true, "format": "string"},
        {"name": "WEATHER_UNITS", "default": "metric"}
      ]
    }
  ],
  "_meta": {
    "io.modelcontextprotocol.registry/publisher-provided": {"tool": "mcp-publisher", "version": "1.3.0", "buildInfo": {"commit": "0a1b2c3", "pipelineId": 4821}},
    "io.modelcontextprotocol.registry/official": {"status": "active", "publishedAt": "2025-11-12T08:00:00.000000Z", "updatedAt": "2025-11-12T08:00:00.000000Z", "isLatest": true}
  }
}`

func TestOfficialDocument(t *testing.T) {
	for _, o := range []Options{strict, lenient} {
		p, err := Parse([]byte(officialDocument), o)
		if err != nil {
			t.Fatalf("strict=%v: %v", o.Strict, err)
		}
		if p.Name != "io.github.Alice/weather-mcp" || p.Namespace != "io.github.alice" || p.Server != "weather-mcp" ||
			p.Title != "Weather" || p.Version != "1.4.2" || !p.HasPackages ||
			p.Description != "Current conditions, forecasts and severe weather alerts for any city <worldwide> & beyond." {
			t.Errorf("strict=%v: fields %+v", o.Strict, p)
		}
		want := []Remote{{"streamable-http", "https://{region}.weather.example.com/mcp"}, {"sse", "https://weather.example.com/sse"}}
		if !reflect.DeepEqual(p.Remotes, want) {
			t.Errorf("strict=%v: remotes %v", o.Strict, p.Remotes)
		}
		sum := sha256.Sum256(p.Canonical)
		if p.Digest != "sha256:"+hex.EncodeToString(sum[:]) {
			t.Errorf("strict=%v: digest is not sha256 of Canonical", o.Strict)
		}
		if bytes.Contains(p.Canonical, []byte(MetaOfficial)) || !bytes.Contains(p.Canonical, []byte(MetaPublisher)) {
			t.Errorf("strict=%v: _meta handling: %s", o.Strict, p.Canonical)
		}
		if !bytes.Contains(p.Canonical, []byte("<worldwide> & beyond")) {
			t.Errorf("strict=%v: HTML characters were escaped", o.Strict)
		}
		// Everything except the registry-managed key survives, value for value.
		var got, orig map[string]any
		if err := json.Unmarshal(p.Canonical, &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(officialDocument), &orig); err != nil {
			t.Fatal(err)
		}
		delete(orig["_meta"].(map[string]any), MetaOfficial)
		if !reflect.DeepEqual(got, orig) {
			t.Errorf("strict=%v: canonical document differs from the input", o.Strict)
		}
		again, err := Parse(p.Canonical, o)
		if err != nil || !bytes.Equal(again.Canonical, p.Canonical) || again.Digest != p.Digest {
			t.Errorf("strict=%v: not idempotent: %v", o.Strict, err)
		}
	}
	a, _ := Parse([]byte(officialDocument), strict)
	b, _ := Parse([]byte(officialDocument), lenient)
	if a.Digest != b.Digest {
		t.Error("a publishable document must have one digest in both modes")
	}
}

// Every code must be reachable; a new code needs a case here and above.
func TestEveryCodeIsCovered(t *testing.T) {
	cases := map[string][]byte{
		codeTooLarge:           bytes.Repeat([]byte(" "), 64<<10+1),
		codeInvalidJSON:        []byte(`{"name":"io.github.alice/weather","name":"io.github.bob/weather"}`),
		codeNameInvalid:        mustJSON(t, with(obj{"name": "weather"})),
		codeNamespaceInvalid:   mustJSON(t, with(obj{"name": "alice/weather"})),
		codeVersionInvalid:     mustJSON(t, with(obj{"version": "latest"})),
		codeSecretValue:        mustJSON(t, with(remote(obj{"headers": arr{obj{"name": "Authorization", "isSecret": true, "value": "abc"}}}))),
		codeDescriptionInvalid: mustJSON(t, with(obj{"description": nil})),
		codeTitleInvalid:       mustJSON(t, with(obj{"title": ""})),
		codeSchemaInvalid:      mustJSON(t, with(obj{"$schema": 1})),
		codeRepositoryInvalid:  mustJSON(t, with(obj{"repository": obj{"url": "https://github.com/alice/weather"}})),
		codeWebsiteInvalid:     mustJSON(t, with(obj{"websiteUrl": "http://example.com"})),
		codeIconInvalid:        mustJSON(t, with(obj{"icons": arr{obj{}}})),
		codePackageInvalid:     mustJSON(t, with(obj{"packages": arr{obj{}}})),
		codeRemoteInvalid:      mustJSON(t, with(obj{"remotes": arr{obj{}}})),
		codeNoTransport:        mustJSON(t, with(obj{"remotes": nil})),
		codeMetaInvalid:        mustJSON(t, with(obj{"_meta": obj{"x": 1}})),
	}
	for code, data := range cases {
		_, err := Parse(data, strict)
		if codeOf(err) != code {
			t.Errorf("%s: got code %q (%v)", code, codeOf(err), err)
		}
		if err != nil && err.Error() == "" {
			t.Errorf("%s: empty message", code)
		}
	}
	if len(cases) != 16 {
		t.Errorf("%d codes covered, want 16", len(cases))
	}
}
