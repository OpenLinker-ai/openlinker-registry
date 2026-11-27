package api

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"slices"

	"github.com/OpenLinker-ai/openlinker-registry/internal/auth"
	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/registry"
)

func (s *Server) projectionRoutes(m *http.ServeMux) {
	m.HandleFunc("PUT /v1/projections/skills/{name}/versions/{version}", s.projectVersion)
	m.HandleFunc("PUT /v1/projections/skills/{name}", s.projectSkill)
	m.HandleFunc("PUT /v1/projections/mcp/{name}", s.projectMCP)
	m.HandleFunc("POST /v1/projections/validate", s.projectValidate)
	m.HandleFunc("POST /v1/projections/namespaces/preflight", s.projectNamespaces)
	m.HandleFunc("PATCH /v1/admin/gateways/{consumer}", s.admin(s.patchGateway))
}

func (s *Server) projector(r *http.Request) (registry.Projector, error) {
	p := principal(r)
	if !p.Authenticated {
		return registry.Projector{}, &registry.Error{Status: http.StatusUnauthorized, Code: "unauthorized", Msg: "authentication required"}
	}
	if p.Gateway == nil || !p.Gateway.Projection {
		return registry.Projector{}, &registry.Error{Status: http.StatusForbidden, Code: "projection_not_allowed", Msg: "this token may not project entries"}
	}
	if p.User == nil {
		return registry.Projector{}, &registry.Error{Status: http.StatusForbidden, Code: "user_required", Msg: "projection needs a user assertion"}
	}
	if !p.HasScope(auth.ScopeWrite) || !p.User.Allows(auth.ActProject) {
		return registry.Projector{}, &registry.Error{Status: http.StatusForbidden, Code: "forbidden", Msg: "projection needs skills:write and the project action"}
	}
	return registry.Projector{Actor: p.Subject, Sub: p.User.Sub,
		Target: model.ProjectionTarget{Consumer: p.Gateway.Consumer, Owner: auth.OpenLinkerSubject(p.Gateway.Consumer, p.User.Sub)}}, nil
}

func (s *Server) readBundle(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/vnd.openlinker.skill-bundle.v1+json" {
		return nil, &registry.Error{Status: http.StatusUnsupportedMediaType, Code: "format_unsupported", Msg: "projection versions are Core bundles"}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.MaxUploadBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, &registry.Error{Status: http.StatusRequestEntityTooLarge, Code: "body_too_large", Msg: "bundle exceeds the size limit"}
		}
		return nil, &registry.Error{Status: http.StatusBadRequest, Code: "body_unreadable", Msg: "could not read request body"}
	}
	return body, nil
}

func (s *Server) projectVersion(w http.ResponseWriter, r *http.Request) {
	p, err := s.projector(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for k := range r.URL.Query() {
		if k != "license" {
			s.fail(w, r, &registry.Error{Status: http.StatusBadRequest, Code: "invalid_query", Msg: "unknown query parameter"})
			return
		}
	}
	body, err := s.readBundle(w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sk, v, created, err := s.Svc.IngestProjectedVersion(r.Context(), p, r.PathValue("name"), r.PathValue("version"), r.URL.Query().Get("license"), body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"namespace": sk.Namespace, "name": sk.Name, "version": v.Version, "digest": v.ArtifactDigest})
}

func (s *Server) projectSkill(w http.ResponseWriter, r *http.Request) {
	p, err := s.projector(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var st registry.SkillState
	if err := decodeJSON(w, r, &st); err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.Svc.ApplySkillState(r.Context(), p, r.PathValue("name"), st)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) projectMCP(w http.ResponseWriter, r *http.Request) {
	p, err := s.projector(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var st registry.MCPState
	if err := decodeJSON(w, r, &st); err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.Svc.ApplyMCPState(r.Context(), p, r.PathValue("name"), st)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) projectValidate(w http.ResponseWriter, r *http.Request) {
	if _, err := s.projector(r); err != nil {
		s.fail(w, r, err)
		return
	}
	body, err := s.readBundle(w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ok, code, msg := s.Svc.ValidateProjection(body)
	out := map[string]any{"ok": ok}
	if !ok {
		out["code"], out["message"] = code, msg
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) projectNamespaces(w http.ResponseWriter, r *http.Request) {
	p, err := s.projector(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var req struct {
		Subjects []string `json:"subjects"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if len(req.Subjects) > 1000 || slices.ContainsFunc(req.Subjects, func(sub string) bool { return !auth.ValidUserSubject("openlinker:x:" + sub) }) {
		s.fail(w, r, &registry.Error{Status: http.StatusUnprocessableEntity, Code: "subjects_invalid", Msg: "subjects must be at most 1000 UUIDs"})
		return
	}
	out, err := s.Svc.PreflightNamespaces(r.Context(), p, req.Subjects, func(sub string) string {
		return auth.OpenLinkerSubject(p.Target.Consumer, sub)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"namespaces": out})
}

func (s *Server) patchGateway(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Projection *bool `json:"projection"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.Projection == nil {
		s.fail(w, r, &registry.Error{Status: http.StatusUnprocessableEntity, Code: "patch_empty", Msg: "nothing to change"})
		return
	}
	consumer := r.PathValue("consumer")
	if err := s.Svc.Store.SetGatewayProjection(r.Context(), consumer, *req.Projection, model.AuditEvent{Actor: principal(r).Subject,
		Action: "gateway.projection", Target: consumer, Details: map[string]any{"projection": *req.Projection}}); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"consumer": consumer, "projection": *req.Projection})
}
