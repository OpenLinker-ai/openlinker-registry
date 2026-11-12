package api

import (
	"encoding/base64"
	"errors"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/registry"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
)

var errNotFound = &registry.Error{Status: http.StatusNotFound, Code: "not_found", Msg: "not found"}

// loadVisible returns the skill only if the caller may see it; invisible
// skills are indistinguishable from missing ones.
func (s *Server) loadVisible(r *http.Request) (model.Skill, error) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	sk, err := s.Svc.Store.GetSkill(r.Context(), ns, name)
	// A local miss may be forwarded to the upstream registries.
	if errors.Is(err, model.ErrNotFound) {
		if ferr := s.allowForward(r); ferr == nil {
			sk, err = s.Svc.ResolveSkill(r.Context(), ns, name, true)
		} else if ferr != errNoForward {
			return model.Skill{}, ferr
		}
	}
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return model.Skill{}, errNotFound
		}
		return model.Skill{}, err
	}
	if !principal(r).Viewer().CanSee(sk.Namespace, sk.Visibility) {
		return model.Skill{}, errNotFound
	}
	return sk, nil
}

func (s *Server) loadVisibleVersion(r *http.Request) (model.Skill, model.Version, error) {
	sk, err := s.loadVisible(r)
	if err != nil {
		return sk, model.Version{}, err
	}
	v, err := s.Svc.Store.GetVersion(r.Context(), sk.ID, r.PathValue("version"))
	if errors.Is(err, model.ErrNotFound) {
		return sk, v, errNotFound
	}
	return sk, v, err
}

func skillJSON(sk model.Skill, latest string) map[string]any {
	out := map[string]any{
		"namespace": sk.Namespace, "name": sk.Name, "description": sk.Description, "visibility": sk.Visibility,
		"listed": sk.Listed, "status": sk.Status, "mode": sk.Mode, "latest_version": nullable(latest),
		"created_at": sk.CreatedAt, "updated_at": sk.UpdatedAt,
	}
	if sk.StatusReason != "" {
		out["status_reason"] = sk.StatusReason
	}
	if sk.GlobalAlias != "" {
		out["global_alias"] = sk.GlobalAlias
	}
	if sk.Upstream != "" {
		out["upstream"] = sk.Upstream
	}
	return out
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Server) versionJSON(sk model.Skill, v model.Version) map[string]any {
	acc := registry.Access(v)
	out := map[string]any{
		"namespace": sk.Namespace, "name": sk.Name, "version": v.Version, "mode": v.Mode,
		"created_at": v.CreatedAt, "created_by": v.CreatedBy,
		"frontmatter":     map[string]any{"name": v.FrontmatterName, "description": v.FrontmatterDescription},
		"license":         map[string]any{"kind": v.LicenseKind, "id": v.LicenseID, "approved": v.LicenseApproved},
		"core_compatible": v.CoreCompatible,
		"files":           v.Files,
		"artifact":        map[string]any{"type": v.ArtifactType, "digest": v.ArtifactDigest, "size": v.ArtifactSize},
		"content_access":  map[string]any{"full_text": acc.FullText, "artifact": acc.Artifact, "reason": nullable(acc.Reason)},
		"yanked":          nil,
	}
	if acc.Artifact {
		out["artifact"].(map[string]any)["url"] = s.Svc.VersionURL(sk.Namespace, sk.Name, v.Version, "artifact")
	}
	if v.ArchiveDigest != "" {
		a := map[string]any{"digest": v.ArchiveDigest}
		if acc.Artifact {
			a["url"] = s.Svc.VersionURL(sk.Namespace, sk.Name, v.Version, "archive.tar.gz")
		}
		out["archive"] = a
	}
	if v.Source != nil {
		out["source"] = v.Source
	}
	if v.Locator != nil {
		out["locator"] = v.Locator
		out["indexed_by"] = v.IndexedBy
		out["check"] = map[string]any{"result": nullable(v.CheckResult), "checked_at": v.CheckedAt}
	}
	if u := s.Svc.SourceURL(v); u != "" {
		out["source_url"] = u
	}
	if v.LicenseText != "" && acc.FullText {
		out["license"].(map[string]any)["text"] = v.LicenseText
	}
	if v.YankedAt != nil {
		out["yanked"] = map[string]any{"at": v.YankedAt, "reason": v.YankReason}
	}
	return out
}

