package registry

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path"
	"time"
	"unicode/utf16"

	"github.com/OpenLinker-ai/openlinker-registry/internal/archive"
	"github.com/OpenLinker-ai/openlinker-registry/internal/blobstore"
	"github.com/OpenLinker-ai/openlinker-registry/internal/github"
	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
)

// Access returns what may be served for a version.
func Access(v model.Version) skill.Access {
	return skill.ContentAccess(v.Mode, v.LicenseKind, v.LicenseApproved)
}

// SourceURL returns a browsable source link for indexed or GitHub-imported
// versions ("" otherwise).
func (s *Service) SourceURL(v model.Version) string {
	if s.GitHub == nil {
		return ""
	}
	if l := v.Locator; l != nil && l.Kind == "github" {
		return s.GitHub.WebURL(l.Owner, l.Repo, l.Commit, l.Path, l.SingleFile)
	}
	if src := v.Source; src != nil && src.Kind == "github" {
		return s.GitHub.WebURL(src.Owner, src.Repo, src.Commit, src.Path, false)
	}
	return ""
}

// checkServable enforces status, yank and license rules before any content
// read (artifact, archive or single file).
func (s *Service) checkServable(sk model.Skill, v model.Version, fullText bool) error {
	if sk.Status == model.StatusYanked {
		return fail(http.StatusGone, "skill_yanked", "this skill was yanked")
	}
	if v.YankedAt != nil {
		return fail(http.StatusGone, "version_yanked", "this version was yanked")
	}
	acc := Access(v)
	if (fullText && !acc.FullText) || (!fullText && !acc.Artifact) {
		return &Error{Status: http.StatusForbidden, Code: acc.Reason,
			Msg: "content is not redistributed by this registry; use the source link", SourceURL: s.SourceURL(v)}
	}
	return nil
}

// Materialize returns the version's files and artifact bytes. Hosted bytes
// come from the permanent store. Indexed content is served from the evictable
// cache only while the last successful source verification is younger than
// Cfg.VerifyInterval; otherwise the source is re-read and re-verified
// (commit, tree SHA, git blob SHA-1 and artifact digest) before anything is
// served, even when the bytes are cached. Verification failures update the
// check result and return no content.
func (s *Service) Materialize(ctx context.Context, sk model.Skill, v model.Version, fullText bool) ([]skill.File, []byte, error) {
	if err := s.checkServable(sk, v, fullText); err != nil {
		return nil, nil, err
	}
	if v.Mode == skill.ModeHosted {
		data, err := s.Blobs.Get(ctx, blobstore.Artifacts, v.ArtifactDigest)
		if err != nil {
			return nil, nil, err
		}
		files, err := archive.FilesFromArtifact(v.ArtifactType, data)
		return files, data, err
	}
	if s.recentlyVerified(v) {
		if data, err := s.Blobs.Get(ctx, blobstore.Cache, v.ArtifactDigest); err == nil {
			files, err := archive.FilesFromArtifact(v.ArtifactType, data)
			return files, data, err
		} else if !errors.Is(err, blobstore.ErrNotFound) {
			return nil, nil, err
		}
	}
	l := v.Locator
	if l == nil || l.Kind != "github" || s.GitHub == nil {
		return nil, nil, fail(http.StatusBadGateway, model.CheckSourceUnavailable, "the source cannot be read")
	}
	snap, err := s.GitHub.Fetch(ctx, github.Ref{Owner: l.Owner, Repo: l.Repo, Dir: l.Path, SingleFile: l.SingleFile}, l.Commit)
	if err != nil {
		var ge *github.Error
		result := model.CheckSourceUnavailable
		if errors.As(err, &ge) && ge.Code == "integrity_mismatch" {
			result = model.CheckSourceChanged
		}
		return nil, nil, s.markCheck(ctx, v, result)
	}
	if snap.TreeSHA != l.TreeSHA {
		return nil, nil, s.markCheck(ctx, v, model.CheckSourceChanged)
	}
	_, data, err := s.snapshotArtifact(snap)
	if err != nil || skill.Digest(data) != v.ArtifactDigest {
		return nil, nil, s.markCheck(ctx, v, model.CheckSourceChanged)
	}
	_, _ = s.Blobs.Put(ctx, blobstore.Cache, data)
	_ = s.Store.SetVersionCheck(ctx, v.ID, model.CheckOK, s.Now())
	return snap.Files, data, nil
}

// recentlyVerified reports whether v's source was verified successfully
// within the configured interval.
func (s *Service) recentlyVerified(v model.Version) bool {
	if v.CheckResult != model.CheckOK || v.CheckedAt == nil || s.Cfg.VerifyInterval <= 0 {
		return false
	}
	return s.Now().Sub(*v.CheckedAt) < s.Cfg.VerifyInterval
}

