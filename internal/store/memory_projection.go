package store

import (
	"context"
	"slices"
	"time"

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

func newer(p model.Projection, m model.Managed) bool {
	return p.Revision > m.Revision || (p.Revision == m.Revision && p.Phase > m.Phase)
}

type projectionState struct {
	ops map[string]model.ProjectionResult
}

func (m *Memory) pj() *projectionState {
	if m.projection == nil {
		m.projection = &projectionState{ops: map[string]model.ProjectionResult{}}
	}
	return m.projection
}

func (m *Memory) ProjectionNamespace(_ context.Context, owner string, candidates []string, create bool, audit model.AuditEvent) (model.Namespace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var generated *model.Namespace
	for _, ns := range m.ns {
		if ns.OpenLinkerSubject == owner {
			return ns, nil
		}
		if ns.GeneratedFor == owner {
			n := ns
			generated = &n
		}
	}
	if generated != nil {
		return *generated, nil
	}
	for _, name := range candidates {
		if _, taken := m.ns[name]; taken {
			continue
		}
		ns := model.Namespace{Name: name, Kind: "openlinker", GeneratedFor: owner}
		if !create {
			return ns, nil
		}
		ns.ID, ns.CreatedAt = m.id(), time.Now().UTC()
		m.ns[name] = ns
		m.appendAudit(audit)
		return ns, nil
	}
	return model.Namespace{}, model.ErrProjectionNamespace()
}

func (m *Memory) ApplySkillProjection(_ context.Context, p model.Projection, audit model.AuditEvent) (model.ProjectionResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := p.Target.Consumer + "/" + p.Op
	if prior, ok := m.pj().ops[key]; ok {
		return prior, nil
	}
	if _, ok := m.ns[p.Namespace]; !ok {
		return model.ProjectionResult{}, model.ErrNotFound
	}
	var out model.ProjectionResult
	s, ok := m.findSkill(p.Namespace, p.Name)
	switch {
	case !ok:
		if !p.Hidden && len(p.Versions) > 0 {
			return model.ProjectionResult{}, model.ErrProjectionVersionMissing()
		}
		now := time.Now().UTC()
		s = model.Skill{ID: m.id(), Namespace: p.Namespace, Name: p.Name, Visibility: model.VisibilityPublic, Status: model.StatusActive,
			Mode: "hosted", ReviewState: model.ReviewApproved, CreatedAt: now, UpdatedAt: now,
			Managed: model.Managed{By: p.Target.Consumer, Owner: p.Target.Owner, Revision: p.Revision, Phase: p.Phase, Hidden: true, Legacy: p.Legacy}}
		m.skills[s.ID] = s
		out.Applied = true
	default:
		if err := managedWrite(s.Managed, &p.Target); err != nil {
			return model.ProjectionResult{}, err
		}
		if !newer(p, s.Managed) {
			break
		}
		found := 0
		for _, v := range m.versionsOf(s.ID) {
			if slices.Contains(p.Versions, v.Version) {
				found++
			}
		}
		if found != len(p.Versions) {
			return model.ProjectionResult{}, model.ErrProjectionVersionMissing()
		}
		for id, v := range m.versions {
			if v.SkillID == s.ID {
				v.Hidden = !slices.Contains(p.Versions, v.Version)
				m.versions[id] = v
			}
		}
		s.Managed.Hidden = p.Hidden || len(p.Versions) == 0
		s.Managed.Revision, s.Managed.Phase = p.Revision, p.Phase
		if len(p.Legacy) > 0 {
			s.Managed.Legacy = p.Legacy
		}
		s.Listed, s.CurrentVersion, s.UpdatedAt = p.Listed, p.Current, time.Now().UTC()
		if p.Description != "" {
			s.Description = p.Description
		}
		m.skills[s.ID] = s
		out.Applied = true
	}
	out.Namespace = p.Namespace
	m.pj().ops[key] = out
	m.appendAudit(audit)
	return out, nil
}

func (m *Memory) ApplyMCPProjection(_ context.Context, p model.Projection, audit model.AuditEvent) (model.ProjectionResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := p.Target.Consumer + "/" + p.Op
	if prior, ok := m.pj().ops[key]; ok {
		return prior, nil
	}
	if _, ok := m.ns[p.Namespace]; !ok {
		return model.ProjectionResult{}, model.ErrNotFound
	}
	full := p.Namespace + "/" + p.Name
	var out model.ProjectionResult
	s, ok := m.findMCP(full)
	switch {
	case !ok:
		if !p.Hidden {
			return model.ProjectionResult{}, model.ErrProjectionVersionMissing()
		}
		now := micro(time.Now())
		s = model.MCPServer{ID: m.id(), Namespace: p.Namespace, Name: full, Mode: model.MCPModeRegistered, Visibility: model.VisibilityPublic,
			ReviewState: model.ReviewApproved, CreatedBy: p.Target.Consumer, CreatedAt: now, UpdatedAt: now,
			Managed: model.Managed{By: p.Target.Consumer, Owner: p.Target.Owner, Revision: p.Revision, Phase: p.Phase, Hidden: true, Legacy: p.Legacy}}
		m.mcp[s.ID] = s
		out.Applied = true
	default:
		if s.Name != full {
			return model.ProjectionResult{}, model.ErrProjectionNameConflict()
		}
		if err := managedWrite(s.Managed, &p.Target); err != nil {
			return model.ProjectionResult{}, err
		}
		if !newer(p, s.Managed) {
			break
		}
		now := time.Now().UTC()
		s.Managed.Hidden, s.Managed.Revision, s.Managed.Phase = p.Hidden, p.Revision, p.Phase
		if len(p.Legacy) > 0 {
			s.Managed.Legacy = p.Legacy
		}
		s.Listed, s.UpdatedAt = p.Listed, micro(now)
		m.mcp[s.ID] = s
		m.stampMCPVersions(s.ID, now)
		out.Applied = true
	}
	out.Namespace = p.Namespace
	m.pj().ops[key] = out
	m.appendAudit(audit)
	return out, nil
}
