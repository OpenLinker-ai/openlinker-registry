package api

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/archive"
	"github.com/OpenLinker-ai/openlinker-registry/internal/auth"
	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/registry"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
)

func hexToBase64(h string) (string, error) {
	b, err := hex.DecodeString(h)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// requireRole checks authentication, the write scope and a namespace role.
func requireRole(r *http.Request, namespace string, min model.Role) error {
	p := principal(r)
	if !p.Authenticated {
		return &registry.Error{Status: http.StatusUnauthorized, Code: "unauthorized", Msg: "authentication required"}
	}
	if !p.HasScope(auth.ScopeWrite) || !p.HasRole(namespace, min) {
		return &registry.Error{Status: http.StatusForbidden, Code: "forbidden", Msg: "the " + string(min) + " role in this namespace is required"}
	}
	return nil
}

// loadForWrite authorizes then loads the skill. Authorization happens first
// so callers without a role learn nothing about private skills.
func (s *Server) loadForWrite(r *http.Request, min model.Role) (model.Skill, error) {
	if err := requireRole(r, r.PathValue("ns"), min); err != nil {
		return model.Skill{}, err
	}
	sk, err := s.Svc.Store.GetSkill(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if errors.Is(err, model.ErrNotFound) {
		return sk, errNotFound
	}
	return sk, err
}

func uploadFormat(contentType string) string {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	switch mt {
	case "text/markdown", "text/plain":
		return registry.UploadSkillMD
	case "application/x-tar":
		return archive.FormatTar
	case "application/gzip", "application/x-gzip":
		return archive.FormatTarGz
	case "application/zip":
		return archive.FormatZip
	case "application/vnd.openlinker.skill-bundle.v1+json":
		return archive.FormatBundle
	}
	return ""
}

// upload handles POST /v1/skills/{ns}/{name}/versions with the raw artifact
// as the body and metadata in query parameters (version, visibility, listed,
// license).
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	if err := requireRole(r, ns, model.RolePublisher); err != nil {
		s.fail(w, r, err)
		return
	}
	format := uploadFormat(r.Header.Get("Content-Type"))
	if format == "" {
		writeError(w, &registry.Error{Status: http.StatusUnsupportedMediaType, Code: "format_unsupported",
			Msg: "Content-Type must be text/markdown, application/x-tar, application/gzip, application/zip or application/vnd.openlinker.skill-bundle.v1+json"})
		return
	}
	q := r.URL.Query()
	for k := range q {
		if !slices.Contains([]string{"version", "visibility", "listed", "license"}, k) {
			writeError(w, &registry.Error{Status: 400, Code: "invalid_query", Msg: "unknown query parameter " + strconv.Quote(k)})
			return
		}
	}
	o := registry.PublishOptions{Actor: principal(r).Subject, Namespace: ns, Name: r.PathValue("name"),
		Version: q.Get("version"), Visibility: q.Get("visibility"), License: q.Get("license")}
	if l := q.Get("listed"); l != "" {
		b, err := strconv.ParseBool(l)
		if err != nil {
			writeError(w, &registry.Error{Status: 400, Code: "invalid_query", Msg: "listed must be a boolean"})
			return
		}
		o.Listed = &b
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.MaxUploadBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, &registry.Error{Status: http.StatusRequestEntityTooLarge, Code: "body_too_large", Msg: "upload exceeds the size limit"})
			return
		}
		writeError(w, &registry.Error{Status: 400, Code: "body_unreadable", Msg: "could not read request body"})
		return
	}
	sk, v, err := s.Svc.PublishUpload(r.Context(), o, format, body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.versionJSON(sk, v))
}

type previewRequest struct {
	URL string `json:"url"`
	Ref string `json:"ref"`
}

func (s *Server) githubPreview(w http.ResponseWriter, r *http.Request) {
	if err := requireRole(r, r.PathValue("ns"), model.RolePublisher); err != nil {
		s.fail(w, r, err)
		return
	}
	var req previewRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	p, err := s.Svc.PreviewGitHub(r.Context(), req.URL, req.Ref)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) githubImport(w http.ResponseWriter, r *http.Request) {
	s.githubWrite(w, r, s.Svc.ImportGitHub)
}

func (s *Server) githubIndex(w http.ResponseWriter, r *http.Request) {
	s.githubWrite(w, r, s.Svc.IndexGitHub)
}

