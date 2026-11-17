// Package registry implements the registry use cases (hosted publishing,
// GitHub preview/import/indexing, content materialization with
// re-verification, status changes and well-known discovery) on top of the
// store, blob store and GitHub client. HTTP concerns live in package api.
package registry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OpenLinker-ai/openlinker-registry/internal/archive"
	"github.com/OpenLinker-ai/openlinker-registry/internal/blobstore"
	"github.com/OpenLinker-ai/openlinker-registry/internal/github"
	"github.com/OpenLinker-ai/openlinker-registry/internal/mcp"
	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/safehttp"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

// Error is a use-case failure mapped to an HTTP status by the API layer.
type Error struct {
	Status int
	Code   string
	Msg    string
	// SourceURL is set when content is withheld but a source link exists.
	SourceURL string
	// RetryAfter is the number of seconds a rate-limited caller should wait.
	RetryAfter int
}

func (e *Error) Error() string { return e.Msg }

func fail(status int, code, msg string) error { return &Error{Status: status, Code: code, Msg: msg} }

// AsError normalizes errors from lower layers into *Error.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	var ve *skill.ValidationError
	if errors.As(err, &ve) {
		return &Error{Status: http.StatusUnprocessableEntity, Code: ve.Code, Msg: ve.Msg}
	}
	var ae *archive.Error
	if errors.As(err, &ae) {
		return &Error{Status: http.StatusUnprocessableEntity, Code: ae.Code, Msg: ae.Msg}
	}
	var ge *github.Error
	if errors.As(err, &ge) {
		return &Error{Status: ge.Status, Code: ge.Code, Msg: ge.Msg}
	}
	var me *mcp.Error
	if errors.As(err, &me) {
		return &Error{Status: http.StatusUnprocessableEntity, Code: me.Code, Msg: me.Msg}
	}
	var pe *model.PolicyError
	if errors.As(err, &pe) {
		return &Error{Status: http.StatusUnprocessableEntity, Code: pe.Code, Msg: pe.Msg}
	}
	var ce *model.ConflictError
	if errors.As(err, &ce) {
		return &Error{Status: http.StatusConflict, Code: ce.Code, Msg: ce.Msg}
	}
	var ne *skill.NamespaceError
	if errors.As(err, &ne) {
		return &Error{Status: http.StatusUnprocessableEntity, Code: "namespace_invalid", Msg: ne.Msg}
	}
	if errors.Is(err, archive.ErrPathTooLong) {
		return &Error{Status: http.StatusUnprocessableEntity, Code: "path_too_long", Msg: "a path is too long for the canonical archive"}
	}
	if errors.Is(err, model.ErrNotFound) {
		return &Error{Status: http.StatusNotFound, Code: "not_found", Msg: "not found"}
	}
	return &Error{Status: http.StatusInternalServerError, Code: "internal", Msg: "internal error"}
}

// Config holds service policy.
type Config struct {
	Mode        string // public | private
	AllowPublic bool
	Limits      skill.Limits
	// PublicURL is the external URL of the registry root, e.g.
	// https://openlinker.ai/registry. Used for well-known artifact URLs.
	PublicURL string
	// VerifyInterval bounds how long cached indexed content is served
	// without re-verifying the source. Zero re-verifies on every read.
	VerifyInterval time.Duration
	// UpstreamPolicy says where requests to upstream registries may go.
	UpstreamPolicy safehttp.Policy
	// UpstreamToken returns the bearer token for an upstream by name ("" for
	// none). Tokens come from the environment and are never stored.
	UpstreamToken func(name string) string
}

// Service implements the use cases.
type Service struct {
	Store  store.Store
	Blobs  blobstore.Store
	GitHub *github.Client
	Cfg    Config
	Now    func() time.Time
	// HTTP is the outbound client for upstream registries.
	HTTP *http.Client
	Log  *slog.Logger

	locks      digestLocks
	clients    clientCache
	misses     missCache
	forwarding forwardingCache
	// Test hooks for deterministic concurrency tests; nil in production.
	afterArtifactPut func(digest string)
	gcBeforeLock     func(digest string)
	gcBeforeDelete   func(digest string)
}

