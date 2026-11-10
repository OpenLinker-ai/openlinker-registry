package store

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
)

// Memory is an in-memory Store with the same semantics as the Postgres store.
type Memory struct {
	mu       sync.Mutex
	nextID   int64
	ns       map[string]model.Namespace
	roles    map[string]map[string]model.Role // subject -> namespace -> role
	tokens   map[string]model.Token
	skills   map[int64]model.Skill
	versions map[int64]model.Version
	audit    []model.AuditEvent
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		ns: map[string]model.Namespace{}, roles: map[string]map[string]model.Role{},
		tokens: map[string]model.Token{}, skills: map[int64]model.Skill{}, versions: map[int64]model.Version{},
	}
}

var _ Store = (*Memory)(nil)

func (m *Memory) id() int64 { m.nextID++; return m.nextID }

func conflict(code, msg string) error { return &model.ConflictError{Code: code, Msg: msg} }

// Ping implements Store.
func (m *Memory) Ping(context.Context) error { return nil }

// CreateNamespace implements Store.
func (m *Memory) CreateNamespace(_ context.Context, ns model.Namespace, audit model.AuditEvent) (model.Namespace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.ns[ns.Name]; ok {
		return model.Namespace{}, conflict("namespace_exists", "namespace already exists")
	}
	if ns.GitHubID != nil {
		for _, other := range m.ns {
			if other.GitHubID != nil && *other.GitHubID == *ns.GitHubID {
				return model.Namespace{}, conflict("github_id_bound", "GitHub id is already bound to a namespace")
			}
		}
	}
	ns.ID, ns.CreatedAt = m.id(), time.Now().UTC()
	m.ns[ns.Name] = ns
	m.appendAudit(audit)
	return ns, nil
}

// GetNamespace implements Store.
func (m *Memory) GetNamespace(_ context.Context, name string) (model.Namespace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ns, ok := m.ns[name]
	if !ok {
		return model.Namespace{}, model.ErrNotFound
	}
	return ns, nil
}

// SetRole implements Store.
func (m *Memory) SetRole(_ context.Context, namespace, subject string, role model.Role, audit model.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.ns[namespace]; !ok {
		return model.ErrNotFound
	}
	if m.roles[subject] == nil {
		m.roles[subject] = map[string]model.Role{}
	}
	m.roles[subject][namespace] = role
	m.appendAudit(audit)
	return nil
}

// RolesForSubject implements Store.
func (m *Memory) RolesForSubject(_ context.Context, subject string) (map[string]model.Role, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]model.Role{}
	for k, v := range m.roles[subject] {
		out[k] = v
	}
	return out, nil
}

// CreateToken implements Store.
func (m *Memory) CreateToken(_ context.Context, t model.Token, roles []model.RoleBinding, audit model.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tokens[t.ID]; ok {
		return conflict("token_exists", "token id collision")
	}
	for _, rb := range roles {
		if _, ok := m.ns[rb.Namespace]; !ok {
			return model.ErrNotFound
		}
	}
	t.CreatedAt = time.Now().UTC()
	t.Scopes = slices.Clone(t.Scopes)
	m.tokens[t.ID] = t
	subject := "token:" + t.ID
	for _, rb := range roles {
		if m.roles[subject] == nil {
			m.roles[subject] = map[string]model.Role{}
		}
		m.roles[subject][rb.Namespace] = rb.Role
	}
	m.appendAudit(audit)
	return nil
}

// GetToken implements Store.
func (m *Memory) GetToken(_ context.Context, id string) (model.Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[id]
	if !ok {
		return model.Token{}, model.ErrNotFound
	}
	return t, nil
}

// RevokeToken implements Store.
func (m *Memory) RevokeToken(_ context.Context, id string, at time.Time, audit model.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[id]
	if !ok {
		return model.ErrNotFound
	}
	if t.RevokedAt == nil {
		t.RevokedAt = &at
		m.tokens[id] = t
	}
	m.appendAudit(audit)
	return nil
}

func (m *Memory) findSkill(namespace, name string) (model.Skill, bool) {
	for _, s := range m.skills {
		if s.Namespace == namespace && s.Name == name {
			return s, true
		}
	}
	return model.Skill{}, false
}

// GetSkill implements Store.
func (m *Memory) GetSkill(_ context.Context, namespace, name string) (model.Skill, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.findSkill(namespace, name)
	if !ok {
		return model.Skill{}, model.ErrNotFound
	}
	return s, nil
}