func (s *Server) githubWrite(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, actor, ns string, req registry.ImportRequest) (model.Skill, model.Version, error)) {
	ns := r.PathValue("ns")
	if err := requireRole(r, ns, model.RolePublisher); err != nil {
		s.fail(w, r, err)
		return
	}
	var req registry.ImportRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	sk, v, err := fn(r.Context(), principal(r).Subject, ns, req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.versionJSON(sk, v))
}

type reasonRequest struct {
	Reason string `json:"reason"`
}

func readReason(w http.ResponseWriter, r *http.Request) (string, error) {
	var req reasonRequest
	if r.ContentLength == 0 {
		return "", nil
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return "", err
	}
	if len(req.Reason) > 500 {
		return "", &registry.Error{Status: 422, Code: "reason_too_long", Msg: "reason must be at most 500 bytes"}
	}
	return req.Reason, nil
}

func (s *Server) setStatus(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sk, err := s.loadForWrite(r, model.RolePublisher)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		reason, err := readReason(w, r)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		out, err := s.Svc.SetStatus(r.Context(), principal(r).Subject, sk, status, reason)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, skillJSON(out, ""))
	}
}

func (s *Server) yankVersion(w http.ResponseWriter, r *http.Request) {
	sk, err := s.loadForWrite(r, model.RolePublisher)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.Svc.Store.GetVersion(r.Context(), sk.ID, r.PathValue("version"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	reason, err := readReason(w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Svc.YankVersion(r.Context(), principal(r).Subject, sk, v, reason); err != nil {
		s.fail(w, r, err)
		return
	}
	v, err = s.Svc.Store.GetVersion(r.Context(), sk.ID, v.Version)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.versionJSON(sk, v))
}

type patchRequest struct {
	Visibility *string `json:"visibility"`
	Listed     *bool   `json:"listed"`
}

func (s *Server) patchSkill(w http.ResponseWriter, r *http.Request) {
	sk, err := s.loadForWrite(r, model.RoleOwner)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var req patchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.Svc.UpdateSettings(r.Context(), principal(r).Subject, sk, model.SkillPatch{Visibility: req.Visibility, Listed: req.Listed})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, skillJSON(out, ""))
}

// --- admin ---

type namespaceRequest struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	GitHubID    *int64 `json:"github_id"`
	DisplayName string `json:"display_name"`
}

func (s *Server) createNamespace(w http.ResponseWriter, r *http.Request) {
	var req namespaceRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := skill.ValidateNamespace(req.Name, req.Kind, s.Svc.Cfg.Mode); err != nil {
		s.fail(w, r, err)
		return
	}
	if (req.Kind == skill.NSKindGitHub) != (req.GitHubID != nil) || (req.GitHubID != nil && *req.GitHubID <= 0) {
		writeError(w, &registry.Error{Status: 422, Code: "github_id_required", Msg: "github namespaces require a positive numeric github_id; other kinds must not set it"})
		return
	}
	if len(req.DisplayName) > 200 {
		writeError(w, &registry.Error{Status: 422, Code: "display_name_too_long", Msg: "display_name is too long"})
		return
	}
	ns, err := s.Svc.Store.CreateNamespace(r.Context(), model.Namespace{Name: req.Name, Kind: req.Kind, GitHubID: req.GitHubID, DisplayName: req.DisplayName},
		model.AuditEvent{Actor: principal(r).Subject, Action: "namespace.create", Namespace: req.Name,
			Details: map[string]any{"kind": req.Kind, "github_id": req.GitHubID}})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"name": ns.Name, "kind": ns.Kind, "github_id": ns.GitHubID,
		"display_name": ns.DisplayName, "created_at": ns.CreatedAt})
}

var subjectRe = regexp.MustCompile(`^token:[a-z0-9]{16,64}$`)

type roleRequest struct {
	Subject string     `json:"subject"`
	Role    model.Role `json:"role"`
}

func (s *Server) setRole(w http.ResponseWriter, r *http.Request) {
	var req roleRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if !subjectRe.MatchString(req.Subject) || req.Role.Rank() == 0 {
		writeError(w, &registry.Error{Status: 422, Code: "role_invalid", Msg: "subject must be token:<id> and role owner, publisher or reader"})
		return
	}
	ns := r.PathValue("ns")
	if err := s.Svc.Store.SetRole(r.Context(), ns, req.Subject, req.Role, model.AuditEvent{Actor: principal(r).Subject,
		Action: "namespace.role", Namespace: ns, Target: req.Subject, Details: map[string]any{"role": req.Role}}); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"namespace": ns, "subject": req.Subject, "role": req.Role})
}

