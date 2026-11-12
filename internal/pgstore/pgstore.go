// Package pgstore implements store.Store on PostgreSQL with embedded,
// forward-only migrations.
package pgstore

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockID is a fixed advisory lock key for migrations.
const migrationLockID = 0x6f6c726567 // "olreg"

// Store is the Postgres store.
type Store struct{ pool *pgxpool.Pool }

var _ store.Store = (*Store)(nil)

// Open connects using a pgx DSN.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid database DSN")
	}
	return OpenConfig(ctx, cfg)
}

// OpenConfig connects with a prepared pool config.
func OpenConfig(ctx context.Context, cfg *pgxpool.Config) (*Store, error) {
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect database: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping implements store.Store.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Migrate applies pending embedded migrations under an advisory lock.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockID) //nolint:errcheck
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	names, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	slices.Sort(names)
	for _, name := range names {
		base := strings.TrimPrefix(name, "migrations/")
		num, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("bad migration name %q", base)
		}
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, num).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		body, err := migrationFS.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", base, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, num); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insertAudit(ctx context.Context, q execer, e model.AuditEvent) error {
	details := e.Details
	if details == nil {
		details = map[string]any{}
	}
	b, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO audit_events (actor, action, namespace, target, details) VALUES ($1,$2,$3,$4,$5)`,
		e.Actor, e.Action, e.Namespace, e.Target, b)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	return nil
}

// inTx runs fn and the audit insert in one transaction.
func (s *Store) inTx(ctx context.Context, audit model.AuditEvent, fn func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		return insertAudit(ctx, tx, audit)
	})
}

func conflict(code, msg string) error { return &model.ConflictError{Code: code, Msg: msg} }

func uniqueViolation(err error) (string, bool) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return pe.ConstraintName, true
	}
	return "", false
}

func foreignKeyViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23503"
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ErrNotFound
	}
	return err
}

const nsCols = `id, name, kind, github_id, display_name, COALESCE(upstream_id, 0), created_at`

func scanNamespace(row pgx.Row) (model.Namespace, error) {
	var ns model.Namespace
	err := row.Scan(&ns.ID, &ns.Name, &ns.Kind, &ns.GitHubID, &ns.DisplayName, &ns.UpstreamID, &ns.CreatedAt)
	return ns, err
}

// CreateNamespace implements store.Store. It only creates local namespaces;
// upstream-managed ones come from EnsureUpstreamNamespace.
func (s *Store) CreateNamespace(ctx context.Context, ns model.Namespace, audit model.AuditEvent) (model.Namespace, error) {
	if ns.Kind == model.NSKindUpstream {
		return model.Namespace{}, store.ErrNamespaceKind()
	}
	ns.UpstreamID = 0
	err := s.inTx(ctx, audit, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO namespaces (name, kind, github_id, display_name)
		VALUES ($1, $2, $3, $4) RETURNING id, created_at`, ns.Name, ns.Kind, ns.GitHubID, ns.DisplayName).Scan(&ns.ID, &ns.CreatedAt)
	})
	if c, ok := uniqueViolation(err); ok {
		if strings.Contains(c, "github_id") {
			return model.Namespace{}, conflict("github_id_bound", "GitHub id is already bound to a namespace")
		}
		return model.Namespace{}, conflict("namespace_exists", "namespace already exists")
	}
	return ns, err
}

// GetNamespace implements store.Store.
func (s *Store) GetNamespace(ctx context.Context, name string) (model.Namespace, error) {
	ns, err := scanNamespace(s.pool.QueryRow(ctx, `SELECT `+nsCols+` FROM namespaces WHERE name = $1`, name))
	return ns, notFound(err)
}

// ListNamespaces implements store.Store. Names sort in byte order, like the
// in-memory store, whatever the database collation.
func (s *Store) ListNamespaces(ctx context.Context) ([]model.Namespace, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+nsCols+` FROM namespaces ORDER BY name COLLATE "C"`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Namespace, error) { return scanNamespace(row) })
}

