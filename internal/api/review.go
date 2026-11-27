package api

import (
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/OpenLinker-ai/openlinker-registry/internal/auth"
	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/registry"
)

func (s *Server) reviewRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /v1/namespaces", s.claimNamespace)
	m.HandleFunc("POST /v1/skills/{ns}/{name}/claim", s.claimSkill)
	m.HandleFunc("POST /v1/reports", s.createReport)
	m.HandleFunc("GET /v1/admin/review", s.admin(s.listReview))
	m.HandleFunc("POST /v1/admin/review/skills/{ns}/{name}", s.admin(s.decideSkillReview))
	m.HandleFunc("POST /v1/admin/review/mcp/servers/{ns}/{name}", s.admin(s.decideMCPReview))
	m.HandleFunc("GET /v1/admin/reports", s.admin(s.listReports))
	m.HandleFunc("POST /v1/admin/reports/{id}/resolve", s.admin(s.resolveReport))
	m.HandleFunc("PATCH /v1/admin/namespaces/{ns}", s.admin(s.patchNamespace))
}

func (s *Server) claimant(r *http.Request) (registry.Claimant, error) {
	p := principal(r)
	if !p.Authenticated {
		return registry.Claimant{}, &registry.Error{Status: http.StatusUnauthorized, Code: "unauthorized", Msg: "authentication required"}
	}
	if p.Gateway == nil || p.User == nil {
		return registry.Claimant{}, &registry.Error{Status: http.StatusForbidden, Code: "user_required", Msg: "this action needs a user assertion from a gateway"}
	}
	if !p.HasScope(auth.ScopeWrite) || !p.User.Allows(auth.ActWrite) {
		return registry.Claimant{}, &registry.Error{Status: http.StatusForbidden, Code: "forbidden", Msg: "write access is required"}
	}
	return registry.Claimant{Actor: p.Subject, Subject: auth.OpenLinkerSubject(p.User.Consumer, p.User.Sub), GitHubID: p.User.GitHubID,
		OpenLinker: slices.Contains(p.Gateway.Identities, model.IdentityOpenLinker),
		GitHub:     slices.Contains(p.Gateway.Identities, model.IdentityGitHub)}, nil
}

func namespaceJSON(ns model.Namespace) map[string]any {
	return map[string]any{"name": ns.Name, "kind": ns.Kind, "display_name": ns.DisplayName, "review_trusted": ns.ReviewTrusted,
		"created_at": ns.CreatedAt}
}

