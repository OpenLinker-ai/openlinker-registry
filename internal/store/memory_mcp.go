package store

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
)

// LatestMCPVersion picks which of a server's versions carries IsLatest once a
// status change invalidated the flag. It replays the publish rule over the
// non-deleted versions in publication order (published_at, then id): a
// version replaces the pick unless it compares lower. So the highest semantic
// version wins, and among versions that do not compare the newest does. ok is
// false when every version is deleted. Package pgstore uses it too, so both
// stores select the same row.
func LatestMCPVersion(versions []model.MCPVersion) (id int64, ok bool) {
	vs := slices.Clone(versions)
	slices.SortFunc(vs, func(a, b model.MCPVersion) int {
		if c := a.PublishedAt.Compare(b.PublishedAt); c != 0 {
			return c
		}
		return int(a.ID - b.ID)
	})
	var best model.MCPVersion
	for _, v := range vs {
		if v.Status == model.MCPStatusDeleted {
			continue
		}
		if !ok || model.CompareVersions(v.Version, best.Version) >= 0 {
			best, ok = v, true
		}
	}
	return best.ID, ok
}

func errServerName() error {
	return conflict("server_name_conflict", "another server already uses this name")
}

// mcpOut fills the read-only Upstream field of a server leaving the store.
func (m *Memory) mcpOut(s model.MCPServer) model.MCPServer {
	s.Upstream = m.upstreamName(s.Namespace)
	return s
}

// findMCP looks a server up ignoring letter case: names differing only by
// case are one name.
func (m *Memory) findMCP(name string) (model.MCPServer, bool) {
	lower := strings.ToLower(name)
	for _, s := range m.mcp {
		if strings.ToLower(s.Name) == lower {
			return s, true
		}
	}
	return model.MCPServer{}, false
}

func (m *Memory) mcpVersionsOf(serverID int64) []model.MCPVersion {
	var out []model.MCPVersion
	for _, v := range m.mcpVers {
		if v.ServerID == serverID {
			out = append(out, v)
		}
	}
	return out
}

func (m *Memory) findMCPVersion(serverID int64, version string) (model.MCPVersion, bool) {
	for _, v := range m.mcpVers {
		if v.ServerID == serverID && v.Version == version {
			return v, true
		}
	}
	return model.MCPVersion{}, false
}

func (m *Memory) latestMCP(serverID int64) (model.MCPVersion, bool) {
	for _, v := range m.mcpVers {
		if v.ServerID == serverID && v.IsLatest {
			return v, true
		}
	}
	return model.MCPVersion{}, false
}

// moveMCPLatest makes latestID (0 for none) the only version of the server
// flagged IsLatest, stamping the rows whose flag flipped: readers that sync
// incrementally by UpdatedAt must see both sides of the move.
func (m *Memory) moveMCPLatest(serverID, latestID int64, at time.Time) {
	for _, v := range m.mcpVersionsOf(serverID) {
		if want := v.ID == latestID; v.IsLatest != want {
			v.IsLatest, v.UpdatedAt = want, at
			m.mcpVers[v.ID] = v
		}
	}
}

// PublishMCPVersion implements Store.
func (m *Memory) PublishMCPVersion(_ context.Context, in model.MCPPublishInput, audit model.AuditEvent) (model.MCPServer, model.MCPVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fail := func(err error) (model.MCPServer, model.MCPVersion, error) {
		return model.MCPServer{}, model.MCPVersion{}, err
	}
	ns, ok := m.ns[in.Namespace]
	if !ok {
		return fail(model.ErrNotFound)
	}
	if ns.UpstreamID != 0 {
		return fail(model.ErrNamespaceManaged())
	}
	now := micro(time.Now())
	s, ok := m.findMCP(in.Name)
	if ok {
		switch {
		case s.Name != in.Name:
			return fail(errServerName())
		case s.Mode != in.Mode:
			return fail(conflict("mode_mismatch", "server exists with a different mode"))
		case s.Namespace != in.Namespace:
			return fail(errServerName())
		}
		if _, dup := m.findMCPVersion(s.ID, in.Version.Version); dup {
			return fail(conflict("version_exists", "version already exists and is immutable"))
		}
	} else {
		s = model.MCPServer{ID: m.id(), Namespace: in.Namespace, Name: in.Name, Mode: in.Mode, Visibility: in.Visibility,
			Listed: in.Listed, CreatedBy: in.Version.CreatedBy, CreatedAt: now}
	}
	at := now
	if !in.Version.PublishedAt.IsZero() {
		at = micro(in.Version.PublishedAt)
	}
	v := model.MCPVersion{ID: m.id(), ServerID: s.ID, Version: in.Version.Version, ServerJSON: slices.Clone(in.Version.ServerJSON),
		Digest: in.Version.Digest, Status: model.MCPStatusActive, StatusChangedAt: at, PublishedAt: at, UpdatedAt: at,
		CreatedBy: in.Version.CreatedBy}
	cur, hasLatest := m.latestMCP(s.ID)
	v.IsLatest = !hasLatest || model.CompareVersions(v.Version, cur.Version) >= 0
	m.mcpVers[v.ID] = v
	if v.IsLatest {
		m.moveMCPLatest(s.ID, v.ID, now)
	}
	s.UpdatedAt = now
	m.mcp[s.ID] = s
	m.appendAudit(audit)
	return m.mcpOut(s), v, nil
}

