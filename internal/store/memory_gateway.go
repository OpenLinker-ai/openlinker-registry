package store

import (
	"context"
	"slices"
	"sort"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
)

type gatewayState struct {
	byConsumer map[string]model.Gateway
	keys       map[string]map[string]model.GatewayKey
	jtis       map[string]time.Time
}

func (m *Memory) gw() *gatewayState {
	if m.gateways == nil {
		m.gateways = &gatewayState{byConsumer: map[string]model.Gateway{}, keys: map[string]map[string]model.GatewayKey{}, jtis: map[string]time.Time{}}
	}
	return m.gateways
}

func (m *Memory) CreateGateway(_ context.Context, g model.Gateway, audit model.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tokens[g.TokenID]; !ok {
		return model.ErrNotFound
	}
	st := m.gw()
	if _, ok := st.byConsumer[g.Consumer]; ok {
		return conflict("gateway_exists", "gateway consumer already exists")
	}
	for _, other := range st.byConsumer {
		if other.TokenID == g.TokenID {
			return conflict("gateway_token_bound", "token is already a gateway")
		}
	}
	g.CreatedAt = time.Now().UTC()
	g.Identities = slices.Clone(g.Identities)
	st.byConsumer[g.Consumer] = g
	m.appendAudit(audit)
	return nil
}

func (m *Memory) ListGateways(context.Context) ([]model.Gateway, []model.GatewayKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.gw()
	var gs []model.Gateway
	var ks []model.GatewayKey
	for _, g := range st.byConsumer {
		g.Identities = slices.Clone(g.Identities)
		gs = append(gs, g)
		for _, k := range st.keys[g.Consumer] {
			ks = append(ks, k)
		}
	}
	sort.Slice(gs, func(i, j int) bool { return gs[i].Consumer < gs[j].Consumer })
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].Consumer != ks[j].Consumer {
			return ks[i].Consumer < ks[j].Consumer
		}
		return ks[i].KID < ks[j].KID
	})
	return gs, ks, nil
}

func (m *Memory) GatewayForToken(_ context.Context, tokenID string) (model.Gateway, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, g := range m.gw().byConsumer {
		if g.TokenID == tokenID {
			g.Identities = slices.Clone(g.Identities)
			return g, nil
		}
	}
	return model.Gateway{}, model.ErrNotFound
}

func (m *Memory) AddGatewayKey(_ context.Context, k model.GatewayKey, audit model.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.gw()
	if _, ok := st.byConsumer[k.Consumer]; !ok {
		return model.ErrNotFound
	}
	if st.keys[k.Consumer] == nil {
		st.keys[k.Consumer] = map[string]model.GatewayKey{}
	}
	if _, ok := st.keys[k.Consumer][k.KID]; ok {
		return conflict("gateway_key_exists", "key id already exists for this gateway")
	}
	k.CreatedAt, k.RevokedAt, k.PublicKey = time.Now().UTC(), nil, slices.Clone(k.PublicKey)
	st.keys[k.Consumer][k.KID] = k
	m.appendAudit(audit)
	return nil
}

func (m *Memory) RevokeGatewayKey(_ context.Context, consumer, kid string, at time.Time, audit model.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.gw().keys[consumer][kid]
	if !ok {
		return model.ErrNotFound
	}
	if k.RevokedAt == nil {
		k.RevokedAt = &at
		m.gateways.keys[consumer][kid] = k
	}
	m.appendAudit(audit)
	return nil
}

func (m *Memory) GetGatewayKey(_ context.Context, consumer, kid string) (model.GatewayKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.gw().keys[consumer][kid]
	if !ok {
		return model.GatewayKey{}, model.ErrNotFound
	}
	k.PublicKey = slices.Clone(k.PublicKey)
	return k, nil
}

func (m *Memory) ClaimAssertion(_ context.Context, consumer, jti string, expires, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.gw()
	for key, exp := range st.jtis {
		if exp.Before(now) {
			delete(st.jtis, key)
		}
	}
	key := consumer + "\x00" + jti
	if _, ok := st.jtis[key]; ok {
		return false, nil
	}
	st.jtis[key] = expires
	return true, nil
}

func (m *Memory) NamespaceForGitHubID(_ context.Context, githubID int64) (model.Namespace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ns := range m.ns {
		if ns.GitHubID != nil && *ns.GitHubID == githubID && ns.UpstreamID == 0 {
			return ns, nil
		}
	}
	return model.Namespace{}, model.ErrNotFound
}
