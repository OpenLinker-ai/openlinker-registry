package pgstore

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

const mcpServerCols = `m.id, n.name, m.name, m.mode, m.visibility, m.listed, COALESCE(u.name, ''), m.created_by,
	m.created_at, m.updated_at`

// mcpServerFrom joins what mcpServerCols reads: the namespace and, for an
// upstream-managed namespace, its upstream.
const mcpServerFrom = ` FROM mcp_servers m JOIN namespaces n ON n.id = m.namespace_id
	LEFT JOIN upstreams u ON u.id = n.upstream_id `

const mcpVersionCols = `v.id, v.server_id, v.version, v.server_json, v.digest, v.status, v.status_message,
	v.status_changed_at, v.published_at, v.updated_at, v.is_latest, v.created_by`

func mcpServerDest(s *model.MCPServer) []any {
	return []any{&s.ID, &s.Namespace, &s.Name, &s.Mode, &s.Visibility, &s.Listed, &s.Upstream, &s.CreatedBy,
		&s.CreatedAt, &s.UpdatedAt}
}

func mcpVersionDest(v *model.MCPVersion) []any {
	return []any{&v.ID, &v.ServerID, &v.Version, (*[]byte)(&v.ServerJSON), &v.Digest, &v.Status, &v.StatusMessage,
		&v.StatusChangedAt, &v.PublishedAt, &v.UpdatedAt, &v.IsLatest, &v.CreatedBy}
}

func scanMCPServer(row pgx.Row) (model.MCPServer, error) {
	var s model.MCPServer
	err := row.Scan(mcpServerDest(&s)...)
	return s, err
}

func scanMCPVersion(row pgx.Row) (model.MCPVersion, error) {
	var v model.MCPVersion
	err := row.Scan(mcpVersionDest(&v)...)
	return v, err
}

func errServerName() error {
	return conflict("server_name_conflict", "another server already uses this name")
}

// timeOrNil encodes a zero time as NULL so the statement can default it.
func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// lockMCPServer returns the server holding this name in any letter case with
// its row locked, creating it from the given settings when there is none (a
// concurrent creator wins the insert and its row is returned). Every version
// write and IsLatest decision of a server happens under this lock. nsID is
// the namespace of the returned server, which the caller must compare with
// its own along with the exact name and mode.
func lockMCPServer(ctx context.Context, tx pgx.Tx, create model.MCPServer, createNS int64) (srv model.MCPServer, nsID int64, err error) {
	sel := `SELECT ` + mcpServerCols + `, m.namespace_id` + mcpServerFrom + `WHERE lower(m.name) = lower($1) FOR UPDATE OF m`
	dest := append(mcpServerDest(&srv), &nsID)
	err = tx.QueryRow(ctx, sel, create.Name).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err = tx.Exec(ctx, `INSERT INTO mcp_servers (namespace_id, name, mode, visibility, listed, created_by)
			VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (lower(name)) DO NOTHING`,
			createNS, create.Name, create.Mode, create.Visibility, create.Listed, create.CreatedBy); err != nil {
			return srv, 0, err
		}
		err = tx.QueryRow(ctx, sel, create.Name).Scan(dest...)
	}
	return srv, nsID, err
}

