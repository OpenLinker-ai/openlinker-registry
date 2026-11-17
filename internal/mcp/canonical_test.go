package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func codeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if err != nil {
		return "untyped: " + err.Error()
	}
	return ""
}

func TestCanonicalizeExact(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"sorted keys, no whitespace",
			" {\n\t\"b\" : 1 ,\r\n \"a\" : [ true , false , null ] } ",
			`{"a":[true,false,null],"b":1}`},
		{"nested objects sorted, array order kept",
			`{"z":[{"b":2,"a":1},[3,2,1]],"a":{"y":{},"x":[]}}`,
			`{"a":{"x":[],"y":{}},"z":[{"a":1,"b":2},[3,2,1]]}`},
		{"numbers verbatim",
			`[9007199254740993, 1.10, 1.0, 1E5, 1e+5, -0, 0.000, 123456789012345678901234567890.5]`,
			`[9007199254740993,1.10,1.0,1E5,1e+5,-0,0.000,123456789012345678901234567890.5]`},
		{"no HTML escaping", `{"t":"<a href=\"x\">&</a>"}`, `{"t":"<a href=\"x\">&</a>"}`},
		{"unicode written literally",
			`{"s":"\u00e9\u4e2d\ud83d\ude00 é中😀"}`,
			`{"s":"é中😀 é中😀"}`},
		{"line separators and DEL not escaped", "[\"\u2028\u2029\\u2028\\u007f\"]", "[\"\u2028\u2029\u2028\u007f\"]"},
		{"control characters use the short or \\u00xx form",
			`["\u0008\u000c\n\r\t\u0001\u001F\/\\\""]`,
			`["\b\f\n\r\t\u0001\u001f/\\\""]`},
		{"keys ordered by code point", `{"é":1,"z":2,"Z":3,"":4,"a b":5,"a":6}`, `{"":4,"Z":3,"a":6,"a b":5,"z":2,"é":1}`},
		{"escaped key unescaped", `{"\u0061":1}`, `{"a":1}`},
		{"scalar document", ` "x" `, `"x"`},
		{"replacement character kept", `["\ufffd�"]`, `["��"]`},
	}
	for _, tc := range cases {
		got, digest, err := Canonicalize([]byte(tc.in))
		if err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
		sum := sha256.Sum256(got)
		if want := "sha256:" + hex.EncodeToString(sum[:]); digest != want {
			t.Errorf("%s: digest %s, want %s", tc.name, digest, want)
		}
		again, digest2, err := Canonicalize(got)
		if err != nil || string(again) != string(got) || digest2 != digest {
			t.Errorf("%s: canonical form is not a fixed point: %s (%v)", tc.name, again, err)
		}
	}
}

func TestCanonicalizeEquivalentDocuments(t *testing.T) {
	groups := [][]string{
		{
			`{"name":"a/b","nested":{"x":[1,{"k":"v","j":null}],"y":"é"},"n":9007199254740993}`,
			`{"n":9007199254740993,"nested":{"y":"\u00e9","x":[1,{"j":null,"k":"v"}]},"name":"a/b"}`,
			"{\n  \"nested\": {\n    \"y\": \"é\",\n    \"x\": [ 1, { \"k\": \"v\", \"j\": null } ]\n  },\n  \"n\": 9007199254740993,\n  \"name\": \"a\\/b\"\n}\n",
		},
		{`[[1,2],[3,[4,{"b":[],"a":{}}]]]`, " [ [ 1 , 2 ] , [ 3 , [ 4 , { \"a\" : { } , \"b\" : [ ] } ] ] ] "},
	}
	for i, group := range groups {
		first, firstDigest, err := Canonicalize([]byte(group[0]))
		if err != nil {
			t.Fatalf("group %d: %v", i, err)
		}
		for _, other := range group[1:] {
			got, digest, err := Canonicalize([]byte(other))
			if err != nil {
				t.Errorf("group %d: %v", i, err)
				continue
			}
			if string(got) != string(first) || digest != firstDigest {
				t.Errorf("group %d: equivalent documents differ:\n%s\n%s", i, first, got)
			}
		}
	}

	// Documents that differ in content must not collapse.
	distinct := []string{`{"a":1}`, `{"a":1.0}`, `{"a":"1"}`, `{"a":[1]}`, `{"A":1}`, `[1,2]`, `[2,1]`}
	seen := map[string]string{}
	for _, d := range distinct {
		_, digest, err := Canonicalize([]byte(d))
		if err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		if prev, dup := seen[digest]; dup {
			t.Errorf("%s and %s share a digest", prev, d)
		}
		seen[digest] = d
	}
}