// SetRole implements store.Store.
func (s *Store) SetRole(ctx context.Context, namespace, subject string, role model.Role, audit model.AuditEvent) error {
	return s.inTx(ctx, audit, func(tx pgx.Tx) error {
		return grantRole(ctx, tx, namespace, subject, role)
	})
}

// grantRole gives subject a role in a local namespace. An upstream-managed
// namespace has no members.
func grantRole(ctx context.Context, tx pgx.Tx, namespace, subject string, role model.Role) error {
	var nsID, upstreamID int64
	if err := tx.QueryRow(ctx, `SELECT id, COALESCE(upstream_id, 0) FROM namespaces WHERE name = $1`, namespace).
		Scan(&nsID, &upstreamID); err != nil {
		return notFound(err)
	}
	if upstreamID != 0 {
		return model.ErrNamespaceManaged()
	}
	_, err := tx.Exec(ctx, `INSERT INTO namespace_roles (namespace_id, subject, role) VALUES ($1, $2, $3)
		ON CONFLICT (namespace_id, subject) DO UPDATE SET role = EXCLUDED.role`, nsID, subject, string(role))
	return err
}

// mirrorVisibility returns the visibility a write on behalf of an upstream
// may give an entry: never public while the upstream mirrors as internal.
// The upstream row is share-locked for the rest of the transaction, so the
// write is ordered against UpdateUpstream: either it sees the lowered
// setting, or UpdateUpstream lowers what it wrote. Every such write takes
// this lock before any entry row, the same order as UpdateUpstream.
func mirrorVisibility(ctx context.Context, tx pgx.Tx, upstreamID int64, v string) (string, error) {
	if upstreamID == 0 {
		return v, nil
	}
	var limit string
	if err := tx.QueryRow(ctx, `SELECT visibility FROM upstreams WHERE id = $1 FOR SHARE`, upstreamID).Scan(&limit); err != nil {
		return "", notFound(err)
	}
	if limit == model.VisibilityInternal && v == model.VisibilityPublic {
		return model.VisibilityInternal, nil
	}
	return v, nil
}

// RolesForSubject implements store.Store.
func (s *Store) RolesForSubject(ctx context.Context, subject string) (map[string]model.Role, error) {
	rows, err := s.pool.Query(ctx, `SELECT n.name, r.role FROM namespace_roles r JOIN namespaces n ON n.id = r.namespace_id WHERE r.subject = $1`, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]model.Role{}
	for rows.Next() {
		var ns, role string
		if err := rows.Scan(&ns, &role); err != nil {
			return nil, err
		}
		out[ns] = model.Role(role)
	}
	return out, rows.Err()
}

// CreateToken implements store.Store.
func (s *Store) CreateToken(ctx context.Context, t model.Token, roles []model.RoleBinding, audit model.AuditEvent) error {
	return s.inTx(ctx, audit, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO api_tokens (id, secret_hash, name, scopes, created_by, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, t.ID, t.SecretHash, t.Name, t.Scopes, t.CreatedBy, t.ExpiresAt)
		if _, ok := uniqueViolation(err); ok {
			return conflict("token_exists", "token id collision")
		}
		if err != nil {
			return err
		}
		for _, rb := range roles {
			if err := grantRole(ctx, tx, rb.Namespace, "token:"+t.ID, rb.Role); err != nil {
				return err
			}
		}
		return nil
	})
}

// GetToken implements store.Store.
func (s *Store) GetToken(ctx context.Context, id string) (model.Token, error) {
	var t model.Token
	err := s.pool.QueryRow(ctx, `SELECT id, secret_hash, name, scopes, created_by, created_at, expires_at, revoked_at
		FROM api_tokens WHERE id = $1`, id).Scan(&t.ID, &t.SecretHash, &t.Name, &t.Scopes, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt, &t.RevokedAt)
	return t, notFound(err)
}