// GCArtifacts deletes permanent artifacts that no hosted version references
// and that were not written or refreshed since before. For each candidate it
// takes the digest's exclusive lock, which waits for any in-flight publish of
// the same digest to commit or fail, then re-checks the reference and the age
// and only then deletes. A publish that starts afterwards waits for the lock
// and rewrites the blob. It returns the number removed.
func (s *Service) GCArtifacts(ctx context.Context, before time.Time) (int, error) {
	old, err := s.Blobs.Older(ctx, blobstore.Artifacts, before)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, d := range old {
		ok, err := s.gcOne(ctx, d, before)
		if err != nil {
			return removed, err
		}
		if ok {
			removed++
		}
	}
	return removed, nil
}

func (s *Service) gcOne(ctx context.Context, digest string, before time.Time) (bool, error) {
	if s.gcBeforeLock != nil {
		s.gcBeforeLock(digest)
	}
	unlock := s.locks.exclusive(digest)
	defer unlock()
	refs, err := s.Store.ReferencedArtifacts(ctx, []string{digest})
	if err != nil || refs[digest] {
		return false, err
	}
	if s.gcBeforeDelete != nil {
		s.gcBeforeDelete(digest)
	}
	return s.Blobs.DeleteIfOlder(ctx, blobstore.Artifacts, digest, before)
}

func (s *Service) markCheck(ctx context.Context, v model.Version, result string) error {
	_ = s.Store.SetVersionCheck(ctx, v.ID, result, s.Now())
	if result == model.CheckSourceChanged {
		return &Error{Status: http.StatusConflict, Code: result, Msg: "the source no longer matches the indexed version", SourceURL: s.SourceURL(v)}
	}
	return &Error{Status: http.StatusBadGateway, Code: result, Msg: "the source is unavailable", SourceURL: s.SourceURL(v)}
}

// Archive returns the deterministic .tar.gz (stored-deflate gzip of the
// canonical tar) used by well-known "archive" entries.
func (s *Service) Archive(ctx context.Context, sk model.Skill, v model.Version) ([]byte, error) {
	if v.ArchiveDigest == "" {
		return nil, fail(http.StatusNotFound, "not_found", "this version is a single SKILL.md; use the artifact")
	}
	files, data, err := s.Materialize(ctx, sk, v, false)
	if err != nil {
		return nil, err
	}
	tarBytes := data
	if v.ArtifactType != archive.TypeTar {
		if tarBytes, err = archive.CanonicalTar(files); err != nil {
			return nil, err
		}
	}
	out := archive.StoredGzip(tarBytes)
	if skill.Digest(out) != v.ArchiveDigest {
		return nil, fail(http.StatusInternalServerError, "archive_digest_mismatch", "archive could not be reproduced")
	}
	return out, nil
}

// --- well-known discovery (agentskills.io discovery v0.2.0) ---

// DiscoverySchema is the v0.2.0 schema URI.
const DiscoverySchema = "https://schemas.agentskills.io/discovery/0.2.0/schema.json"

// IndexEntry is one v0.2.0 discovery entry.
type IndexEntry struct {
	Name        string `json:"name"`
	Type        string `json:"type"` // skill-md | archive
	Description string `json:"description"`
	URL         string `json:"url"`
	Digest      string `json:"digest"`
}

// Index is a v0.2.0 discovery document.
type Index struct {
	Schema string       `json:"$schema"`
	Skills []IndexEntry `json:"skills"`
}

// VersionURL is the public URL of a version sub-resource. URLs never carry
// credentials or signed parameters.
func (s *Service) VersionURL(ns, name, version, suffix string) string {
	return s.Cfg.PublicURL + "/v1/skills/" + url.PathEscape(ns) + "/" + url.PathEscape(name) + "/versions/" + url.PathEscape(version) + "/" + suffix
}

