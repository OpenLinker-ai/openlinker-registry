package mcp

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Bounds from the official schema (2025-12-11) and OpenAPI, plus list caps
// that keep one entry from dominating a listing.
const (
	maxTextRunes      = 100 // description and title
	maxIcons          = 16
	maxIconSrcRunes   = 255
	maxPackages       = 32
	maxRemotes        = 32
	maxPublisherBytes = 4096 // compact JSON of the publisher-provided _meta
)

var (
	// transportURLRe is the official pattern for remote and package transport
	// URLs: an http(s) URL, or a value that starts with a {variable}.
	transportURLRe = regexp.MustCompile(`^(https?://[^\s]+|\{[a-zA-Z_][a-zA-Z0-9_]*\}[^\s]*)$`)
	templateHeadRe = regexp.MustCompile(`^\{[a-zA-Z_][a-zA-Z0-9_]*\}`)
	sha256Re       = regexp.MustCompile(`^[a-f0-9]{64}$`)
	iconSizeRe     = regexp.MustCompile(`^(\d+x\d+|any)$`)

	iconMimeTypes = map[string]bool{
		"image/png": true, "image/jpeg": true, "image/jpg": true,
		"image/svg+xml": true, "image/webp": true,
	}
	iconThemes        = map[string]bool{"light": true, "dark": true}
	remoteTypes       = map[string]bool{"streamable-http": true, "sse": true}
	packageTransports = map[string]bool{"stdio": true, "streamable-http": true, "sse": true}
)

// checkStrict applies the publishing rules. Fields it does not name are
// accepted untouched, at the top level and below, so a document written for a
// newer schema still publishes.
//
// An optional field that is present must have its declared type: JSON null
// is not "absent".
func checkStrict(root map[string]any, o Options) error {
	if d, ok := root["description"].(string); !ok || !runesWithin(d, 1, maxTextRunes) {
		return errf(codeDescriptionInvalid, "description is required: a string of 1-%d characters", maxTextRunes)
	}
	if v, present := root["title"]; present {
		if t, ok := v.(string); !ok || !runesWithin(t, 1, maxTextRunes) {
			return errf(codeTitleInvalid, "title must be a string of 1-%d characters", maxTextRunes)
		}
	}
	if version, _ := root["version"].(string); looksLikeRange(version) {
		return errf(codeVersionInvalid, "version must be one specific version, not a range or wildcard")
	}
	if v, present := root["$schema"]; present {
		if _, ok := v.(string); !ok {
			return errf(codeSchemaInvalid, "$schema must be a string")
		}
	}
	if v, present := root["repository"]; present {
		if problem := checkRepository(v); problem != "" {
			return errf(codeRepositoryInvalid, "repository: %s", problem)
		}
	}
	if v, present := root["websiteUrl"]; present {
		if s, ok := v.(string); !ok || !absoluteURL(s, "https") {
			return errf(codeWebsiteInvalid, "websiteUrl must be an absolute https URL")
		}
	}
	if v, present := root["icons"]; present {
		icons, ok := v.([]any)
		if !ok || len(icons) > maxIcons {
			return errf(codeIconInvalid, "icons must be an array of at most %d items", maxIcons)
		}
		for i, icon := range icons {
			if problem := checkIcon(icon); problem != "" {
				return errf(codeIconInvalid, "icons[%d]: %s", i, problem)
			}
		}
	}

	transports := 0
	if v, present := root["packages"]; present {
		packages, ok := v.([]any)
		if !ok || len(packages) > maxPackages {
			return errf(codePackageInvalid, "packages must be an array of at most %d items", maxPackages)
		}
		for i, pkg := range packages {
			if problem := checkPackage(pkg); problem != "" {
				return errf(codePackageInvalid, "packages[%d]: %s", i, problem)
			}
		}
		transports += len(packages)
	}
	if v, present := root["remotes"]; present {
		remotes, ok := v.([]any)
		if !ok || len(remotes) > maxRemotes {
			return errf(codeRemoteInvalid, "remotes must be an array of at most %d items", maxRemotes)
		}
		seen := map[string]bool{}
		for i, item := range remotes {
			r, problem := remoteOf(item, o)
			if problem != "" {
				return errf(codeRemoteInvalid, "remotes[%d]: %s", i, problem)
			}
			// Exact comparison: two spellings of one endpoint are the
			// publisher's business, a repeated entry is a mistake.
			if seen[r.URL] {
				return errf(codeRemoteInvalid, "remotes[%d]: url repeats an earlier remote", i)
			}
			seen[r.URL] = true
		}
		transports += len(remotes)
	}
	if transports == 0 {
		return errf(codeNoTransport, "at least one of packages or remotes must be non-empty")
	}

	if v, present := root["_meta"]; present {
		if problem := checkMeta(v); problem != "" {
			return errf(codeMetaInvalid, "_meta: %s", problem)
		}
	}
	return nil
}