// UpsertMirroredMCP implements Store. Nothing is written unless every check
// passes, like the single transaction of the Postgres store.
func (m *Memory) UpsertMirroredMCP(_ context.Context, in model.MCPMirrorInput, audit model.AuditEvent) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ns, createNS, err := m.upstreamNS(in.Namespace, in.UpstreamID)
	if err != nil {
		return "", err
	}
	s, found := m.findMCP(in.Name)
	if found && (s.Name != in.Name || s.Mode != model.MCPModeMirrored || s.Namespace != in.Namespace) {
		return "", errServerName()
	}
	now := micro(time.Now())
	orNow := func(t time.Time) time.Time {
		if t.IsZero() {
			return now
		}
		return micro(t)
	}
	status, message := in.Version.Status, in.Version.StatusMessage
	if status == model.MCPStatusActive {
		message = ""
	}
	latest := in.Version.IsLatest && status != model.MCPStatusDeleted

	// The upstream's visibility and listing apply to an existing server too,
	// together with the version.
	settings := found && (s.Visibility != in.Visibility || s.Listed != in.Listed)
	applySettings := func() {
		s.Visibility, s.Listed, s.UpdatedAt = in.Visibility, in.Listed, now
		m.mcp[s.ID] = s
		m.touchMCPVersions(s.ID, now)
	}
	if cur, exists := m.findMCPVersion(s.ID, in.Version.Version); found && exists {
		if cur.Digest != in.Version.Digest {
			return "", conflict("digest_mismatch", "the stored version has a different server.json")
		}
		if cur.Status == status && cur.StatusMessage == message && cur.IsLatest == latest {
			if !settings {
				return model.MirrorUnchanged, nil
			}
			applySettings()
			m.appendAudit(audit)
			return model.MirrorUpdated, nil
		}
		if settings {
			applySettings()
		}
		if latest {
			m.moveMCPLatest(s.ID, cur.ID, now)
		}
		cur = m.mcpVers[cur.ID]
		cur.Status, cur.StatusMessage, cur.IsLatest = status, message, latest
		cur.StatusChangedAt, cur.UpdatedAt = orNow(in.Version.StatusChangedAt), orNow(in.Version.UpdatedAt)
		m.mcpVers[cur.ID] = cur
		m.appendAudit(audit)
		return model.MirrorUpdated, nil
	}
	if settings {
		applySettings()
	}

	if createNS {
		m.createUpstreamNS(ns)
		m.appendAudit(model.AuditEvent{Actor: audit.Actor, Action: "namespace.mirror", Namespace: in.Namespace,
			Details: map[string]any{"upstream_id": in.UpstreamID}})
	}
	if !found {
		s = model.MCPServer{ID: m.id(), Namespace: in.Namespace, Name: in.Name, Mode: model.MCPModeMirrored,
			Visibility: in.Visibility, Listed: in.Listed, CreatedBy: in.Version.CreatedBy, CreatedAt: now}
	}
	v := model.MCPVersion{ID: m.id(), ServerID: s.ID, Version: in.Version.Version, ServerJSON: slices.Clone(in.Version.ServerJSON),
		Digest: in.Version.Digest, Status: status, StatusMessage: message, StatusChangedAt: orNow(in.Version.StatusChangedAt),
		PublishedAt: orNow(in.Version.PublishedAt), UpdatedAt: orNow(in.Version.UpdatedAt), IsLatest: latest,
		CreatedBy: in.Version.CreatedBy}
	m.mcpVers[v.ID] = v
	if latest {
		m.moveMCPLatest(s.ID, v.ID, now)
	}
	s.UpdatedAt = now
	m.mcp[s.ID] = s
	m.appendAudit(audit)
	return model.MirrorCreated, nil
}

// GetMCPServer implements Store.
func (m *Memory) GetMCPServer(_ context.Context, name string) (model.MCPServer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.findMCP(name)
	if !ok || s.Name != name {
		return model.MCPServer{}, model.ErrNotFound
	}
	return m.mcpOut(s), nil
}

// GetMCPVersion implements Store.
func (m *Memory) GetMCPVersion(_ context.Context, serverID int64, version string) (model.MCPVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.findMCPVersion(serverID, version)
	if version == "latest" {
		v, ok = m.latestMCP(serverID)
	}
	if !ok {
		return model.MCPVersion{}, model.ErrNotFound
	}
	return v, nil
}

// ListMCPVersions implements Store (newest first).
func (m *Memory) ListMCPVersions(_ context.Context, serverID int64) ([]model.MCPVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.mcpVersionsOf(serverID)
	slices.SortFunc(out, func(a, b model.MCPVersion) int {
		if c := b.PublishedAt.Compare(a.PublishedAt); c != 0 {
			return c
		}
		return int(b.ID - a.ID)
	})
	return out, nil
}

