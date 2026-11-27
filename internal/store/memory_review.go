package store

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
)

type reviewState struct {
	reports   map[int64]model.Report
	redirects map[string]int64
}

func (m *Memory) rv() *reviewState {
	if m.review == nil {
		m.review = &reviewState{reports: map[int64]model.Report{}, redirects: map[string]int64{}}
	}
	return m.review
}

func (m *Memory) ClaimNamespace(_ context.Context, ns model.Namespace, owner string, audit model.AuditEvent) (model.Namespace, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.ns[ns.Name]; ok {
		if existing.Kind != ns.Kind || existing.UpstreamID != 0 {
			return model.Namespace{}, false, conflict("namespace_exists", "namespace already exists")
		}
		same := ns.OpenLinkerSubject != "" && existing.OpenLinkerSubject == ns.OpenLinkerSubject ||
			ns.OpenLinkerSubject == "" && ns.GitHubID != nil && existing.GitHubID != nil && *existing.GitHubID == *ns.GitHubID
		if !same {
			return model.Namespace{}, false, conflict("namespace_exists", "namespace already exists")
		}
		return existing, false, nil
	}
	for _, other := range m.ns {
		if ns.OpenLinkerSubject != "" && other.OpenLinkerSubject == ns.OpenLinkerSubject {
			return model.Namespace{}, false, conflict("handle_already_chosen", "this account already has an ai.openlinker namespace")
		}
		if ns.GitHubID != nil && other.GitHubID != nil && *other.GitHubID == *ns.GitHubID {
			return model.Namespace{}, false, conflict("github_id_bound", "GitHub id is already bound to a namespace")
		}
	}
	ns.ID, ns.CreatedAt, ns.UpstreamID, ns.ReviewTrusted = m.id(), time.Now().UTC(), 0, false
	m.ns[ns.Name] = ns
	if owner != "" {
		if m.roles[owner] == nil {
			m.roles[owner] = map[string]model.Role{}
		}
		m.roles[owner][ns.Name] = model.RoleOwner
	}
	m.appendAudit(audit)
	return ns, true, nil
}

func (m *Memory) SetNamespaceTrusted(_ context.Context, name string, trusted bool, audit model.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ns, ok := m.ns[name]
	if !ok || ns.UpstreamID != 0 {
		return model.ErrNotFound
	}
	ns.ReviewTrusted = trusted
	m.ns[name] = ns
	m.appendAudit(audit)
	return nil
}

func (m *Memory) openReports(kind string, id int64) (reporters int) {
	seen := map[string]bool{}
	for _, r := range m.rv().reports {
		if r.Kind == kind && r.EntryID == id && r.State == "open" {
			seen[r.Reporter] = true
		}
	}
	return len(seen)
}

