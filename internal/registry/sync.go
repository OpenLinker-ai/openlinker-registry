package registry

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OpenLinker-ai/openlinker-registry/internal/archive"
	"github.com/OpenLinker-ai/openlinker-registry/internal/blobstore"
	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/safehttp"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
	"github.com/OpenLinker-ai/openlinker-registry/internal/upstream"
)

// syncStats counts what one sync run did, by outcome.
type syncStats map[string]int

func (st syncStats) add(key string) { st[key]++ }

// SyncUpstream mirrors one upstream now. Only one run per upstream is active
// at a time across all registry processes.
func (s *Service) SyncUpstream(ctx context.Context, actor, name string) (model.Upstream, error) {
	u, err := s.Store.GetUpstream(ctx, name)
	if err != nil {
		return model.Upstream{}, err
	}
	if !u.Enabled {
		return model.Upstream{}, fail(http.StatusConflict, "upstream_disabled", "this upstream is disabled")
	}
	now := s.Now()
	claimed, err := s.Store.ClaimUpstreamSync(ctx, u.ID, now, now.Add(-syncStale))
	if err != nil {
		return model.Upstream{}, err
	}
	if !claimed {
		return model.Upstream{}, fail(http.StatusConflict, "sync_running", "a sync of this upstream is already running")
	}
	runCtx, cancel := context.WithTimeout(ctx, syncTimeout)
	res := s.runSync(runCtx, u)
	cancel()
	// The result is recorded even when the caller went away mid-run, so the
	// upstream never stays "running" until the stale timeout.
	done, cancelDone := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancelDone()
	details := map[string]any{"status": res.Status}
	for k, v := range res.Stats {
		details[k] = v
	}
	if err := s.Store.FinishUpstreamSync(done, u.ID, res, auditEvent(actor, "upstream.sync", "", u.Name, details)); err != nil {
		return model.Upstream{}, err
	}
	return s.Store.GetUpstream(done, name)
}

// SyncDue runs every enabled upstream whose sync interval has elapsed. It is
// called periodically by the server; failures are recorded on the upstream.
func (s *Service) SyncDue(ctx context.Context) {
	ups, err := s.Store.ListUpstreams(ctx)
	if err != nil {
		s.Log.Warn("listing upstreams failed", "err", err.Error())
		return
	}
	for _, u := range ups {
		if ctx.Err() != nil {
			return
		}
		if !u.Enabled || u.SyncInterval <= 0 || (u.LastSyncAt != nil && s.Now().Sub(*u.LastSyncAt) < u.SyncInterval) {
			continue
		}
		out, err := s.SyncUpstream(ctx, "system:sync", u.Name)
		var re *Error
		switch {
		case errors.As(err, &re) && re.Code == "sync_running":
		case err != nil:
			s.Log.Warn("upstream sync failed", "upstream", u.Name, "err", err.Error())
		default:
			s.Log.Info("upstream synced", "upstream", u.Name, "status", out.LastSyncStatus, "stats", out.LastSyncStats)
		}
	}
}

func (s *Service) runSync(ctx context.Context, u model.Upstream) model.UpstreamSyncResult {
	stats := syncStats{}
	c := s.client(u)
	cursor := u.Cursor
	var err error
	switch u.Kind {
	case model.UpstreamMCP:
		cursor, err = s.syncMCP(ctx, u, c, stats)
	case model.UpstreamRegistry:
		if err = s.syncSkills(ctx, u, c, stats); err == nil {
			cursor, err = s.syncMCP(ctx, u, c, stats)
		}
	case model.UpstreamAgentSkills:
		err = s.syncAgentSkills(ctx, u, c, stats)
	}
	res := model.UpstreamSyncResult{At: s.Now(), Status: model.SyncOK, Cursor: cursor, Stats: stats}
	if err != nil {
		// Upstream error text is not stored verbatim: it may quote a response.
		res.Status, res.Error = model.SyncFailed, syncErrorText(err)
	}
	return res
}

func syncErrorText(err error) string {
	var re *Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "the sync did not finish in time"
	case errors.Is(err, context.Canceled):
		return "the sync was interrupted"
	case errors.As(err, &re):
		return re.Msg
	case isStoreFailure(err) && !isUpstreamError(err):
		return "internal error while storing mirrored entries"
	}
	return clip(err.Error(), 300)
}

