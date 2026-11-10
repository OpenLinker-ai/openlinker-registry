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

// Store is the registry persistence boundary.
type Store interface {
	Ping(ctx context.Context) error

	CreateNamespace(ctx context.Context, ns model.Namespace) (model.Namespace, error)
	GetNamespace(ctx context.Context, name string) (model.Namespace, error)
	SetRole(ctx context.Context, namespace, subject string, role model.Role) error
	RolesForSubject(ctx context.Context, subject string) (map[string]model.Role, error)

	CreateToken(ctx context.Context, t model.Token, roles []model.RoleBinding) error
	GetToken(ctx context.Context, id string) (model.Token, error)
	RevokeToken(ctx context.Context, id string, at time.Time) error

	GetSkill(ctx context.Context, namespace, name string) (model.Skill, error)
	// PublishVersion creates the skill if needed and appends an immutable
	// version atomically. It returns ConflictError codes: mode_mismatch,
	// source_mismatch, source_already_indexed, version_exists, skill_yanked.
	PublishVersion(ctx context.Context, in model.PublishInput) (model.Skill, model.Version, error)
	ListVersions(ctx context.Context, skillID int64) ([]model.Version, error)
	GetVersion(ctx context.Context, skillID int64, version string) (model.Version, error)
	UpdateSkill(ctx context.Context, skillID int64, p model.SkillPatch) (model.Skill, error)
	SetSkillStatus(ctx context.Context, skillID int64, status, reason string) (model.Skill, error)
	SetAlias(ctx context.Context, skillID int64, alias string) error
	YankVersion(ctx context.Context, versionID int64, reason string, at time.Time) error
	ApproveLicense(ctx context.Context, versionID int64) error
	SetVersionCheck(ctx context.Context, versionID int64, result string, at time.Time) error

	Search(ctx context.Context, q model.SearchQuery) ([]model.SkillSummary, error)
	// IndexCandidates returns public, listed, active skills with their latest
	// non-yanked version. namespace filters by namespace; aliasOnly limits to
	// skills with a global alias.
	IndexCandidates(ctx context.Context, namespace string, aliasOnly bool) ([]model.IndexCandidate, error)

	AppendAudit(ctx context.Context, e model.AuditEvent) error
	ListAudit(ctx context.Context, limit int) ([]model.AuditEvent, error)
}