func (s *Server) claimNamespace(w http.ResponseWriter, r *http.Request) {
	c, err := s.claimant(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	ns, created, err := s.Svc.ClaimNamespace(r.Context(), c, req.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, namespaceJSON(ns))
}

func (s *Server) claimSkill(w http.ResponseWriter, r *http.Request) {
	c, err := s.claimant(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	sk, err := s.Svc.Store.GetSkill(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if errors.Is(err, model.ErrNotFound) || (err == nil && !principal(r).Viewer().CanSee(sk.Namespace, sk.ShownVisibility())) {
		s.fail(w, r, errNotFound)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.Svc.ClaimSkill(r.Context(), c, sk, req.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, skillJSON(out, ""))
}

type reportRequest struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Category  string `json:"category"`
	Details   string `json:"details"`
}

func reportJSON(rep model.Report) map[string]any {
	out := map[string]any{"id": rep.ID, "kind": rep.Kind, "namespace": rep.Namespace, "name": rep.Name, "version": rep.Version,
		"category": rep.Category, "state": rep.State, "created_at": rep.CreatedAt}
	return out
}

func (s *Server) createReport(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Authenticated {
		s.fail(w, r, &registry.Error{Status: http.StatusUnauthorized, Code: "unauthorized", Msg: "authentication required"})
		return
	}
	if p.Gateway != nil && p.User == nil {
		s.fail(w, r, &registry.Error{Status: http.StatusForbidden, Code: "user_required", Msg: "reports through a gateway need a user assertion"})
		return
	}
	var req reportRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	viewer := p.Viewer()
	rep := model.Report{Kind: req.Kind, Namespace: req.Namespace, Name: req.Name, Version: req.Version, Reporter: p.Subject,
		Category: req.Category, Details: req.Details}
	switch req.Kind {
	case "skill":
		sk, err := s.Svc.Store.GetSkill(r.Context(), req.Namespace, req.Name)
		if errors.Is(err, model.ErrNotFound) || (err == nil && !viewer.CanSee(sk.Namespace, sk.ShownVisibility())) {
			s.fail(w, r, errNotFound)
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		rep.EntryID = sk.ID
	case "mcp":
		srv, err := s.Svc.Store.GetMCPServer(r.Context(), req.Namespace+"/"+req.Name)
		if errors.Is(err, model.ErrNotFound) || (err == nil && !viewer.CanSee(srv.Namespace, srv.ShownVisibility())) {
			s.fail(w, r, errNotFound)
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		rep.EntryID = srv.ID
	default:
		s.fail(w, r, &registry.Error{Status: http.StatusUnprocessableEntity, Code: "kind_invalid", Msg: "kind must be skill or mcp"})
		return
	}
	res, err := s.Svc.Report(r.Context(), p.Subject, rep, p.Admin)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, reportJSON(res.Report))
}

func (s *Server) listReview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = model.ReviewPending
	}
	kind := q.Get("kind")
	if !slices.Contains([]string{model.ReviewPending, model.ReviewRejected, model.ReviewApproved}, state) || (kind != "" && kind != "skill" && kind != "mcp") {
		s.fail(w, r, &registry.Error{Status: http.StatusBadRequest, Code: "invalid_query", Msg: "state must be pending, rejected or approved; kind skill or mcp"})
		return
	}
	items, err := s.Svc.Store.ListReview(r.Context(), kind, state, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]any{"kind": it.Kind, "namespace": it.Namespace, "name": it.Name, "mode": it.Mode,
			"state": it.State, "reason": it.Reason, "review_revision": it.Revision, "visibility": it.Visibility, "created_by": it.CreatedBy,
			"open_reports": it.OpenReports, "updated_at": it.UpdatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

type decisionRequest struct {
	Decision       string `json:"decision"`
	Revision       *int64 `json:"revision"`
	Reason         string `json:"reason"`
	TrustNamespace bool   `json:"trust_namespace"`
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request, kind string, id int64, ns, name string) {
	var req decisionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.Revision == nil {
		s.fail(w, r, &registry.Error{Status: http.StatusUnprocessableEntity, Code: "revision_required", Msg: "revision must be the review_revision that was reviewed"})
		return
	}
	if err := s.Svc.DecideReview(r.Context(), principal(r).Subject, kind, id, *req.Revision, ns, name, req.Decision, req.Reason, req.TrustNamespace); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"kind": kind, "namespace": ns, "name": name, "decision": req.Decision})
}

func (s *Server) decideSkillReview(w http.ResponseWriter, r *http.Request) {
	sk, err := s.Svc.Store.GetSkill(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.decide(w, r, "skill", sk.ID, sk.Namespace, sk.Name)
}

func (s *Server) decideMCPReview(w http.ResponseWriter, r *http.Request) {
	srv, err := s.Svc.Store.GetMCPServer(r.Context(), r.PathValue("ns")+"/"+r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.decide(w, r, "mcp", srv.ID, srv.Namespace, srv.Name)
}

func (s *Server) listReports(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	if state != "" && state != "open" && state != "dismissed" && state != "actioned" {
		s.fail(w, r, &registry.Error{Status: http.StatusBadRequest, Code: "invalid_query", Msg: "state must be open, dismissed or actioned"})
		return
	}
	var after int64
	if c := q.Get("cursor"); c != "" {
		n, err := strconv.ParseInt(c, 10, 64)
		if err != nil || n < 0 {
			s.fail(w, r, &registry.Error{Status: http.StatusBadRequest, Code: "invalid_query", Msg: "invalid cursor"})
			return
		}
		after = n
	}
	reps, err := s.Svc.Store.ListReports(r.Context(), state, after, 100)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(reps))
	for _, rep := range reps {
		m := reportJSON(rep)
		m["reporter"], m["details"], m["resolved_by"], m["resolved_at"] = rep.Reporter, rep.Details, rep.ResolvedBy, rep.ResolvedAt
		out = append(out, m)
	}
	body := map[string]any{"reports": out}
	if len(reps) == 100 {
		body["next_cursor"] = strconv.FormatInt(reps[len(reps)-1].ID, 10)
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) resolveReport(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.fail(w, r, errNotFound)
		return
	}
	var req struct {
		Resolution string `json:"resolution"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	rep, err := s.Svc.ResolveReport(r.Context(), principal(r).Subject, id, req.Resolution)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, reportJSON(rep))
}

func (s *Server) patchNamespace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ReviewTrusted *bool `json:"review_trusted"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.ReviewTrusted == nil {
		s.fail(w, r, &registry.Error{Status: http.StatusUnprocessableEntity, Code: "patch_empty", Msg: "nothing to change"})
		return
	}
	ns := r.PathValue("ns")
	if err := s.Svc.Store.SetNamespaceTrusted(r.Context(), ns, *req.ReviewTrusted, model.AuditEvent{Actor: principal(r).Subject,
		Action: "namespace.review_trusted", Namespace: ns, Details: map[string]any{"review_trusted": *req.ReviewTrusted}}); err != nil {
		s.fail(w, r, err)
		return
	}
	got, err := s.Svc.Store.GetNamespace(r.Context(), ns)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, namespaceJSON(got))
}