func isUpstreamError(err error) bool {
	var ue *upstream.Error
	var se *safehttp.Error
	return errors.As(err, &ue) || errors.As(err, &se) || errors.Is(err, upstream.ErrNotFound)
}

// syncMCP mirrors MCP servers through the official registry read API.
//
// The cursor is an RFC 3339 watermark: the newest updatedAt among the
// entries this instance stored, never later than the start of the run. A
// timestamp in the future, or one on an entry that was rejected, therefore
// cannot move the watermark past updates that have not been seen yet. A run
// re-reads a window before the watermark, and the watermark is only advanced
// when the whole listing was read.
//
// With LatestOnly the first (full) run asks for latest versions only. Later
// runs read every change since the watermark, because a deletion is not a
// "latest" version and would otherwise never arrive; a change to a version
// that was not mirrored is ignored.
func (s *Service) syncMCP(ctx context.Context, u model.Upstream, c *upstream.Client, stats syncStats) (string, error) {
	start := s.Now()
	var since *time.Time
	var watermark time.Time
	if t, err := time.Parse(time.RFC3339Nano, u.Cursor); err == nil {
		watermark = t
		from := t.Add(-syncOverlap)
		since = &from
	}
	full := since == nil
	page := ""
	for i := 0; ; i++ {
		if i == maxSyncPages {
			return "", fail(http.StatusBadGateway, "upstream_too_large", "the upstream listing exceeds the page limit of one sync")
		}
		docs, next, err := c.ListMCP(ctx, upstream.MCPListOptions{Cursor: page, Limit: syncPageSize, UpdatedSince: since,
			LatestOnly: u.LatestOnly && full})
		if err != nil {
			// A skill-only registry has no MCP API; that is not a failure.
			if i == 0 && u.Kind == model.UpstreamRegistry && errors.Is(err, upstream.ErrNotFound) {
				return u.Cursor, nil
			}
			return "", err
		}
		for _, doc := range docs {
			res, err := s.mirrorMCP(ctx, u, doc, u.LatestOnly && !doc.IsLatest)
			switch {
			case err == nil:
				stats.add("mcp_" + res)
				if doc.UpdatedAt.After(watermark) && !doc.UpdatedAt.After(start) {
					watermark = doc.UpdatedAt
				}
			case isStoreFailure(err):
				return "", err
			case errors.Is(err, model.ErrConflict):
				stats.add("mcp_conflicts")
			default:
				stats.add("mcp_skipped")
			}
		}
		if next == "" || next == page {
			break
		}
		page = next
	}
	if watermark.IsZero() {
		return u.Cursor, nil
	}
	return watermark.UTC().Format(time.RFC3339Nano), nil
}

// syncSkills mirrors the skills of another openlinker-registry: metadata
// now, bytes on first read (see Materialize). The native API has no change
// feed and its listing does not show every change (yanking an old version
// changes neither the latest version nor the status), so each run reads the
// detail of every listed skill.
func (s *Service) syncSkills(ctx context.Context, u model.Upstream, c *upstream.Client, stats syncStats) error {
	cursor := ""
	listed := map[string]bool{}
	for i := 0; ; i++ {
		if i == maxSyncPages {
			return fail(http.StatusBadGateway, "upstream_too_large", "the upstream listing exceeds the page limit of one sync")
		}
		items, next, err := c.ListSkills(ctx, cursor, syncPageSize)
		if err != nil {
			return err
		}
		for _, it := range items {
			if !skill.ValidNamespaceName(it.Namespace) || !skill.ValidSkillName(it.Name) || !u.Allows(it.Namespace) {
				stats.add("skills_skipped")
				continue
			}
			listed[it.Namespace+"/"+it.Name] = true
			if err := s.refreshSkill(ctx, u, c, it.Namespace, it.Name, stats); err != nil {
				return err
			}
		}
		if next == "" || next == cursor {
			break
		}
		cursor = next
	}
	return s.syncDelisted(ctx, u, c, listed, stats)
}

// refreshSkill reads one skill from the upstream and brings the local mirror
// up to date. A skill the upstream no longer serves is restricted here. Only
// failures of this instance or of the transport are returned; a rejected
// entry is counted.
func (s *Service) refreshSkill(ctx context.Context, u model.Upstream, c *upstream.Client, ns, name string, stats syncStats) error {
	detail, err := c.GetSkill(ctx, ns, name)
	if errors.Is(err, upstream.ErrNotFound) {
		return s.restrictSkill(ctx, u, ns, name, "no longer available from the upstream registry", false, stats)
	}
	if err != nil {
		return err
	}
	err = s.mirrorSkill(ctx, u, c, detail, stats)
	switch {
	case err == nil:
	case isStoreFailure(err):
		return err
	case errors.Is(err, model.ErrConflict):
		stats.add("skills_conflicts")
	default:
		stats.add("skills_skipped")
	}
	return nil
}

