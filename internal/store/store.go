// Package store defines the persistence interface of the registry and an
// in-memory implementation used by tests and local experiments. The Postgres
// implementation lives in package pgstore; both are exercised by the shared
// conformance suite in package store/storetest.
package store

import (
	"context"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
)

// Store is the registry persistence boundary. Every mutating method takes the
// audit event describing it and must persist both atomically: if the audit
// row cannot be written, the mutation is rolled back.
type Store interface {
	Ping(ctx context.Context) error

	CreateNamespace(ctx context.Context, ns model.Namespace, audit model.AuditEvent) (model.Namespace, error)
	GetNamespace(ctx context.Context, name string) (model.Namespace, error)
	SetRole(ctx context.Context, namespace, subject string, role model.Role, audit model.AuditEvent) error
	RolesForSubject(ctx context.Context, subject string) (map[string]model.Role, error)

	CreateToken(ctx context.Context, t model.Token, roles []model.RoleBinding, audit model.AuditEvent) error
	GetToken(ctx context.Context, id string) (model.Token, error)
	RevokeToken(ctx context.Context, id string, at time.Time, audit model.AuditEvent) error

	GetSkill(ctx context.Context, namespace, name string) (model.Skill, error)
	// PublishVersion creates the skill if needed and appends an immutable
	// version atomically. It returns ConflictError codes: mode_mismatch,
	// source_mismatch, source_already_indexed, version_exists, skill_yanked.
	PublishVersion(ctx context.Context, in model.PublishInput, audit model.AuditEvent) (model.Skill, model.Version, error)
	ListVersions(ctx context.Context, skillID int64) ([]model.Version, error)
	GetVersion(ctx context.Context, skillID int64, version string) (model.Version, error)
	UpdateSkill(ctx context.Context, skillID int64, p model.SkillPatch, audit model.AuditEvent) (model.Skill, error)
	SetSkillStatus(ctx context.Context, skillID int64, status, reason string, audit model.AuditEvent) (model.Skill, error)
	SetAlias(ctx context.Context, skillID int64, alias string, audit model.AuditEvent) error
	YankVersion(ctx context.Context, versionID int64, reason string, at time.Time, audit model.AuditEvent) error
	ApproveLicense(ctx context.Context, versionID int64, audit model.AuditEvent) error
	SetVersionCheck(ctx context.Context, versionID int64, result string, at time.Time) error

	Search(ctx context.Context, q model.SearchQuery) ([]model.SkillSummary, error)
	// IndexCandidates returns public, listed, active skills with their latest
	// non-yanked version. namespace filters by namespace; aliasOnly limits to
	// skills with a global alias.
	IndexCandidates(ctx context.Context, namespace string, aliasOnly bool) ([]model.IndexCandidate, error)

	// ReferencedArtifacts reports which of the given digests are referenced
	// by any hosted version (used by artifact garbage collection).
	ReferencedArtifacts(ctx context.Context, digests []string) (map[string]bool, error)

	AppendAudit(ctx context.Context, e model.AuditEvent) error
	ListAudit(ctx context.Context, limit int) ([]model.AuditEvent, error)

	// ListNamespaces returns every namespace ordered by name.
	ListNamespaces(ctx context.Context) ([]model.Namespace, error)
	// ListTokens returns every token (newest first). SecretHash is not set.
	ListTokens(ctx context.Context) ([]model.Token, error)

	// --- MCP servers ---

	// PublishMCPVersion creates the server if needed and appends an immutable
	// version atomically, deciding IsLatest under the server row lock. The
	// namespace must be local (not upstream-managed). ConflictError codes:
	// namespace_upstream_managed, mode_mismatch, version_exists,
	// server_name_conflict (another server differs only by letter case).
	PublishMCPVersion(ctx context.Context, in model.MCPPublishInput, audit model.AuditEvent) (model.MCPServer, model.MCPVersion, error)
	// UpsertMirroredMCP stores one mirrored version: it creates the
	// upstream-managed namespace and the server when missing, inserts the
	// version, or updates the status fields and IsLatest of an existing one.
	// It returns MirrorCreated, MirrorUpdated or MirrorUnchanged; the audit
	// event is written atomically with a created or updated version and not
	// at all for an unchanged one.
	// ConflictError codes: namespace_upstream_managed (the namespace is local
	// or belongs to another upstream), server_name_conflict, digest_mismatch
	// (the stored version has a different server.json; it is left untouched).
	UpsertMirroredMCP(ctx context.Context, in model.MCPMirrorInput, audit model.AuditEvent) (string, error)
	// GetMCPServer looks a server up by its exact full name.
	GetMCPServer(ctx context.Context, name string) (model.MCPServer, error)
	// GetMCPVersion returns one version; version "latest" resolves to the
	// version flagged IsLatest.
	GetMCPVersion(ctx context.Context, serverID int64, version string) (model.MCPVersion, error)
	// ListMCPVersions returns every version of a server, newest first.
	ListMCPVersions(ctx context.Context, serverID int64) ([]model.MCPVersion, error)
	// SetMCPStatus changes the status of one version, or of every version
	// when version is "". It returns the number of versions changed. When the
	// latest version is deleted, IsLatest moves to the best remaining
	// non-deleted version (and back when a better version is restored).
	SetMCPStatus(ctx context.Context, serverID int64, version, status, message string, at time.Time, audit model.AuditEvent) (int, error)
	UpdateMCPServer(ctx context.Context, serverID int64, p model.MCPPatch, audit model.AuditEvent) (model.MCPServer, error)
	// SearchMCP lists (server, version) rows; see model.MCPQuery.
	SearchMCP(ctx context.Context, q model.MCPQuery) ([]model.MCPEntry, error)

	// --- upstreams ---

	// CreateUpstream stores a new upstream (ConflictError upstream_exists).
	CreateUpstream(ctx context.Context, u model.Upstream, audit model.AuditEvent) (model.Upstream, error)
	GetUpstream(ctx context.Context, name string) (model.Upstream, error)
	// ListUpstreams returns upstreams ordered by priority, then name.
	ListUpstreams(ctx context.Context) ([]model.Upstream, error)
	UpdateUpstream(ctx context.Context, name string, p model.UpstreamPatch, audit model.AuditEvent) (model.Upstream, error)
	// DeleteUpstream removes an upstream that manages no namespace
	// (ConflictError upstream_in_use otherwise).
	DeleteUpstream(ctx context.Context, name string, audit model.AuditEvent) error
	// ClaimUpstreamSync marks the upstream as syncing and returns true unless
	// a sync that started after staleBefore is still running. It is atomic
	// across processes.
	ClaimUpstreamSync(ctx context.Context, id int64, now, staleBefore time.Time) (bool, error)
	// FinishUpstreamSync records a sync result and returns the upstream to
	// idle, atomically with the audit event. The cursor is stored only when
	// the result status is SyncOK.
	FinishUpstreamSync(ctx context.Context, id int64, res model.UpstreamSyncResult, audit model.AuditEvent) error
	// EnsureUpstreamNamespace returns the namespace managed by the upstream,
	// creating it (kind "upstream") when missing. A namespace that is local
	// or managed by another upstream yields ConflictError
	// namespace_upstream_managed. The audit event is written atomically with
	// a creation and not at all when the namespace already exists.
	EnsureUpstreamNamespace(ctx context.Context, name string, upstreamID int64, audit model.AuditEvent) (model.Namespace, error)
}
