package pgstore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
)

type reviewFields struct {
	state    string
	reason   string
	revision int64
}

func nextReview(ctx context.Context, tx pgx.Tx, table string, id, nsID int64, created bool, current string, rule model.ReviewRule) (reviewFields, error) {
	next := rule.Next(current)
	if created {
		var trusted bool
		if err := tx.QueryRow(ctx, `SELECT review_trusted FROM namespaces WHERE id = $1`, nsID).Scan(&trusted); err != nil {
			return reviewFields{}, err
		}
		next = rule.Initial(trusted)
	}
	var out reviewFields
	err := tx.QueryRow(ctx, `UPDATE `+table+` SET review_state = $2,
		review_reason = CASE WHEN review_state = $2 THEN review_reason ELSE '' END,
		review_revision = review_revision + 1 WHERE id = $1
		RETURNING review_state, review_reason, review_revision`, id, next).Scan(&out.state, &out.reason, &out.revision)
	return out, err
}

func (s *Store) ClaimNamespace(ctx context.Context, ns model.Namespace, owner string, audit model.AuditEvent) (model.Namespace, bool, error) {
	var out model.Namespace
	created := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		existing, err := scanNamespace(tx.QueryRow(ctx, `SELECT `+nsCols+` FROM namespaces WHERE name = $1 FOR UPDATE`, ns.Name))
		if err == nil {
			if !sameOwner(existing, ns) {
				return conflict("namespace_exists", "namespace already exists")
			}
			out = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var subject *string
		if ns.OpenLinkerSubject != "" {
			subject = &ns.OpenLinkerSubject
		}
		err = tx.QueryRow(ctx, `INSERT INTO namespaces (name, kind, github_id, display_name, openlinker_subject)
			VALUES ($1, $2, $3, $4, $5) RETURNING `+nsCols, ns.Name, ns.Kind, ns.GitHubID, ns.DisplayName, subject).
			Scan(&out.ID, &out.Name, &out.Kind, &out.GitHubID, &out.DisplayName, &out.UpstreamID, &out.OpenLinkerSubject,
				&out.ReviewTrusted, &out.CreatedAt)
		if c, ok := uniqueViolation(err); ok {
			switch {
			case strings.Contains(c, "openlinker_subject"):
				return conflict("handle_already_chosen", "this account already has an ai.openlinker namespace")
			case strings.Contains(c, "github_id"):
				return conflict("github_id_bound", "GitHub id is already bound to a namespace")
			}
			return conflict("namespace_exists", "namespace already exists")
		}
		if err != nil {
			return err
		}
		if owner != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO namespace_roles (namespace_id, subject, role) VALUES ($1, $2, 'owner')`, out.ID, owner); err != nil {
				return err
			}
		}
		created = true
		return insertAudit(ctx, tx, audit)
	})
	return out, created, err
}

func sameOwner(existing, want model.Namespace) bool {
	if existing.Kind != want.Kind || existing.UpstreamID != 0 {
		return false
	}
	if want.OpenLinkerSubject != "" {
		return existing.OpenLinkerSubject == want.OpenLinkerSubject
	}
	return want.GitHubID != nil && existing.GitHubID != nil && *existing.GitHubID == *want.GitHubID
}

func (s *Store) SetNamespaceTrusted(ctx context.Context, name string, trusted bool, audit model.AuditEvent) error {
	return s.inTx(ctx, audit, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE namespaces SET review_trusted = $2 WHERE name = $1 AND upstream_id IS NULL`, name, trusted)
		if err == nil && tag.RowsAffected() == 0 {
			return model.ErrNotFound
		}
		return err
	})
}

func reviewTable(kind string) (string, error) {
	switch kind {
	case "skill":
		return "skills", nil
	case "mcp":
		return "mcp_servers", nil
	}
	return "", model.ErrNotFound
}

