package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

const upstreamCols = `id, name, kind, base_url, enabled, priority, visibility, namespace_allow, namespace_deny,
	target_namespace, sync_interval_seconds, proxy_on_miss, latest_only, cursor, sync_state, sync_started_at,
	last_sync_at, last_sync_status, last_sync_error, last_sync_stats, created_at, updated_at, config_version`

func scanUpstream(row pgx.Row) (model.Upstream, error) {
	var u model.Upstream
	var seconds int64
	var stats []byte
	err := row.Scan(&u.ID, &u.Name, &u.Kind, &u.BaseURL, &u.Enabled, &u.Priority, &u.Visibility, &u.NamespaceAllow,
		&u.NamespaceDeny, &u.TargetNamespace, &seconds, &u.ProxyOnMiss, &u.LatestOnly, &u.Cursor, &u.SyncState,
		&u.SyncStartedAt, &u.LastSyncAt, &u.LastSyncStatus, &u.LastSyncError, &stats, &u.CreatedAt, &u.UpdatedAt, &u.ConfigVersion)
	if err != nil {
		return u, err
	}
	u.SyncInterval = time.Duration(seconds) * time.Second
	return u, json.Unmarshal(stats, &u.LastSyncStats)
}

// textArray never encodes as NULL: the list columns are NOT NULL, and in a
// patch an empty list must clear the column rather than keep it.
func textArray(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// CreateUpstream implements store.Store. Sync state always starts empty.
func (s *Store) CreateUpstream(ctx context.Context, u model.Upstream, audit model.AuditEvent) (model.Upstream, error) {
	var out model.Upstream
	err := s.inTx(ctx, audit, func(tx pgx.Tx) error {
		var err error
		out, err = scanUpstream(tx.QueryRow(ctx, `INSERT INTO upstreams (name, kind, base_url, enabled, priority, visibility,
			namespace_allow, namespace_deny, target_namespace, sync_interval_seconds, proxy_on_miss, latest_only)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING `+upstreamCols,
			u.Name, u.Kind, u.BaseURL, u.Enabled, u.Priority, u.Visibility, textArray(u.NamespaceAllow), textArray(u.NamespaceDeny),
			u.TargetNamespace, int64(u.SyncInterval/time.Second), u.ProxyOnMiss, u.LatestOnly))
		return err
	})
	if _, ok := uniqueViolation(err); ok {
		return model.Upstream{}, conflict("upstream_exists", "upstream already exists")
	}
	if err != nil {
		return model.Upstream{}, err
	}
	return out, nil
}

// GetUpstream implements store.Store.
func (s *Store) GetUpstream(ctx context.Context, name string) (model.Upstream, error) {
	u, err := scanUpstream(s.pool.QueryRow(ctx, `SELECT `+upstreamCols+` FROM upstreams WHERE name = $1`, name))
	if err != nil {
		return model.Upstream{}, notFound(err)
	}
	return u, nil
}

// ListUpstreams implements store.Store. Names sort in byte order, like the
// in-memory store, whatever the database collation.
func (s *Store) ListUpstreams(ctx context.Context) ([]model.Upstream, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+upstreamCols+` FROM upstreams ORDER BY priority, name COLLATE "C"`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Upstream, error) { return scanUpstream(row) })
}