// New builds a service.
func New(st store.Store, blobs blobstore.Store, gh *github.Client, cfg Config) *Service {
	if cfg.Limits.MaxFiles == 0 {
		cfg.Limits = skill.DefaultLimits
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	return &Service{Store: st, Blobs: blobs, GitHub: gh, Cfg: cfg, Now: func() time.Time { return time.Now().UTC() },
		HTTP: safehttp.NewClient(cfg.UpstreamPolicy), Log: slog.Default()}
}

// DefaultVisibility is public on the public instance and internal otherwise.
func (s *Service) DefaultVisibility() string {
	if s.Cfg.Mode == "public" {
		return model.VisibilityPublic
	}
	return model.VisibilityInternal
}

func (s *Service) checkVisibility(v string) error {
	switch v {
	case model.VisibilityPublic:
		if !s.Cfg.AllowPublic {
			return fail(http.StatusUnprocessableEntity, "public_disabled", "public visibility is disabled on this instance")
		}
	case model.VisibilityInternal, model.VisibilityPrivate:
	default:
		return fail(http.StatusUnprocessableEntity, "visibility_invalid", "visibility must be public, internal or private")
	}
	return nil
}

// auditEvent builds the audit row that the store persists atomically with
// the mutation it describes.
func auditEvent(actor, action, ns, target string, details map[string]any) model.AuditEvent {
	return model.AuditEvent{Actor: actor, Action: action, Namespace: ns, Target: target, Details: details}
}

// built is a validated version ready to persist.
type built struct {
	version  model.Version
	artifact []byte
	files    []skill.File
	fm       skill.Frontmatter
	reasons  []string
}

// build validates frontmatter and license and computes manifest, digests and
// Core compatibility. artifact holds the bytes stored or cached for the
// version (raw SKILL.md, canonical tar or canonical bundle).
func (s *Service) build(files []skill.File, artifactType string, artifact []byte, license skill.License, licenseText string) (*built, error) {
	var md []byte
	for _, f := range files {
		if f.Path == skill.SkillFile {
			md = f.Data
		}
	}
	fm, err := skill.ParseFrontmatter(md)
	if err != nil {
		return nil, err
	}
	compat, reasons := skill.CoreCompatible(fm, files)
	v := model.Version{
		FrontmatterName: fm.Name, FrontmatterDescription: fm.Description,
		LicenseKind: license.Kind, LicenseID: license.ID, LicenseText: licenseText,
		CoreCompatible: compat, Files: skill.BuildManifest(files),
		ArtifactDigest: skill.Digest(artifact), ArtifactType: artifactType, ArtifactSize: int64(len(artifact)),
	}
	if artifactType != archive.TypeSkillMD {
		tarBytes := artifact
		if artifactType != archive.TypeTar {
			if tarBytes, err = archive.CanonicalTar(files); err != nil {
				return nil, err
			}
		}
		v.ArchiveDigest = skill.Digest(archive.StoredGzip(tarBytes))
	}
	return &built{version: v, artifact: artifact, files: files, fm: fm, reasons: reasons}, nil
}

// PublishOptions are common to hosted publishing paths.
type PublishOptions struct {
	Actor       string
	Namespace   string
	Name        string
	Version     string
	Visibility  string // optional; only applies when the skill is created
	Listed      *bool  // optional; only applies when the skill is created
	License     string // optional override of frontmatter license
	LicenseText string // required for custom unless a LICENSE file is included
}

func (s *Service) validateTarget(ctx context.Context, o *PublishOptions, mode string) (existing *model.Skill, err error) {
	if !skill.ValidSkillName(o.Name) {
		return nil, fail(http.StatusUnprocessableEntity, "name_invalid", "skill name must be a lowercase slug (a-z, 0-9, single hyphens, max 64)")
	}
	if !skill.ValidVersion(o.Version) {
		return nil, fail(http.StatusUnprocessableEntity, "version_invalid", "version must be 1-64 letters, digits, dots, hyphens or underscores")
	}
	if _, err := s.Store.GetNamespace(ctx, o.Namespace); err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, fail(http.StatusNotFound, "namespace_not_found", "namespace not found")
		}
		return nil, err
	}
	sk, err := s.Store.GetSkill(ctx, o.Namespace, o.Name)
	switch {
	case err == nil:
		if sk.Mode != mode {
			return nil, fail(http.StatusConflict, "mode_mismatch", "skill exists with mode "+sk.Mode)
		}
		o.Visibility = sk.Visibility
		return &sk, nil
	case errors.Is(err, model.ErrNotFound):
		if o.Visibility == "" {
			o.Visibility = s.DefaultVisibility()
		}
		return nil, s.checkVisibility(o.Visibility)
	default:
		return nil, err
	}
}