// SetMCPStatus implements Store.
func (m *Memory) SetMCPStatus(_ context.Context, serverID int64, version, status, message string, at time.Time, audit model.AuditEvent) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.mcp[serverID]; !ok {
		return 0, model.ErrNotFound
	}
	targets := m.mcpVersionsOf(serverID)
	if version != "" {
		targets = slices.DeleteFunc(targets, func(v model.MCPVersion) bool { return v.Version != version })
		if len(targets) == 0 {
			return 0, model.ErrNotFound
		}
	}
	if status == model.MCPStatusActive {
		message = ""
	}
	at = micro(at)
	changed := 0
	for _, v := range targets {
		if v.Status == status && v.StatusMessage == message {
			continue
		}
		v.Status, v.StatusMessage, v.StatusChangedAt, v.UpdatedAt = status, message, at, at
		m.mcpVers[v.ID] = v
		changed++
	}
	latest, _ := LatestMCPVersion(m.mcpVersionsOf(serverID))
	m.moveMCPLatest(serverID, latest, at)
	m.appendAudit(audit)
	return changed, nil
}

// UpdateMCPServer implements Store.
func (m *Memory) UpdateMCPServer(_ context.Context, serverID int64, p model.MCPPatch, audit model.AuditEvent) (model.MCPServer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.mcp[serverID]
	if !ok {
		return model.MCPServer{}, model.ErrNotFound
	}
	now := micro(time.Now())
	changed := (p.Visibility != nil && *p.Visibility != s.Visibility) || (p.Listed != nil && *p.Listed != s.Listed)
	if p.Visibility != nil {
		s.Visibility = *p.Visibility
	}
	if p.Listed != nil {
		s.Listed = *p.Listed
	}
	s.UpdatedAt = now
	m.mcp[serverID] = s
	if changed {
		m.touchMCPVersions(serverID, now)
	}
	m.appendAudit(audit)
	return m.mcpOut(s), nil
}

// jsonText is the search text of a top-level server.json member: the string
// value, the JSON text of another value, "" when the member is missing.
func jsonText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// MCPSearchText returns the title and description a server.json is searched
// by. Package pgstore stores them in their own columns, so both stores match
// the same text.
func MCPSearchText(serverJSON json.RawMessage) (title, description string) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(serverJSON, &doc) != nil {
		return "", ""
	}
	return jsonText(doc["title"]), jsonText(doc["description"])
}

// mcpMatches reports whether the lowercase needle occurs in the server name,
// title or description.
func mcpMatches(name string, serverJSON json.RawMessage, needle string) bool {
	if strings.Contains(strings.ToLower(name), needle) {
		return true
	}
	title, description := MCPSearchText(serverJSON)
	return strings.Contains(strings.ToLower(title), needle) || strings.Contains(strings.ToLower(description), needle)
}

// SearchMCP implements Store.
func (m *Memory) SearchMCP(_ context.Context, q model.MCPQuery) ([]model.MCPEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	needle := strings.ToLower(q.Search)
	var out []model.MCPEntry
	for _, v := range m.mcpVers {
		s := m.mcp[v.ServerID]
		if !s.Listed || !q.Viewer.CanSee(s.Namespace, s.Visibility) {
			continue
		}
		if q.Namespace != "" && s.Namespace != q.Namespace || q.Mode != "" && s.Mode != q.Mode {
			continue
		}
		if !q.IncludeDeleted && v.Status == model.MCPStatusDeleted {
			continue
		}
		if q.Version == "latest" && !v.IsLatest || q.Version != "" && q.Version != "latest" && v.Version != q.Version {
			continue
		}
		if q.UpdatedSince != nil && v.UpdatedAt.Before(micro(*q.UpdatedSince)) {
			continue
		}
		if q.AfterName != "" && (s.Name < q.AfterName || s.Name == q.AfterName && v.Version <= q.AfterVersion) {
			continue
		}
		if needle != "" && !mcpMatches(s.Name, v.ServerJSON, needle) {
			continue
		}
		out = append(out, model.MCPEntry{Server: m.mcpOut(s), Version: v})
	}
	slices.SortFunc(out, func(a, b model.MCPEntry) int {
		if c := strings.Compare(a.Server.Name, b.Server.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Version.Version, b.Version.Version)
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// touchMCPVersions stamps every version of a server. A change to who may see
// the server changes what each version's listing entry says, and readers
// that sync incrementally by updated_at must see it.
func (m *Memory) touchMCPVersions(serverID int64, at time.Time) {
	for id, v := range m.mcpVers {
		if v.ServerID == serverID {
			v.UpdatedAt = at
			m.mcpVers[id] = v
		}
	}
}

// ListMirroredMCP implements Store.
func (m *Memory) ListMirroredMCP(_ context.Context, upstreamID int64, afterName string, limit int) ([]model.MCPServer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.MCPServer
	for _, s := range m.mcp {
		if upstreamID == 0 || m.ns[s.Namespace].UpstreamID != upstreamID || s.Name <= afterName {
			continue
		}
		out = append(out, m.mcpOut(s))
	}
	slices.SortFunc(out, func(a, b model.MCPServer) int { return strings.Compare(a.Name, b.Name) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