// UpdateUpstream implements store.Store.
func (s *Store) UpdateUpstream(ctx context.Context, name string, p model.UpstreamPatch, audit model.AuditEvent) (model.Upstream, error) {
	var allow, deny any
	if p.NamespaceAllow != nil {
		allow = textArray(*p.NamespaceAllow)
	}
	if p.NamespaceDeny != nil {
		deny = textArray(*p.NamespaceDeny)
	}
	var seconds *int64
	if p.SyncInterval != nil {
		n := int64(*p.SyncInterval / time.Second)
		seconds = &n
	}
	var out model.Upstream
	err := s.inTx(ctx, audit, func(tx pgx.Tx) error {
		// Every SET expression reads the row as it was. A cursor describes
		// one selection of one registry: any change to what is selected or
		// how it is shown starts over.
		var err error
		out, err = scanUpstream(tx.QueryRow(ctx, `UPDATE upstreams SET
			cursor = CASE WHEN ($2::text IS NOT NULL AND $2 <> base_url) OR ($5::text IS NOT NULL AND $5 <> visibility)
				OR ($6::text[] IS NOT NULL AND $6 <> namespace_allow) OR ($7::text[] IS NOT NULL AND $7 <> namespace_deny)
				OR ($10::boolean IS NOT NULL AND $10 <> latest_only) THEN '' ELSE cursor END,
			config_version = config_version + CASE WHEN ($2::text IS NOT NULL AND $2 <> base_url)
				OR ($5::text IS NOT NULL AND $5 <> visibility) OR ($6::text[] IS NOT NULL AND $6 <> namespace_allow)
				OR ($7::text[] IS NOT NULL AND $7 <> namespace_deny) OR ($10::boolean IS NOT NULL AND $10 <> latest_only)
				THEN 1 ELSE 0 END,
			base_url = COALESCE($2, base_url), enabled = COALESCE($3, enabled), priority = COALESCE($4, priority),
			visibility = COALESCE($5, visibility), namespace_allow = COALESCE($6, namespace_allow),
			namespace_deny = COALESCE($7, namespace_deny), sync_interval_seconds = COALESCE($8, sync_interval_seconds),
			proxy_on_miss = COALESCE($9, proxy_on_miss), latest_only = COALESCE($10, latest_only), updated_at = now()
			WHERE name = $1 RETURNING `+upstreamCols,
			name, p.BaseURL, p.Enabled, p.Priority, p.Visibility, allow, deny, seconds, p.ProxyOnMiss, p.LatestOnly))
		if err != nil {
			return notFound(err)
		}
		if out.Visibility != model.VisibilityInternal {
			return nil
		}
		// Lowering visibility takes effect at once for what is already here.
		const inUpstream = `namespace_id IN (SELECT id FROM namespaces WHERE upstream_id = $1)`
		if _, err := tx.Exec(ctx, `UPDATE skills SET visibility = 'internal', updated_at = now()
			WHERE visibility = 'public' AND `+inUpstream, out.ID); err != nil {
			return err
		}
		// Servers before their versions: the order every other writer takes
		// these rows in.
		rows, err := tx.Query(ctx, `UPDATE mcp_servers SET visibility = 'internal', updated_at = now()
			WHERE visibility = 'public' AND `+inUpstream+` RETURNING id`, out.ID)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil || len(ids) == 0 {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE mcp_server_versions SET updated_at = now() WHERE server_id = ANY($1)`, ids)
		return err
	})
	if err != nil {
		return model.Upstream{}, err
	}
	return out, nil
}

// DeleteUpstream implements store.Store. The foreign key from namespaces
// decides "in use", so a namespace created concurrently cannot be orphaned.
func (s *Store) DeleteUpstream(ctx context.Context, name string, audit model.AuditEvent) error {
	err := s.inTx(ctx, audit, func(tx pgx.Tx) error {
		return rowsOrNotFound(tx.Exec(ctx, `DELETE FROM upstreams WHERE name = $1`, name))
	})
	if foreignKeyViolation(err) {
		return conflict("upstream_in_use", "upstream still manages namespaces")
	}
	return err
}

// ClaimUpstreamSync implements store.Store. The condition is evaluated on
// the locked row, so of any number of concurrent claimers exactly one wins.
func (s *Store) ClaimUpstreamSync(ctx context.Context, id int64, now, staleBefore time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE upstreams SET sync_state = 'running', sync_started_at = $2
		WHERE id = $1 AND (sync_state = 'idle' OR sync_started_at IS NULL OR sync_started_at < $3)`, id, now, staleBefore)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM upstreams WHERE id = $1)`, id).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, model.ErrNotFound
	}
	return false, nil
}

// FinishUpstreamSync implements store.Store.
func (s *Store) FinishUpstreamSync(ctx context.Context, id int64, res model.UpstreamSyncResult, audit model.AuditEvent) error {
	stats := res.Stats
	if stats == nil {
		stats = map[string]int{}
	}
	b, err := json.Marshal(stats)
	if err != nil {
		return err
	}
	return s.inTx(ctx, audit, func(tx pgx.Tx) error {
		return rowsOrNotFound(tx.Exec(ctx, `UPDATE upstreams SET sync_state = 'idle', sync_started_at = NULL, last_sync_at = $2,
			last_sync_status = $3, last_sync_error = $4, last_sync_stats = $5,
			cursor = CASE WHEN $3 = 'ok' AND config_version = $7 THEN $6 ELSE cursor END WHERE id = $1`,
			id, res.At, res.Status, store.SyncError(res.Error), b, res.Cursor, res.ConfigVersion))
	})
}

// ensureUpstreamNS returns the namespace the upstream writes into, creating
// it when missing. Concurrent creators both succeed: the loser of the insert
// re-reads the winner's row.
// The audit event is written only when this call creates the namespace.
func ensureUpstreamNS(ctx context.Context, tx pgx.Tx, name string, upstreamID int64, audit model.AuditEvent) (model.Namespace, error) {
	if !skill.ValidNamespaceName(name) {
		return model.Namespace{}, store.ErrNamespaceSyntax()
	}
	sel := `SELECT ` + nsCols + ` FROM namespaces WHERE name = $1`
	ns, err := scanNamespace(tx.QueryRow(ctx, sel, name))
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return model.Namespace{}, err
	}
	if found && upstreamID != 0 && ns.UpstreamID == upstreamID {
		return ns, nil // the foreign key proves the upstream exists
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM upstreams WHERE id = $1)`, upstreamID).Scan(&exists); err != nil {
		return model.Namespace{}, err
	}
	if !exists {
		return model.Namespace{}, model.ErrNotFound
	}
	if found {
		return model.Namespace{}, model.ErrNamespaceManaged()
	}
	tag, err := tx.Exec(ctx, `INSERT INTO namespaces (name, kind, upstream_id) VALUES ($1, $2, $3) ON CONFLICT (name) DO NOTHING`,
		name, model.NSKindUpstream, upstreamID)
	if foreignKeyViolation(err) {
		return model.Namespace{}, model.ErrNotFound // the upstream was deleted meanwhile
	}
	if err != nil {
		return model.Namespace{}, err
	}
	if tag.RowsAffected() == 1 {
		if err := insertAudit(ctx, tx, audit); err != nil {
			return model.Namespace{}, err
		}
	}
	if ns, err = scanNamespace(tx.QueryRow(ctx, sel, name)); err != nil {
		return model.Namespace{}, err
	}
	if ns.UpstreamID != upstreamID {
		return model.Namespace{}, model.ErrNamespaceManaged()
	}
	return ns, nil
}

// EnsureUpstreamNamespace implements store.Store.
func (s *Store) EnsureUpstreamNamespace(ctx context.Context, name string, upstreamID int64, audit model.AuditEvent) (model.Namespace, error) {
	var ns model.Namespace
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := contextGuard(ctx, tx); err != nil {
			return err
		}
		var err error
		ns, err = ensureUpstreamNS(ctx, tx, name, upstreamID, audit)
		return err
	})
	return ns, err
}