// maxLicenseText bounds stored license text.
const maxLicenseText = 64 << 10

// licenseFileText returns the trimmed text of the first root license file
// (LICENSE, LICENSE.md, LICENSE.txt, LICENCE, COPYING), or "".
func licenseFileText(files []skill.File) string {
	for _, f := range files {
		switch strings.ToUpper(f.Path) {
		case "LICENSE", "LICENSE.MD", "LICENSE.TXT", "LICENCE", "COPYING":
			if t := strings.TrimSpace(string(f.Data)); t != "" && utf8.ValidString(t) {
				return t
			}
		}
	}
	return ""
}

// hostedLicense applies the hosted license rules of design §6: public hosting
// needs an SPDX license or custom with text; unknown is internal/private only.
// The returned text is the custom license text that is stored and exposed:
// license_text if given, otherwise the non-empty root LICENSE file.
func hostedLicense(o PublishOptions, fm skill.Frontmatter, files []skill.File) (skill.License, string, error) {
	declared := o.License
	if declared == "" {
		declared = fm.License
	}
	lic := skill.ClassifyLicense(declared)
	text := ""
	if lic.Kind == skill.LicenseCustom {
		text = strings.TrimSpace(o.LicenseText)
		if text == "" {
			text = licenseFileText(files)
		}
		if text == "" {
			return lic, "", fail(http.StatusUnprocessableEntity, "license_text_required", "a custom license needs non-empty license_text or a non-empty LICENSE file in the skill")
		}
		if len(text) > maxLicenseText || !utf8.ValidString(text) {
			return lic, "", fail(http.StatusUnprocessableEntity, "license_text_invalid", "license text must be UTF-8 and at most 64 KiB")
		}
	}
	if lic.Kind == skill.LicenseUnknown && o.Visibility == model.VisibilityPublic {
		return lic, "", fail(http.StatusUnprocessableEntity, "license_required", "public hosted skills must declare a license (SPDX id or custom with text)")
	}
	return lic, text, nil
}

func (s *Service) publishHosted(ctx context.Context, o PublishOptions, files []skill.File, artifactType string, artifact []byte, src model.Source) (model.Skill, model.Version, error) {
	var md []byte
	for _, f := range files {
		if f.Path == skill.SkillFile {
			md = f.Data
		}
	}
	fm, err := skill.ParseFrontmatter(md)
	if err != nil {
		return model.Skill{}, model.Version{}, err
	}
	lic, licText, err := hostedLicense(o, fm, files)
	if err != nil {
		return model.Skill{}, model.Version{}, err
	}
	b, err := s.build(files, artifactType, artifact, lic, licText)
	if err != nil {
		return model.Skill{}, model.Version{}, err
	}
	// The blob is written before the version row is committed, under a
	// shared lease that GCArtifacts must wait for. If the publish fails, the
	// blob may be orphaned; GCArtifacts removes it after the grace period.
	digest := skill.Digest(artifact)
	unlock := s.locks.shared(digest)
	defer unlock()
	if _, err := s.Blobs.Put(ctx, blobstore.Artifacts, artifact); err != nil {
		return model.Skill{}, model.Version{}, err
	}
	if s.afterArtifactPut != nil {
		s.afterArtifactPut(digest)
	}
	v := b.version
	v.Version, v.Source, v.CreatedBy = o.Version, &src, o.Actor
	listed := true
	if o.Listed != nil {
		listed = *o.Listed
	}
	return s.Store.PublishVersion(ctx, model.PublishInput{Namespace: o.Namespace, Name: o.Name, Mode: skill.ModeHosted,
		Visibility: o.Visibility, Listed: listed, Version: v},
		auditEvent(o.Actor, "skill.publish", o.Namespace, o.Name+"@"+o.Version,
			map[string]any{"digest": v.ArtifactDigest, "artifact_type": v.ArtifactType, "source": src.Kind}))
}

