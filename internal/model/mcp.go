package model

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// MCP entry modes (design §4): registered by the namespace owner, indexed by
// someone else ("not published by the author"), or mirrored from an upstream.
const (
	MCPModeRegistered = "registered"
	MCPModeIndexed    = "indexed"
	MCPModeMirrored   = "mirrored"
)

// MCP version statuses, identical to the official MCP Registry.
const (
	MCPStatusActive     = "active"
	MCPStatusDeprecated = "deprecated"
	MCPStatusDeleted    = "deleted"
)

// MaxMCPStatusMessage bounds a status message (official registry limit).
const MaxMCPStatusMessage = 500

// ValidMCPStatus reports whether s is a known version status.
func ValidMCPStatus(s string) bool {
	return s == MCPStatusActive || s == MCPStatusDeprecated || s == MCPStatusDeleted
}

// MCPServer is a named MCP server entry. Name is the full server.json name
// ("<namespace>/<server>") exactly as published; Namespace is the lowercase
// namespace row that owns it.
type MCPServer struct {
	ID         int64
	Namespace  string
	Name       string
	Mode       string
	Visibility string
	Listed     bool
	// Upstream is the name of the upstream that manages the namespace ("" for
	// local entries).
	Upstream  string
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// MCPVersion is one published version. ServerJSON, Digest, Version,
// PublishedAt and CreatedBy never change; status fields and IsLatest may.
type MCPVersion struct {
	ID              int64
	ServerID        int64
	Version         string
	ServerJSON      json.RawMessage
	Digest          string // sha256 of the canonical server.json
	Status          string
	StatusMessage   string
	StatusChangedAt time.Time
	PublishedAt     time.Time
	UpdatedAt       time.Time
	IsLatest        bool
	CreatedBy       string
}

// MCPEntry is one (server, version) row of a listing.
type MCPEntry struct {
	Server  MCPServer
	Version MCPVersion
}

// MCPPublishInput creates the server on first publish and appends a version.
// The store decides IsLatest atomically: the new version becomes latest when
// there is no latest version or CompareVersions(new, latest) > 0.
type MCPPublishInput struct {
	Namespace  string // lowercase namespace row; must exist
	Name       string // full server name
	Mode       string // registered | indexed
	Visibility string // used only when creating the server
	Listed     bool   // used only when creating the server
	Version    MCPVersion
}

// MCPMirrorInput upserts one version mirrored from an upstream. The namespace
// row is created (kind "upstream") when missing. Visibility and Listed are
// how widely the upstream allows the server to be shown; they are applied to
// an existing server too, in the same transaction as the version.
type MCPMirrorInput struct {
	UpstreamID int64
	// ConfigVersion is the upstream's ConfigVersion the caller read its
	// settings under (see PublishInput.UpstreamConfigVersion).
	ConfigVersion int64
	Namespace     string
	Name          string
	Visibility    string
	Listed        bool
	Version       MCPVersion
	// Fresh says the document was read from the upstream just now, under
	// the entry's mirror lock. A document that is not fresh (it came with a
	// listing page read some time ago) may be older than what a fresh read
	// has applied since, so it may only add a version or narrow who sees
	// the server; anything else is left alone and reported as MirrorStale.
	Fresh bool
	// StatusOnly changes nothing but the state of a version that is already
	// stored (ErrNotFound otherwise): the server's visibility and listing
	// stay as they are, and Visibility and Listed are ignored. It is how a
	// version that is gone upstream is marked deleted without touching who
	// may see the server.
	StatusOnly bool
}

// Mirror upsert outcomes.
const (
	MirrorCreated   = "created"
	MirrorUpdated   = "updated"
	MirrorUnchanged = "unchanged"
	// MirrorStale: the document was not fresh and would have changed the
	// state of an existing version or widened who sees the server. What it
	// could safely do was done; the caller reads the entry again.
	MirrorStale = "stale"
)

// NarrowerVisibility returns the more restrictive of two visibilities.
func NarrowerVisibility(a, b string) string {
	rank := func(v string) int {
		switch v {
		case VisibilityPublic:
			return 3
		case VisibilityInternal:
			return 2
		}
		return 1
	}
	if rank(a) <= rank(b) {
		return a
	}
	return b
}

// MCPPatch updates mutable server settings.
type MCPPatch struct {
	Visibility *string
	Listed     *bool
}

// MCPQuery filters an MCP listing. Results are ordered by (name, version) in
// byte order. Unlisted servers are excluded; deleted versions are excluded
// unless IncludeDeleted is set.
type MCPQuery struct {
	Viewer Viewer
	// Search is a case-insensitive substring match on the server name, title
	// and description.
	Search    string
	Namespace string
	Mode      string
	// Version is "" (every version), "latest" or an exact version.
	Version        string
	UpdatedSince   *time.Time
	IncludeDeleted bool
	AfterName      string
	AfterVersion   string
	Limit          int
}

// CompareVersions orders two version strings. When both are semantic versions
// (MAJOR.MINOR.PATCH with optional pre-release and build metadata, optional
// leading "v") they compare by semver precedence; otherwise the result is 0
// and callers fall back to publication order.
func CompareVersions(a, b string) int {
	sa, okA := parseSemver(a)
	sb, okB := parseSemver(b)
	if !okA || !okB {
		return 0
	}
	for i := range 3 {
		if sa.num[i] != sb.num[i] {
			if sa.num[i] < sb.num[i] {
				return -1
			}
			return 1
		}
	}
	return comparePre(sa.pre, sb.pre)
}

type semver struct {
	num [3]uint64
	pre []string
}

func parseSemver(s string) (semver, bool) {
	var out semver
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		if p == "" || (len(p) > 1 && p[0] == '0') {
			return out, false
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return out, false
		}
		out.num[i] = n
	}
	if hasPre {
		if pre == "" {
			return out, false
		}
		out.pre = strings.Split(pre, ".")
		for _, id := range out.pre {
			if id == "" {
				return out, false
			}
		}
	}
	return out, true
}

// comparePre implements semver pre-release precedence: no pre-release ranks
// above any pre-release; identifiers compare numerically when both are
// numeric, numeric below alphanumeric, otherwise in ASCII order; a longer
// list wins when all shared identifiers are equal.
func comparePre(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return 1
	case len(b) == 0:
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		na, errA := strconv.ParseUint(a[i], 10, 64)
		nb, errB := strconv.ParseUint(b[i], 10, 64)
		switch {
		case errA == nil && errB == nil:
			if na != nb {
				if na < nb {
					return -1
				}
				return 1
			}
		case errA == nil:
			return -1
		case errB == nil:
			return 1
		default:
			if c := strings.Compare(a[i], b[i]); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}