// ListTokens implements store.Store. The secret hash is never selected.
func (s *Store) ListTokens(ctx context.Context) ([]model.Token, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, scopes, created_by, created_at, expires_at, revoked_at
		FROM api_tokens ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Token, error) {
		var t model.Token
		err := row.Scan(&t.ID, &t.Name, &t.Scopes, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt, &t.RevokedAt)
		return t, err
	})
}

// RevokeToken implements store.Store.
func (s *Store) RevokeToken(ctx context.Context, id string, at time.Time, audit model.AuditEvent) error {
	return s.inTx(ctx, audit, func(tx pgx.Tx) error {
		return rowsOrNotFound(tx.Exec(ctx, `UPDATE api_tokens SET revoked_at = COALESCE(revoked_at, $2) WHERE id = $1`, id, at))
	})
}

func rowsOrNotFound(tag pgconn.CommandTag, err error) error {
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return model.ErrNotFound
	}
	return nil
}

const skillCols = `s.id, n.name, s.name, s.description, s.visibility, s.listed, s.status, s.status_reason,
	s.mode, COALESCE(s.source_key, ''), COALESCE(s.global_alias, ''), COALESCE(u.name, ''), s.created_at, s.updated_at`

// skillFrom joins what skillCols reads: the namespace and, for an
// upstream-managed namespace, its upstream.
const skillFrom = ` FROM skills s JOIN namespaces n ON n.id = s.namespace_id LEFT JOIN upstreams u ON u.id = n.upstream_id `

func scanSkill(row pgx.Row, extra ...any) (model.Skill, error) {
	var k model.Skill
	dest := append([]any{&k.ID, &k.Namespace, &k.Name, &k.Description, &k.Visibility, &k.Listed, &k.Status,
		&k.StatusReason, &k.Mode, &k.SourceKey, &k.GlobalAlias, &k.Upstream, &k.CreatedAt, &k.UpdatedAt}, extra...)
	err := row.Scan(dest...)
	return k, err
}

func (s *Store) getSkillByID(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id int64) (model.Skill, error) {
	k, err := scanSkill(q.QueryRow(ctx, `SELECT `+skillCols+skillFrom+`WHERE s.id = $1`, id))
	return k, notFound(err)
}

// GetSkill implements store.Store.
func (s *Store) GetSkill(ctx context.Context, namespace, name string) (model.Skill, error) {
	k, err := scanSkill(s.pool.QueryRow(ctx, `SELECT `+skillCols+skillFrom+`WHERE n.name = $1 AND s.name = $2`, namespace, name))
	return k, notFound(err)
}