// syncDelisted follows up on mirrored skills the upstream no longer lists (a
// yanked or unlisted skill drops out of the listing): their current state is
// read by name.
func (s *Service) syncDelisted(ctx context.Context, u model.Upstream, c *upstream.Client, listed map[string]bool, stats syncStats) error {
	q := model.SearchQuery{Viewer: model.Viewer{Admin: true}, Mode: skill.ModeIndexed, Limit: 200}
	for {
		rows, err := s.Store.Search(ctx, q)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Upstream != u.Name || listed[row.Namespace+"/"+row.Name] {
				continue
			}
			if err := s.refreshSkill(ctx, u, c, row.Namespace, row.Name, stats); err != nil {
				return err
			}
		}
		if len(rows) < q.Limit {
			return nil
		}
		last := rows[len(rows)-1]
		q.AfterNS, q.AfterName = last.Namespace, last.Name
	}
}

// restrictSkill withdraws a mirrored skill the upstream stopped serving to
// this instance. It is deprecated rather than yanked, because yanking is
// terminal and the upstream may only be hiding the entry for a while. When
// hide is set (the upstream made it private) it also leaves listings and
// anonymous reach, since this instance cannot reproduce the upstream's
// access list.
func (s *Service) restrictSkill(ctx context.Context, u model.Upstream, ns, name, reason string, hide bool, stats syncStats) error {
	local, err := s.Store.GetSkill(ctx, ns, name)
	if errors.Is(err, model.ErrNotFound) || (err == nil && local.Upstream != u.Name) {
		return nil
	}
	if err != nil {
		return err
	}
	actor := "upstream:" + u.Name
	if local.Status == model.StatusActive {
		if _, err := s.Store.SetSkillStatus(ctx, local.ID, model.StatusDeprecated, reason,
			auditEvent(actor, "skill.deprecated", ns, name, map[string]any{"reason": reason})); err != nil {
			return err
		}
		stats.add("skills_updated")
	}
	if hide && (local.Listed || local.Visibility == model.VisibilityPublic) {
		internal, unlisted := model.VisibilityInternal, false
		if _, err := s.Store.UpdateSkill(ctx, local.ID, model.SkillPatch{Visibility: &internal, Listed: &unlisted},
			auditEvent(actor, "skill.update", ns, name, map[string]any{"visibility": internal, "listed": false, "reason": reason})); err != nil {
			return err
		}
		stats.add("skills_updated")
	}
	return nil
}

// mirrorVisibility decides how widely a mirrored entry may be shown: never
// more widely than the upstream shows it, and never more widely than the
// upstream's configured mirror visibility. ok is false for an entry that is
// private upstream, or whose visibility the upstream does not state: its
// access list cannot be reproduced here, so it is not mirrored.
func mirrorVisibility(u model.Upstream, source string) (visibility string, ok bool) {
	switch source {
	case model.VisibilityPublic:
		return u.Visibility, true
	case model.VisibilityInternal:
		return model.VisibilityInternal, true
	}
	return "", false
}

// maxMirrorVersions bounds how many versions of one skill a single refresh
// records, newest first.
const maxMirrorVersions = 100

