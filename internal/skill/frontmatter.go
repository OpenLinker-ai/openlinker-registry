package skill

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Limits on SKILL.md frontmatter values. The name/description bounds follow
// OpenLinker Core (120 / 2000 runes); the discovery index additionally caps
// descriptions at 1024 characters (see WellKnownDescriptionMax).
const (
	MaxFrontmatterBytes     = 64 << 10
	MaxNameRunes            = 120
	MaxDescriptionRunes     = 2000
	WellKnownDescriptionMax = 1024
	MaxLicenseRunes         = 200
)

// Frontmatter is the subset of SKILL.md frontmatter the registry records.
type Frontmatter struct {
	Name        string
	Description string
	License     string
	// HasBody reports whether non-whitespace instructions follow the frontmatter.
	HasBody bool
}

// ValidationError is a client-facing validation failure with a stable code.
type ValidationError struct {
	Code string
	Msg  string
}

func (e *ValidationError) Error() string { return e.Msg }

func verr(code, msg string) error { return &ValidationError{Code: code, Msg: msg} }

// ParseFrontmatter extracts and validates YAML frontmatter from SKILL.md bytes.
// CRLF line endings and a UTF-8 BOM are tolerated for parsing only; the
// stored bytes are never rewritten.
func ParseFrontmatter(raw []byte) (Frontmatter, error) {
	if !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
		return Frontmatter{}, verr("skill_md_encoding", "SKILL.md must be UTF-8 text without NUL bytes")
	}
	text := strings.TrimPrefix(string(raw), "\uFEFF")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return Frontmatter{}, verr("frontmatter_missing", "SKILL.md must start with a --- YAML frontmatter block")
	}
	rest := text[4:]
	var yamlText, body string
	if strings.HasPrefix(rest, "---\n") || rest == "---" {
		yamlText, body = "", strings.TrimPrefix(strings.TrimPrefix(rest, "---"), "\n")
	} else {
		end := strings.Index(rest, "\n---\n")
		if end < 0 {
			if strings.HasSuffix(rest, "\n---") {
				end = len(rest) - 4
			} else {
				return Frontmatter{}, verr("frontmatter_unterminated", "SKILL.md frontmatter must end with a --- line")
			}
		}
		yamlText = rest[:end]
		body = strings.TrimPrefix(rest[end+4:], "\n")
	}
	if len(yamlText) > MaxFrontmatterBytes {
		return Frontmatter{}, verr("frontmatter_too_large", "SKILL.md frontmatter exceeds 64 KiB")
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(yamlText), &doc); err != nil {
		return Frontmatter{}, verr("frontmatter_invalid", "SKILL.md frontmatter is not valid YAML")
	}
	str := func(key string) (string, bool) {
		v, ok := doc[key]
		if !ok || v == nil {
			return "", true
		}
		s, ok := v.(string)
		return strings.TrimSpace(s), ok
	}
	name, ok1 := str("name")
	desc, ok2 := str("description")
	lic, ok3 := str("license")
	if !ok1 || !ok2 || !ok3 {
		return Frontmatter{}, verr("frontmatter_invalid", "frontmatter name, description and license must be strings")
	}
	if n := utf8.RuneCountInString(name); n < 1 || n > MaxNameRunes || hasControl(name) {
		return Frontmatter{}, verr("frontmatter_name", "frontmatter name is required (1-120 characters, no control characters)")
	}
	if n := utf8.RuneCountInString(desc); n < 1 || n > MaxDescriptionRunes || hasControl(desc) {
		return Frontmatter{}, verr("frontmatter_description", "frontmatter description is required (1-2000 characters, no control characters)")
	}
	if utf8.RuneCountInString(lic) > MaxLicenseRunes || hasControl(lic) {
		return Frontmatter{}, verr("frontmatter_license", "frontmatter license is too long or contains control characters")
	}
	return Frontmatter{Name: name, Description: desc, License: lic, HasBody: strings.TrimSpace(body) != ""}, nil
}

// hasControl reports control characters other than tab/newline (descriptions
// may legitimately be folded YAML containing newlines).
func hasControl(s string) bool {
	for _, r := range s {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return true
		}
	}
	return false
}
