// Package skill holds pure validation rules for skill names, versions,
// namespaces, SKILL.md frontmatter, file sets, licenses and content access.
// It performs no I/O.
package skill

import (
	"regexp"
	"strings"
)

var (
	slugRe      = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	versionRe   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
	nsLabel     = `[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?`
	namespaceRe = regexp.MustCompile(`^` + nsLabel + `(\.` + nsLabel + `)+$`)
	githubLogin = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,37}[a-z0-9])?$`)
)

// reservedNames collide with fixed API path segments under /v1/skills/{ns}/.
var reservedNames = map[string]bool{"indexes": true, "github-imports": true}

// ValidSlug reports whether s is a skill slug as required by the skills CLI and
// the agentskills.io discovery format: 1-64 chars of [a-z0-9-], no leading,
// trailing or doubled hyphen.
func ValidSlug(s string) bool {
	return len(s) >= 1 && len(s) <= 64 && slugRe.MatchString(s)
}

// ValidSkillName is ValidSlug minus names reserved by the API.
func ValidSkillName(s string) bool { return ValidSlug(s) && !reservedNames[s] }

// ValidVersion uses the same pattern as OpenLinker Core skill package versions.
func ValidVersion(s string) bool { return versionRe.MatchString(s) }

// Namespace kinds.
const (
	NSKindGitHub     = "github"     // io.github.<login>, bound to a numeric GitHub id
	NSKindOpenLinker = "openlinker" // ai.openlinker.<handle>, public instance only
	NSKindInternal   = "internal"   // internal.<team>, private instance only
	NSKindDomain     = "domain"     // reverse DNS, verification not implemented in R1
)

// ValidNamespaceName reports whether name has the namespace syntax (lowercase
// reverse-DNS labels, at most 128 bytes), whatever its kind.
func ValidNamespaceName(name string) bool {
	return len(name) <= 128 && namespaceRe.MatchString(name)
}

// NamespaceError describes why a namespace name was rejected.
type NamespaceError struct{ Msg string }

func (e *NamespaceError) Error() string { return e.Msg }

// ValidateNamespace checks the reverse-DNS name syntax and that the name is
// consistent with its kind and the instance mode ("public" or "private").
func ValidateNamespace(name, kind, mode string) error {
	if !ValidNamespaceName(name) {
		return &NamespaceError{"namespace must be lowercase reverse-DNS labels, e.g. io.github.alice"}
	}
	labels := strings.Split(name, ".")
	switch kind {
	case NSKindGitHub:
		if len(labels) != 3 || labels[0] != "io" || labels[1] != "github" || !githubLogin.MatchString(labels[2]) {
			return &NamespaceError{"github namespaces must be io.github.<login>"}
		}
	case NSKindOpenLinker:
		if mode != "public" {
			return &NamespaceError{"ai.openlinker namespaces exist only on the public instance"}
		}
		if len(labels) != 3 || labels[0] != "ai" || labels[1] != "openlinker" {
			return &NamespaceError{"openlinker namespaces must be ai.openlinker.<handle>"}
		}
	case NSKindInternal:
		if mode != "private" {
			return &NamespaceError{"internal namespaces exist only on private instances"}
		}
		if len(labels) != 2 || labels[0] != "internal" {
			return &NamespaceError{"internal namespaces must be internal.<team>"}
		}
	case NSKindDomain:
		switch {
		case labels[0] == "internal",
			labels[0] == "io" && labels[1] == "github",
			labels[0] == "ai" && labels[1] == "openlinker":
			return &NamespaceError{"reverse-domain namespaces cannot use a reserved prefix"}
		}
	default:
		return &NamespaceError{"unknown namespace kind"}
	}
	return nil
}
