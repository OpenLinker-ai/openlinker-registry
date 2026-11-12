package store

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
)

// The helpers below are shared with package pgstore so both implementations
// answer with the same errors and stored values.

// ErrNamespaceSyntax is returned for a namespace name the schema refuses.
func ErrNamespaceSyntax() error {
	return &model.PolicyError{Code: "namespace_invalid", Msg: "namespace must be lowercase reverse-DNS labels, e.g. io.github.alice"}
}

// ErrNamespaceKind is returned when CreateNamespace is asked for an
// upstream-managed namespace.
func ErrNamespaceKind() error {
	return &model.PolicyError{Code: "namespace_invalid", Msg: "upstream namespaces are created by their upstream, not directly"}
}

// MaxSyncError bounds the stored error of a sync run, in bytes.
const MaxSyncError = 1000

// SyncError returns msg as stored in an upstream's last sync error: valid
// UTF-8 without NUL bytes (Postgres text accepts neither), cut on a rune
// boundary to at most MaxSyncError bytes.
func SyncError(msg string) string {
	msg = strings.ReplaceAll(strings.ToValidUTF8(msg, "�"), "\x00", "")
	if len(msg) <= MaxSyncError {
		return msg
	}
	cut := MaxSyncError
	for !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut]
}

// micro reduces a time to what Postgres stores (microseconds), so equality
// and ordering of stored times behave the same in both stores.
func micro(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func microPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := micro(*t)
	return &u
}

// copyUpstream detaches slices, maps and time pointers from the stored value.
func copyUpstream(u model.Upstream) model.Upstream {
	u.NamespaceAllow, u.NamespaceDeny = slices.Clone(u.NamespaceAllow), slices.Clone(u.NamespaceDeny)
	u.LastSyncStats = maps.Clone(u.LastSyncStats)
	u.SyncStartedAt, u.LastSyncAt = microPtr(u.SyncStartedAt), microPtr(u.LastSyncAt)
	return u
}

func (m *Memory) findUpstream(name string) (model.Upstream, bool) {
	for _, u := range m.upstreams {
		if u.Name == name {
			return u, true
		}
	}
	return model.Upstream{}, false
}

// upstreamName is the name of the upstream managing a namespace ("" when the
// namespace is local).
func (m *Memory) upstreamName(namespace string) string {
	return m.upstreams[m.ns[namespace].UpstreamID].Name
}

// CreateUpstream implements Store. Sync state always starts empty.
func (m *Memory) CreateUpstream(_ context.Context, u model.Upstream, audit model.AuditEvent) (model.Upstream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.findUpstream(u.Name); ok {
		return model.Upstream{}, conflict("upstream_exists", "upstream already exists")
	}
	now := micro(time.Now())
	u = model.Upstream{ID: m.id(), Name: u.Name, Kind: u.Kind, BaseURL: u.BaseURL, Enabled: u.Enabled, Priority: u.Priority,
		Visibility: u.Visibility, NamespaceAllow: slices.Clone(u.NamespaceAllow), NamespaceDeny: slices.Clone(u.NamespaceDeny),
		TargetNamespace: u.TargetNamespace, SyncInterval: u.SyncInterval.Truncate(time.Second), ProxyOnMiss: u.ProxyOnMiss,
		LatestOnly: u.LatestOnly, SyncState: model.SyncIdle, CreatedAt: now, UpdatedAt: now}
	m.upstreams[u.ID] = u
	m.appendAudit(audit)
	return copyUpstream(u), nil
}

// GetUpstream implements Store.
func (m *Memory) GetUpstream(_ context.Context, name string) (model.Upstream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.findUpstream(name)
	if !ok {
		return model.Upstream{}, model.ErrNotFound
	}
	return copyUpstream(u), nil
}