func (s *Store) ListReview(ctx context.Context, kind, state, afterKind string, afterID int64, limit int) ([]model.ReviewItem, error) {
	query := `SELECT * FROM (
		SELECT 'mcp' AS kind, m.id, n.name AS ns, m.name, m.mode, m.review_state, m.review_reason, m.review_revision, m.visibility,
			m.created_by, m.updated_at,
			(SELECT count(*) FROM reports r WHERE r.kind = 'mcp' AND r.entry_id = m.id AND r.state = 'open') AS open_reports
		FROM mcp_servers m JOIN namespaces n ON n.id = m.namespace_id WHERE m.review_state = $1 AND ($2 = '' OR $2 = 'mcp')
		UNION ALL
		SELECT 'skill', s.id, n.name, s.name, s.mode, s.review_state, s.review_reason, s.review_revision, s.visibility,
			COALESCE((SELECT created_by FROM skill_versions v WHERE v.skill_id = s.id ORDER BY v.id LIMIT 1), ''), s.updated_at,
			(SELECT count(*) FROM reports r WHERE r.kind = 'skill' AND r.entry_id = s.id AND r.state = 'open')
		FROM skills s JOIN namespaces n ON n.id = s.namespace_id WHERE s.review_state = $1 AND ($2 = '' OR $2 = 'skill')
	) q WHERE (q.kind, q.id) > ($3, $4) ORDER BY q.kind, q.id LIMIT $5`
	rows, err := s.pool.Query(ctx, query, state, kind, afterKind, afterID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.ReviewItem, error) {
		var it model.ReviewItem
		err := row.Scan(&it.Kind, &it.ID, &it.Namespace, &it.Name, &it.Mode, &it.State, &it.Reason, &it.Revision, &it.Visibility,
			&it.CreatedBy, &it.UpdatedAt, &it.OpenReports)
		return it, err
	})
}

func (s *Store) DecideReview(ctx context.Context, d model.ReviewDecision, audit model.AuditEvent) error {
	table, err := reviewTable(d.Kind)
	if err != nil {
		return err
	}
	return s.inTx(ctx, audit, func(tx pgx.Tx) error {
		var nsID, revision int64
		if err := tx.QueryRow(ctx, `SELECT namespace_id, review_revision FROM `+table+` WHERE id = $1 FOR UPDATE`, d.ID).Scan(&nsID, &revision); err != nil {
			return notFound(err)
		}
		if revision != d.Revision {
			return conflict("review_stale", "the entry changed since it was reviewed; review it again")
		}
		_, err := tx.Exec(ctx, `UPDATE `+table+` SET review_state = $2, review_reason = $3, updated_at = now() WHERE id = $1`, d.ID, d.State, d.Reason)
		if err != nil {
			return err
		}
		if err := stampMCPVersions(ctx, tx, d.Kind, d.ID); err != nil {
			return err
		}
		if d.TrustNamespace && d.State == model.ReviewApproved {
			_, err = tx.Exec(ctx, `UPDATE namespaces SET review_trusted = true WHERE id = $1 AND upstream_id IS NULL`, nsID)
		}
		return err
	})
}

const reportCols = `id, kind, entry_id, namespace, name, version, reporter, category, details, state, created_at, resolved_at, resolved_by`

func scanReport(row pgx.Row) (model.Report, error) {
	var r model.Report
	err := row.Scan(&r.ID, &r.Kind, &r.EntryID, &r.Namespace, &r.Name, &r.Version, &r.Reporter, &r.Category, &r.Details,
		&r.State, &r.CreatedAt, &r.ResolvedAt, &r.ResolvedBy)
	return r, err
}

func (s *Store) CreateReport(ctx context.Context, rep model.Report, threshold int, reopenNow bool, audit model.AuditEvent) (model.ReportResult, error) {
	table, err := reviewTable(rep.Kind)
	if err != nil {
		return model.ReportResult{}, err
	}
	var res model.ReportResult
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(ctx, `SELECT review_state FROM `+table+` WHERE id = $1 FOR UPDATE`, rep.EntryID).Scan(&state); err != nil {
			return notFound(err)
		}
		existing, err := scanReport(tx.QueryRow(ctx, `SELECT `+reportCols+` FROM reports
			WHERE kind = $1 AND entry_id = $2 AND reporter = $3 AND state = 'open'`, rep.Kind, rep.EntryID, rep.Reporter))
		if err == nil {
			res.Report = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		res.Report, err = scanReport(tx.QueryRow(ctx, `INSERT INTO reports (kind, entry_id, namespace, name, version, reporter, category, details)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+reportCols,
			rep.Kind, rep.EntryID, rep.Namespace, rep.Name, rep.Version, rep.Reporter, rep.Category, rep.Details))
		if err != nil {
			return err
		}
		res.Created = true
		if err := insertAudit(ctx, tx, audit); err != nil {
			return err
		}
		if state != model.ReviewApproved {
			return nil
		}
		var open int
		if err := tx.QueryRow(ctx, `SELECT count(DISTINCT reporter) FROM reports WHERE kind = $1 AND entry_id = $2 AND state = 'open'`,
			rep.Kind, rep.EntryID).Scan(&open); err != nil {
			return err
		}
		if !reopenNow && (threshold <= 0 || open < threshold) {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE `+table+` SET review_state = 'pending', review_reason = 'reports',
			review_revision = review_revision + 1, updated_at = now() WHERE id = $1`, rep.EntryID); err != nil {
			return err
		}
		if err := stampMCPVersions(ctx, tx, rep.Kind, rep.EntryID); err != nil {
			return err
		}
		res.Reopened = true
		return insertAudit(ctx, tx, model.AuditEvent{Actor: audit.Actor, Action: "review.reopen", Namespace: rep.Namespace,
			Target: rep.Name, Details: map[string]any{"kind": rep.Kind, "open_reports": open}})
	})
	return res, err
}