// mirrorSkill brings one upstream skill up to date.
//
// Versions are recorded oldest first and only when they are newer than every
// version already mirrored, so local order always matches the upstream's and
// "latest" means the same on both sides. Yanks of mirrored versions, status,
// visibility and listing are propagated.
func (s *Service) mirrorSkill(ctx context.Context, u model.Upstream, c *upstream.Client, d upstream.SkillDetail, stats syncStats) error {
	if !skill.ValidNamespaceName(d.Namespace) || !skill.ValidSkillName(d.Name) {
		return invalid("name_invalid", "upstream skill has an unusable name")
	}
	if !u.Allows(d.Namespace) {
		return fail(http.StatusForbidden, "namespace_not_allowed", "namespace is not allowed for this upstream")
	}
	visibility, ok := mirrorVisibility(u, d.Visibility)
	if !ok {
		if err := s.restrictSkill(ctx, u, d.Namespace, d.Name, "no longer shared by the upstream registry", true, stats); err != nil {
			return err
		}
		return fail(http.StatusForbidden, "upstream_private", "the upstream does not share this entry")
	}
	actor := "upstream:" + u.Name
	local, err := s.Store.GetSkill(ctx, d.Namespace, d.Name)
	exists := err == nil
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		return err
	}
	if exists && local.Upstream != u.Name {
		return model.ErrNamespaceManaged()
	}
	have := map[string]model.Version{}
	if exists {
		versions, err := s.Store.ListVersions(ctx, local.ID)
		if err != nil {
			return err
		}
		for _, v := range versions {
			have[v.Version] = v
		}
	}
	// d.Versions is newest first. Everything before the newest version that
	// is already here is new.
	fresh := len(d.Versions)
	for i, ref := range d.Versions {
		if _, ok := have[ref.Version]; ok {
			fresh = i
			break
		}
	}
	changed := false
	for _, ref := range d.Versions[fresh:] {
		if v, ok := have[ref.Version]; ok && ref.Yanked && v.YankedAt == nil {
			if err := s.Store.YankVersion(ctx, v.ID, "yanked upstream", s.Now(),
				auditEvent(actor, "version.yank", d.Namespace, d.Name+"@"+ref.Version, map[string]any{"reason": "yanked upstream"})); err != nil {
				return err
			}
			changed = true
		}
	}
	for i := min(fresh, maxMirrorVersions) - 1; i >= 0; i-- {
		ref := d.Versions[i]
		if !skill.ValidVersion(ref.Version) {
			stats.add("skills_skipped")
			continue
		}
		sv, err := c.GetSkillVersion(ctx, d.Namespace, d.Name, ref.Version)
		if errors.Is(err, upstream.ErrNotFound) {
			stats.add("skills_skipped")
			continue
		}
		if err != nil {
			return err
		}
		sk, err := s.mirrorSkillVersion(ctx, u, sv, visibility, d.Listed)
		switch {
		case err == nil:
			local, exists, changed = sk, true, true
			stats.add("skills_created")
		case isStoreFailure(err):
			return err
		case errors.Is(err, model.ErrConflict):
			var ce *model.ConflictError
			if errors.As(err, &ce) && ce.Code == "namespace_upstream_managed" {
				return err
			}
			stats.add("skills_conflicts")
		default:
			stats.add("skills_skipped")
		}
	}
	if !exists {
		return nil
	}
	if d.Status != local.Status && local.Status != model.StatusYanked &&
		(d.Status == model.StatusActive || d.Status == model.StatusDeprecated || d.Status == model.StatusYanked) {
		reason := clip(d.StatusReason, 500)
		if _, err := s.Store.SetSkillStatus(ctx, local.ID, d.Status, reason,
			auditEvent(actor, "skill."+d.Status, d.Namespace, d.Name, map[string]any{"reason": reason})); err != nil {
			return err
		}
		changed = true
	}
	if local.Visibility != visibility || local.Listed != d.Listed {
		listed := d.Listed
		if _, err := s.Store.UpdateSkill(ctx, local.ID, model.SkillPatch{Visibility: &visibility, Listed: &listed},
			auditEvent(actor, "skill.update", d.Namespace, d.Name, map[string]any{"visibility": visibility, "listed": listed})); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		stats.add("skills_updated")
	} else {
		stats.add("skills_unchanged")
	}
	return nil
}

func hex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// boundedText reports whether s can be stored as is: bounded, valid UTF-8 and
// free of NUL bytes.
func boundedText(s string, max int) bool {
	return len(s) <= max && utf8.ValidString(s) && !strings.Contains(s, "\x00")
}