// Upload formats accepted by PublishUpload.
const (
	UploadSkillMD = "skill-md"
)

// PublishUpload publishes an uploaded SKILL.md, archive or Core bundle.
func (s *Service) PublishUpload(ctx context.Context, o PublishOptions, format string, body []byte) (model.Skill, model.Version, error) {
	if _, err := s.validateTarget(ctx, &o, skill.ModeHosted); err != nil {
		return model.Skill{}, model.Version{}, err
	}
	var files []skill.File
	var artifact []byte
	artifactType := archive.TypeTar
	var err error
	switch format {
	case UploadSkillMD:
		if int64(len(body)) > s.Cfg.Limits.MaxFileBytes {
			return model.Skill{}, model.Version{}, fail(http.StatusRequestEntityTooLarge, "file_too_large", "SKILL.md exceeds the per-file size limit")
		}
		files, artifact, artifactType = []skill.File{{Path: skill.SkillFile, Data: body}}, body, archive.TypeSkillMD
	case archive.FormatTar, archive.FormatTarGz, archive.FormatZip:
		if files, err = archive.Extract(body, format, s.Cfg.Limits); err != nil {
			return model.Skill{}, model.Version{}, err
		}
		if artifact, err = archive.CanonicalTar(files); err != nil {
			return model.Skill{}, model.Version{}, err
		}
	case archive.FormatBundle:
		var bundle archive.Bundle
		if bundle, files, err = archive.ParseBundle(body, s.Cfg.Limits); err != nil {
			return model.Skill{}, model.Version{}, err
		}
		fm, err := skill.ParseFrontmatter(fileData(files, skill.SkillFile))
		if err != nil {
			return model.Skill{}, model.Version{}, err
		}
		if bundle.Name != fm.Name || bundle.Description != fm.Description {
			return model.Skill{}, model.Version{}, fail(http.StatusUnprocessableEntity, "bundle_invalid", "bundle name and description must match SKILL.md frontmatter")
		}
		artifact, artifactType = body, archive.TypeBundle
	default:
		return model.Skill{}, model.Version{}, fail(http.StatusUnsupportedMediaType, "format_unsupported", "unsupported upload format")
	}
	if _, err := skill.ValidateFiles(files, s.Cfg.Limits); err != nil {
		return model.Skill{}, model.Version{}, err
	}
	return s.publishHosted(ctx, o, files, artifactType, artifact, model.Source{Kind: "upload", Format: format})
}

func fileData(files []skill.File, p string) []byte {
	for _, f := range files {
		if f.Path == p {
			return f.Data
		}
	}
	return nil
}

// Preview is the result of resolving a GitHub URL. Nothing is persisted.
type Preview struct {
	Owner          string                `json:"owner"`
	Repo           string                `json:"repo"`
	Path           string                `json:"path"`
	SingleFile     bool                  `json:"single_file"`
	Commit         string                `json:"commit"`
	TreeSHA        string                `json:"tree_sha"`
	WebURL         string                `json:"web_url"`
	SuggestedName  string                `json:"suggested_name"`
	Frontmatter    map[string]string     `json:"frontmatter"`
	License        map[string]string     `json:"license"`
	CoreCompatible bool                  `json:"core_compatible"`
	CoreReasons    []string              `json:"core_incompatible_reasons,omitempty"`
	ArtifactType   string                `json:"artifact_type"`
	ArtifactDigest string                `json:"artifact_digest"`
	ArchiveDigest  string                `json:"archive_digest,omitempty"`
	Files          []skill.ManifestEntry `json:"files"`
}

func (s *Service) snapshotArtifact(snap *github.Snapshot) (string, []byte, error) {
	if snap.Ref.SingleFile {
		return archive.TypeSkillMD, snap.Files[0].Data, nil
	}
	tarBytes, err := archive.CanonicalTar(snap.Files)
	return archive.TypeTar, tarBytes, err
}

