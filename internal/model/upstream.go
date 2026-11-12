package model

import (
	"strings"
	"time"
)

// Upstream kinds: what protocol the upstream speaks.
const (
	// UpstreamRegistry is another openlinker-registry (native /v1 for skills
	// and /v0.1 for MCP servers).
	UpstreamRegistry = "openlinker-registry"
	// UpstreamMCP is any registry serving the official MCP Registry read API
	// (GET /v0.1/servers), including registry.modelcontextprotocol.io.
	UpstreamMCP = "mcp-registry"
	// UpstreamAgentSkills is a site publishing an agentskills.io discovery
	// index at /.well-known/agent-skills/index.json.
	UpstreamAgentSkills = "agent-skills"
)

// ValidUpstreamKind reports whether k is a known upstream kind.
func ValidUpstreamKind(k string) bool {
	return k == UpstreamRegistry || k == UpstreamMCP || k == UpstreamAgentSkills
}

// Upstream sync states and results.
const (
	SyncIdle    = "idle"
	SyncRunning = "running"
	SyncOK      = "ok"
	SyncFailed  = "failed"
)

// NSKindUpstream marks a namespace whose entries are managed by an upstream.
// Nothing may be published into it locally.
const NSKindUpstream = "upstream"

// Upstream is another registry this instance forwards to and mirrors from.
// Content only ever flows from the upstream to this instance. The bearer
// token for a private upstream is never stored: it is read from the
// environment (see config) by upstream name.
type Upstream struct {
	ID       int64
	Name     string
	Kind     string
	BaseURL  string
	Enabled  bool
	Priority int // lower is consulted first
	// Visibility given to mirrored entries: public or internal.
	Visibility string
	// NamespaceAllow and NamespaceDeny are patterns over upstream namespaces
	// (see MatchNamespace). An empty allow list allows every namespace.
	NamespaceAllow []string
	NamespaceDeny  []string
	// TargetNamespace is the local namespace that receives entries of an
	// agent-skills upstream (such indexes carry no namespaces).
	TargetNamespace string
	// SyncInterval is the period of background mirroring; 0 disables it
	// (manual sync and forward-on-miss still work).
	SyncInterval time.Duration
	// ProxyOnMiss forwards a direct read of an entry that is not stored
	// locally to this upstream and mirrors the answer.
	ProxyOnMiss bool
	// LatestOnly mirrors only the latest version of each MCP server.
	LatestOnly bool

	Cursor         string
	SyncState      string
	SyncStartedAt  *time.Time
	LastSyncAt     *time.Time
	LastSyncStatus string
	LastSyncError  string
	LastSyncStats  map[string]int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// UpstreamPatch updates mutable upstream settings. Changing BaseURL resets
// the sync cursor.
type UpstreamPatch struct {
	BaseURL        *string
	Enabled        *bool
	Priority       *int
	Visibility     *string
	NamespaceAllow *[]string
	NamespaceDeny  *[]string
	SyncInterval   *time.Duration
	ProxyOnMiss    *bool
	LatestOnly     *bool
}

// UpstreamSyncResult records the outcome of one sync run. Cursor is stored
// only when Status is SyncOK.
type UpstreamSyncResult struct {
	At     time.Time
	Status string
	Error  string
	Cursor string
	Stats  map[string]int
}

// MatchNamespace reports whether ns matches one of the patterns. A pattern is
// an exact namespace, "*" (everything) or a prefix ending in ".*" which
// matches the prefix itself and anything below it ("io.github.*" matches
// "io.github.alice" but not "io.githubx").
func MatchNamespace(patterns []string, ns string) bool {
	for _, p := range patterns {
		switch {
		case p == "*", p == ns:
			return true
		case strings.HasSuffix(p, ".*"):
			prefix := strings.TrimSuffix(p, ".*")
			if ns == prefix || strings.HasPrefix(ns, prefix+".") {
				return true
			}
		}
	}
	return false
}

// Allows applies the allow and deny lists to an upstream namespace.
func (u Upstream) Allows(ns string) bool {
	if MatchNamespace(u.NamespaceDeny, ns) {
		return false
	}
	return len(u.NamespaceAllow) == 0 || MatchNamespace(u.NamespaceAllow, ns)
}

// ValidNamespacePattern reports whether p is a usable allow/deny pattern.
func ValidNamespacePattern(p string) bool {
	if p == "*" {
		return true
	}
	base := strings.TrimSuffix(p, ".*")
	if base == "" || len(p) > 140 {
		return false
	}
	for _, label := range strings.Split(base, ".") {
		if label == "" {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