// Eligible decides whether a candidate may appear in a discovery index under
// entryName, and builds the entry (design §5).
func (s *Service) Eligible(c model.IndexCandidate, entryName string) (IndexEntry, bool, string) {
	sk, v := c.Skill, c.Version
	switch {
	case sk.Visibility != model.VisibilityPublic || !sk.Listed || sk.Status != model.StatusActive || v.YankedAt != nil:
		return IndexEntry{}, false, "not_public_listed_active"
	case !skill.ValidSlug(entryName):
		return IndexEntry{}, false, "name_not_slug"
	}
	desc := v.FrontmatterDescription
	if n := len(utf16.Encode([]rune(desc))); n == 0 || n > skill.WellKnownDescriptionMax {
		return IndexEntry{}, false, "description_length"
	}
	e := IndexEntry{Name: entryName, Description: desc}
	if v.Mode == skill.ModeHosted {
		if v.ArtifactType == archive.TypeSkillMD {
			e.Type, e.URL, e.Digest = "skill-md", s.VersionURL(sk.Namespace, sk.Name, v.Version, "artifact"), v.ArtifactDigest
		} else {
			e.Type, e.URL, e.Digest = "archive", s.VersionURL(sk.Namespace, sk.Name, v.Version, "archive.tar.gz"), v.ArchiveDigest
		}
		return e, true, ""
	}
	if v.CheckResult == model.CheckSourceChanged || v.CheckResult == model.CheckSourceUnavailable {
		return IndexEntry{}, false, "source_check_failed"
	}
	l := v.Locator
	if l == nil || l.Kind != "github" || s.GitHub == nil {
		return IndexEntry{}, false, "no_stable_url"
	}
	if l.SingleFile {
		// A single public file: point at the immutable raw URL of the commit.
		e.Type, e.URL, e.Digest = "skill-md", s.GitHub.RawURL(l.Owner, l.Repo, l.Commit, path.Join(l.Path, skill.SkillFile)), v.ArtifactDigest
		return e, true, ""
	}
	if !Access(v).Artifact {
		return IndexEntry{}, false, "license_not_redistributable"
	}
	e.Type, e.URL, e.Digest = "archive", s.VersionURL(sk.Namespace, sk.Name, v.Version, "archive.tar.gz"), v.ArchiveDigest
	return e, true, ""
}

// WellKnown builds the site index (namespace == "": aliased entries only,
// named by alias) or a namespace index (named by skill slug).
func (s *Service) WellKnown(ctx context.Context, namespace string) (Index, error) {
	idx := Index{Schema: DiscoverySchema, Skills: []IndexEntry{}}
	cands, err := s.Store.IndexCandidates(ctx, namespace, namespace == "")
	if err != nil {
		return idx, err
	}
	for _, c := range cands {
		name := c.Skill.Name
		if namespace == "" {
			name = c.Skill.GlobalAlias
		}
		if e, ok, _ := s.Eligible(c, name); ok {
			idx.Skills = append(idx.Skills, e)
		}
	}
	return idx, nil
}

// --- status and settings ---

// SetStatus deprecates or yanks a skill.
func (s *Service) SetStatus(ctx context.Context, actor string, sk model.Skill, status, reason string) (model.Skill, error) {
	if sk.Status == model.StatusYanked && status != model.StatusYanked {
		return model.Skill{}, fail(http.StatusConflict, "skill_yanked", "a yanked skill cannot change status")
	}
	return s.Store.SetSkillStatus(ctx, sk.ID, status, reason,
		auditEvent(actor, "skill."+status, sk.Namespace, sk.Name, map[string]any{"reason": reason}))
}

// YankVersion yanks one version; later downloads return 410.
func (s *Service) YankVersion(ctx context.Context, actor string, sk model.Skill, v model.Version, reason string) error {
	return s.Store.YankVersion(ctx, v.ID, reason, s.Now(),
		auditEvent(actor, "version.yank", sk.Namespace, sk.Name+"@"+v.Version, map[string]any{"reason": reason}))
}

// UpdateSettings changes visibility/listed with the license rule for public
// hosted skills.
func (s *Service) UpdateSettings(ctx context.Context, actor string, sk model.Skill, p model.SkillPatch) (model.Skill, error) {
	if p.Visibility != nil {
		if err := s.checkVisibility(*p.Visibility); err != nil {
			return model.Skill{}, err
		}
		if *p.Visibility == model.VisibilityPublic && sk.Mode == skill.ModeHosted {
			versions, err := s.Store.ListVersions(ctx, sk.ID)
			if err != nil {
				return model.Skill{}, err
			}
			for _, v := range versions {
				if v.YankedAt == nil && v.LicenseKind == skill.LicenseUnknown {
					return model.Skill{}, fail(http.StatusUnprocessableEntity, "license_required",
						"version "+v.Version+" has no license; public hosted skills need a license on every non-yanked version")
				}
			}
		}
	}
	details := map[string]any{}
	if p.Visibility != nil {
		details["visibility"] = *p.Visibility
	}
	if p.Listed != nil {
		details["listed"] = *p.Listed
	}
	return s.Store.UpdateSkill(ctx, sk.ID, p, auditEvent(actor, "skill.update", sk.Namespace, sk.Name, details))
}
