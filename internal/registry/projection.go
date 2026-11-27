package registry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/OpenLinker-ai/openlinker-registry/internal/archive"
	"github.com/OpenLinker-ai/openlinker-registry/internal/mcp"
	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
)

const (
	PlatformTermsLicense = "LicenseRef-openlinker-platform-terms"
	platformTermsText    = "Published on OpenLinker under the OpenLinker platform terms; redistributable on OpenLinker and its registry."
	maxLegacyBytes       = 16 << 10
)

var (
	opRe          = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	generatedRe   = regexp.MustCompile(`^u[0-9a-f]{12}([0-9a-f]{4})?$`)
	projectNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

func (s *Service) projectionReview(ctx context.Context, p *model.ProjectionTarget) model.ReviewRule {
	if p != nil {
		return model.ReviewRule{}
	}
	return s.reviewRule(ctx, false)
}

func generatedHandle(handle string) bool { return generatedRe.MatchString(handle) }

func namespaceCandidates(sub string) []string {
	hex := strings.ReplaceAll(sub, "-", "")
	return []string{openLinkerPrefix + "u" + hex[:12], openLinkerPrefix + "u" + hex[:16]}
}

type Projector struct {
	Actor  string
	Target model.ProjectionTarget
	Sub    string
}

func (s *Service) projectionNamespace(ctx context.Context, p Projector, create bool) (model.Namespace, error) {
	if s.Cfg.Mode != "public" {
		return model.Namespace{}, fail(http.StatusUnprocessableEntity, "projection_public_only", "projection targets must be public instances")
	}
	return s.Store.ProjectionNamespace(ctx, p.Target.Owner, namespaceCandidates(p.Sub), create,
		auditEvent(p.Actor, "namespace.generate", "", "", map[string]any{"owner": p.Target.Owner}))
}

func validProjectionName(name string) error {
	if len(name) > 64 || !projectNameRe.MatchString(name) {
		return fail(http.StatusUnprocessableEntity, "name_invalid", "projection names are lowercase slugs of at most 64 characters")
	}
	return nil
}

func (s *Service) IngestProjectedVersion(ctx context.Context, p Projector, name, version, license string, body []byte) (model.Skill, model.Version, bool, error) {
	if err := validProjectionName(name); err != nil {
		return model.Skill{}, model.Version{}, false, err
	}
	ns, err := s.projectionNamespace(ctx, p, true)
	if err != nil {
		return model.Skill{}, model.Version{}, false, err
	}
	target := p.Target
	o := PublishOptions{Actor: p.Actor, Namespace: ns.Name, Name: name, Version: version, Visibility: model.VisibilityPublic,
		License: license, Projection: &target}
	sk, v, err := s.PublishUpload(ctx, o, archive.FormatBundle, body)
	var ce *model.ConflictError
	if errors.As(err, &ce) && ce.Code == "version_exists" {
		existingSkill, gerr := s.Store.GetSkill(ctx, ns.Name, name)
		if gerr != nil {
			return model.Skill{}, model.Version{}, false, gerr
		}
		if merr := managedOwner(existingSkill.Managed, target); merr != nil {
			return model.Skill{}, model.Version{}, false, merr
		}
		existing, gerr := s.Store.GetVersion(ctx, existingSkill.ID, version)
		if gerr != nil {
			return model.Skill{}, model.Version{}, false, gerr
		}
		if existing.ArtifactDigest != skill.Digest(body) {
			return model.Skill{}, model.Version{}, false, err
		}
		return existingSkill, existing, false, nil
	}
	return sk, v, err == nil, err
}

func managedOwner(m model.Managed, t model.ProjectionTarget) error {
	if !m.IsManaged() {
		return model.ErrProjectionNameConflict()
	}
	if m.By != t.Consumer || m.Owner != t.Owner {
		return model.ErrManagedByCore()
	}
	return nil
}

type SkillState struct {
	Op       string `json:"op"`
	Revision int64  `json:"revision"`
	Phase    int    `json:"phase"`
	Owner    struct {
		Sub string `json:"sub"`
	} `json:"owner"`
	State    string   `json:"state"`
	Listed   bool     `json:"listed"`
	Versions []string `json:"versions"`
	Current  string   `json:"current_version"`
	Display  struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"display"`
	Legacy json.RawMessage `json:"legacy"`
}

func (st SkillState) check(p Projector) error {
	switch {
	case !opRe.MatchString(st.Op):
		return fail(http.StatusUnprocessableEntity, "op_invalid", "op must be a lowercase UUID")
	case st.Revision < 1 || (st.Phase != 0 && st.Phase != 1):
		return fail(http.StatusUnprocessableEntity, "revision_invalid", "revision must be positive and phase 0 or 1")
	case st.Owner.Sub != p.Sub:
		return fail(http.StatusForbidden, "owner_mismatch", "the owner must be the asserted user")
	case st.State != "visible" && st.State != "hidden":
		return fail(http.StatusUnprocessableEntity, "state_invalid", "state must be visible or hidden")
	case len(st.Legacy) > maxLegacyBytes || (len(st.Legacy) > 0 && st.Legacy[0] != '{'):
		return fail(http.StatusUnprocessableEntity, "legacy_invalid", "legacy must be a JSON object of at most 16 KiB")
	case len(st.Display.Description) > 1024:
		return fail(http.StatusUnprocessableEntity, "display_invalid", "the description is limited to 1024 bytes")
	}
	for _, v := range st.Versions {
		if !skill.ValidVersion(v) {
			return fail(http.StatusUnprocessableEntity, "version_invalid", "invalid version "+v)
		}
	}
	if len(st.Versions) > 0 && !contains(st.Versions, st.Current) {
		return fail(http.StatusUnprocessableEntity, "current_invalid", "current_version must be one of versions")
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Service) ApplySkillState(ctx context.Context, p Projector, name string, st SkillState) (model.ProjectionResult, error) {
	if err := validProjectionName(name); err != nil {
		return model.ProjectionResult{}, err
	}
	if err := st.check(p); err != nil {
		return model.ProjectionResult{}, err
	}
	ns, err := s.projectionNamespace(ctx, p, true)
	if err != nil {
		return model.ProjectionResult{}, err
	}
	versions := st.Versions
	if versions == nil {
		versions = []string{}
	}
	return s.Store.ApplySkillProjection(ctx, model.Projection{Target: p.Target, Namespace: ns.Name, Name: name, Op: st.Op,
		Revision: st.Revision, Phase: st.Phase, Hidden: st.State == "hidden", Listed: st.Listed, Versions: versions, Current: st.Current,
		Description: st.Display.Description, Legacy: st.Legacy},
		auditEvent(p.Actor, "projection.skill", ns.Name, name, map[string]any{"op": st.Op, "revision": st.Revision, "phase": st.Phase, "state": st.State}))
}

type MCPState struct {
	Op       string `json:"op"`
	Revision int64  `json:"revision"`
	Phase    int    `json:"phase"`
	Owner    struct {
		Sub string `json:"sub"`
	} `json:"owner"`
	State            string          `json:"state"`
	Listed           bool            `json:"listed"`
	Version          string          `json:"version"`
	Server           json.RawMessage `json:"server"`
	Metadata         json.RawMessage `json:"metadata"`
	MetadataRevision int64           `json:"metadata_revision"`
	Legacy           json.RawMessage `json:"legacy"`
}

func (s *Service) ApplyMCPState(ctx context.Context, p Projector, name string, st MCPState) (model.ProjectionResult, error) {
	if err := validProjectionName(name); err != nil {
		return model.ProjectionResult{}, err
	}
	base := SkillState{Op: st.Op, Revision: st.Revision, Phase: st.Phase, State: st.State, Legacy: st.Legacy}
	base.Owner.Sub = st.Owner.Sub
	if err := base.check(p); err != nil {
		return model.ProjectionResult{}, err
	}
	ns, err := s.projectionNamespace(ctx, p, true)
	if err != nil {
		return model.ProjectionResult{}, err
	}
	full := ns.Name + "/" + name
	legacy := map[string]json.RawMessage{}
	if len(st.Legacy) > 0 {
		if err := json.Unmarshal(st.Legacy, &legacy); err != nil {
			return model.ProjectionResult{}, fail(http.StatusUnprocessableEntity, "legacy_invalid", "legacy must be a JSON object")
		}
	}
	if len(st.Metadata) > 0 {
		legacy["metadata"] = st.Metadata
		legacy["metadata_revision"], _ = json.Marshal(st.MetadataRevision)
	}
	merged, _ := json.Marshal(legacy)
	if len(merged) > maxLegacyBytes {
		return model.ProjectionResult{}, fail(http.StatusUnprocessableEntity, "legacy_invalid", "legacy and metadata are limited to 16 KiB")
	}
	if st.State == "visible" {
		doc := map[string]json.RawMessage{}
		if err := json.Unmarshal(st.Server, &doc); err != nil {
			return model.ProjectionResult{}, fail(http.StatusUnprocessableEntity, "server_invalid", "server must be a JSON object")
		}
		doc["name"], _ = json.Marshal(full)
		doc["version"], _ = json.Marshal(st.Version)
		body, _ := json.Marshal(doc)
		parsed, err := mcp.Parse(body, mcp.Options{Strict: true})
		if err != nil {
			return model.ProjectionResult{}, err
		}
		target := p.Target
		_, _, err = s.Store.PublishMCPVersion(ctx, model.MCPPublishInput{Namespace: ns.Name, Name: full, Mode: model.MCPModeRegistered,
			Visibility: model.VisibilityPublic, Listed: st.Listed, Projection: &target,
			Version: model.MCPVersion{Version: parsed.Version, ServerJSON: parsed.Canonical, Digest: parsed.Digest, CreatedBy: p.Actor}},
			auditEvent(p.Actor, "projection.mcp.version", ns.Name, full+"@"+parsed.Version, map[string]any{"digest": parsed.Digest}))
		var ce *model.ConflictError
		if errors.As(err, &ce) && ce.Code == "version_exists" {
			srv, gerr := s.Store.GetMCPServer(ctx, full)
			if gerr != nil {
				return model.ProjectionResult{}, gerr
			}
			existing, gerr := s.Store.GetMCPVersion(ctx, srv.ID, parsed.Version)
			if gerr != nil {
				return model.ProjectionResult{}, gerr
			}
			if existing.Digest != parsed.Digest {
				return model.ProjectionResult{}, err
			}
		} else if err != nil {
			return model.ProjectionResult{}, err
		}
	}
	return s.Store.ApplyMCPProjection(ctx, model.Projection{Target: p.Target, Namespace: ns.Name, Name: name, Op: st.Op,
		Revision: st.Revision, Phase: st.Phase, Hidden: st.State == "hidden", Listed: st.Listed, Legacy: merged},
		auditEvent(p.Actor, "projection.mcp", ns.Name, full, map[string]any{"op": st.Op, "revision": st.Revision, "phase": st.Phase, "state": st.State}))
}

func (s *Service) ValidateProjection(body []byte) (bool, string, string) {
	_, files, err := archive.ParseBundle(body, s.Cfg.Limits)
	if err == nil {
		_, err = skill.ValidateFiles(files, s.Cfg.Limits)
	}
	if err == nil {
		_, err = skill.ParseFrontmatter(fileData(files, skill.SkillFile))
	}
	if err != nil {
		e := AsError(err)
		return false, e.Code, e.Msg
	}
	return true, "", ""
}

func (s *Service) PreflightNamespaces(ctx context.Context, p Projector, subs []string, owner func(sub string) string) ([]map[string]string, error) {
	out := make([]map[string]string, 0, len(subs))
	for _, sub := range subs {
		q := Projector{Actor: p.Actor, Sub: sub, Target: model.ProjectionTarget{Consumer: p.Target.Consumer, Owner: owner(sub)}}
		ns, err := s.projectionNamespace(ctx, q, false)
		var ce *model.ConflictError
		switch {
		case errors.As(err, &ce):
			out = append(out, map[string]string{"sub": sub, "conflict": ce.Code})
		case err != nil:
			return nil, err
		default:
			out = append(out, map[string]string{"sub": sub, "namespace": ns.Name})
		}
	}
	return out, nil
}

func (s *Service) managedSkill(ctx context.Context, p Projector, name string) (model.Skill, error) {
	ns, err := s.projectionNamespace(ctx, p, false)
	if err != nil {
		return model.Skill{}, err
	}
	sk, err := s.Store.GetSkill(ctx, ns.Name, name)
	if err != nil {
		return model.Skill{}, err
	}
	if managedOwner(sk.Managed, p.Target) != nil {
		return model.Skill{}, model.ErrNotFound
	}
	return sk, nil
}

func (s *Service) SkillProjectionState(ctx context.Context, p Projector, name string) (map[string]any, error) {
	sk, err := s.managedSkill(ctx, p, name)
	if err != nil {
		return nil, err
	}
	versions, err := s.Store.ListVersionSummaries(ctx, sk.ID)
	if err != nil {
		return nil, err
	}
	visible, ingested := []string{}, []string{}
	digests := map[string]string{}
	for i := len(versions) - 1; i >= 0; i-- {
		v := versions[i]
		ingested = append(ingested, v.Version)
		digests[v.Version] = v.ArtifactDigest
		if !v.Hidden && v.YankedAt == nil {
			visible = append(visible, v.Version)
		}
	}
	return map[string]any{"namespace": sk.Namespace, "name": sk.Name, "revision": sk.Managed.Revision, "phase": sk.Managed.Phase,
		"hidden": sk.Managed.Hidden, "listed": sk.Listed, "current_version": sk.CurrentVersion, "versions": visible,
		"ingested": ingested, "digests": digests, "review": sk.ReviewState}, nil
}

func (s *Service) MCPProjectionState(ctx context.Context, p Projector, name string) (map[string]any, error) {
	ns, err := s.projectionNamespace(ctx, p, false)
	if err != nil {
		return nil, err
	}
	srv, err := s.Store.GetMCPServer(ctx, ns.Name+"/"+name)
	if err != nil {
		return nil, err
	}
	if managedOwner(srv.Managed, p.Target) != nil {
		return nil, model.ErrNotFound
	}
	out := map[string]any{"namespace": ns.Name, "name": srv.Name, "revision": srv.Managed.Revision, "phase": srv.Managed.Phase,
		"hidden": srv.Managed.Hidden, "listed": srv.Listed, "version": "", "digest": "", "review": srv.ReviewState}
	if v, err := s.Store.GetMCPVersion(ctx, srv.ID, "latest"); err == nil {
		out["version"], out["digest"] = v.Version, v.Digest
	} else if !errors.Is(err, model.ErrNotFound) {
		return nil, err
	}
	return out, nil
}
