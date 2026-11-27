package pgstore

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
)

func managedWrite(m model.Managed, p *model.ProjectionTarget) error {
	switch {
	case p == nil && m.IsManaged():
		return model.ErrManagedByCore()
	case p != nil && !m.IsManaged():
		return model.ErrProjectionNameConflict()
	case p != nil && (m.By != p.Consumer || m.Owner != p.Owner):
		return model.ErrManagedByCore()
	}
	return nil
}

func (s *Store) ProjectionNamespace(ctx context.Context, owner string, candidates []string, create bool, audit model.AuditEvent) (model.Namespace, error) {
	var out model.Namespace
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		ns, err := scanNamespace(tx.QueryRow(ctx, `SELECT `+nsCols+` FROM namespaces WHERE openlinker_subject = $1 OR generated_for = $1
			ORDER BY openlinker_subject IS NULL LIMIT 1`, owner))
		if err == nil {
			out = ns
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		for _, name := range candidates {
			var taken bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM namespaces WHERE name = $1)`, name).Scan(&taken); err != nil {
				return err
			}
			if taken {
				continue
			}
			if !create {
				out = model.Namespace{Name: name, Kind: "openlinker", GeneratedFor: owner}
				return nil
			}
			ns, err := scanNamespace(tx.QueryRow(ctx, `INSERT INTO namespaces (name, kind, generated_for) VALUES ($1, 'openlinker', $2) RETURNING `+nsCols, name, owner))
			if err != nil {
				return err
			}
			out = ns
			return insertAudit(ctx, tx, audit)
		}
		return model.ErrProjectionNamespace()
	})
	return out, err
}

func priorResult(ctx context.Context, tx pgx.Tx, consumer, op string) (*model.ProjectionResult, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT result FROM projection_ops WHERE consumer = $1 AND op = $2`, consumer, op).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var res model.ProjectionResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func recordResult(ctx context.Context, tx pgx.Tx, consumer, op string, res model.ProjectionResult) error {
	raw, _ := json.Marshal(res)
	_, err := tx.Exec(ctx, `INSERT INTO projection_ops (consumer, op, result) VALUES ($1, $2, $3)`, consumer, op, raw)
	return err
}

func newer(p model.Projection, m model.Managed) bool {
	return p.Revision > m.Revision || (p.Revision == m.Revision && p.Phase > m.Phase)
}

func legacyOrNil(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	return raw
}

func (s *Store) ApplySkillProjection(ctx context.Context, p model.Projection, audit model.AuditEvent) (model.ProjectionResult, error) {
	var out model.ProjectionResult
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if prior, err := priorResult(ctx, tx, p.Target.Consumer, p.Op); err != nil || prior != nil {
			if prior != nil {
				out = *prior
			}
			return err
		}
		var nsID int64
		if err := tx.QueryRow(ctx, `SELECT id FROM namespaces WHERE name = $1`, p.Namespace).Scan(&nsID); err != nil {
			return notFound(err)
		}
		k, err := scanSkill(tx.QueryRow(ctx, `SELECT `+skillCols+skillFrom+`WHERE s.namespace_id = $1 AND s.name = $2 FOR UPDATE OF s`, nsID, p.Name))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if !p.Hidden && len(p.Versions) > 0 {
				return model.ErrProjectionVersionMissing()
			}
			if _, err := tx.Exec(ctx, `INSERT INTO skills (namespace_id, name, visibility, listed, mode, managed_by, managed_owner,
				managed_revision, managed_phase, projection_hidden, legacy) VALUES ($1, $2, 'public', false, 'hosted', $3, $4, $5, $6, true, $7)`,
				nsID, p.Name, p.Target.Consumer, p.Target.Owner, p.Revision, p.Phase, legacyOrNil(p.Legacy)); err != nil {
				return err
			}
			out = model.ProjectionResult{Applied: true}
		case err != nil:
			return err
		default:
			if err := managedWrite(k.Managed, &p.Target); err != nil {
				return err
			}
			if !newer(p, k.Managed) {
				out = model.ProjectionResult{Applied: false}
				break
			}
			var found int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM skill_versions WHERE skill_id = $1 AND version = ANY($2)`, k.ID, p.Versions).Scan(&found); err != nil {
				return err
			}
			if found != len(p.Versions) {
				return model.ErrProjectionVersionMissing()
			}
			hidden := p.Hidden || len(p.Versions) == 0
			if _, err := tx.Exec(ctx, `UPDATE skills SET projection_hidden = $2, listed = $3, description = CASE WHEN $4 = '' THEN description ELSE $4 END,
				current_version = $5, legacy = COALESCE($6, legacy), managed_revision = $7, managed_phase = $8, updated_at = now() WHERE id = $1`,
				k.ID, hidden, p.Listed, p.Description, p.Current, legacyOrNil(p.Legacy), p.Revision, p.Phase); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE skill_versions SET projection_hidden = NOT (version = ANY($2)) WHERE skill_id = $1`, k.ID, p.Versions); err != nil {
				return err
			}
			out = model.ProjectionResult{Applied: true}
		}
		out.Namespace = p.Namespace
		if err := recordResult(ctx, tx, p.Target.Consumer, p.Op, out); err != nil {
			return err
		}
		return insertAudit(ctx, tx, audit)
	})
	return out, err
}

