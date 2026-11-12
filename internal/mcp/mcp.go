// Package mcp holds pure validation and canonicalization rules for MCP
// server.json documents (the official MCP Registry "ServerDetail"). It
// performs no I/O: it never fetches a schema, resolves a host or contacts a
// remote.
//
// Two rule sets exist. Entries published to this registry are validated
// strictly against the official schema (2025-12-11). Entries mirrored from an
// upstream registry are validated leniently: only the rules the registry
// itself depends on apply, so upstream schema drift never drops an entry.
// In both modes the registry refuses to keep secret values.
package mcp

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Metadata keys.
const (
	MetaOfficial  = "io.modelcontextprotocol.registry/official"
	MetaPublisher = "io.modelcontextprotocol.registry/publisher-provided"
	MetaRegistry  = "ai.openlinker.registry/entry"
	SchemaURL     = "https://static.modelcontextprotocol.io/schemas/2025-12-11/server.schema.json"
)

// Default document size limits. Published documents follow the official
// registry's order of magnitude; mirrored ones get headroom because rejecting
// an upstream entry for size alone would silently hide it.
const (
	defaultStrictBytes  = 64 << 10
	defaultLenientBytes = 256 << 10
)

// Stable error codes.
const (
	codeTooLarge           = "too_large"
	codeInvalidJSON        = "invalid_json"
	codeNameInvalid        = "name_invalid"
	codeNamespaceInvalid   = "namespace_invalid"
	codeVersionInvalid     = "version_invalid"
	codeSecretValue        = "secret_value"
	codeDescriptionInvalid = "description_invalid"
	codeTitleInvalid       = "title_invalid"
	codeSchemaInvalid      = "schema_invalid"
	codeRepositoryInvalid  = "repository_invalid"
	codeWebsiteInvalid     = "website_invalid"
	codeIconInvalid        = "icon_invalid"
	codePackageInvalid     = "package_invalid"
	codeRemoteInvalid      = "remote_invalid"
	codeNoTransport        = "no_transport"
	codeMetaInvalid        = "meta_invalid"
)

// Error is a validation failure with a stable code (e.g. "name_invalid").
// Msg is client-facing; it may name a field path but never repeats a value
// from the document other than an object key.
type Error struct{ Code, Msg string }

func (e *Error) Error() string { return e.Msg }

func errf(code, format string, args ...any) error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Remote is one remote transport of a server.
type Remote struct{ Type, URL string } // Type: "streamable-http" | "sse"

// Parsed is a validated server.json.
type Parsed struct {
	Name        string // full name exactly as given, e.g. "io.github.Alice/weather"
	Namespace   string // lowercase namespace part, e.g. "io.github.alice"
	Server      string // part after the slash, as given
	Title       string
	Description string
	Version     string
	// Remotes lists, in document order, the remotes with a valid type and
	// url. Under Strict every remote is valid or Parse fails. Otherwise an
	// entry is listed when its type is known and its url is well formed (the
	// official pattern and, unless it starts with a {variable}, an absolute
	// URL with a host); the publishing policy (https only, no userinfo, no
	// fragment, no duplicates) is not applied, so callers must not treat a
	// mirrored URL as safe to dial.
	Remotes     []Remote
	HasPackages bool   // packages is a non-empty array
	Canonical   []byte // canonical JSON of the (possibly redacted) document
	Digest      string // "sha256:<64 hex>" of Canonical
}

// Options selects the rule set.
type Options struct {
	// Strict applies the publishing rules. When false (mirroring), only the
	// minimal rules apply and secret literals are redacted instead of
	// rejected.
	Strict   bool
	MaxBytes int // 0 = default (64 KiB strict, 256 KiB lenient)
	// AllowHTTPRemotes permits http:// remote URLs (private instances).
	AllowHTTPRemotes bool
}

// Parse validates a server.json document and returns its canonical form.
//
// Rules in both modes: the input is one JSON object within MaxBytes, valid
// UTF-8, without duplicate keys and at most 32 levels deep; name and version
// are well formed; the registry-managed _meta keys (MetaOfficial,
// MetaRegistry) are removed, because only the registry may state them; and no
// secret input keeps a literal value (rejected under Strict, removed
// otherwise).
//
// Canonical never exceeds the input in length and parsing it again with the
// same options returns the same Canonical and Digest.
func Parse(data []byte, o Options) (*Parsed, error) {
	limit := o.MaxBytes
	if limit <= 0 {
		limit = defaultLenientBytes
		if o.Strict {
			limit = defaultStrictBytes
		}
	}
	if len(data) > limit {
		return nil, errf(codeTooLarge, "server.json exceeds %d bytes", limit)
	}
	v, err := decode(data)
	if err != nil {
		return nil, err
	}
	root, ok := v.(map[string]any)
	if !ok {
		return nil, errf(codeInvalidJSON, "server.json must be a JSON object")
	}

	name, _ := root["name"].(string)
	namespace, server, code := splitName(name)
	switch code {
	case codeNameInvalid:
		return nil, errf(code, "name must be <namespace>/<server>: 3-200 characters, letters, digits, dots and hyphens before the single slash, letters, digits, dots, underscores and hyphens after it")
	case codeNamespaceInvalid:
		return nil, errf(code, "the namespace before the slash must be at least two reverse-DNS labels of at most 128 characters in total, e.g. io.github.alice")
	}
	version, _ := root["version"].(string)
	if n := utf8.RuneCountInString(version); n < 1 || n > 255 || version == "latest" {
		return nil, errf(codeVersionInvalid, "version must be a string of 1-255 characters other than \"latest\"")
	}

	stripRegistryMeta(root)
	if path := scrubSecrets(root, !o.Strict); path != "" {
		return nil, errf(codeSecretValue, "%s: the registry does not store secret values; use a {variable} placeholder", shortPath(path))
	}
	if o.Strict {
		if err := checkStrict(root, o); err != nil {
			return nil, err
		}
	}

	p := &Parsed{Name: name, Namespace: namespace, Server: server, Version: version}
	p.Title, _ = root["title"].(string)
	p.Description, _ = root["description"].(string)
	if remotes, ok := root["remotes"].([]any); ok {
		for _, item := range remotes {
			if r, problem := remoteOf(item, o); problem == "" {
				p.Remotes = append(p.Remotes, r)
			}
		}
	}
	packages, _ := root["packages"].([]any)
	p.HasPackages = len(packages) > 0
	p.Canonical = appendValue(make([]byte, 0, len(data)), root)
	p.Digest = digestOf(p.Canonical)
	return p, nil
}