// looksLikeRange reports whether a version reads as a range or wildcard
// instead of one version: a leading comparison operator, "||", a hyphen range
// (" - "), "*", or a dot-separated component that is exactly x or X.
// Build metadata and pre-release tags ("1.0.0+build.1", "2025.11.12-beta")
// are not ranges.
func looksLikeRange(v string) bool {
	if v == "" {
		return false
	}
	if strings.ContainsRune("^~><=", rune(v[0])) ||
		strings.Contains(v, "||") || strings.Contains(v, " - ") || strings.Contains(v, "*") {
		return true
	}
	for part := range strings.SplitSeq(v, ".") {
		if part == "x" || part == "X" {
			return true
		}
	}
	return false
}

func checkRepository(v any) string {
	repo, ok := v.(map[string]any)
	if !ok {
		return "must be an object"
	}
	if u, ok := repo["url"].(string); !ok || !absoluteURL(u, "http", "https") {
		return "url is required: an absolute http or https URL"
	}
	if s, ok := repo["source"].(string); !ok || s == "" {
		return "source is required: a non-empty string"
	}
	if v, present := repo["id"]; present {
		if _, ok := v.(string); !ok {
			return "id must be a string"
		}
	}
	if v, present := repo["subfolder"]; present {
		// The subfolder is later joined onto a repository checkout or URL, so
		// it must stay inside the repository.
		s, ok := v.(string)
		if !ok || strings.HasPrefix(s, "/") || strings.Contains(s, "\\") {
			return "subfolder must be a relative path without backslashes"
		}
		for seg := range strings.SplitSeq(s, "/") {
			if seg == ".." {
				return "subfolder must not contain a .. segment"
			}
		}
	}
	return ""
}

func checkIcon(v any) string {
	icon, ok := v.(map[string]any)
	if !ok {
		return "must be an object"
	}
	if src, ok := icon["src"].(string); !ok || !runesWithin(src, 1, maxIconSrcRunes) || !absoluteURL(src, "https") {
		return fmt.Sprintf("src is required: an absolute https URL of at most %d characters", maxIconSrcRunes)
	}
	if v, present := icon["mimeType"]; present {
		if s, ok := v.(string); !ok || !iconMimeTypes[s] {
			return "mimeType must be image/png, image/jpeg, image/jpg, image/svg+xml or image/webp"
		}
	}
	if v, present := icon["sizes"]; present {
		sizes, ok := v.([]any)
		if !ok {
			return "sizes must be an array"
		}
		for _, size := range sizes {
			if s, ok := size.(string); !ok || !iconSizeRe.MatchString(s) {
				return "sizes items must be <width>x<height> or any"
			}
		}
	}
	if v, present := icon["theme"]; present {
		if s, ok := v.(string); !ok || !iconThemes[s] {
			return "theme must be light or dark"
		}
	}
	return ""
}

func checkPackage(v any) string {
	pkg, ok := v.(map[string]any)
	if !ok {
		return "must be an object"
	}
	for _, key := range []string{"registryType", "identifier"} {
		if s, ok := pkg[key].(string); !ok || s == "" {
			return key + " is required: a non-empty string"
		}
	}
	if v, present := pkg["version"]; present {
		if s, ok := v.(string); !ok || !runesWithin(s, 1, 255) || s == "latest" {
			return "version must be a string of 1-255 characters other than \"latest\""
		}
	}
	if v, present := pkg["fileSha256"]; present {
		if s, ok := v.(string); !ok || !sha256Re.MatchString(s) {
			return "fileSha256 must be 64 lowercase hex characters"
		}
	}
	if v, present := pkg["registryBaseUrl"]; present {
		if s, ok := v.(string); !ok || !absoluteURL(s, "http", "https") {
			return "registryBaseUrl must be an absolute http or https URL"
		}
	}
	if v, present := pkg["runtimeHint"]; present {
		if _, ok := v.(string); !ok {
			return "runtimeHint must be a string"
		}
	}
	transport, ok := pkg["transport"].(map[string]any)
	if !ok {
		return "transport is required: an object"
	}
	kind, _ := transport["type"].(string)
	if !packageTransports[kind] {
		return "transport.type must be stdio, streamable-http or sse"
	}
	if kind != "stdio" {
		// A package transport URL describes a server the user starts
		// locally, so only the official pattern applies (http://localhost
		// is the normal case); the https policy is for remotes.
		if u, ok := transport["url"].(string); !ok || !transportURL(u) {
			return "transport.url is required: an http(s) URL or a value starting with a {variable}"
		}
	}
	if problem := checkNamedInputs(transport, "headers"); problem != "" {
		return "transport." + problem
	}
	if problem := checkNamedInputs(pkg, "environmentVariables"); problem != "" {
		return problem
	}
	for _, key := range []string{"runtimeArguments", "packageArguments"} {
		if problem := checkArguments(pkg, key); problem != "" {
			return problem
		}
	}
	return ""
}