// PublishVersion implements store.Store.
func (s *Store) PublishVersion(ctx context.Context, in model.PublishInput, audit model.AuditEvent) (model.Skill, model.Version, error) {
	var outSkill model.Skill
	var outVer model.Version
	err := s.inTx(ctx, audit, func(tx pgx.Tx) error {
		var nsID, nsUpstream int64
		if err := tx.QueryRow(ctx, `SELECT id, COALESCE(upstream_id, 0) FROM namespaces WHERE name = $1`, in.Namespace).
			Scan(&nsID, &nsUpstream); err != nil {
			return notFound(err)
		}
		// The owner of a namespace never changes (schema trigger), so this
		// needs no lock.
		if nsUpstream != in.UpstreamID {
			return model.ErrNamespaceManaged()
		}
		visibility, err := mirrorVisibility(ctx, tx, in.UpstreamID, in.Visibility)
		if err != nil {
			return err
		}
		var sourceKey *string
		if in.SourceKey != "" {
			sourceKey = &in.SourceKey
		}
		// Create the skill if missing; a concurrent creator wins and we re-read.
		_, err = tx.Exec(ctx, `INSERT INTO skills (namespace_id, name, visibility, listed, mode, source_key)
			VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (namespace_id, name) DO NOTHING`,
			nsID, in.Name, visibility, in.Listed, in.Mode, sourceKey)
		if c, ok := uniqueViolation(err); ok && strings.Contains(c, "source_key") {
			// Do not reveal which (possibly private) entry holds the source.
			return conflict("source_already_indexed", "this source is already indexed on this registry")
		}
		if err != nil {
			return err
		}
		k, err := scanSkill(tx.QueryRow(ctx, `SELECT `+skillCols+skillFrom+`WHERE s.namespace_id = $1 AND s.name = $2 FOR UPDATE OF s`,
			nsID, in.Name))
		if err != nil {
			return err
		}
		if k.Mode != in.Mode {
			return conflict("mode_mismatch", "skill exists with a different mode")
		}
		if k.SourceKey != in.SourceKey {
			return conflict("source_mismatch", "indexed skill tracks a different source")
		}
		if k.Status == model.StatusYanked {
			return conflict("skill_yanked", "skill is yanked")
		}
		// Checked under the skill row lock, so a concurrent visibility
		// change cannot interleave with this publish.
		if in.Mode == "hosted" && k.Visibility == model.VisibilityPublic && in.Version.LicenseKind == "unknown" {
			return &model.PolicyError{Code: model.CodeLicenseRequired, Msg: model.MsgLicenseRequired}
		}
		if in.ExpectNewest != nil {
			var newest string
			if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT version FROM skill_versions WHERE skill_id = $1
				ORDER BY id DESC LIMIT 1), '')`, k.ID).Scan(&newest); err != nil {
				return err
			}
			if newest != *in.ExpectNewest {
				return conflict("version_order", "the newest version is not the expected one")
			}
		}
		v := in.Version
		files, _ := json.Marshal(v.Files)
		src, loc := jsonOrNil(v.Source), jsonOrNil(v.Locator)
		var archive *string
		if v.ArchiveDigest != "" {
			archive = &v.ArchiveDigest
		}
		err = tx.QueryRow(ctx, `INSERT INTO skill_versions (skill_id, version, mode, frontmatter_name, frontmatter_description,
			license_kind, license_id, license_text, license_approved, core_compatible, files, artifact_digest, artifact_type,
			artifact_size, archive_digest, source, locator, indexed_by, check_result, checked_at, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,NULLIF($19,''),$20,$21)
			RETURNING id, created_at`,
			k.ID, v.Version, in.Mode, v.FrontmatterName, v.FrontmatterDescription, v.LicenseKind, v.LicenseID, v.LicenseText,
			v.LicenseApproved, v.CoreCompatible, files, v.ArtifactDigest, v.ArtifactType, v.ArtifactSize, archive, src, loc,
			v.IndexedBy, v.CheckResult, v.CheckedAt, v.CreatedBy).Scan(&v.ID, &v.CreatedAt)
		if _, ok := uniqueViolation(err); ok {
			return conflict("version_exists", "version already exists and is immutable")
		}
		if err != nil {
			return err
		}
		v.SkillID, v.Mode = k.ID, in.Mode
		if err := tx.QueryRow(ctx, `UPDATE skills SET description = $2, updated_at = now() WHERE id = $1 RETURNING updated_at`,
			k.ID, v.FrontmatterDescription).Scan(&k.UpdatedAt); err != nil {
			return err
		}
		k.Description = v.FrontmatterDescription
		outSkill, outVer = k, v
		return nil
	})
	return outSkill, outVer, err
}

func jsonOrNil[T any](v *T) []byte {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}

const versionCols = `id, skill_id, version, mode, frontmatter_name, frontmatter_description, license_kind, license_id,
	license_text, license_approved, core_compatible, files, artifact_digest, artifact_type, artifact_size,
	COALESCE(archive_digest, ''), source, locator, indexed_by, COALESCE(check_result, ''), checked_at, yanked_at,
	yank_reason, created_by, created_at`

func scanVersion(row pgx.Row) (model.Version, error) {
	var v model.Version
	var files, src, loc []byte
	err := row.Scan(&v.ID, &v.SkillID, &v.Version, &v.Mode, &v.FrontmatterName, &v.FrontmatterDescription, &v.LicenseKind,
		&v.LicenseID, &v.LicenseText, &v.LicenseApproved, &v.CoreCompatible, &files, &v.ArtifactDigest, &v.ArtifactType,
		&v.ArtifactSize, &v.ArchiveDigest, &src, &loc, &v.IndexedBy, &v.CheckResult, &v.CheckedAt, &v.YankedAt,
		&v.YankReason, &v.CreatedBy, &v.CreatedAt)
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal(files, &v.Files); err != nil {
		return v, err
	}
	if src != nil {
		v.Source = new(model.Source)
		if err := json.Unmarshal(src, v.Source); err != nil {
			return v, err
		}
	}
	if loc != nil {
		v.Locator = new(model.Locator)
		if err := json.Unmarshal(loc, v.Locator); err != nil {
			return v, err
		}
	}
	return v, nil
}

// ListVersions implements store.Store (newest first).
func (s *Store) ListVersions(ctx context.Context, skillID int64) ([]model.Version, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+versionCols+` FROM skill_versions WHERE skill_id = $1 ORDER BY id DESC`, skillID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetVersion implements store.Store.
func (s *Store) GetVersion(ctx context.Context, skillID int64, version string) (model.Version, error) {
	v, err := scanVersion(s.pool.QueryRow(ctx, `SELECT `+versionCols+` FROM skill_versions WHERE skill_id = $1 AND version = $2`, skillID, version))
	return v, notFound(err)
}

// UpdateSkill implements store.Store.
func (s *Store) UpdateSkill(ctx context.Context, skillID int64, p model.SkillPatch, audit model.AuditEvent) (model.Skill, error) {
	var out model.Skill
	err := s.inTx(ctx, audit, func(tx pgx.Tx) error {
		// The upstream of a mirrored skill, read (and share-locked) before
		// the skill row: see mirrorVisibility.
		var upstreamID int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(n.upstream_id, 0) FROM skills s JOIN namespaces n ON n.id = s.namespace_id
			WHERE s.id = $1`, skillID).Scan(&upstreamID); err != nil {
			return notFound(err)
		}
		if p.Visibility != nil {
			v, err := mirrorVisibility(ctx, tx, upstreamID, *p.Visibility)
			if err != nil {
				return err
			}
			p.Visibility = &v
		}
		var mode string
		if err := tx.QueryRow(ctx, `SELECT mode FROM skills WHERE id = $1 FOR UPDATE`, skillID).Scan(&mode); err != nil {
			return notFound(err)
		}
		// Publishes lock the same row, so this sees every committed version.
		if p.Visibility != nil && *p.Visibility == model.VisibilityPublic && mode == "hosted" {
			var unknown bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM skill_versions WHERE skill_id = $1
				AND yanked_at IS NULL AND license_kind = 'unknown')`, skillID).Scan(&unknown); err != nil {
				return err
			}
			if unknown {
				return &model.PolicyError{Code: model.CodeLicenseRequired, Msg: model.MsgLicenseRequired}
			}
		}
		if err := rowsOrNotFound(tx.Exec(ctx, `UPDATE skills SET visibility = COALESCE($2, visibility), listed = COALESCE($3, listed),
			updated_at = now() WHERE id = $1`, skillID, p.Visibility, p.Listed)); err != nil {
			return err
		}
		var err error
		out, err = s.getSkillByID(ctx, tx, skillID)
		return err
	})
	return out, err
}

// SetSkillStatus implements store.Store.
func (s *Store) SetSkillStatus(ctx context.Context, skillID int64, status, reason string, audit model.AuditEvent) (model.Skill, error) {
	var out model.Skill
	err := s.inTx(ctx, audit, func(tx pgx.Tx) error {
		// The yanked state is terminal: the condition is evaluated on the
		// current row, never on a caller's earlier snapshot.
		tag, err := tx.Exec(ctx, `UPDATE skills SET status = $2, status_reason = $3, updated_at = now()
			WHERE id = $1 AND (status <> 'yanked' OR $2 = 'yanked')`, skillID, status, reason)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM skills WHERE id = $1)`, skillID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return model.ErrNotFound
			}
			return model.ErrSkillYanked()
		}
		out, err = s.getSkillByID(ctx, tx, skillID)
		return err
	})
	return out, err
}