func encodeCursor(ns, name string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(ns + "\x00" + name))
}

func decodeCursor(c string) (string, string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || len(b) > 256 {
		return "", "", false
	}
	ns, name, ok := strings.Cut(string(b), "\x00")
	return ns, name, ok && utf8.ValidString(ns) && utf8.ValidString(name)
}

func (s *Server) listSkills(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	q := model.SearchQuery{Viewer: principal(r).Viewer(), Q: strings.TrimSpace(qv.Get("q")), Namespace: qv.Get("namespace"), Mode: qv.Get("mode"), Limit: 50}
	if len(q.Q) > 200 {
		writeError(w, &registry.Error{Status: 400, Code: "invalid_query", Msg: "q is too long"})
		return
	}
	if q.Mode != "" && q.Mode != skill.ModeHosted && q.Mode != skill.ModeIndexed {
		writeError(w, &registry.Error{Status: 400, Code: "invalid_query", Msg: "mode must be hosted or indexed"})
		return
	}
	if c := qv.Get("compatible"); c != "" {
		b, err := strconv.ParseBool(c)
		if err != nil {
			writeError(w, &registry.Error{Status: 400, Code: "invalid_query", Msg: "compatible must be a boolean"})
			return
		}
		q.Compatible = b
	}
	if l := qv.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 100 {
			writeError(w, &registry.Error{Status: 400, Code: "invalid_query", Msg: "limit must be 1-100"})
			return
		}
		q.Limit = n
	}
	if c := qv.Get("cursor"); c != "" {
		ns, name, ok := decodeCursor(c)
		if !ok {
			writeError(w, &registry.Error{Status: 400, Code: "invalid_query", Msg: "invalid cursor"})
			return
		}
		q.AfterNS, q.AfterName = ns, name
	}
	limit := q.Limit
	q.Limit = limit + 1
	rows, err := s.Svc.Store.Search(r.Context(), q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var next any
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		next = encodeCursor(last.Namespace, last.Name)
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, skillJSON(row.Skill, row.LatestVersion))
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": items, "next_cursor": next})
}

func (s *Server) getSkill(w http.ResponseWriter, r *http.Request) {
	sk, err := s.loadVisible(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	versions, err := s.Svc.Store.ListVersions(r.Context(), sk.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	latest := ""
	list := make([]map[string]any, 0, len(versions))
	for _, v := range versions {
		if latest == "" && v.YankedAt == nil {
			latest = v.Version
		}
		list = append(list, map[string]any{"version": v.Version, "created_at": v.CreatedAt, "artifact_digest": v.ArtifactDigest,
			"yanked": v.YankedAt != nil, "core_compatible": v.CoreCompatible, "license_kind": v.LicenseKind})
	}
	out := skillJSON(sk, latest)
	out["versions"] = list
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getVersion(w http.ResponseWriter, r *http.Request) {
	sk, v, err := s.loadVisibleVersion(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.versionJSON(sk, v))
}

// sharedCacheMaxAge is deliberately short: visibility, listing and status
// can change after a response is cached.
const sharedCacheMaxAge = "60"

// setCache allows shared caching only for responses that are identical for
// every caller: anonymous reads enabled, an anonymous request, and content
// that is public (and, for indexes, built only from public entries).
// Everything else is "private, no-store". Vary: Authorization is always set
// so a cache never serves an anonymous response to an authenticated request
// or vice versa.
func (s *Server) setCache(w http.ResponseWriter, r *http.Request, public bool) {
	h := w.Header()
	h.Add("Vary", "Authorization")
	if public && s.AllowAnonRead && r.Header.Get("Authorization") == "" && !principal(r).Authenticated {
		h.Set("Cache-Control", "public, max-age="+sharedCacheMaxAge)
		return
	}
	h.Set("Cache-Control", "private, no-store")
}

func safeFilename(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' || r == '/' || r == ';' {
			return '_'
		}
		return r
	}, s)
}

