// Package model defines registry domain records shared by storage and API.
package model

import (
	"errors"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
)

// Storage errors.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// ConflictError carries a stable code for a uniqueness or state conflict.
type ConflictError struct{ Code, Msg string }

func (e *ConflictError) Error() string { return e.Msg }
func (e *ConflictError) Unwrap() error { return ErrConflict }

// Visibility levels.
const (
	VisibilityPublic   = "public"
	VisibilityInternal = "internal"
	VisibilityPrivate  = "private"
)

// Skill statuses.
const (
	StatusActive     = "active"
	StatusDeprecated = "deprecated"
	StatusYanked     = "yanked"
)

// Check results for indexed versions.
const (
	CheckOK                = "ok"
	CheckSourceChanged     = "source_changed"
	CheckSourceUnavailable = "source_unavailable"
)

// Role is a namespace role.
type Role string

const (
	RoleReader    Role = "reader"
	RolePublisher Role = "publisher"
	RoleOwner     Role = "owner"
)

// Rank orders roles; unknown roles rank 0.
func (r Role) Rank() int {
	switch r {
	case RoleReader:
		return 1
	case RolePublisher:
		return 2
	case RoleOwner:
		return 3
	}
	return 0
}

// Namespace owns skills.
type Namespace struct {
	ID          int64
	Name        string
	Kind        string
	GitHubID    *int64
	DisplayName string
	CreatedAt   time.Time
}

// Token is a stored API token (only the secret hash is persisted).
type Token struct {
	ID         string
	SecretHash []byte
	Name       string
	Scopes     []string
	CreatedBy  string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
}

// RoleBinding grants a subject a role in a namespace.
type RoleBinding struct {
	Namespace string `json:"namespace"`
	Role      Role   `json:"role"`
}

// Skill is a named entry in a namespace.
type Skill struct {
	ID           int64
	Namespace    string
	Name         string
	Description  string
	Visibility   string
	Listed       bool
	Status       string
	StatusReason string
	Mode         string
	SourceKey    string // indexed only
	GlobalAlias  string // empty when unset
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Source describes where hosted bytes came from.
type Source struct {
	Kind    string `json:"kind"` // upload | github
	Format  string `json:"format,omitempty"`
	Owner   string `json:"owner,omitempty"`
	Repo    string `json:"repo,omitempty"`
	Commit  string `json:"commit,omitempty"`
	Path    string `json:"path,omitempty"`
	TreeSHA string `json:"tree_sha,omitempty"`
}

// Locator identifies indexed content without storing it.
type Locator struct {
	Kind string `json:"kind"` // github | url
	// GitHub
	Owner      string `json:"owner,omitempty"`
	Repo       string `json:"repo,omitempty"`
	Commit     string `json:"commit,omitempty"`
	Path       string `json:"path,omitempty"`
	TreeSHA    string `json:"tree_sha,omitempty"`
	SingleFile bool   `json:"single_file,omitempty"`
	// URL (schema only in R1; no importer yet)
	URL    string `json:"url,omitempty"`
	Digest string `json:"digest,omitempty"`
}

// Version is an immutable published or indexed version. Only the review,
// check and yank fields may change after creation.
type Version struct {
	ID                     int64
	SkillID                int64
	Version                string
	Mode                   string
	FrontmatterName        string
	FrontmatterDescription string
	LicenseKind            string
	LicenseID              string
	LicenseText            string
	LicenseApproved        bool
	CoreCompatible         bool
	Files                  []skill.ManifestEntry
	ArtifactDigest         string
	ArtifactType           string
	ArtifactSize           int64
	ArchiveDigest          string // StoredGzip(CanonicalTar(files)); empty for skill-md
	Source                 *Source
	Locator                *Locator
	IndexedBy              string
	CheckResult            string
	CheckedAt              *time.Time
	YankedAt               *time.Time
	YankReason             string
	CreatedBy              string
	CreatedAt              time.Time
}

// SkillSummary is a list/search row.
type SkillSummary struct {
	Skill
	LatestVersion string
}

// IndexCandidate is a skill with its latest non-yanked version.
type IndexCandidate struct {
	Skill   Skill
	Version Version
}

// AuditEvent is an append-only record.
type AuditEvent struct {
	ID        int64
	At        time.Time
	Actor     string
	Action    string
	Namespace string
	Target    string
	Details   map[string]any
}

// Viewer is the visibility filter derived from the caller.
type Viewer struct {
	Admin         bool
	Authenticated bool
	// Namespaces where the caller holds any role.
	Namespaces []string
}

// CanSee applies the visibility policy for one skill: public to anyone,
// internal to any authenticated caller of this instance, private to members of
// the namespace; admins see everything.
func (v Viewer) CanSee(namespace, visibility string) bool {
	if v.Admin {
		return true
	}
	switch visibility {
	case VisibilityPublic:
		return true
	case VisibilityInternal:
		return v.Authenticated
	case VisibilityPrivate:
		for _, ns := range v.Namespaces {
			if ns == namespace {
				return true
			}
		}
	}
	return false
}

// SearchQuery filters list/search. Unlisted and yanked skills are always
// excluded.
type SearchQuery struct {
	Viewer     Viewer
	Q          string
	Namespace  string
	Mode       string
	Compatible bool
	AfterNS    string
	AfterName  string
	Limit      int
}

// PublishInput creates the skill on first publish and appends a version.
type PublishInput struct {
	Namespace  string
	Name       string
	Mode       string
	SourceKey  string
	Visibility string // used only when creating the skill
	Listed     bool   // used only when creating the skill
	Version    Version
}

// SkillPatch updates mutable skill settings.
type SkillPatch struct {
	Visibility *string
	Listed     *bool
}