func (s *Store) ApplyMCPProjection(ctx context.Context, p model.Projection, audit model.AuditEvent) (model.ProjectionResult, error) {
	var out model.ProjectionResult
	full := p.Namespace + "/" + p.Name
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if prior, err := priorResult(ctx, tx, p.Target.Consumer, p.Op); err != nil || prior != nil {
			if prior != nil {
				out = *prior
			}
			return err
		}
		var nsID int64
		if err := tx.QueryRow(ctx, `SELECT id FROM namespaces WHERE name = $1`, p.Namespace).Scan(&nsID); err != nil {
			return notFound(err)
		}
		var srv model.MCPServer
		var srvNS int64
		err := tx.QueryRow(ctx, `SELECT `+mcpServerCols+`, m.namespace_id`+mcpServerFrom+`WHERE lower(m.name) = lower($1) FOR UPDATE OF m`, full).
			Scan(append(mcpServerDest(&srv), &srvNS)...)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if !p.Hidden {
				return model.ErrProjectionVersionMissing()
			}
			if _, err := tx.Exec(ctx, `INSERT INTO mcp_servers (namespace_id, name, mode, visibility, listed, created_by, managed_by, managed_owner,
				managed_revision, managed_phase, projection_hidden, legacy) VALUES ($1, $2, 'registered', 'public', false, $3, $3, $4, $5, $6, true, $7)`,
				nsID, full, p.Target.Consumer, p.Target.Owner, p.Revision, p.Phase, legacyOrNil(p.Legacy)); err != nil {
				return err
			}
			out = model.ProjectionResult{Applied: true}
		case err != nil:
			return err
		default:
			if srv.Name != full || srvNS != nsID {
				return model.ErrProjectionNameConflict()
			}
			if err := managedWrite(srv.Managed, &p.Target); err != nil {
				return err
			}
			if !newer(p, srv.Managed) {
				out = model.ProjectionResult{Applied: false}
				break
			}
			if _, err := tx.Exec(ctx, `UPDATE mcp_servers SET projection_hidden = $2, listed = $3, legacy = COALESCE($4, legacy),
				managed_revision = $5, managed_phase = $6, updated_at = now() WHERE id = $1`,
				srv.ID, p.Hidden, p.Listed, legacyOrNil(p.Legacy), p.Revision, p.Phase); err != nil {
				return err
			}
			if err := stampMCPVersions(ctx, tx, "mcp", srv.ID); err != nil {
				return err
			}
			out = model.ProjectionResult{Applied: true}
		}
		out.Namespace = p.Namespace
		if err := recordResult(ctx, tx, p.Target.Consumer, p.Op, out); err != nil {
			return err
		}
		return insertAudit(ctx, tx, audit)
	})
	return out, err
}