// PublishVersion implements Store.
func (m *Memory) PublishVersion(_ context.Context, in model.PublishInput, audit model.AuditEvent) (model.Skill, model.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.ns[in.Namespace]; !ok {
		return model.Skill{}, model.Version{}, model.ErrNotFound
	}
	now := time.Now().UTC()
	s, ok := m.findSkill(in.Namespace, in.Name)
	if ok {
		if s.Mode != in.Mode {
			return model.Skill{}, model.Version{}, conflict("mode_mismatch", "skill exists with a different mode")
		}
		if s.SourceKey != in.SourceKey {
			return model.Skill{}, model.Version{}, conflict("source_mismatch", "indexed skill tracks a different source")
		}
		if s.Status == model.StatusYanked {
			return model.Skill{}, model.Version{}, conflict("skill_yanked", "skill is yanked")
		}
		for _, v := range m.versions {
			if v.SkillID == s.ID && v.Version == in.Version.Version {
				return model.Skill{}, model.Version{}, conflict("version_exists", "version already exists and is immutable")
			}
		}
	} else {
		if in.SourceKey != "" {
			for _, other := range m.skills {
				if other.SourceKey == in.SourceKey {
					return model.Skill{}, model.Version{}, conflict("source_already_indexed", "this source is already indexed on this registry")
				}
			}
		}
		s = model.Skill{ID: m.id(), Namespace: in.Namespace, Name: in.Name, Visibility: in.Visibility, Listed: in.Listed,
			Status: model.StatusActive, Mode: in.Mode, SourceKey: in.SourceKey, CreatedAt: now}
	}
	v := in.Version
	v.ID, v.SkillID, v.CreatedAt, v.Mode = m.id(), s.ID, now, in.Mode
	v.Files = slices.Clone(v.Files)
	m.versions[v.ID] = v
	s.Description, s.UpdatedAt = v.FrontmatterDescription, now
	m.skills[s.ID] = s
	m.appendAudit(audit)
	return s, v, nil
}

// ListVersions implements Store (newest first).
func (m *Memory) ListVersions(_ context.Context, skillID int64) ([]model.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.versionsOf(skillID), nil
}

func (m *Memory) versionsOf(skillID int64) []model.Version {
	var out []model.Version
	for _, v := range m.versions {
		if v.SkillID == skillID {
			out = append(out, v)
		}
	}
	slices.SortFunc(out, func(a, b model.Version) int { return int(b.ID - a.ID) })
	return out
}

func (m *Memory) latest(skillID int64) (model.Version, bool) {
	for _, v := range m.versionsOf(skillID) {
		if v.YankedAt == nil {
			return v, true
		}
	}
	return model.Version{}, false
}

// GetVersion implements Store.
func (m *Memory) GetVersion(_ context.Context, skillID int64, version string) (model.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.versions {
		if v.SkillID == skillID && v.Version == version {
			return v, nil
		}
	}
	return model.Version{}, model.ErrNotFound
}

// UpdateSkill implements Store.
func (m *Memory) UpdateSkill(_ context.Context, skillID int64, p model.SkillPatch, audit model.AuditEvent) (model.Skill, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.skills[skillID]
	if !ok {
		return model.Skill{}, model.ErrNotFound
	}
	if p.Visibility != nil {
		s.Visibility = *p.Visibility
	}
	if p.Listed != nil {
		s.Listed = *p.Listed
	}
	s.UpdatedAt = time.Now().UTC()
	m.skills[skillID] = s
	m.appendAudit(audit)
	return s, nil
}

// SetSkillStatus implements Store.
func (m *Memory) SetSkillStatus(_ context.Context, skillID int64, status, reason string, audit model.AuditEvent) (model.Skill, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.skills[skillID]
	if !ok {
		return model.Skill{}, model.ErrNotFound
	}
	s.Status, s.StatusReason, s.UpdatedAt = status, reason, time.Now().UTC()
	m.skills[skillID] = s
	m.appendAudit(audit)
	return s, nil
}

// SetAlias implements Store.
func (m *Memory) SetAlias(_ context.Context, skillID int64, alias string, audit model.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.skills[skillID]
	if !ok {
		return model.ErrNotFound
	}
	if alias != "" {
		for _, other := range m.skills {
			if other.ID != skillID && other.GlobalAlias == alias {
				return conflict("alias_taken", "global alias is already assigned")
			}
		}
	}
	s.GlobalAlias = alias
	m.skills[skillID] = s
	m.appendAudit(audit)
	return nil
}