// mirrorSkillVersion records one upstream version as an indexed version with
// a registry locator. Every field comes from an untrusted peer and is
// checked here; the bytes are verified against the recorded digest and file
// manifest before they are ever served.
func (s *Service) mirrorSkillVersion(ctx context.Context, u model.Upstream, sv upstream.SkillVersion, visibility string, listed bool) (model.Skill, error) {
	bad := func(what string) (model.Skill, error) {
		return model.Skill{}, invalid("upstream_version_invalid", "upstream version has an invalid "+what)
	}
	switch {
	case !skill.ValidNamespaceName(sv.Namespace) || !skill.ValidSkillName(sv.Name) || !skill.ValidVersion(sv.Version):
		return bad("name")
	case sv.ArtifactType != archive.TypeSkillMD && sv.ArtifactType != archive.TypeTar && sv.ArtifactType != archive.TypeBundle:
		return bad("artifact type")
	case !blobstore.IsDigest(sv.ArtifactDigest) || (sv.ArchiveDigest != "" && !blobstore.IsDigest(sv.ArchiveDigest)):
		return bad("digest")
	case sv.ArtifactSize < 0 || sv.ArtifactSize > s.fetchLimit():
		return bad("artifact size")
	case sv.LicenseKind != skill.LicenseSPDX && sv.LicenseKind != skill.LicenseCustom && sv.LicenseKind != skill.LicenseUnknown:
		return bad("license")
	case !boundedText(sv.FrontmatterName, 256) || !boundedText(sv.FrontmatterDescription, 8192) ||
		!boundedText(sv.LicenseID, 256) || !boundedText(sv.LicenseText, maxLicenseText):
		return bad("text field")
	case len(sv.Files) == 0 || len(sv.Files) > s.Cfg.Limits.MaxFiles:
		return bad("file manifest")
	}
	files := make([]skill.ManifestEntry, 0, len(sv.Files))
	for _, f := range sv.Files {
		if skill.ValidatePath(f.Path) != nil || f.Size < 0 || f.Size > s.Cfg.Limits.MaxFileBytes || !hex64(f.SHA256) {
			return bad("file manifest")
		}
		files = append(files, skill.ManifestEntry{Path: f.Path, Size: f.Size, SHA256: f.SHA256})
	}
	slices.SortFunc(files, func(a, b skill.ManifestEntry) int { return strings.Compare(a.Path, b.Path) })
	actor := "upstream:" + u.Name
	if _, err := s.Store.EnsureUpstreamNamespace(ctx, sv.Namespace, u.ID, namespaceAudit(u, sv.Namespace)); err != nil {
		return model.Skill{}, err
	}
	v := model.Version{
		Version: sv.Version, FrontmatterName: sv.FrontmatterName, FrontmatterDescription: sv.FrontmatterDescription,
		LicenseKind: sv.LicenseKind, LicenseID: sv.LicenseID, LicenseApproved: sv.LicenseApproved,
		CoreCompatible: sv.CoreCompatible, Files: files,
		ArtifactDigest: sv.ArtifactDigest, ArtifactType: sv.ArtifactType, ArtifactSize: sv.ArtifactSize, ArchiveDigest: sv.ArchiveDigest,
		Locator: &model.Locator{Kind: "registry", Upstream: u.Name, Namespace: sv.Namespace, Name: sv.Name, Version: sv.Version,
			Artifact: sv.AccessArtifact, FullText: sv.AccessFullText},
		IndexedBy: actor, CreatedBy: actor,
	}
	if sv.AccessFullText {
		v.LicenseText = sv.LicenseText
	}
	sk, stored, err := s.Store.PublishVersion(ctx, model.PublishInput{Namespace: sv.Namespace, Name: sv.Name, Mode: skill.ModeIndexed,
		SourceKey: "upstream:" + u.Name + ":" + sv.Namespace + "/" + sv.Name, Visibility: visibility, Listed: listed,
		UpstreamID: u.ID, Version: v},
		auditEvent(actor, "skill.mirror", sv.Namespace, sv.Name+"@"+sv.Version, map[string]any{"digest": sv.ArtifactDigest, "upstream": u.Name}))
	if err != nil {
		return model.Skill{}, err
	}
	if sv.Yanked {
		reason := clip(sv.YankReason, 500)
		if err := s.Store.YankVersion(ctx, stored.ID, reason, s.Now(),
			auditEvent(actor, "version.yank", sv.Namespace, sv.Name+"@"+sv.Version, map[string]any{"reason": reason})); err != nil {
			return model.Skill{}, err
		}
	}
	return sk, nil
}

// fetchLimit bounds one artifact or discovery entry download.
func (s *Service) fetchLimit() int64 { return s.Cfg.Limits.MaxTotalBytes + 4<<20 }