// ListUpstreams implements Store.
func (m *Memory) ListUpstreams(context.Context) ([]model.Upstream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.Upstream, 0, len(m.upstreams))
	for _, u := range m.upstreams {
		out = append(out, copyUpstream(u))
	}
	slices.SortFunc(out, func(a, b model.Upstream) int {
		if a.Priority != b.Priority {
			return a.Priority - b.Priority
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// UpdateUpstream implements Store.
func (m *Memory) UpdateUpstream(_ context.Context, name string, p model.UpstreamPatch, audit model.AuditEvent) (model.Upstream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.findUpstream(name)
	if !ok {
		return model.Upstream{}, model.ErrNotFound
	}
	if p.BaseURL != nil {
		// A cursor is only meaningful to the registry that issued it.
		if *p.BaseURL != u.BaseURL {
			u.Cursor = ""
		}
		u.BaseURL = *p.BaseURL
	}
	if p.Enabled != nil {
		u.Enabled = *p.Enabled
	}
	if p.Priority != nil {
		u.Priority = *p.Priority
	}
	if p.Visibility != nil {
		u.Visibility = *p.Visibility
	}
	if p.NamespaceAllow != nil {
		u.NamespaceAllow = slices.Clone(*p.NamespaceAllow)
	}
	if p.NamespaceDeny != nil {
		u.NamespaceDeny = slices.Clone(*p.NamespaceDeny)
	}
	if p.SyncInterval != nil {
		u.SyncInterval = p.SyncInterval.Truncate(time.Second)
	}
	if p.ProxyOnMiss != nil {
		u.ProxyOnMiss = *p.ProxyOnMiss
	}
	if p.LatestOnly != nil {
		u.LatestOnly = *p.LatestOnly
	}
	u.UpdatedAt = micro(time.Now())
	m.upstreams[u.ID] = u
	m.appendAudit(audit)
	return copyUpstream(u), nil
}

// DeleteUpstream implements Store.
func (m *Memory) DeleteUpstream(_ context.Context, name string, audit model.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.findUpstream(name)
	if !ok {
		return model.ErrNotFound
	}
	for _, ns := range m.ns {
		if ns.UpstreamID == u.ID {
			return conflict("upstream_in_use", "upstream still manages namespaces")
		}
	}
	delete(m.upstreams, u.ID)
	m.appendAudit(audit)
	return nil
}

// ClaimUpstreamSync implements Store.
func (m *Memory) ClaimUpstreamSync(_ context.Context, id int64, now, staleBefore time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.upstreams[id]
	if !ok {
		return false, model.ErrNotFound
	}
	if u.SyncState == model.SyncRunning && u.SyncStartedAt != nil && !u.SyncStartedAt.Before(micro(staleBefore)) {
		return false, nil
	}
	u.SyncState, u.SyncStartedAt = model.SyncRunning, microPtr(&now)
	m.upstreams[id] = u
	return true, nil
}

// FinishUpstreamSync implements Store.
func (m *Memory) FinishUpstreamSync(_ context.Context, id int64, res model.UpstreamSyncResult, audit model.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.upstreams[id]
	if !ok {
		return model.ErrNotFound
	}
	u.SyncState, u.SyncStartedAt, u.LastSyncAt = model.SyncIdle, nil, microPtr(&res.At)
	u.LastSyncStatus, u.LastSyncError, u.LastSyncStats = res.Status, SyncError(res.Error), maps.Clone(res.Stats)
	if res.Status == model.SyncOK {
		u.Cursor = res.Cursor
	}
	m.upstreams[id] = u
	m.appendAudit(audit)
	return nil
}

// upstreamNS resolves the namespace an upstream writes into without creating
// it. create is true when the namespace is missing: the caller inserts it
// with createUpstreamNS once every other check of its operation has passed.
func (m *Memory) upstreamNS(name string, upstreamID int64) (ns model.Namespace, create bool, err error) {
	if !skill.ValidNamespaceName(name) {
		return model.Namespace{}, false, ErrNamespaceSyntax()
	}
	if _, ok := m.upstreams[upstreamID]; !ok {
		return model.Namespace{}, false, model.ErrNotFound
	}
	ns, ok := m.ns[name]
	if !ok {
		return model.Namespace{Name: name, Kind: model.NSKindUpstream, UpstreamID: upstreamID}, true, nil
	}
	if ns.UpstreamID != upstreamID {
		return model.Namespace{}, false, model.ErrNamespaceManaged()
	}
	return ns, false, nil
}

func (m *Memory) createUpstreamNS(ns model.Namespace) model.Namespace {
	ns.ID, ns.CreatedAt = m.id(), time.Now().UTC()
	m.ns[ns.Name] = ns
	return ns
}

// EnsureUpstreamNamespace implements Store.
func (m *Memory) EnsureUpstreamNamespace(_ context.Context, name string, upstreamID int64, audit model.AuditEvent) (model.Namespace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ns, create, err := m.upstreamNS(name, upstreamID)
	if err != nil {
		return model.Namespace{}, err
	}
	if create {
		ns = m.createUpstreamNS(ns)
		m.appendAudit(audit)
	}
	return ns, nil
}