// PreviewGitHub resolves url (and optional ref) to a commit and lists and
// verifies the files. It performs no writes.
func (s *Service) PreviewGitHub(ctx context.Context, rawURL, ref string) (*Preview, error) {
	if s.GitHub == nil {
		return nil, fail(http.StatusServiceUnavailable, "github_disabled", "GitHub access is not configured")
	}
	r, err := s.GitHub.ParseURL(rawURL, ref)
	if err != nil {
		return nil, err
	}
	commit, err := s.GitHub.ResolveCommit(ctx, r)
	if err != nil {
		return nil, err
	}
	snap, err := s.GitHub.Fetch(ctx, r, commit)
	if err != nil {
		return nil, err
	}
	typ, artifact, err := s.snapshotArtifact(snap)
	if err != nil {
		return nil, err
	}
	b, err := s.build(snap.Files, typ, artifact, skill.License{Kind: skill.LicenseUnknown}, "")
	if err != nil {
		return nil, err
	}
	lic := skill.ClassifyLicense(b.fm.License)
	return &Preview{
		Owner: r.Owner, Repo: r.Repo, Path: r.Dir, SingleFile: r.SingleFile, Commit: commit, TreeSHA: snap.TreeSHA,
		WebURL:         s.GitHub.WebURL(r.Owner, r.Repo, commit, r.Dir, r.SingleFile),
		SuggestedName:  suggestName(b.fm.Name, r),
		Frontmatter:    map[string]string{"name": b.fm.Name, "description": b.fm.Description, "license": b.fm.License},
		License:        map[string]string{"kind": lic.Kind, "id": lic.ID},
		CoreCompatible: b.version.CoreCompatible, CoreReasons: b.reasons,
		ArtifactType: typ, ArtifactDigest: b.version.ArtifactDigest, ArchiveDigest: b.version.ArchiveDigest,
		Files: b.version.Files,
	}, nil
}

func suggestName(fmName string, r github.Ref) string {
	if skill.ValidSkillName(fmName) {
		return fmName
	}
	base := strings.ToLower(path.Base(r.Dir))
	if r.Dir == "" {
		base = strings.ToLower(r.Repo)
	}
	if skill.ValidSkillName(base) {
		return base
	}
	return ""
}

// ImportRequest locks a GitHub import or index to the previewed commit/tree.
type ImportRequest struct {
	URL             string `json:"url"`
	Commit          string `json:"commit"`
	ExpectedTreeSHA string `json:"expected_tree_sha"`
	Name            string `json:"name"`
	Version         string `json:"version"`
	Visibility      string `json:"visibility"`
	Listed          *bool  `json:"listed"`
	License         string `json:"license"`
	LicenseText     string `json:"license_text"`
}

func (s *Service) fetchLocked(ctx context.Context, req ImportRequest) (github.Ref, *github.Snapshot, error) {
	if s.GitHub == nil {
		return github.Ref{}, nil, fail(http.StatusServiceUnavailable, "github_disabled", "GitHub access is not configured")
	}
	if !github.CommitRe.MatchString(req.Commit) || !github.CommitRe.MatchString(req.ExpectedTreeSHA) {
		return github.Ref{}, nil, fail(http.StatusUnprocessableEntity, "preview_required", "commit and expected_tree_sha from the preview are required (40 lowercase hex)")
	}
	r, err := s.GitHub.ParseURL(req.URL, req.Commit)
	if err != nil {
		return github.Ref{}, nil, err
	}
	snap, err := s.GitHub.Fetch(ctx, r, req.Commit)
	if err != nil {
		return github.Ref{}, nil, err
	}
	if snap.TreeSHA != req.ExpectedTreeSHA {
		return github.Ref{}, nil, fail(http.StatusConflict, "tree_sha_mismatch", "the source tree differs from the previewed tree; preview again")
	}
	return r, snap, nil
}

func defaultVersion(o *PublishOptions, commit string) {
	if o.Version == "" {
		o.Version = "git-" + commit[:12]
	}
}