// buildURLEntry turns the bytes of an agent-skills discovery entry into
// validated files and this registry's artifact for them: the raw SKILL.md,
// or the canonical tar of an archive's files.
func (s *Service) buildURLEntry(entryType string, data []byte) ([]skill.File, string, []byte, error) {
	switch entryType {
	case "skill-md":
		if int64(len(data)) > s.Cfg.Limits.MaxFileBytes {
			return nil, "", nil, invalid("file_too_large", "SKILL.md exceeds the per-file size limit")
		}
		return []skill.File{{Path: skill.SkillFile, Data: data}}, archive.TypeSkillMD, data, nil
	case "archive":
		format := archive.FormatTarGz
		if bytes.HasPrefix(data, []byte("PK\x03\x04")) {
			format = archive.FormatZip
		}
		files, err := archive.Extract(data, format, s.Cfg.Limits)
		if err != nil {
			return nil, "", nil, err
		}
		if files, err = skill.ValidateFiles(files, s.Cfg.Limits); err != nil {
			return nil, "", nil, err
		}
		tarBytes, err := archive.CanonicalTar(files)
		if err != nil {
			return nil, "", nil, err
		}
		return files, archive.TypeTar, tarBytes, nil
	}
	return nil, "", nil, invalid("entry_type_invalid", "unknown discovery entry type")
}

// syncAgentSkills indexes the entries of an agentskills.io discovery index
// into the upstream's target namespace. Such an index has no versions, so
// each distinct digest becomes the version "sha-<first 12 hex digits>".
func (s *Service) syncAgentSkills(ctx context.Context, u model.Upstream, c *upstream.Client, stats syncStats) error {
	entries, dropped, err := c.WellKnownIndex(ctx)
	if err != nil {
		return err
	}
	stats["skills_skipped"] += dropped
	if len(entries) > maxIndexEntries {
		stats["skills_skipped"] += len(entries) - maxIndexEntries
		entries = entries[:maxIndexEntries]
	}
	ns := u.TargetNamespace
	if _, err := s.Store.EnsureUpstreamNamespace(ctx, ns, u.ID, namespaceAudit(u, ns)); err != nil {
		return err
	}
	actor := "upstream:" + u.Name
	for _, e := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !skill.ValidSkillName(e.Name) || !blobstore.IsDigest(e.Digest) || !plainURL(e.URL) {
			stats.add("skills_skipped")
			continue
		}
		version := "sha-" + strings.TrimPrefix(e.Digest, "sha256:")[:12]
		if local, err := s.Store.GetSkill(ctx, ns, e.Name); err == nil {
			if _, err := s.Store.GetVersion(ctx, local.ID, version); err == nil {
				stats.add("skills_unchanged")
				continue
			} else if !errors.Is(err, model.ErrNotFound) {
				return err
			}
		} else if !errors.Is(err, model.ErrNotFound) {
			return err
		}
		data, err := c.Fetch(ctx, e.URL, s.fetchLimit())
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			stats.add("skills_unavailable")
			continue
		}
		if skill.Digest(data) != e.Digest {
			stats.add("skills_digest_mismatch")
			continue
		}
		files, typ, artifact, err := s.buildURLEntry(e.Type, data)
		if err != nil {
			stats.add("skills_skipped")
			continue
		}
		fm, err := skill.ParseFrontmatter(fileData(files, skill.SkillFile))
		if err != nil {
			stats.add("skills_skipped")
			continue
		}
		lic := skill.ClassifyLicense(fm.License)
		licText := ""
		if lic.Kind == skill.LicenseCustom {
			if licText = licenseFileText(files); len(licText) > maxLicenseText {
				licText = ""
			}
		}
		b, err := s.build(files, typ, artifact, lic, licText)
		if err != nil {
			stats.add("skills_skipped")
			continue
		}
		if skill.ContentAccess(skill.ModeIndexed, lic.Kind, false).Artifact {
			_, _ = s.Blobs.Put(ctx, blobstore.Cache, artifact)
		}
		now := s.Now()
		v := b.version
		v.Version = version
		v.Locator = &model.Locator{Kind: "url", URL: e.URL, Digest: e.Digest, EntryType: e.Type, Upstream: u.Name}
		v.IndexedBy, v.CreatedBy, v.CheckResult, v.CheckedAt = actor, actor, model.CheckOK, &now
		_, _, err = s.Store.PublishVersion(ctx, model.PublishInput{Namespace: ns, Name: e.Name, Mode: skill.ModeIndexed,
			SourceKey: "upstream:" + u.Name + ":" + e.Name, Visibility: u.Visibility, Listed: true, UpstreamID: u.ID, Version: v},
			auditEvent(actor, "skill.mirror", ns, e.Name+"@"+version, map[string]any{"digest": v.ArtifactDigest, "upstream": u.Name, "url_digest": e.Digest}))
		switch {
		case err == nil:
			stats.add("skills_created")
		case isStoreFailure(err):
			return err
		case errors.Is(err, model.ErrConflict):
			stats.add("skills_conflicts")
		default:
			stats.add("skills_skipped")
		}
	}
	return nil
}