func (m *Memory) mutateVersion(id int64, audit *model.AuditEvent, f func(*model.Version)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.versions[id]
	if !ok {
		return model.ErrNotFound
	}
	f(&v)
	m.versions[id] = v
	if audit != nil {
		m.appendAudit(*audit)
	}
	return nil
}

// YankVersion implements Store.
func (m *Memory) YankVersion(_ context.Context, id int64, reason string, at time.Time, audit model.AuditEvent) error {
	return m.mutateVersion(id, &audit, func(v *model.Version) {
		if v.YankedAt == nil {
			v.YankedAt, v.YankReason = &at, reason
		}
	})
}

// ApproveLicense implements Store.
func (m *Memory) ApproveLicense(_ context.Context, id int64, audit model.AuditEvent) error {
	return m.mutateVersion(id, &audit, func(v *model.Version) { v.LicenseApproved = true })
}

// SetVersionCheck implements Store.
func (m *Memory) SetVersionCheck(_ context.Context, id int64, result string, at time.Time) error {
	return m.mutateVersion(id, nil, func(v *model.Version) { v.CheckResult, v.CheckedAt = result, &at })
}

// Search implements Store.
func (m *Memory) Search(_ context.Context, q model.SearchQuery) ([]model.SkillSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	needle := strings.ToLower(q.Q)
	var out []model.SkillSummary
	for _, s := range m.skills {
		if !s.Listed || s.Status == model.StatusYanked || !q.Viewer.CanSee(s.Namespace, s.Visibility) {
			continue
		}
		if q.Namespace != "" && s.Namespace != q.Namespace || q.Mode != "" && s.Mode != q.Mode {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(s.Name), needle) &&
			!strings.Contains(strings.ToLower(s.Description), needle) && !strings.Contains(s.Namespace, needle) {
			continue
		}
		lv, hasLatest := m.latest(s.ID)
		if q.Compatible && (!hasLatest || !lv.CoreCompatible) {
			continue
		}
		if q.AfterNS != "" && (s.Namespace < q.AfterNS || s.Namespace == q.AfterNS && s.Name <= q.AfterName) {
			continue
		}
		out = append(out, model.SkillSummary{Skill: s, LatestVersion: lv.Version})
	}
	slices.SortFunc(out, func(a, b model.SkillSummary) int {
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// IndexCandidates implements Store.
func (m *Memory) IndexCandidates(_ context.Context, namespace string, aliasOnly bool) ([]model.IndexCandidate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.IndexCandidate
	for _, s := range m.skills {
		if s.Visibility != model.VisibilityPublic || !s.Listed || s.Status != model.StatusActive {
			continue
		}
		if namespace != "" && s.Namespace != namespace || aliasOnly && s.GlobalAlias == "" {
			continue
		}
		if v, ok := m.latest(s.ID); ok {
			out = append(out, model.IndexCandidate{Skill: s, Version: v})
		}
	}
	slices.SortFunc(out, func(a, b model.IndexCandidate) int {
		if c := strings.Compare(a.Skill.Namespace, b.Skill.Namespace); c != 0 {
			return c
		}
		return strings.Compare(a.Skill.Name, b.Skill.Name)
	})
	return out, nil
}

func (m *Memory) appendAudit(e model.AuditEvent) {
	e.ID, e.At = m.id(), time.Now().UTC()
	m.audit = append(m.audit, e)
}

// AppendAudit implements Store.
func (m *Memory) AppendAudit(_ context.Context, e model.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appendAudit(e)
	return nil
}

// ReferencedArtifacts implements Store.
func (m *Memory) ReferencedArtifacts(_ context.Context, digests []string) (map[string]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[string]bool{}
	for _, d := range digests {
		want[d] = true
	}
	out := map[string]bool{}
	for _, v := range m.versions {
		if v.Mode == "hosted" && want[v.ArtifactDigest] {
			out[v.ArtifactDigest] = true
		}
	}
	return out, nil
}

// ListAudit implements Store (newest first).
func (m *Memory) ListAudit(_ context.Context, limit int) ([]model.AuditEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := slices.Clone(m.audit)
	slices.Reverse(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