// ImportGitHub publishes a hosted version from a fixed GitHub commit.
func (s *Service) ImportGitHub(ctx context.Context, actor, namespace string, req ImportRequest) (model.Skill, model.Version, error) {
	r, snap, err := s.fetchLocked(ctx, req)
	if err != nil {
		return model.Skill{}, model.Version{}, err
	}
	o := PublishOptions{Actor: actor, Namespace: namespace, Name: req.Name, Version: req.Version, Visibility: req.Visibility,
		Listed: req.Listed, License: req.License, LicenseText: req.LicenseText}
	defaultVersion(&o, snap.Commit)
	if o.Name == "" {
		o.Name = suggestName(fmNameOf(snap.Files), r)
	}
	if _, err := s.validateTarget(ctx, &o, skill.ModeHosted); err != nil {
		return model.Skill{}, model.Version{}, err
	}
	typ, artifact, err := s.snapshotArtifact(snap)
	if err != nil {
		return model.Skill{}, model.Version{}, err
	}
	return s.publishHosted(ctx, o, snap.Files, typ, artifact, model.Source{Kind: "github", Owner: r.Owner, Repo: r.Repo,
		Commit: snap.Commit, Path: r.Dir, TreeSHA: snap.TreeSHA})
}

func fmNameOf(files []skill.File) string {
	fm, err := skill.ParseFrontmatter(fileData(files, skill.SkillFile))
	if err != nil {
		return ""
	}
	return fm.Name
}

// IndexGitHub records an indexed version: locator, manifest and digests only.
// Content bytes go to the evictable cache, and only when the license allows
// serving them.
func (s *Service) IndexGitHub(ctx context.Context, actor, namespace string, req ImportRequest) (model.Skill, model.Version, error) {
	if req.License != "" || req.LicenseText != "" {
		return model.Skill{}, model.Version{}, fail(http.StatusUnprocessableEntity, "license_not_assertable",
			"indexed entries take their license from the source; license overrides are only accepted for hosted content")
	}
	r, snap, err := s.fetchLocked(ctx, req)
	if err != nil {
		return model.Skill{}, model.Version{}, err
	}
	o := PublishOptions{Actor: actor, Namespace: namespace, Name: req.Name, Version: req.Version, Visibility: req.Visibility, Listed: req.Listed}
	defaultVersion(&o, snap.Commit)
	if o.Name == "" {
		o.Name = suggestName(fmNameOf(snap.Files), r)
	}
	existing, err := s.validateTarget(ctx, &o, skill.ModeIndexed)
	if err != nil {
		return model.Skill{}, model.Version{}, err
	}
	if existing != nil && existing.SourceKey != r.SourceKey() {
		return model.Skill{}, model.Version{}, fail(http.StatusConflict, "source_mismatch", "this indexed skill tracks a different source")
	}
	typ, artifact, err := s.snapshotArtifact(snap)
	if err != nil {
		return model.Skill{}, model.Version{}, err
	}
	fm, err := skill.ParseFrontmatter(fileData(snap.Files, skill.SkillFile))
	if err != nil {
		return model.Skill{}, model.Version{}, err
	}
	lic := skill.ClassifyLicense(fm.License)
	licText := ""
	if lic.Kind == skill.LicenseCustom {
		// Kept for reviewers; exposed only after approval.
		if licText = licenseFileText(snap.Files); len(licText) > maxLicenseText {
			licText = ""
		}
	}
	b, err := s.build(snap.Files, typ, artifact, lic, licText)
	if err != nil {
		return model.Skill{}, model.Version{}, err
	}
	if skill.ContentAccess(skill.ModeIndexed, lic.Kind, false).Artifact {
		_, _ = s.Blobs.Put(ctx, blobstore.Cache, artifact)
	}
	now := s.Now()
	v := b.version
	v.Version = o.Version
	v.Locator = &model.Locator{Kind: "github", Owner: r.Owner, Repo: r.Repo, Commit: snap.Commit, Path: r.Dir,
		TreeSHA: snap.TreeSHA, SingleFile: r.SingleFile}
	v.IndexedBy, v.CreatedBy, v.CheckResult, v.CheckedAt = actor, actor, model.CheckOK, &now
	listed := true
	if o.Listed != nil {
		listed = *o.Listed
	}
	return s.Store.PublishVersion(ctx, model.PublishInput{Namespace: namespace, Name: o.Name, Mode: skill.ModeIndexed,
		SourceKey: r.SourceKey(), Visibility: o.Visibility, Listed: listed, Version: v},
		auditEvent(actor, "skill.index", namespace, o.Name+"@"+o.Version,
			map[string]any{"digest": v.ArtifactDigest, "commit": snap.Commit, "tree_sha": snap.TreeSHA, "source_key": r.SourceKey()}))
}