// SetAlias implements store.Store.
func (s *Store) SetAlias(ctx context.Context, skillID int64, alias string, audit model.AuditEvent) error {
	var a *string
	if alias != "" {
		a = &alias
	}
	err := s.inTx(ctx, audit, func(tx pgx.Tx) error {
		return rowsOrNotFound(tx.Exec(ctx, `UPDATE skills SET global_alias = $2, updated_at = now() WHERE id = $1`, skillID, a))
	})
	if _, ok := uniqueViolation(err); ok {
		return conflict("alias_taken", "global alias is already assigned")
	}
	return err
}

func (s *Store) execVersionAudit(ctx context.Context, audit model.AuditEvent, sql string, args ...any) error {
	return s.inTx(ctx, audit, func(tx pgx.Tx) error { return rowsOrNotFound(tx.Exec(ctx, sql, args...)) })
}

func (s *Store) execVersion(ctx context.Context, sql string, args ...any) error {
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return model.ErrNotFound
	}
	return nil
}

// YankVersion implements store.Store.
func (s *Store) YankVersion(ctx context.Context, id int64, reason string, at time.Time, audit model.AuditEvent) error {
	return s.execVersionAudit(ctx, audit, `UPDATE skill_versions SET yanked_at = COALESCE(yanked_at, $2),
		yank_reason = CASE WHEN yanked_at IS NULL THEN $3 ELSE yank_reason END WHERE id = $1`, id, at, reason)
}