// readMirrored re-reads a mirrored or URL-indexed version from its source and
// verifies it. It returns a check result ("" when verified) instead of an
// error so the caller records what happened.
func (s *Service) readMirrored(ctx context.Context, v model.Version) ([]skill.File, []byte, string) {
	l := v.Locator
	u, err := s.Store.GetUpstream(ctx, l.Upstream)
	if err != nil || !u.Enabled {
		return nil, nil, model.CheckSourceUnavailable
	}
	c := s.client(u)
	switch l.Kind {
	case "registry":
		data, err := c.GetSkillArtifact(ctx, l.Namespace, l.Name, l.Version, s.fetchLimit())
		if err != nil {
			return nil, nil, model.CheckSourceUnavailable
		}
		if skill.Digest(data) != v.ArtifactDigest {
			return nil, nil, model.CheckSourceChanged
		}
		files, err := s.untrustedFiles(v.ArtifactType, data)
		// The manifest recorded at mirror time came from the upstream's
		// metadata; it must describe exactly these bytes.
		if err != nil || !sameManifest(skill.BuildManifest(files), v.Files) {
			return nil, nil, model.CheckSourceChanged
		}
		return files, data, ""
	case "url":
		data, err := c.Fetch(ctx, l.URL, s.fetchLimit())
		if err != nil {
			return nil, nil, model.CheckSourceUnavailable
		}
		if skill.Digest(data) != l.Digest {
			return nil, nil, model.CheckSourceChanged
		}
		files, typ, artifact, err := s.buildURLEntry(l.EntryType, data)
		if err != nil || typ != v.ArtifactType || skill.Digest(artifact) != v.ArtifactDigest {
			return nil, nil, model.CheckSourceChanged
		}
		return files, artifact, ""
	}
	return nil, nil, model.CheckSourceUnavailable
}

// untrustedFiles decodes artifact bytes received from an upstream. Unlike
// bytes this instance stored itself they may be anything, so every size is
// checked before memory is allocated for it and only the exact encodings
// this registry writes are accepted: a tar must be the canonical tar of its
// own files (which rules out sparse files, PAX headers and padding tricks),
// a bundle the canonical Core bundle.
func (s *Service) untrustedFiles(artifactType string, data []byte) ([]skill.File, error) {
	switch artifactType {
	case archive.TypeSkillMD:
		if int64(len(data)) > s.Cfg.Limits.MaxFileBytes {
			return nil, invalid("file_too_large", "SKILL.md exceeds the per-file size limit")
		}
		return []skill.File{{Path: skill.SkillFile, Data: data}}, nil
	case archive.TypeTar:
		files, err := archive.Extract(data, archive.FormatTar, s.Cfg.Limits)
		if err != nil {
			return nil, err
		}
		canonical, err := archive.CanonicalTar(files)
		if err != nil || !bytes.Equal(canonical, data) {
			return nil, invalid("archive_not_canonical", "the artifact is not a canonical tar")
		}
		return files, nil
	case archive.TypeBundle:
		_, files, err := archive.ParseBundle(data, s.Cfg.Limits)
		return files, err
	}
	return nil, invalid("artifact_type_invalid", "unknown artifact type")
}

// plainURL reports whether a URL found in upstream data may be stored and
// republished: http(s) with no userinfo, query or fragment. A query string is
// where tokens and signatures travel, and every URL this registry publishes
// must be free of them.
func plainURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil &&
		u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
}

func namespaceAudit(u model.Upstream, ns string) model.AuditEvent {
	return auditEvent("upstream:"+u.Name, "namespace.mirror", ns, "", map[string]any{"upstream": u.Name})
}

func sameManifest(a, b []skill.ManifestEntry) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(x, y skill.ManifestEntry) int { return strings.Compare(x.Path, y.Path) }
	a, b = slices.Clone(a), slices.Clone(b)
	slices.SortFunc(a, key)
	slices.SortFunc(b, key)
	for i := range a {
		if a[i].Path != b[i].Path || a[i].Size != b[i].Size || a[i].SHA256 != b[i].SHA256 {
			return false
		}
	}
	return true
}