// checkNamedInputs validates an optional array of key/value inputs (headers,
// environment variables): each item is an object with a non-empty name.
func checkNamedInputs(parent map[string]any, key string) string {
	v, present := parent[key]
	if !present {
		return ""
	}
	items, ok := v.([]any)
	if !ok {
		return key + " must be an array"
	}
	for i, item := range items {
		input, ok := item.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s[%d] must be an object", key, i)
		}
		if name, ok := input["name"].(string); !ok || name == "" {
			return fmt.Sprintf("%s[%d].name is required: a non-empty string", key, i)
		}
	}
	return ""
}

// checkArguments validates an optional array of command-line arguments. A
// named argument needs its flag name; a positional one needs a value or a
// hint, otherwise a client has nothing to pass or to ask for.
func checkArguments(parent map[string]any, key string) string {
	v, present := parent[key]
	if !present {
		return ""
	}
	items, ok := v.([]any)
	if !ok {
		return key + " must be an array"
	}
	for i, item := range items {
		arg, ok := item.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s[%d] must be an object", key, i)
		}
		switch kind, _ := arg["type"].(string); kind {
		case "named":
			if name, ok := arg["name"].(string); !ok || name == "" {
				return fmt.Sprintf("%s[%d].name is required for a named argument", key, i)
			}
		case "positional":
			_, hasHint := arg["valueHint"].(string)
			_, hasValue := arg["value"].(string)
			if !hasHint && !hasValue {
				return fmt.Sprintf("%s[%d] needs a string valueHint or value for a positional argument", key, i)
			}
		default:
			return fmt.Sprintf("%s[%d].type must be positional or named", key, i)
		}
	}
	return ""
}

// remoteOf reads one remotes[] item. problem is "" when the remote is valid
// under o: with Strict that is the full publishing policy, otherwise only
// what makes the entry a recognisable remote (see Parsed.Remotes).
func remoteOf(v any, o Options) (r Remote, problem string) {
	remote, ok := v.(map[string]any)
	if !ok {
		return r, "must be an object"
	}
	r.Type, _ = remote["type"].(string)
	if !remoteTypes[r.Type] {
		return r, "type must be streamable-http or sse"
	}
	r.URL, ok = remote["url"].(string)
	if !ok || !transportURL(r.URL) {
		return r, "url is required: an http(s) URL or a value starting with a {variable}"
	}
	if !templateHeadRe.MatchString(r.URL) {
		// {variable} references may sit anywhere in the URL (a tenant label,
		// a port, a path segment); net/url refuses braces in a host, so each
		// reference is replaced by a digit, which is valid in all of those
		// positions, before the URL is examined.
		u, err := url.Parse(placeholderRe.ReplaceAllString(r.URL, "0"))
		if err != nil || u.Hostname() == "" {
			return r, "url must be an absolute URL with a host"
		}
		if o.Strict {
			switch {
			case u.User != nil:
				// Credentials in a URL are a secret the registry would store.
				return r, "url must not contain user information"
			case strings.Contains(r.URL, "#"):
				return r, "url must not contain a fragment"
			case u.Scheme != "https" && !o.AllowHTTPRemotes:
				return r, "url must use https"
			}
		}
	}
	if o.Strict {
		if problem := checkNamedInputs(remote, "headers"); problem != "" {
			return r, problem
		}
	}
	return r, ""
}

func checkMeta(v any) string {
	meta, ok := v.(map[string]any)
	if !ok {
		return "must be an object"
	}
	// The registry-managed keys were already removed; anything else is
	// neither the publisher's slot nor ours.
	for key := range meta {
		if key != MetaPublisher {
			return "only the " + MetaPublisher + " key is accepted"
		}
	}
	if value, present := meta[MetaPublisher]; present {
		if _, ok := value.(map[string]any); !ok {
			return MetaPublisher + " must be an object"
		}
		if len(appendValue(nil, value)) > maxPublisherBytes {
			return fmt.Sprintf("%s must not exceed %d bytes of compact JSON", MetaPublisher, maxPublisherBytes)
		}
	}
	return ""
}

// transportURL applies the official URL pattern. The pattern's \s is ASCII
// whitespace in Go but Unicode whitespace in the JSON Schema dialect, so
// Unicode spaces and control characters are excluded explicitly.
func transportURL(s string) bool {
	return transportURLRe.MatchString(s) && !hasSpaceOrControl(s)
}

// absoluteURL reports whether s is an absolute URL with a host and one of
// the given schemes. User information is refused: a link never needs it, and
// "https://<token>@github.com/..." is how credentials end up in metadata.
func absoluteURL(s string, schemes ...string) bool {
	if s == "" || hasSpaceOrControl(s) {
		return false
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return false
	}
	for _, scheme := range schemes {
		if u.Scheme == scheme {
			return true
		}
	}
	return false
}

func hasSpaceOrControl(s string) bool {
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func runesWithin(s string, lo, hi int) bool {
	n := utf8.RuneCountInString(s)
	return n >= lo && n <= hi
}