type tokenRequest struct {
	Name           string              `json:"name"`
	Scopes         []string            `json:"scopes"`
	Roles          []model.RoleBinding `json:"roles"`
	ExpiresInHours int                 `json:"expires_in_hours"`
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var req tokenRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.Name == "" || len(req.Name) > 100 || len(req.Scopes) == 0 || len(req.Roles) > 100 || req.ExpiresInHours < 0 || req.ExpiresInHours > 24*366 {
		writeError(w, &registry.Error{Status: 422, Code: "token_invalid", Msg: "name (1-100), at least one scope, at most 100 roles and expires_in_hours 0-8784 are required"})
		return
	}
	for _, sc := range req.Scopes {
		if !slices.Contains(auth.ValidScopes, sc) {
			writeError(w, &registry.Error{Status: 422, Code: "scope_invalid", Msg: "unknown scope " + strconv.Quote(sc)})
			return
		}
	}
	for _, rb := range req.Roles {
		if rb.Role.Rank() == 0 {
			writeError(w, &registry.Error{Status: 422, Code: "role_invalid", Msg: "role must be owner, publisher or reader"})
			return
		}
	}
	plain, id, hash, err := auth.NewToken()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	t := model.Token{ID: id, SecretHash: hash, Name: req.Name, Scopes: req.Scopes, CreatedBy: principal(r).Subject}
	if req.ExpiresInHours > 0 {
		exp := time.Now().UTC().Add(time.Duration(req.ExpiresInHours) * time.Hour)
		t.ExpiresAt = &exp
	}
	if err := s.Svc.Store.CreateToken(r.Context(), t, req.Roles, model.AuditEvent{Actor: principal(r).Subject, Action: "token.create",
		Target: "token:" + id, Details: map[string]any{"name": req.Name, "scopes": req.Scopes, "roles": req.Roles}}); err != nil {
		if errors.Is(err, model.ErrNotFound) {
			err = &registry.Error{Status: 422, Code: "namespace_not_found", Msg: "a role references an unknown namespace"}
		}
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "subject": "token:" + id, "token": plain, "scopes": req.Scopes,
		"roles": req.Roles, "expires_at": t.ExpiresAt})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Svc.Store.RevokeToken(r.Context(), id, time.Now().UTC(),
		model.AuditEvent{Actor: principal(r).Subject, Action: "token.revoke", Target: "token:" + id}); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "revoked": true})
}

type aliasRequest struct {
	Alias string `json:"alias"`
}

func (s *Server) setAlias(w http.ResponseWriter, r *http.Request) {
	var req aliasRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.Alias != "" && !skill.ValidSlug(req.Alias) {
		writeError(w, &registry.Error{Status: 422, Code: "alias_invalid", Msg: "alias must be a slug or empty to clear"})
		return
	}
	sk, err := s.Svc.Store.GetSkill(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Svc.Store.SetAlias(r.Context(), sk.ID, req.Alias, model.AuditEvent{Actor: principal(r).Subject, Action: "skill.alias",
		Namespace: sk.Namespace, Target: sk.Name, Details: map[string]any{"alias": req.Alias}}); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"namespace": sk.Namespace, "name": sk.Name, "global_alias": nullable(req.Alias)})
}

func (s *Server) approveLicense(w http.ResponseWriter, r *http.Request) {
	sk, err := s.Svc.Store.GetSkill(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.Svc.Store.GetVersion(r.Context(), sk.ID, r.PathValue("version"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if v.LicenseKind != skill.LicenseCustom {
		writeError(w, &registry.Error{Status: 422, Code: "not_custom", Msg: "only custom licenses need approval"})
		return
	}
	if err := s.Svc.Store.ApproveLicense(r.Context(), v.ID, model.AuditEvent{Actor: principal(r).Subject, Action: "license.approve",
		Namespace: sk.Namespace, Target: sk.Name + "@" + v.Version, Details: map[string]any{"license": v.LicenseID}}); err != nil {
		s.fail(w, r, err)
		return
	}
	v.LicenseApproved = true
	writeJSON(w, http.StatusOK, s.versionJSON(sk, v))
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 1000 {
			writeError(w, &registry.Error{Status: 400, Code: "invalid_query", Msg: "limit must be 1-1000"})
			return
		}
		limit = n
	}
	events, err := s.Svc.Store.ListAudit(r.Context(), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		out = append(out, map[string]any{"id": e.ID, "at": timeOrNil(e.At), "actor": e.Actor, "action": e.Action,
			"namespace": e.Namespace, "target": e.Target, "details": e.Details})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}