// clearMCPLatest drops IsLatest from every version of the server except
// keepID. The partial unique index allows one latest row per server, so this
// runs before another row takes the flag. The cleared rows are stamped
// because readers that sync incrementally by updated_at must see the flip.
func clearMCPLatest(ctx context.Context, tx pgx.Tx, serverID, keepID int64, at *time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE mcp_server_versions SET is_latest = false, updated_at = COALESCE($3, now())
		WHERE server_id = $1 AND is_latest AND id <> $2`, serverID, keepID, at)
	return err
}

// PublishMCPVersion implements store.Store.
func (s *Store) PublishMCPVersion(ctx context.Context, in model.MCPPublishInput, audit model.AuditEvent) (model.MCPServer, model.MCPVersion, error) {
	var srv model.MCPServer
	var ver model.MCPVersion
	err := s.inTx(ctx, audit, func(tx pgx.Tx) error {
		var nsID, nsUpstream int64
		if err := tx.QueryRow(ctx, `SELECT id, COALESCE(upstream_id, 0) FROM namespaces WHERE name = $1`, in.Namespace).
			Scan(&nsID, &nsUpstream); err != nil {
			return notFound(err)
		}
		if nsUpstream != 0 {
			return model.ErrNamespaceManaged()
		}
		var srvNS int64
		var err error
		srv, srvNS, err = lockMCPServer(ctx, tx, model.MCPServer{Name: in.Name, Mode: in.Mode, Visibility: in.Visibility,
			Listed: in.Listed, CreatedBy: in.Version.CreatedBy}, nsID)
		if err != nil {
			return err
		}
		switch {
		case srv.Name != in.Name:
			return errServerName()
		case srv.Mode != in.Mode:
			return conflict("mode_mismatch", "server exists with a different mode")
		case srvNS != nsID:
			return errServerName()
		}
		// Under the server lock: the new version takes the flag unless the
		// current latest is a higher semantic version.
		var curVersion string
		err = tx.QueryRow(ctx, `SELECT version FROM mcp_server_versions WHERE server_id = $1 AND is_latest`, srv.ID).Scan(&curVersion)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		latest := err != nil || model.CompareVersions(in.Version.Version, curVersion) >= 0
		if latest {
			if err := clearMCPLatest(ctx, tx, srv.ID, 0, nil); err != nil {
				return err
			}
		}
		ver, err = scanMCPVersion(tx.QueryRow(ctx, `INSERT INTO mcp_server_versions AS v (server_id, version, server_json, digest,
			status_changed_at, published_at, updated_at, is_latest, created_by)
			VALUES ($1, $2, $3, $4, COALESCE($5, now()), COALESCE($5, now()), COALESCE($5, now()), $6, $7)
			RETURNING `+mcpVersionCols,
			srv.ID, in.Version.Version, []byte(in.Version.ServerJSON), in.Version.Digest, timeOrNil(in.Version.PublishedAt),
			latest, in.Version.CreatedBy))
		if c, ok := uniqueViolation(err); ok && c != "mcp_server_versions_latest_key" {
			return conflict("version_exists", "version already exists and is immutable")
		}
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `UPDATE mcp_servers SET updated_at = now() WHERE id = $1 RETURNING updated_at`, srv.ID).Scan(&srv.UpdatedAt)
	})
	if err != nil {
		return model.MCPServer{}, model.MCPVersion{}, err
	}
	return srv, ver, nil
}

// UpsertMirroredMCP implements store.Store.
func (s *Store) UpsertMirroredMCP(ctx context.Context, in model.MCPMirrorInput) (string, error) {
	v := in.Version
	status, message := v.Status, v.StatusMessage
	if status == model.MCPStatusActive {
		message = ""
	}
	latest := v.IsLatest && status != model.MCPStatusDeleted
	outcome := model.MirrorUnchanged
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		ns, err := ensureUpstreamNS(ctx, tx, in.Namespace, in.UpstreamID)
		if err != nil {
			return err
		}
		srv, srvNS, err := lockMCPServer(ctx, tx, model.MCPServer{Name: in.Name, Mode: model.MCPModeMirrored,
			Visibility: in.Visibility, Listed: true, CreatedBy: v.CreatedBy}, ns.ID)
		if err != nil {
			return err
		}
		if srv.Name != in.Name || srv.Mode != model.MCPModeMirrored || srvNS != ns.ID {
			return errServerName()
		}
		var cur model.MCPVersion
		err = tx.QueryRow(ctx, `SELECT id, digest, status, status_message, is_latest FROM mcp_server_versions
			WHERE server_id = $1 AND version = $2`, srv.ID, v.Version).
			Scan(&cur.ID, &cur.Digest, &cur.Status, &cur.StatusMessage, &cur.IsLatest)
		missing := errors.Is(err, pgx.ErrNoRows)
		switch {
		case missing:
		case err != nil:
			return err
		case cur.Digest != v.Digest:
			return conflict("digest_mismatch", "the stored version has a different server.json")
		case cur.Status == status && cur.StatusMessage == message && cur.IsLatest == latest:
			return nil
		}
		if latest {
			if err := clearMCPLatest(ctx, tx, srv.ID, cur.ID, nil); err != nil {
				return err
			}
		}
		if !missing {
			outcome = model.MirrorUpdated
			_, err := tx.Exec(ctx, `UPDATE mcp_server_versions SET status = $2, status_message = $3, is_latest = $4,
				status_changed_at = COALESCE($5, now()), updated_at = COALESCE($6, now()) WHERE id = $1`,
				cur.ID, status, message, latest, timeOrNil(v.StatusChangedAt), timeOrNil(v.UpdatedAt))
			return err
		}
		outcome = model.MirrorCreated
		if _, err := tx.Exec(ctx, `INSERT INTO mcp_server_versions (server_id, version, server_json, digest, status,
			status_message, status_changed_at, published_at, updated_at, is_latest, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, now()), COALESCE($8, now()), COALESCE($9, now()), $10, $11)`,
			srv.ID, v.Version, []byte(v.ServerJSON), v.Digest, status, message, timeOrNil(v.StatusChangedAt),
			timeOrNil(v.PublishedAt), timeOrNil(v.UpdatedAt), latest, v.CreatedBy); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE mcp_servers SET updated_at = now() WHERE id = $1`, srv.ID)
		return err
	})
	if err != nil {
		return "", err
	}
	return outcome, nil
}