func (s *Store) ListReports(ctx context.Context, state string, afterID int64, limit int) ([]model.Report, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+reportCols+` FROM reports WHERE ($1 = '' OR state = $1) AND id > $2 ORDER BY id LIMIT $3`,
		state, afterID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Report, error) { return scanReport(row) })
}

func (s *Store) ResolveReport(ctx context.Context, id int64, state, by string, at time.Time, audit model.AuditEvent) (model.Report, error) {
	var out model.Report
	err := s.inTx(ctx, audit, func(tx pgx.Tx) error {
		r, err := scanReport(tx.QueryRow(ctx, `UPDATE reports SET state = $2, resolved_by = $3, resolved_at = $4
			WHERE id = $1 AND state = 'open' RETURNING `+reportCols, id, state, by, at))
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if qerr := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM reports WHERE id = $1)`, id).Scan(&exists); qerr != nil {
				return qerr
			}
			if exists {
				return conflict("report_resolved", "the report is already resolved")
			}
			return model.ErrNotFound
		}
		out = r
		return err
	})
	return out, err
}

func (s *Store) MoveSkill(ctx context.Context, skillID int64, toNamespace, toName string, expectedRevision int64, audit model.AuditEvent) (model.Skill, error) {
	var out model.Skill
	err := s.inTx(ctx, audit, func(tx pgx.Tx) error {
		k, err := scanSkill(tx.QueryRow(ctx, `SELECT `+skillCols+skillFrom+`WHERE s.id = $1 FOR UPDATE OF s`, skillID))
		if err != nil {
			return notFound(err)
		}
		if k.ReviewRevision != expectedRevision {
			return conflict("entry_changed", "the entry changed while the claim was checked; try again")
		}
		var nsID, nsUpstream int64
		if err := tx.QueryRow(ctx, `SELECT id, COALESCE(upstream_id, 0) FROM namespaces WHERE name = $1`, toNamespace).
			Scan(&nsID, &nsUpstream); err != nil {
			return notFound(err)
		}
		if nsUpstream != 0 || k.Upstream != "" {
			return model.ErrNamespaceManaged()
		}
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM skills WHERE namespace_id = $1 AND name = $2)
			OR EXISTS (SELECT 1 FROM skill_redirects WHERE from_namespace = $3 AND from_name = $2)`, nsID, toName, toNamespace).Scan(&taken); err != nil {
			return err
		}
		if taken {
			return conflict("claim_name_taken", "this name is already used in the target namespace")
		}
		if _, err := tx.Exec(ctx, `UPDATE skills SET namespace_id = $2, name = $3, claimed = true, updated_at = now() WHERE id = $1`,
			skillID, nsID, toName); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO skill_redirects (from_namespace, from_name, skill_id) VALUES ($1, $2, $3)`,
			k.Namespace, k.Name, skillID); err != nil {
			return err
		}
		out, err = scanSkill(tx.QueryRow(ctx, `SELECT `+skillCols+skillFrom+`WHERE s.id = $1`, skillID))
		return err
	})
	return out, err
}

func (s *Store) SkillRedirect(ctx context.Context, namespace, name string) (model.Skill, error) {
	k, err := scanSkill(s.pool.QueryRow(ctx, `SELECT `+skillCols+skillFrom+`JOIN skill_redirects r ON r.skill_id = s.id
		WHERE r.from_namespace = $1 AND r.from_name = $2`, namespace, name))
	return k, notFound(err)
}

func stampMCPVersions(ctx context.Context, tx pgx.Tx, kind string, serverID int64) error {
	if kind != "mcp" {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE mcp_server_versions SET updated_at = GREATEST(clock_timestamp(), updated_at + interval '1 microsecond') WHERE server_id = $1`, serverID)
	return err
}