var (
	// nameRe is the official server name pattern: exactly one slash.
	nameRe = regexp.MustCompile(`^[a-zA-Z0-9.-]+/[a-zA-Z0-9._-]+$`)
	// namespaceRe is the registry namespace syntax, the same expression the
	// skill package applies to skill namespaces: an MCP entry and a skill of
	// one owner live under the same namespace row.
	nsLabel     = `[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?`
	namespaceRe = regexp.MustCompile(`^` + nsLabel + `(\.` + nsLabel + `)+$`)
	// placeholderRe finds a {variable} reference inside a value.
	placeholderRe = regexp.MustCompile(`\{[a-zA-Z_][a-zA-Z0-9_]*\}`)
)

// SplitName splits "<namespace>/<server>" and returns the lowercase
// namespace; ok is false unless name matches the official pattern (3-200
// characters, exactly one slash) and its namespace is a valid registry
// namespace: at least two dot-separated labels, at most 128 characters.
//
// A server part of "." or ".." is refused although the official pattern
// allows it: names are used as URL path segments, where dot segments are
// rewritten by clients and proxies.
func SplitName(name string) (namespace, server string, ok bool) {
	namespace, server, code := splitName(name)
	return namespace, server, code == ""
}

// splitName returns "" as code on success, otherwise the error code.
func splitName(name string) (namespace, server, code string) {
	if len(name) < 3 || len(name) > 200 || !nameRe.MatchString(name) {
		return "", "", codeNameInvalid
	}
	namespace, server, _ = strings.Cut(name, "/")
	if server == "." || server == ".." {
		return "", "", codeNameInvalid
	}
	namespace = strings.ToLower(namespace)
	if len(namespace) > 128 || !namespaceRe.MatchString(namespace) {
		return "", "", codeNamespaceInvalid
	}
	return namespace, server, ""
}

// stripRegistryMeta removes the _meta keys only the registry may state. A
// document copied from a registry response carries them; keeping them would
// let a publisher (or an upstream) forge status, timestamps or provenance.
// _meta is dropped when the removal leaves it empty, and left alone when it
// held neither key.
func stripRegistryMeta(root map[string]any) {
	meta, ok := root["_meta"].(map[string]any)
	if !ok {
		return
	}
	before := len(meta)
	delete(meta, MetaOfficial)
	delete(meta, MetaRegistry)
	if len(meta) == 0 && before > 0 {
		delete(root, "_meta")
	}
}

// scrubSecrets enforces that no secret input carries a literal. An input is
// any object with "isSecret": true, wherever it sits: that covers the
// official positions (headers, environmentVariables, runtimeArguments,
// packageArguments and the values of every variables object, recursively)
// and any position a later schema adds. On such an object "value" and
// "default" may only be absent, null, empty or a string that refers to a
// {variable}; anything else is a literal.
//
// With redact set, literals are deleted in place (a literal value takes the
// default with it) and "" is returned. Otherwise nothing is modified and the
// path of the first literal, in key order, is returned.
func scrubSecrets(v any, redact bool) (path string) {
	switch t := v.(type) {
	case map[string]any:
		if secret, _ := t["isSecret"].(bool); secret {
			valueLiteral, defaultLiteral := isLiteral(t["value"]), isLiteral(t["default"])
			switch {
			case redact && valueLiteral:
				delete(t, "value")
				delete(t, "default")
			case redact && defaultLiteral:
				delete(t, "default")
			case valueLiteral:
				return "value"
			case defaultLiteral:
				return "default"
			}
		}
		for _, k := range slices.Sorted(maps.Keys(t)) {
			if sub := scrubSecrets(t[k], redact); sub != "" {
				if strings.HasPrefix(sub, "[") {
					return k + sub
				}
				return k + "." + sub
			}
		}
	case []any:
		for i, e := range t {
			if sub := scrubSecrets(e, redact); sub != "" {
				if strings.HasPrefix(sub, "[") {
					return fmt.Sprintf("[%d]%s", i, sub)
				}
				return fmt.Sprintf("[%d].%s", i, sub)
			}
		}
	}
	return ""
}

// isLiteral reports whether a secret input's value or default holds data
// rather than a reference: a non-empty string without a {variable}, or any
// non-string, non-null JSON value.
func isLiteral(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != "" && !placeholderRe.MatchString(t)
	default:
		return true
	}
}

// shortPath bounds a field path built from document keys.
func shortPath(p string) string {
	const limit = 200
	if len(p) <= limit {
		return p
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(p[cut]) {
		cut--
	}
	return p[:cut] + "..."
}
