package mcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxDepth bounds container nesting. The top-level value counts as depth 1,
// so a document may open at most 32 objects/arrays inside one another. The
// official schema needs about 8 levels; the bound keeps every walk in this
// package (decode, redact, encode) shallowly recursive.
const maxDepth = 32

// Canonicalize returns the canonical encoding (object keys sorted, no
// insignificant whitespace, numbers preserved exactly as written via
// json.Number, valid UTF-8, no HTML escaping) and its sha256 digest. Two
// documents that differ only in key order or whitespace canonicalize to the
// same bytes.
//
// The input may be any single JSON value. It is rejected with code
// "invalid_json" when it is not valid UTF-8, is not exactly one JSON value,
// repeats a key inside any object, nests deeper than 32 levels, or contains a
// string that is not Unicode text (an escaped NUL or an unpaired surrogate
// escape).
//
// Details that are fixed because digests are persisted:
//   - Keys are ordered by their UTF-8 bytes (equivalently, by code point).
//   - Strings are written as literal UTF-8; only '"', '\\' and U+0000-U+001F
//     are escaped (\b \f \n \r \t, otherwise \u00xx in lowercase hex). The
//     encoder is written out here instead of delegating to encoding/json so
//     the bytes cannot drift with the Go release.
//   - Numbers are copied verbatim, so 1.10, 1.1 and 11e-1 are three different
//     documents.
func Canonicalize(data []byte) (canonical []byte, digest string, err error) {
	v, err := decode(data)
	if err != nil {
		return nil, "", err
	}
	canonical = appendValue(make([]byte, 0, len(data)), v)
	return canonical, digestOf(canonical), nil
}

func digestOf(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// decode parses exactly one JSON value into map[string]any, []any, string,
// json.Number, bool and nil.
//
// encoding/json alone is too forgiving for a document whose digest is stored:
// it keeps the last of two duplicate keys (other parsers keep the first, so
// two readers could disagree about what was published) and silently turns
// unpaired surrogate escapes into U+FFFD. Both are rejected here.
func decode(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, errf(codeInvalidJSON, "document must be valid UTF-8")
	}
	// json.Valid settles the grammar (one value, nothing after it); the token
	// walk below then only has to enforce what the grammar does not.
	if !json.Valid(data) {
		return nil, errf(codeInvalidJSON, "document must be a single valid JSON value")
	}
	if err := checkEscapes(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return readValue(dec, 1)
}

func readValue(dec *json.Decoder, depth int) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, errf(codeInvalidJSON, "document must be a single valid JSON value")
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		if n, isNumber := tok.(json.Number); isNumber && !storableNumber(string(n)) {
			return nil, errf(codeInvalidJSON, "number is too long or too large")
		}
		return tok, nil // string, json.Number, bool or nil
	}
	if depth > maxDepth {
		return nil, errf(codeInvalidJSON, "document is nested deeper than %d levels", maxDepth)
	}
	if delim == '[' {
		arr := []any{}
		for dec.More() {
			v, err := readValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		if _, err := dec.Token(); err != nil { // closing ']'
			return nil, errf(codeInvalidJSON, "document must be a single valid JSON value")
		}
		return arr, nil
	}
	obj := map[string]any{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, errf(codeInvalidJSON, "document must be a single valid JSON value")
		}
		key, ok := kt.(string)
		if !ok {
			return nil, errf(codeInvalidJSON, "document must be a single valid JSON value")
		}
		// Keys are compared after unescaping, so "a" and "a" collide.
		if _, dup := obj[key]; dup {
			return nil, errf(codeInvalidJSON, "duplicate object key %s", quoteShort(key))
		}
		v, err := readValue(dec, depth+1)
		if err != nil {
			return nil, err
		}
		obj[key] = v
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return nil, errf(codeInvalidJSON, "document must be a single valid JSON value")
	}
	return obj, nil
}