func nest(open, close string, n int, inner string) string {
	return strings.Repeat(open, n) + inner + strings.Repeat(close, n)
}

func TestCanonicalizeRejects(t *testing.T) {
	bad := map[string]string{
		"empty":                        ``,
		"whitespace only":              ` `,
		"trailing value":               `{} {}`,
		"trailing garbage":             `{"a":1}x`,
		"trailing comma":               `{"a":1,}`,
		"missing comma":                `[1 2]`,
		"missing colon":                `{"a" 1}`,
		"single quotes":                `{'a':1}`,
		"comment":                      `{"a":1} // c`,
		"unterminated":                 `{"a":[1,2`,
		"leading zero":                 `[01]`,
		"bare word":                    `nul`,
		"byte order mark":              "\uFEFF{}",
		"invalid UTF-8 in string":      "{\"a\":\"\xff\"}",
		"invalid UTF-8 outside string": "{}\xff",
		"raw control character":        "[\"a\nb\"]",
		"duplicate key":                `{"a":1,"a":2}`,
		"duplicate key, same value":    `{"a":1,"a":1}`,
		"duplicate escaped key":        `{"a":1,"\u0061":2}`,
		"duplicate nested key":         `{"x":[0,{"y":{"k":1,"z":[],"k":2}}]}`,
		"duplicate empty key":          `{"":1,"":2}`,
		"escaped NUL":                  `["a\u0000b"]`,
		"escaped NUL in key":           `{"\u0000":1}`,
		"lone high surrogate":          `["\ud83d"]`,
		"lone low surrogate":           `["\ude00"]`,
		"reversed surrogates":          `["\ude00\ud83d"]`,
		"high surrogate then text":     `["\ud83dx"]`,
		"high surrogate then BMP":      `["\ud83d\u0041"]`,
		"lone surrogate in key":        `{"\udc00":1}`,
		"33 arrays":                    nest("[", "]", 33, ""),
		"33 objects":                   nest(`{"a":`, "}", 33, "1"),
		"object plus 32 arrays":        `{"a":` + nest("[", "]", 32, "") + `}`,
	}
	for name, in := range bad {
		if _, _, err := Canonicalize([]byte(in)); codeOf(err) != codeInvalidJSON {
			t.Errorf("%s: code %q, want %q", name, codeOf(err), codeInvalidJSON)
		}
	}

	good := map[string]string{
		"32 arrays":                   nest("[", "]", 32, ""),
		"32 arrays around a scalar":   nest("[", "]", 32, "1"),
		"32 objects":                  nest(`{"a":`, "}", 32, "1"),
		"object plus 31 arrays":       `{"a":` + nest("[", "]", 31, "") + `}`,
		"same key in sibling objects": `[{"a":1},{"a":2}]`,
		"same key at two depths":      `{"a":{"a":{"a":1}}}`,
		"surrogate pair":              `["\uD83D\uDE00"]`,
		"backslash before u":          `["\\ud83d"]`,
		"escaped quote then u":        `["\"\\u0000"]`,
		"u outside a string is data":  `{"u":"\\"}`,
	}
	for name, in := range good {
		if _, _, err := Canonicalize([]byte(in)); err != nil {
			t.Errorf("%s: unexpected error: %v", name, err)
		}
	}
}

func TestErrorMessageQuotesKeysSafely(t *testing.T) {
	long := strings.Repeat("é", 100)
	_, _, err := Canonicalize([]byte(`{"` + long + `":1,"` + long + `":2}`))
	var e *Error
	if !errors.As(err, &e) || e.Code != codeInvalidJSON {
		t.Fatalf("got %v", err)
	}
	if len(e.Msg) > 120 || !strings.HasSuffix(e.Msg, `..."`) {
		t.Errorf("message not truncated on a rune boundary: %q", e.Msg)
	}
	if e.Error() != e.Msg {
		t.Errorf("Error() = %q, want Msg", e.Error())
	}
}

func TestNumbersMustBeStorable(t *testing.T) {
	for lit, ok := range map[string]bool{
		"0": true, "-12.5": true, "9007199254740993": true, "1e5": true, "1E+1000": true, "2.5e-1000": true,
		"1e1001": false, "1E400000": false, "1e-1001": false, "1" + strings.Repeat("0", 64): false,
	} {
		_, _, err := Canonicalize([]byte(`{"n":` + lit + `}`))
		if (err == nil) != ok {
			t.Errorf("%s: err = %v, want ok = %v", lit, err, ok)
		}
	}
}