func (m *Memory) ListReview(_ context.Context, kind, state string, limit int) ([]model.ReviewItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.ReviewItem
	if kind == "" || kind == "skill" {
		for _, s := range m.skills {
			if reviewOf(s.ReviewState) != state {
				continue
			}
			creator := ""
			if vs := m.versionsOf(s.ID); len(vs) > 0 {
				creator = vs[len(vs)-1].CreatedBy
			}
			out = append(out, model.ReviewItem{Kind: "skill", ID: s.ID, Namespace: s.Namespace, Name: s.Name, Mode: s.Mode,
				State: reviewOf(s.ReviewState), Reason: s.ReviewReason, Visibility: s.Visibility, CreatedBy: creator,
				OpenReports: m.openCount("skill", s.ID), UpdatedAt: s.UpdatedAt})
		}
	}
	if kind == "" || kind == "mcp" {
		for _, s := range m.mcp {
			if reviewOf(s.ReviewState) != state {
				continue
			}
			out = append(out, model.ReviewItem{Kind: "mcp", ID: s.ID, Namespace: s.Namespace, Name: s.Name, Mode: s.Mode,
				State: reviewOf(s.ReviewState), Reason: s.ReviewReason, Visibility: s.Visibility, CreatedBy: s.CreatedBy,
				OpenReports: m.openCount("mcp", s.ID), UpdatedAt: s.UpdatedAt})
		}
	}
	slices.SortFunc(out, func(a, b model.ReviewItem) int {
		if c := a.UpdatedAt.Compare(b.UpdatedAt); c != 0 {
			return c
		}
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return int(a.ID - b.ID)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) openCount(kind string, id int64) int {
	n := 0
	for _, r := range m.rv().reports {
		if r.Kind == kind && r.EntryID == id && r.State == "open" {
			n++
		}
	}
	return n
}

func reviewOf(state string) string {
	if state == "" {
		return model.ReviewApproved
	}
	return state
}

func (m *Memory) DecideReview(_ context.Context, d model.ReviewDecision, audit model.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	var ns string
	switch d.Kind {
	case "skill":
		s, ok := m.skills[d.ID]
		if !ok {
			return model.ErrNotFound
		}
		s.ReviewState, s.ReviewReason, s.UpdatedAt = d.State, d.Reason, now
		m.skills[d.ID] = s
		ns = s.Namespace
	case "mcp":
		s, ok := m.mcp[d.ID]
		if !ok {
			return model.ErrNotFound
		}
		s.ReviewState, s.ReviewReason, s.UpdatedAt = d.State, d.Reason, now
		m.mcp[d.ID] = s
		ns = s.Namespace
	default:
		return model.ErrNotFound
	}
	if d.TrustNamespace && d.State == model.ReviewApproved {
		if n, ok := m.ns[ns]; ok && n.UpstreamID == 0 {
			n.ReviewTrusted = true
			m.ns[ns] = n
		}
	}
	m.appendAudit(audit)
	return nil
}

func (m *Memory) CreateReport(_ context.Context, rep model.Report, threshold int, reopenNow bool, audit model.AuditEvent) (model.ReportResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var state string
	switch rep.Kind {
	case "skill":
		s, ok := m.skills[rep.EntryID]
		if !ok {
			return model.ReportResult{}, model.ErrNotFound
		}
		state = reviewOf(s.ReviewState)
	case "mcp":
		s, ok := m.mcp[rep.EntryID]
		if !ok {
			return model.ReportResult{}, model.ErrNotFound
		}
		state = reviewOf(s.ReviewState)
	default:
		return model.ReportResult{}, model.ErrNotFound
	}
	for _, r := range m.rv().reports {
		if r.Kind == rep.Kind && r.EntryID == rep.EntryID && r.Reporter == rep.Reporter && r.State == "open" {
			return model.ReportResult{Report: r}, nil
		}
	}
	rep.ID, rep.State, rep.CreatedAt = m.id(), "open", time.Now().UTC()
	m.rv().reports[rep.ID] = rep
	m.appendAudit(audit)
	res := model.ReportResult{Report: rep, Created: true}
	if state != model.ReviewApproved {
		return res, nil
	}
	open := m.openReports(rep.Kind, rep.EntryID)
	if !reopenNow && (threshold <= 0 || open < threshold) {
		return res, nil
	}
	now := time.Now().UTC()
	if rep.Kind == "skill" {
		s := m.skills[rep.EntryID]
		s.ReviewState, s.ReviewReason, s.UpdatedAt = model.ReviewPending, "reports", now
		m.skills[rep.EntryID] = s
	} else {
		s := m.mcp[rep.EntryID]
		s.ReviewState, s.ReviewReason, s.UpdatedAt = model.ReviewPending, "reports", now
		m.mcp[rep.EntryID] = s
	}
	res.Reopened = true
	m.appendAudit(model.AuditEvent{Actor: audit.Actor, Action: "review.reopen", Namespace: rep.Namespace, Target: rep.Name,
		Details: map[string]any{"kind": rep.Kind, "open_reports": open}})
	return res, nil
}

func (m *Memory) ListReports(_ context.Context, state string, afterID int64, limit int) ([]model.Report, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.Report
	for _, r := range m.rv().reports {
		if (state == "" || r.State == state) && r.ID > afterID {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b model.Report) int { return int(a.ID - b.ID) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) ResolveReport(_ context.Context, id int64, state, by string, at time.Time, audit model.AuditEvent) (model.Report, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rv().reports[id]
	if !ok {
		return model.Report{}, model.ErrNotFound
	}
	if r.State != "open" {
		return model.Report{}, conflict("report_resolved", "the report is already resolved")
	}
	r.State, r.ResolvedBy, r.ResolvedAt = state, by, &at
	m.rv().reports[id] = r
	m.appendAudit(audit)
	return r, nil
}

func (m *Memory) MoveSkill(ctx context.Context, skillID int64, toNamespace, toName string, audit model.AuditEvent) (model.Skill, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.guard(ctx); err != nil {
		return model.Skill{}, err
	}
	s, ok := m.skills[skillID]
	if !ok {
		return model.Skill{}, model.ErrNotFound
	}
	ns, ok := m.ns[toNamespace]
	if !ok {
		return model.Skill{}, model.ErrNotFound
	}
	if ns.UpstreamID != 0 || m.upstreamName(s.Namespace) != "" {
		return model.Skill{}, model.ErrNamespaceManaged()
	}
	_, exists := m.findSkill(toNamespace, toName)
	_, reserved := m.rv().redirects[toNamespace+"/"+toName]
	if exists || reserved {
		return model.Skill{}, conflict("claim_name_taken", "this name is already used in the target namespace")
	}
	m.rv().redirects[s.Namespace+"/"+s.Name] = skillID
	s.Namespace, s.Name, s.Claimed, s.UpdatedAt = toNamespace, toName, true, time.Now().UTC()
	m.skills[skillID] = s
	m.appendAudit(audit)
	return m.withUpstream(s), nil
}

func (m *Memory) SkillRedirect(_ context.Context, namespace, name string) (model.Skill, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.rv().redirects[namespace+"/"+name]
	if !ok {
		return model.Skill{}, model.ErrNotFound
	}
	s, ok := m.skills[id]
	if !ok {
		return model.Skill{}, model.ErrNotFound
	}
	return m.withUpstream(s), nil
}