// storableNumber bounds number literals to what PostgreSQL jsonb stores
// (numeric holds at most 131072 digits before the decimal point). Without
// the bound one document with "1e400000" could not be inserted, which a
// mirror would see as a storage failure rather than a bad document.
func storableNumber(lit string) bool {
	if len(lit) > 64 {
		return false
	}
	i := strings.IndexAny(lit, "eE")
	if i < 0 {
		return true
	}
	exp, err := strconv.Atoi(lit[i+1:])
	return err == nil && exp >= -1000 && exp <= 1000
}

// checkEscapes rejects \u escapes that do not denote Unicode text: \u0000
// (PostgreSQL jsonb cannot store it, and the official registry therefore
// never serves it) and surrogate escapes that are not a high/low pair
// (encoding/json would replace them with U+FFFD, changing the document).
// data has already passed json.Valid, so every backslash inside a string
// starts a well-formed escape; the bounds checks are defensive only.
func checkEscapes(data []byte) error {
	inString := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if !inString {
			inString = c == '"'
			continue
		}
		switch c {
		case '"':
			inString = false
		case '\\':
			i++
			if i >= len(data) || data[i] != 'u' {
				continue // two-character escape such as \" or \\
			}
			r, ok := hex4(data, i+1)
			if !ok {
				return errf(codeInvalidJSON, "document must be a single valid JSON value")
			}
			i += 4
			switch {
			case r == 0:
				return errf(codeInvalidJSON, "strings must not contain the NUL character")
			case r >= 0xD800 && r <= 0xDBFF:
				lo, ok := uint16(0), false
				if i+2 < len(data) && data[i+1] == '\\' && data[i+2] == 'u' {
					lo, ok = hex4(data, i+3)
				}
				if !ok || lo < 0xDC00 || lo > 0xDFFF {
					return errf(codeInvalidJSON, "strings must not contain unpaired surrogate escapes")
				}
				i += 6
			case r >= 0xDC00 && r <= 0xDFFF:
				return errf(codeInvalidJSON, "strings must not contain unpaired surrogate escapes")
			}
		}
	}
	return nil
}

// hex4 decodes the four hex digits at data[at:at+4].
func hex4(data []byte, at int) (uint16, bool) {
	if at < 0 || at+4 > len(data) {
		return 0, false
	}
	var r uint16
	for _, c := range data[at : at+4] {
		switch {
		case c >= '0' && c <= '9':
			c -= '0'
		case c >= 'a' && c <= 'f':
			c = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			c = c - 'A' + 10
		default:
			return 0, false
		}
		r = r<<4 | uint16(c)
	}
	return r, true
}

// appendValue writes the canonical encoding of a value produced by decode.
func appendValue(b []byte, v any) []byte {
	switch t := v.(type) {
	case map[string]any:
		b = append(b, '{')
		for i, k := range slices.Sorted(maps.Keys(t)) {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendString(b, k)
			b = append(b, ':')
			b = appendValue(b, t[k])
		}
		return append(b, '}')
	case []any:
		b = append(b, '[')
		for i, e := range t {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendValue(b, e)
		}
		return append(b, ']')
	case string:
		return appendString(b, t)
	case json.Number:
		return append(b, t...)
	case bool:
		if t {
			return append(b, "true"...)
		}
		return append(b, "false"...)
	default: // nil
		return append(b, "null"...)
	}
}

const hexDigits = "0123456789abcdef"

// appendString writes s with the minimal escaping JSON requires. s is valid
// UTF-8 (decode guarantees it), so the remaining bytes are copied as they are;
// '<', '>', '&', U+2028 and U+2029 are deliberately not escaped.
func appendString(b []byte, s string) []byte {
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		b = append(b, s[start:i]...)
		start = i + 1
		switch c {
		case '"', '\\':
			b = append(b, '\\', c)
		case '\b':
			b = append(b, '\\', 'b')
		case '\f':
			b = append(b, '\\', 'f')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
		}
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}

// quoteShort quotes a document-supplied string for an error message, cut to
// a length that keeps messages readable.
func quoteShort(s string) string {
	const limit = 64
	if len(s) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "..."
	}
	return string(appendString(nil, s))
}
