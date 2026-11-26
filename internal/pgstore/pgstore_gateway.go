package pgstore

import (
	"context"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/jackc/pgx/v5"
)

func (s *Store) CreateGateway(ctx context.Context, g model.Gateway, audit model.AuditEvent) error {
	return s.inTx(ctx, audit, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO gateways (consumer, token_id, identities) VALUES ($1, $2, $3)`, g.Consumer, g.TokenID, g.Identities)
		if name, ok := uniqueViolation(err); ok {
			if name == "gateways_pkey" {
				return conflict("gateway_exists", "gateway consumer already exists")
			}
			return conflict("gateway_token_bound", "token is already a gateway")
		}
		if foreignKeyViolation(err) {
			return model.ErrNotFound
		}
		return err
	})
}

func (s *Store) ListGateways(ctx context.Context) ([]model.Gateway, []model.GatewayKey, error) {
	rows, err := s.pool.Query(ctx, `SELECT consumer, token_id, identities, created_at FROM gateways ORDER BY consumer`)
	if err != nil {
		return nil, nil, err
	}
	gs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Gateway, error) {
		var g model.Gateway
		err := row.Scan(&g.Consumer, &g.TokenID, &g.Identities, &g.CreatedAt)
		return g, err
	})
	if err != nil {
		return nil, nil, err
	}
	rows, err = s.pool.Query(ctx, `SELECT consumer, kid, public_key, created_at, revoked_at FROM gateway_keys ORDER BY consumer, kid`)
	if err != nil {
		return nil, nil, err
	}
	ks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.GatewayKey, error) {
		var k model.GatewayKey
		err := row.Scan(&k.Consumer, &k.KID, &k.PublicKey, &k.CreatedAt, &k.RevokedAt)
		return k, err
	})
	return gs, ks, err
}

func (s *Store) GatewayForToken(ctx context.Context, tokenID string) (model.Gateway, error) {
	var g model.Gateway
	err := s.pool.QueryRow(ctx, `SELECT consumer, token_id, identities, created_at FROM gateways WHERE token_id = $1`, tokenID).
		Scan(&g.Consumer, &g.TokenID, &g.Identities, &g.CreatedAt)
	return g, notFound(err)
}

func (s *Store) AddGatewayKey(ctx context.Context, k model.GatewayKey, audit model.AuditEvent) error {
	return s.inTx(ctx, audit, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO gateway_keys (consumer, kid, public_key) VALUES ($1, $2, $3)`, k.Consumer, k.KID, k.PublicKey)
		if _, ok := uniqueViolation(err); ok {
			return conflict("gateway_key_exists", "key id already exists for this gateway")
		}
		if foreignKeyViolation(err) {
			return model.ErrNotFound
		}
		return err
	})
}

func (s *Store) RevokeGatewayKey(ctx context.Context, consumer, kid string, at time.Time, audit model.AuditEvent) error {
	return s.inTx(ctx, audit, func(tx pgx.Tx) error {
		return rowsOrNotFound(tx.Exec(ctx, `UPDATE gateway_keys SET revoked_at = COALESCE(revoked_at, $3) WHERE consumer = $1 AND kid = $2`, consumer, kid, at))
	})
}

func (s *Store) GetGatewayKey(ctx context.Context, consumer, kid string) (model.GatewayKey, error) {
	var k model.GatewayKey
	err := s.pool.QueryRow(ctx, `SELECT consumer, kid, public_key, created_at, revoked_at FROM gateway_keys WHERE consumer = $1 AND kid = $2`, consumer, kid).
		Scan(&k.Consumer, &k.KID, &k.PublicKey, &k.CreatedAt, &k.RevokedAt)
	return k, notFound(err)
}

func (s *Store) ClaimAssertion(ctx context.Context, consumer, jti string, expires, now time.Time) (bool, error) {
	if _, err := s.pool.Exec(ctx, `DELETE FROM gateway_assertion_jtis WHERE ctid IN (SELECT ctid FROM gateway_assertion_jtis WHERE expires_at < $1 LIMIT 500)`, now); err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO gateway_assertion_jtis (consumer, jti, expires_at) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, consumer, jti, expires)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) NamespaceForGitHubID(ctx context.Context, githubID int64) (model.Namespace, error) {
	ns, err := scanNamespace(s.pool.QueryRow(ctx, `SELECT `+nsCols+` FROM namespaces WHERE github_id = $1 AND upstream_id IS NULL`, githubID))
	return ns, notFound(err)
}