// ApproveLicense implements store.Store.
func (s *Store) ApproveLicense(ctx context.Context, id int64, audit model.AuditEvent) error {
	return s.execVersionAudit(ctx, audit, `UPDATE skill_versions SET license_approved = true WHERE id = $1`, id)
}

// SetVersionCheck implements store.Store.
func (s *Store) SetVersionCheck(ctx context.Context, id int64, result string, at time.Time) error {
	return s.execVersion(ctx, `UPDATE skill_versions SET check_result = $2, checked_at = $3 WHERE id = $1`, id, result, at)
}

// latestJoin selects the newest non-yanked version of each skill.
const latestJoin = `LEFT JOIN LATERAL (SELECT * FROM skill_versions v WHERE v.skill_id = s.id AND v.yanked_at IS NULL
	ORDER BY v.id DESC LIMIT 1) lv ON true`

// visibleSQL is model.Viewer.CanSee as a SQL condition over the visibility
// column col and the namespace name n.name; it is "" for admins, who see
// everything. arg binds a query parameter and returns its placeholder.
func visibleSQL(col string, v model.Viewer, arg func(any) string) string {
	if v.Admin {
		return ""
	}
	vis := col + " = 'public'"
	if v.Authenticated {
		vis += " OR " + col + " = 'internal'"
	}
	if len(v.Namespaces) > 0 {
		vis += " OR (" + col + " = 'private' AND n.name = ANY(" + arg(v.Namespaces) + "))"
	}
	return "(" + vis + ")"
}