// GetMCPServer implements store.Store. The lower(name) condition only
// selects the unique index; the match itself is exact.
func (s *Store) GetMCPServer(ctx context.Context, name string) (model.MCPServer, error) {
	srv, err := scanMCPServer(s.pool.QueryRow(ctx, `SELECT `+mcpServerCols+mcpServerFrom+
		`WHERE lower(m.name) = lower($1) AND m.name = $1`, name))
	if err != nil {
		return model.MCPServer{}, notFound(err)
	}
	return srv, nil
}

// GetMCPVersion implements store.Store.
func (s *Store) GetMCPVersion(ctx context.Context, serverID int64, version string) (model.MCPVersion, error) {
	sql, args := `SELECT `+mcpVersionCols+` FROM mcp_server_versions v WHERE v.server_id = $1 AND v.version = $2`, []any{serverID, version}
	if version == "latest" {
		sql, args = `SELECT `+mcpVersionCols+` FROM mcp_server_versions v WHERE v.server_id = $1 AND v.is_latest`, args[:1]
	}
	v, err := scanMCPVersion(s.pool.QueryRow(ctx, sql, args...))
	if err != nil {
		return model.MCPVersion{}, notFound(err)
	}
	return v, nil
}

// ListMCPVersions implements store.Store (newest first).
func (s *Store) ListMCPVersions(ctx context.Context, serverID int64) ([]model.MCPVersion, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+mcpVersionCols+` FROM mcp_server_versions v WHERE v.server_id = $1
		ORDER BY v.published_at DESC, v.id DESC`, serverID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.MCPVersion, error) { return scanMCPVersion(row) })
}