func (s *Server) writeBlob(w http.ResponseWriter, r *http.Request, sk model.Skill, data []byte, contentType, disposition, digest string) {
	h := w.Header()
	s.setCache(w, r, sk.Visibility == model.VisibilityPublic)
	h.Set("Content-Type", contentType)
	h.Set("Content-Disposition", disposition)
	h.Set("Content-Length", strconv.Itoa(len(data)))
	if digest != "" {
		h.Set("ETag", `"`+digest+`"`)
		raw, _ := hexToBase64(strings.TrimPrefix(digest, "sha256:"))
		h.Set("Repr-Digest", "sha-256=:"+raw+":")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) getArtifact(w http.ResponseWriter, r *http.Request) {
	sk, v, err := s.loadVisibleVersion(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	_, data, err := s.Svc.Materialize(r.Context(), sk, v, false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	base := safeFilename(sk.Name + "-" + v.Version)
	switch v.ArtifactType {
	case "skill-md":
		s.writeBlob(w, r, sk, data, "text/plain; charset=utf-8", `attachment; filename="SKILL.md"`, v.ArtifactDigest)
	case "tar":
		s.writeBlob(w, r, sk, data, "application/x-tar", `attachment; filename="`+base+`.tar"`, v.ArtifactDigest)
	default:
		s.writeBlob(w, r, sk, data, "application/json", `attachment; filename="`+base+`.json"`, v.ArtifactDigest)
	}
}

func (s *Server) getArchive(w http.ResponseWriter, r *http.Request) {
	sk, v, err := s.loadVisibleVersion(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data, err := s.Svc.Archive(r.Context(), sk, v)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeBlob(w, r, sk, data, "application/gzip", `attachment; filename="`+safeFilename(sk.Name+"-"+v.Version)+`.tar.gz"`, v.ArchiveDigest)
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("path")
	if err := skill.ValidatePath(p); err != nil {
		writeError(w, &registry.Error{Status: 400, Code: "path_invalid", Msg: "invalid file path"})
		return
	}
	sk, v, err := s.loadVisibleVersion(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	found := false
	for _, f := range v.Files {
		found = found || f.Path == p
	}
	if !found {
		s.fail(w, r, errNotFound)
		return
	}
	files, _, err := s.Svc.Materialize(r.Context(), sk, v, true)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, f := range files {
		if f.Path != p {
			continue
		}
		if !utf8.Valid(f.Data) {
			writeError(w, &registry.Error{Status: http.StatusUnsupportedMediaType, Code: "not_text", Msg: "file is not UTF-8 text; download the artifact"})
			return
		}
		s.writeBlob(w, r, sk, f.Data, "text/plain; charset=utf-8", `inline; filename="`+safeFilename(path.Base(p))+`"`, skill.Digest(f.Data))
		return
	}
	s.fail(w, r, errNotFound)
}

func (s *Server) wellKnownSite(w http.ResponseWriter, r *http.Request) {
	s.wellKnown(w, r, "")
}

// wellKnownNamespace returns the same empty index for unknown namespaces and
// for namespaces without public entries, so existence is not revealed.
func (s *Server) wellKnownNamespace(w http.ResponseWriter, r *http.Request) {
	s.wellKnown(w, r, r.PathValue("ns"))
}

func (s *Server) wellKnown(w http.ResponseWriter, r *http.Request, ns string) {
	idx, err := s.Svc.WellKnown(r.Context(), ns)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Indexes contain only public entries, but they are mutable.
	s.setCache(w, r, true)
	writeJSON(w, http.StatusOK, idx)
}

// timeOrNil is used by audit output.
func timeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