// Search implements store.Store. Visibility is enforced in SQL with the same
// rules as model.Viewer.CanSee.
func (s *Store) Search(ctx context.Context, q model.SearchQuery) ([]model.SkillSummary, error) {
	where := []string{"s.listed", "s.status <> 'yanked'"}
	args := []any{}
	arg := func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) }
	if vis := visibleSQL("s.visibility", q.Viewer, arg); vis != "" {
		where = append(where, vis)
	}
	if q.Q != "" {
		p := arg("%" + likeEscape(strings.ToLower(q.Q)) + "%")
		where = append(where, "(lower(s.name) LIKE "+p+" OR lower(s.description) LIKE "+p+" OR n.name LIKE "+p+")")
	}
	if q.Namespace != "" {
		where = append(where, "n.name = "+arg(q.Namespace))
	}
	if q.Mode != "" {
		where = append(where, "s.mode = "+arg(q.Mode))
	}
	if q.Compatible {
		where = append(where, "lv.core_compatible IS TRUE")
	}
	if q.AfterNS != "" {
		where = append(where, "(n.name, s.name) > ("+arg(q.AfterNS)+", "+arg(q.AfterName)+")")
	}
	sql := `SELECT ` + skillCols + `, COALESCE(lv.version, '')` + skillFrom +
		latestJoin + ` WHERE ` + strings.Join(where, " AND ") + ` ORDER BY n.name, s.name`
	if q.Limit > 0 {
		sql += " LIMIT " + arg(q.Limit)
	}
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.SkillSummary
	for rows.Next() {
		var sum model.SkillSummary
		k, err := scanSkill(rows, &sum.LatestVersion)
		if err != nil {
			return nil, err
		}
		sum.Skill = k
		out = append(out, sum)
	}
	return out, rows.Err()
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// IndexCandidates implements store.Store.
func (s *Store) IndexCandidates(ctx context.Context, namespace string, aliasOnly bool) ([]model.IndexCandidate, error) {
	rows, err := s.pool.Query(ctx, `SELECT s.id, v.version FROM skills s JOIN namespaces n ON n.id = s.namespace_id
		JOIN LATERAL (SELECT version FROM skill_versions v WHERE v.skill_id = s.id AND v.yanked_at IS NULL ORDER BY v.id DESC LIMIT 1) v ON true
		WHERE s.visibility = 'public' AND s.listed AND s.status = 'active'
		AND ($1 = '' OR n.name = $1) AND (NOT $2 OR s.global_alias IS NOT NULL)
		ORDER BY n.name, s.name`, namespace, aliasOnly)
	if err != nil {
		return nil, err
	}
	type key struct {
		id  int64
		ver string
	}
	var keys []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.id, &k.ver); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]model.IndexCandidate, 0, len(keys))
	for _, k := range keys {
		sk, err := s.getSkillByID(ctx, s.pool, k.id)
		if err != nil {
			return nil, err
		}
		v, err := s.GetVersion(ctx, k.id, k.ver)
		if err != nil {
			return nil, err
		}
		out = append(out, model.IndexCandidate{Skill: sk, Version: v})
	}
	return out, nil
}

// AppendAudit implements store.Store.
func (s *Store) AppendAudit(ctx context.Context, e model.AuditEvent) error {
	return insertAudit(ctx, s.pool, e)
}

// ReferencedArtifacts implements store.Store.
func (s *Store) ReferencedArtifacts(ctx context.Context, digests []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(digests) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT artifact_digest FROM skill_versions WHERE mode = 'hosted' AND artifact_digest = ANY($1)`, digests)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out[d] = true
	}
	return out, rows.Err()
}

// ListAudit implements store.Store (newest first).
func (s *Store) ListAudit(ctx context.Context, limit int) ([]model.AuditEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT id, at, actor, action, namespace, target, details FROM audit_events ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.AuditEvent
	for rows.Next() {
		var e model.AuditEvent
		var d []byte
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Action, &e.Namespace, &e.Target, &d); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(d, &e.Details)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListMirroredSkills implements store.Store.
func (s *Store) ListMirroredSkills(ctx context.Context, upstreamID int64, afterNS, afterName string, limit int) ([]model.Skill, error) {
	sql := `SELECT ` + skillCols + skillFrom + `WHERE n.upstream_id = $1
		AND (n.name COLLATE "C", s.name COLLATE "C") > ($2, $3) ORDER BY n.name COLLATE "C", s.name COLLATE "C"`
	args := []any{upstreamID, afterNS, afterName}
	if limit > 0 {
		sql += " LIMIT $4"
		args = append(args, limit)
	}
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Skill, error) { return scanSkill(row) })
}