// SetMCPStatus implements store.Store.
func (s *Store) SetMCPStatus(ctx context.Context, serverID int64, version, status, message string, at time.Time, audit model.AuditEvent) (int, error) {
	if status == model.MCPStatusActive {
		message = ""
	}
	var changed int
	err := s.inTx(ctx, audit, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT true FROM mcp_servers WHERE id = $1 FOR UPDATE`, serverID).Scan(&exists); err != nil {
			return notFound(err)
		}
		if version != "" {
			if err := tx.QueryRow(ctx, `SELECT true FROM mcp_server_versions WHERE server_id = $1 AND version = $2`,
				serverID, version).Scan(&exists); err != nil {
				return notFound(err)
			}
		}
		// A deleted version may not keep the flag (schema CHECK), so it
		// drops in the same statement; the flag is reassigned below.
		tag, err := tx.Exec(ctx, `UPDATE mcp_server_versions SET status = $3, status_message = $4, status_changed_at = $5,
			updated_at = $5, is_latest = is_latest AND $3 <> 'deleted'
			WHERE server_id = $1 AND ($2::text = '' OR version = $2) AND (status <> $3 OR status_message <> $4)`,
			serverID, version, status, message, at)
		if err != nil {
			return err
		}
		changed = int(tag.RowsAffected())

		rows, err := tx.Query(ctx, `SELECT id, version, published_at FROM mcp_server_versions
			WHERE server_id = $1 AND status <> 'deleted'`, serverID)
		if err != nil {
			return err
		}
		live, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.MCPVersion, error) {
			var v model.MCPVersion
			err := row.Scan(&v.ID, &v.Version, &v.PublishedAt)
			return v, err
		})
		if err != nil {
			return err
		}
		latest, ok := store.LatestMCPVersion(live)
		if err := clearMCPLatest(ctx, tx, serverID, latest, &at); err != nil {
			return err
		}
		if ok {
			_, err = tx.Exec(ctx, `UPDATE mcp_server_versions SET is_latest = true, updated_at = $2 WHERE id = $1 AND NOT is_latest`, latest, at)
		}
		return err
	})
	if err != nil {
		return 0, err
	}
	return changed, nil
}

// UpdateMCPServer implements store.Store.
func (s *Store) UpdateMCPServer(ctx context.Context, serverID int64, p model.MCPPatch, audit model.AuditEvent) (model.MCPServer, error) {
	var out model.MCPServer
	err := s.inTx(ctx, audit, func(tx pgx.Tx) error {
		if err := rowsOrNotFound(tx.Exec(ctx, `UPDATE mcp_servers SET visibility = COALESCE($2, visibility),
			listed = COALESCE($3, listed), updated_at = now() WHERE id = $1`, serverID, p.Visibility, p.Listed)); err != nil {
			return err
		}
		var err error
		out, err = scanMCPServer(tx.QueryRow(ctx, `SELECT `+mcpServerCols+mcpServerFrom+`WHERE m.id = $1`, serverID))
		return err
	})
	if err != nil {
		return model.MCPServer{}, err
	}
	return out, nil
}

// SearchMCP implements store.Store. Ordering and the cursor use the "C"
// collation: byte order, identical to the in-memory store and independent of
// the database locale.
func (s *Store) SearchMCP(ctx context.Context, q model.MCPQuery) ([]model.MCPEntry, error) {
	where := []string{"m.listed"}
	args := []any{}
	arg := func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) }
	if vis := visibleSQL("m.visibility", q.Viewer, arg); vis != "" {
		where = append(where, vis)
	}
	if q.Search != "" {
		p := arg("%" + likeEscape(strings.ToLower(q.Search)) + "%")
		where = append(where, "(lower(m.name) LIKE "+p+" OR lower(v.server_json->>'title') LIKE "+p+
			" OR lower(v.server_json->>'description') LIKE "+p+")")
	}
	if q.Namespace != "" {
		where = append(where, "n.name = "+arg(q.Namespace))
	}
	if q.Mode != "" {
		where = append(where, "m.mode = "+arg(q.Mode))
	}
	switch q.Version {
	case "":
	case "latest":
		where = append(where, "v.is_latest")
	default:
		where = append(where, "v.version = "+arg(q.Version))
	}
	if q.UpdatedSince != nil {
		where = append(where, "v.updated_at >= "+arg(*q.UpdatedSince))
	}
	if !q.IncludeDeleted {
		where = append(where, "v.status <> 'deleted'")
	}
	if q.AfterName != "" {
		where = append(where, `(m.name COLLATE "C", v.version COLLATE "C") > (`+arg(q.AfterName)+", "+arg(q.AfterVersion)+")")
	}
	sql := `SELECT ` + mcpServerCols + `, ` + mcpVersionCols + mcpServerFrom + `JOIN mcp_server_versions v ON v.server_id = m.id
		WHERE ` + strings.Join(where, " AND ") + ` ORDER BY m.name COLLATE "C", v.version COLLATE "C"`
	if q.Limit > 0 {
		sql += " LIMIT " + arg(q.Limit)
	}
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.MCPEntry, error) {
		var e model.MCPEntry
		err := row.Scan(append(mcpServerDest(&e.Server), mcpVersionDest(&e.Version)...)...)
		return e, err
	})
}
