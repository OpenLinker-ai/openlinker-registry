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
	// A sync walks the whole catalogue: when the upstream throttles it, it
	// waits and goes on instead of failing and starting over.
	// Everything the run writes is conditional on the settings it read
	// (see model.WithUpstreamGuard): if an administrator changes them
	// meanwhile, the run stops instead of writing by the old ones.
	runCtx, cancel := context.WithTimeout(model.WithUpstreamGuard(upstream.WithThrottleRetry(ctx), u.ID, u.ConfigVersion), syncTimeout)
	res := s.runSync(runCtx, u)
	cancel()
	// The cursor is only stored if the settings the run read under are
	// still the upstream's.
	res.ConfigVersion = u.ConfigVersion
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
		// Skills and MCP servers are independent: a failure of one must not
		// keep the other from being reconciled.
		skillErr := s.syncSkills(ctx, u, c, stats)
		var mcpErr error
		if ctx.Err() == nil {
			cursor, mcpErr = s.syncMCP(ctx, u, c, stats)
		}
		err = errors.Join(skillErr, mcpErr)
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

// errUpstreamKind is returned when an upstream configured as a plain MCP
// registry turns out to be an openlinker-registry. Such an upstream states
// per-entry visibility and withdraws entries without leaving a trace in the
// official change feed; only the openlinker-registry kind reconciles that.
var errUpstreamKind = fail(http.StatusConflict, "upstream_kind_mismatch",
	"this upstream is an openlinker-registry; configure it with kind openlinker-registry")

// syncMCP mirrors MCP servers through the official registry read API.
//
// A full run lists everything (deleted versions included) and then looks up,
// by name, every server mirrored earlier that the listing did not show:
// servers the upstream deleted, unlisted or stopped sharing do not appear in
// a listing, and must not stay here as they were.
//
// An openlinker-registry is always read in full: its entries change
// visibility and listing, which the change feed does not report. The
// official registry is read in full the first time and incrementally
// afterwards. The cursor of incremental runs is an RFC 3339 watermark: the
// newest updatedAt among the entries this instance stored, never later than
// the start of the run, so a timestamp in the future or on a rejected entry
// cannot move it past updates that have not been seen. Each run re-reads a
// window before the watermark.
//
// With LatestOnly a full run asks for latest versions only. Incremental runs
// read every change, because a deletion is not a "latest" version and would
// otherwise never arrive; a change to a version that was not mirrored is
// ignored.
func (s *Service) syncMCP(ctx context.Context, u model.Upstream, c *upstream.Client, stats syncStats) (string, error) {
	start := s.Now()
	var since *time.Time
	var watermark time.Time
	if t, err := time.Parse(time.RFC3339Nano, u.Cursor); err == nil && u.Kind == model.UpstreamMCP {
		watermark = t
		from := t.Add(-syncOverlap)
		since = &from
	}
	full := since == nil
	// What a full listing showed, by server name and by name and version.
	seen := map[string]bool{}
	// pending: a change could not be confirmed with the upstream this time.
	// The watermark then stays where it is, so the next run reads the same
	// changes again instead of moving past the one that was not applied.
	pending := false
	page, pageSize := "", syncPageSize
	for i := 0; ; i++ {
		if i == maxSyncPages {
			return "", fail(http.StatusBadGateway, "upstream_too_large", "the upstream listing exceeds the page limit of one sync")
		}
		docs, next, err := c.ListMCP(ctx, upstream.MCPListOptions{Cursor: page, Limit: pageSize, UpdatedSince: since,
			LatestOnly: u.LatestOnly && full, IncludeDeleted: full})
		if tooLarge(err) && pageSize > 1 {
			// A page of large documents can exceed the response cap although
			// each document is fine: ask for less and read the page again.
			pageSize /= 2
			i--
			continue
		}
		if err != nil {
			// A skill-only registry has no MCP API; that is not a failure.
			if i == 0 && u.Kind == model.UpstreamRegistry && errors.Is(err, upstream.ErrNotFound) {
				return "", nil
			}
			return "", err
		}
		for _, doc := range docs {
			seen[doc.Name], seen[doc.Name+"@"+doc.Version] = true, true
			res, err := s.mirrorListedMCP(ctx, u, c, doc, u.LatestOnly && !doc.IsLatest)
			switch {
			case err == nil:
				stats.add("mcp_" + res)
				if doc.UpdatedAt.After(watermark) && !doc.UpdatedAt.After(start) {
					watermark = doc.UpdatedAt
				}
			case errors.Is(err, errUpstreamKind) || isStoreFailure(err) || configChanged(err):
				return "", err
			case errors.Is(err, errUnconfirmed):
				pending = true
				stats.add("mcp_pending")
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
	if full {
		unconfirmed, err := s.reconcileMCP(ctx, u, c, seen, stats)
		if err != nil {
			return "", err
		}
		pending = pending || unconfirmed
	}
	if u.Kind != model.UpstreamMCP {
		return "", nil
	}
	if watermark.IsZero() || pending {
		return u.Cursor, nil
	}
	return watermark.UTC().Format(time.RFC3339Nano), nil
}

// reconcileMCP looks up, by name, every mirrored server of the upstream that
// a full listing did not cover, and applies what the upstream says now: its
// versions' current status, or that the server is gone. A server is covered
// when the listing showed every version mirrored here: a version it did not
// show (a latest-only listing shows one per server; a version may also have
// been removed) may have been deleted or deprecated since.
//
// pending reports that a server could not be read this time. The caller
// then keeps its watermark: incremental runs do not reconcile, so the next
// run has to be a full one again.
func (s *Service) reconcileMCP(ctx context.Context, u model.Upstream, c *upstream.Client, seen map[string]bool, stats syncStats) (pending bool, err error) {
	after := ""
	for {
		servers, err := s.Store.ListMirroredMCP(ctx, u.ID, after, 200)
		if err != nil {
			return pending, err
		}
		for _, srv := range servers {
			after = srv.Name
			if ctx.Err() != nil {
				return pending, ctx.Err()
			}
			covered := seen[srv.Name]
			if covered {
				local, err := s.Store.ListMCPVersions(ctx, srv.ID)
				if err != nil {
					return pending, err
				}
				for _, v := range local {
					covered = covered && seen[srv.Name+"@"+v.Version]
				}
			}
			if covered {
				continue
			}
			switch err := s.refreshMCPServer(ctx, u, c, srv, stats); {
			case errors.Is(err, errUnconfirmed):
				pending = true
				stats.add("mcp_pending")
			case err != nil:
				return pending, err
			}
		}
		if len(servers) < 200 {
			return pending, nil
		}
	}
}

// refreshMCPServer reads every version of one mirrored server from the
// upstream, under the server's mirror lock, and applies them; a server the
// upstream no longer serves is withdrawn.
func (s *Service) refreshMCPServer(ctx context.Context, u model.Upstream, c *upstream.Client, srv model.MCPServer, stats syncStats) error {
	release, err := s.Store.LockMirror(ctx, "mcp:"+srv.Name)
	if err != nil {
		return err
	}
	defer release()
	_, err = s.refreshMCPServerLocked(ctx, u, c, srv.Name, stats)
	var ue *upstream.Error
	if errors.As(err, &ue) {
		// The upstream's answer was unusable. The rest of the sync goes on,
		// but this server is not settled.
		return errUnconfirmed
	}
	return err
}

// reconcileMissingMCP settles a version that a listing reported changed but
// that the upstream does not serve by name any more. The caller holds the
// server's mirror lock. The server's version list is applied, which marks
// every version it no longer contains as deleted.
func (s *Service) reconcileMissingMCP(ctx context.Context, u model.Upstream, c *upstream.Client, doc upstream.MCPDoc) (string, error) {
	if _, err := s.refreshMCPServerLocked(ctx, u, c, doc.Name, syncStats{}); err != nil {
		if isUpstreamError(err) {
			return "", errUnconfirmed
		}
		return "", err
	}
	return model.MirrorUpdated, nil
}

// refreshMCPServerLocked is refreshMCPServer for a caller that holds the
// mirror lock. It returns the versions the upstream serves, or nil when the
// server is not mirrored here or was withdrawn.
func (s *Service) refreshMCPServerLocked(ctx context.Context, u model.Upstream, c *upstream.Client, name string, stats syncStats) (map[string]bool, error) {
	// Read under the lock: a forwarded read may have changed the server
	// since the caller last looked.
	srv, err := s.Store.GetMCPServer(ctx, name)
	if errors.Is(err, model.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	docs, err := c.ListMCPVersions(ctx, srv.Name)
	switch {
	case errors.Is(err, upstream.ErrNotFound):
		return nil, s.withdrawMCP(ctx, u, srv, stats)
	case err != nil:
		return nil, err
	}
	served := map[string]bool{}
	for _, doc := range docs {
		served[doc.Version] = true
		res, err := s.mirrorMCP(ctx, u, doc, u.LatestOnly && !doc.IsLatest, true)
		switch {
		case err == nil:
			stats.add("mcp_" + res)
		case errors.Is(err, errUpstreamKind) || isStoreFailure(err) || configChanged(err):
			return nil, err
		case errors.Is(err, model.ErrConflict):
			stats.add("mcp_conflicts")
		default:
			stats.add("mcp_skipped")
		}
	}
	// A version mirrored here that the upstream's own list no longer has is
	// gone. Only that version changes: which version is latest was just
	// taken from the upstream's list and is not recomputed by version number.
	local, err := s.Store.ListMCPVersions(ctx, srv.ID)
	if err != nil {
		return nil, err
	}
	actor := "upstream:" + u.Name
	for _, v := range local {
		if served[v.Version] || v.Status == model.MCPStatusDeleted {
			continue
		}
		// Status only: the versions applied above may just have changed who
		// sees the server, and that must stand.
		if _, err := s.Store.UpsertMirroredMCP(ctx, model.MCPMirrorInput{UpstreamID: u.ID, ConfigVersion: u.ConfigVersion,
			Namespace: srv.Namespace, Name: srv.Name, Visibility: srv.Visibility, Listed: srv.Listed, Fresh: true, StatusOnly: true,
			Version: model.MCPVersion{Version: v.Version, ServerJSON: v.ServerJSON, Digest: v.Digest, Status: model.MCPStatusDeleted,
				StatusMessage: "no longer available from the upstream registry", CreatedBy: actor}},
			auditEvent(actor, "mcp.mirror", srv.Namespace, srv.Name+"@"+v.Version, map[string]any{"status": model.MCPStatusDeleted, "upstream": u.Name})); err != nil {
			return nil, err
		}
		stats.add("mcp_updated")
	}
	return served, nil
}

// mirrorListedMCP applies a document that came with a listing page. Such a
// document was read some time ago, and a forwarded read may have applied a
// newer one since. So it is only allowed to add a version or to narrow who
// sees the server; if it would do anything else, the version is read again
// under the server's mirror lock and that answer is applied.
func (s *Service) mirrorListedMCP(ctx context.Context, u model.Upstream, c *upstream.Client, doc upstream.MCPDoc, onlyExisting bool) (string, error) {
	res, err := s.mirrorMCP(ctx, u, doc, onlyExisting, false)
	if err != nil || res != model.MirrorStale {
		return res, err
	}
	release, err := s.Store.LockMirror(ctx, "mcp:"+doc.Name)
	if err != nil {
		return "", err
	}
	defer release()
	fresh, err := c.GetMCP(ctx, doc.Name, doc.Version)
	switch {
	case errors.Is(err, upstream.ErrNotFound):
		// The upstream no longer serves this version by name. Settle it now
		// from the server's own version list, rather than leave a change
		// unapplied that no later run would read again.
		return s.reconcileMissingMCP(ctx, u, c, doc)
	case err != nil && isUpstreamError(err):
		// Unreadable right now. Nothing was changed here; the sync must
		// come back to this entry.
		return "", errUnconfirmed
	case err != nil:
		return "", err
	}
	return s.mirrorMCP(ctx, u, fresh, onlyExisting, true)
}

// configChanged reports whether a mirror write was refused because the
// upstream's settings changed after the sync read them. Everything the sync
// would still write was selected by the old settings, so it stops; the next
// run starts under the new ones.
func configChanged(err error) bool {
	var ce *model.ConflictError
	return errors.As(err, &ce) && ce.Code == "upstream_config_changed"
}

// tooLarge reports whether an upstream response exceeded the size cap.
func tooLarge(err error) bool {
	var se *safehttp.Error
	return errors.As(err, &se) && se.Code == safehttp.CodeTooLarge
}

// errUnconfirmed: a listing document needed a fresh read to be applied and
// the upstream could not be read.
var errUnconfirmed = fail(http.StatusBadGateway, "upstream_unconfirmed", "the upstream could not confirm a change")

// syncSkills mirrors the skills of another openlinker-registry: metadata
// now, bytes on first read (see Materialize). The native API has no change
// feed and its listing does not show every change (yanking an old version
// changes neither the latest version nor the status), so each run reads the
// detail of every listed skill, and then of every skill mirrored earlier
// that the listing no longer shows (yanked, unlisted or no longer shared).
func (s *Service) syncSkills(ctx context.Context, u model.Upstream, c *upstream.Client, stats syncStats) error {
	cursor, pageSize := "", syncPageSize
	listed := map[string]bool{}
	for i := 0; ; i++ {
		if i == maxSyncPages {
			return fail(http.StatusBadGateway, "upstream_too_large", "the upstream listing exceeds the page limit of one sync")
		}
		items, next, err := c.ListSkills(ctx, cursor, pageSize)
		if tooLarge(err) && pageSize > 1 {
			pageSize /= 2
			i--
			continue
		}
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
	afterNS, afterName := "", ""
	for {
		rows, err := s.Store.ListMirroredSkills(ctx, u.ID, afterNS, afterName, 200)
		if err != nil {
			return err
		}
		for _, row := range rows {
			afterNS, afterName = row.Namespace, row.Name
			if listed[row.Namespace+"/"+row.Name] {
				continue
			}
			if err := s.refreshSkill(ctx, u, c, row.Namespace, row.Name, stats); err != nil {
				return err
			}
		}
		if len(rows) < 200 {
			return nil
		}
	}
}

// refreshSkill reads one skill from the upstream and brings the local mirror
// up to date; a skill the upstream no longer serves to this instance is
// withdrawn. It is the only way a mirrored skill is written. Only failures of this instance or of the transport are
// returned: a rejected or malformed entry is counted and skipped, so it
// cannot keep the rest of a sync from running.
func (s *Service) refreshSkill(ctx context.Context, u model.Upstream, c *upstream.Client, ns, name string, stats syncStats) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Reading and applying happen under the skill's mirror lock, so a sync
	// and a forwarded read (in any registry process) cannot apply their
	// answers in the wrong order.
	release, err := s.Store.LockMirror(ctx, "skill:"+ns+"/"+name)
	if err != nil {
		return err
	}
	defer release()
	detail, err := c.GetSkill(ctx, ns, name)
	var ue *upstream.Error
	switch {
	case errors.Is(err, upstream.ErrNotFound):
		return s.withdrawSkill(ctx, u, ns, name, stats)
	case errors.As(err, &ue):
		stats.add("skills_skipped")
		return nil
	case err != nil:
		return err
	}
	err = s.mirrorSkill(ctx, u, c, detail, stats)
	switch {
	case err == nil:
	case isStoreFailure(err) || configChanged(err):
		return err
	case errors.Is(err, model.ErrConflict):
		stats.add("skills_conflicts")
	default:
		stats.add("skills_skipped")
	}
	return nil
}

// withdrawnReason is recorded on entries an upstream stopped sharing.
const withdrawnReason = "no longer shared by the upstream registry"

// withdrawSkill takes a mirrored skill out of reach when the upstream no
// longer serves it to this instance: it becomes private and unlisted (only
// administrators still see it) and is marked deprecated. It is not yanked,
// because yanking is terminal; if the upstream shares the skill again, the
// next refresh restores it.
func (s *Service) withdrawSkill(ctx context.Context, u model.Upstream, ns, name string, stats syncStats) error {
	local, err := s.Store.GetSkill(ctx, ns, name)
	if errors.Is(err, model.ErrNotFound) || (err == nil && local.Upstream != u.Name) {
		return nil
	}
	if err != nil {
		return err
	}
	actor := "upstream:" + u.Name
	changed := false
	if local.Visibility != model.VisibilityPrivate || local.Listed {
		private, unlisted := model.VisibilityPrivate, false
		if _, err := s.Store.UpdateSkill(ctx, local.ID, model.SkillPatch{Visibility: &private, Listed: &unlisted},
			auditEvent(actor, "skill.withdraw", ns, name, map[string]any{"reason": withdrawnReason})); err != nil {
			return err
		}
		changed = true
	}
	if local.Status == model.StatusActive {
		if _, err := s.Store.SetSkillStatus(ctx, local.ID, model.StatusDeprecated, withdrawnReason,
			auditEvent(actor, "skill.deprecated", ns, name, map[string]any{"reason": withdrawnReason})); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		stats.add("skills_withdrawn")
	}
	return nil
}

// withdrawMCP is withdrawSkill for an MCP server. A server that is gone from
// a registry with official semantics (where nothing is ever private) is
// marked deleted instead, which is what that registry would have reported.
func (s *Service) withdrawMCP(ctx context.Context, u model.Upstream, srv model.MCPServer, stats syncStats) error {
	if srv.Upstream != u.Name {
		return nil
	}
	actor := "upstream:" + u.Name
	if u.Kind == model.UpstreamMCP {
		n, err := s.Store.SetMCPStatus(ctx, srv.ID, "", model.MCPStatusDeleted, "no longer available from the upstream registry", s.Now(),
			auditEvent(actor, "mcp.status", srv.Namespace, srv.Name, map[string]any{"status": model.MCPStatusDeleted}))
		if err == nil && n > 0 {
			stats.add("mcp_withdrawn")
		}
		return err
	}
	if srv.Visibility == model.VisibilityPrivate && !srv.Listed {
		return nil
	}
	private, unlisted := model.VisibilityPrivate, false
	if _, err := s.Store.UpdateMCPServer(ctx, srv.ID, model.MCPPatch{Visibility: &private, Listed: &unlisted},
		auditEvent(actor, "mcp.withdraw", srv.Namespace, srv.Name, map[string]any{"reason": withdrawnReason})); err != nil {
		return err
	}
	stats.add("mcp_withdrawn")
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

// maxSkillVersions is the most versions a mirrored skill may have upstream.
// A skill is mirrored with all of its versions or not at all, so that no
// version is ever silently missing.
const maxSkillVersions = 1000

// mirrorSkill brings one upstream skill up to date.
//
// Restrictions come first: visibility, listing and status are applied
// before any new version is recorded, so nothing new is ever visible under
// settings the upstream has already withdrawn.
//
// Versions are recorded oldest first and only when newer than every version
// already mirrored; the store refuses a version when the newest local
// version is not the one this refresh expects. Local order therefore always
// matches the upstream's, whatever else runs concurrently, and "latest"
// means the same on both sides.
func (s *Service) mirrorSkill(ctx context.Context, u model.Upstream, c *upstream.Client, d upstream.SkillDetail, stats syncStats) error {
	if !skill.ValidNamespaceName(d.Namespace) || !skill.ValidSkillName(d.Name) {
		return invalid("name_invalid", "upstream skill has an unusable name")
	}
	if !u.Allows(d.Namespace) {
		return fail(http.StatusForbidden, "namespace_not_allowed", "namespace is not allowed for this upstream")
	}
	visibility, ok := mirrorVisibility(u, d.Visibility)
	if !ok {
		if err := s.withdrawSkill(ctx, u, d.Namespace, d.Name, stats); err != nil {
			return err
		}
		return fail(http.StatusForbidden, "upstream_private", "the upstream does not share this entry")
	}
	if len(d.Versions) > maxSkillVersions {
		return invalid("too_many_versions", "upstream skill has too many versions to mirror")
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
	// A yank is final and nothing of a yanked skill is served: there is
	// nothing to gain from recording more of its history, and the store does
	// not add versions to a yanked skill. A skill that was never mirrored is
	// not created just to be yanked.
	if d.Status == model.StatusYanked {
		if !exists {
			return invalid("upstream_yanked", "the upstream yanked this skill")
		}
		_, changed, err := s.applySkillSettings(ctx, actor, local, d, visibility)
		if err != nil {
			return err
		}
		if changed {
			stats.add("skills_updated")
		} else {
			stats.add("skills_unchanged")
		}
		return nil
	}
	changed := false
	if exists {
		if local, changed, err = s.applySkillSettings(ctx, actor, local, d, visibility); err != nil {
			return err
		}
	}
	have := map[string]model.Version{}
	newest := ""
	if exists {
		versions, err := s.Store.ListVersions(ctx, local.ID)
		if err != nil {
			return err
		}
		for i, v := range versions {
			if i == 0 {
				newest = v.Version
			}
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
	// What can change about a version after it was published: it is yanked,
	// a license is approved, and with that the upstream starts serving it.
	for _, ref := range d.Versions[fresh:] {
		v, ok := have[ref.Version]
		if !ok {
			continue
		}
		target := d.Name + "@" + ref.Version
		if ref.Yanked && v.YankedAt == nil {
			if err := s.Store.YankVersion(ctx, v.ID, "yanked upstream", s.Now(),
				auditEvent(actor, "version.yank", d.Namespace, target, map[string]any{"reason": "yanked upstream"})); err != nil {
				return err
			}
			changed = true
		}
		// An upstream that predates these fields does not state them in the
		// detail. A version it withheld may have become servable; only its
		// own record tells.
		if ref.Servable == nil && (v.MirrorServed == nil || !*v.MirrorServed) && v.YankedAt == nil && !ref.Yanked {
			sv, err := c.GetSkillVersion(ctx, d.Namespace, d.Name, ref.Version)
			var ue *upstream.Error
			switch {
			case err == nil:
				served, approved := sv.AccessArtifact && sv.AccessFullText, sv.LicenseApproved
				ref.Servable, ref.LicenseApproved = &served, &approved
			case errors.Is(err, upstream.ErrNotFound) || errors.As(err, &ue):
			default:
				return err
			}
		}
		if ref.LicenseApproved != nil && *ref.LicenseApproved && !v.LicenseApproved {
			if err := s.Store.ApproveLicense(ctx, v.ID, auditEvent(actor, "license.approve", d.Namespace, target,
				map[string]any{"reason": "approved upstream"})); err != nil {
				return err
			}
			changed = true
		}
		if ref.Servable != nil && (v.MirrorServed == nil || *v.MirrorServed != *ref.Servable) {
			if err := s.Store.SetVersionMirrorServed(ctx, v.ID, *ref.Servable, auditEvent(actor, "version.served", d.Namespace, target,
				map[string]any{"served": *ref.Servable})); err != nil {
				return err
			}
			changed = true
		}
	}
	created := false
	// Oldest first, stopping at the first version that cannot be recorded,
	// whatever the reason (not served yet, malformed, beyond this instance's
	// limits). Versions are only ever added after the newest one here, so
	// going on would leave that version out for good, even after the reason
	// is gone; instead the skill waits and every refresh tries again.
	for i := fresh - 1; i >= 0; i-- {
		ref := d.Versions[i]
		if !skill.ValidVersion(ref.Version) {
			stats.add("skills_incomplete")
			break
		}
		sv, err := c.GetSkillVersion(ctx, d.Namespace, d.Name, ref.Version)
		var ue *upstream.Error
		if errors.Is(err, upstream.ErrNotFound) || errors.As(err, &ue) {
			stats.add("skills_incomplete")
			break
		}
		if err != nil {
			return err
		}
		sk, err := s.mirrorSkillVersion(ctx, u, sv, visibility, d.Listed, newest)
		var ce *model.ConflictError
		switch {
		case err == nil:
			if !exists {
				created = true
			}
			local, exists, changed, newest = sk, true, true, sv.Version
			stats.add("skills_created")
		case isStoreFailure(err):
			return err
		case errors.As(err, &ce) && (ce.Code == "namespace_upstream_managed" || ce.Code == "version_order" || ce.Code == "upstream_config_changed"):
			// Not ours to write, or another refresh of this skill is
			// running: leave the rest to it.
			return err
		default:
			stats.add("skills_incomplete")
		}
		if err != nil {
			break
		}
	}
	if created {
		// A skill created just now took its visibility and listing from the
		// first version; its status is still to be set.
		if _, _, err = s.applySkillSettings(ctx, actor, local, d, visibility); err != nil {
			return err
		}
	}
	if changed {
		stats.add("skills_updated")
	} else {
		stats.add("skills_unchanged")
	}
	return nil
}

// applySkillSettings makes visibility, listing and status of a mirrored
// skill what the upstream says they are.
func (s *Service) applySkillSettings(ctx context.Context, actor string, local model.Skill, d upstream.SkillDetail, visibility string) (model.Skill, bool, error) {
	changed := false
	if local.Visibility != visibility || local.Listed != d.Listed {
		listed := d.Listed
		out, err := s.Store.UpdateSkill(ctx, local.ID, model.SkillPatch{Visibility: &visibility, Listed: &listed},
			auditEvent(actor, "skill.update", d.Namespace, d.Name, map[string]any{"visibility": visibility, "listed": listed}))
		if err != nil {
			return local, false, err
		}
		local, changed = out, true
	}
	if d.Status != local.Status && local.Status != model.StatusYanked &&
		(d.Status == model.StatusActive || d.Status == model.StatusDeprecated || d.Status == model.StatusYanked) {
		reason := clip(d.StatusReason, 500)
		out, err := s.Store.SetSkillStatus(ctx, local.ID, d.Status, reason,
			auditEvent(actor, "skill."+d.Status, d.Namespace, d.Name, map[string]any{"reason": reason}))
		if err != nil {
			return local, false, err
		}
		local, changed = out, true
	}
	return local, changed, nil
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
func (s *Service) mirrorSkillVersion(ctx context.Context, u model.Upstream, sv upstream.SkillVersion, visibility string, listed bool, newest string) (model.Skill, error) {
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
	served := sv.AccessArtifact && sv.AccessFullText
	v := model.Version{
		Version: sv.Version, FrontmatterName: sv.FrontmatterName, FrontmatterDescription: sv.FrontmatterDescription,
		LicenseKind: sv.LicenseKind, LicenseID: sv.LicenseID, LicenseApproved: sv.LicenseApproved,
		CoreCompatible: sv.CoreCompatible, Files: files,
		ArtifactDigest: sv.ArtifactDigest, ArtifactType: sv.ArtifactType, ArtifactSize: sv.ArtifactSize, ArchiveDigest: sv.ArchiveDigest,
		Locator:      &model.Locator{Kind: "registry", Upstream: u.Name, Namespace: sv.Namespace, Name: sv.Name, Version: sv.Version},
		MirrorServed: &served,
		IndexedBy:    actor, CreatedBy: actor,
	}
	if sv.AccessFullText {
		v.LicenseText = sv.LicenseText
	}
	sk, stored, err := s.Store.PublishVersion(ctx, model.PublishInput{Namespace: sv.Namespace, Name: sv.Name, Mode: skill.ModeIndexed,
		SourceKey: "upstream:" + u.Name + ":" + sv.Namespace + "/" + sv.Name, Visibility: visibility, Listed: listed,
		UpstreamID: u.ID, UpstreamConfigVersion: u.ConfigVersion, ExpectNewest: &newest, Version: v},
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
	// Every name the index lists, before any cap: an entry is only withdrawn
	// when the index really no longer has it.
	indexed := map[string]bool{}
	for _, e := range entries {
		indexed[e.Name] = true
	}
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
			// An entry that is (again) in the index is shown as the upstream
			// is configured now, whether or not its content changed.
			restored, err := s.restoreIndexSkill(ctx, u, local)
			if err != nil {
				return err
			}
			if _, err := s.Store.GetVersion(ctx, local.ID, version); err == nil {
				if restored {
					stats.add("skills_updated")
				} else {
					stats.add("skills_unchanged")
				}
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
			SourceKey: "upstream:" + u.Name + ":" + e.Name, Visibility: u.Visibility, Listed: true, UpstreamID: u.ID,
			UpstreamConfigVersion: u.ConfigVersion, Version: v},
			auditEvent(actor, "skill.mirror", ns, e.Name+"@"+version, map[string]any{"digest": v.ArtifactDigest, "upstream": u.Name, "url_digest": e.Digest}))
		switch {
		case err == nil:
			stats.add("skills_created")
		case isStoreFailure(err) || configChanged(err):
			return err
		case errors.Is(err, model.ErrConflict):
			stats.add("skills_conflicts")
		default:
			stats.add("skills_skipped")
		}
	}
	// Entries mirrored earlier that the index no longer lists are withdrawn.
	afterNS, afterName := "", ""
	for {
		rows, err := s.Store.ListMirroredSkills(ctx, u.ID, afterNS, afterName, 200)
		if err != nil {
			return err
		}
		for _, row := range rows {
			afterNS, afterName = row.Namespace, row.Name
			if row.Namespace != ns || indexed[row.Name] {
				continue
			}
			if err := s.withdrawSkill(ctx, u, ns, row.Name, stats); err != nil {
				return err
			}
		}
		if len(rows) < 200 {
			return nil
		}
	}
}

// restoreIndexSkill gives a skill that the discovery index lists the
// visibility its upstream is configured with, lists it, and lifts a
// withdrawal. It reports whether anything changed.
func (s *Service) restoreIndexSkill(ctx context.Context, u model.Upstream, local model.Skill) (bool, error) {
	actor := "upstream:" + u.Name
	changed := false
	if local.Visibility != u.Visibility || !local.Listed {
		visibility, listed := u.Visibility, true
		if _, err := s.Store.UpdateSkill(ctx, local.ID, model.SkillPatch{Visibility: &visibility, Listed: &listed},
			auditEvent(actor, "skill.update", local.Namespace, local.Name, map[string]any{"visibility": visibility, "listed": true})); err != nil {
			return false, err
		}
		changed = true
	}
	if local.Status == model.StatusDeprecated && local.StatusReason == withdrawnReason {
		if _, err := s.Store.SetSkillStatus(ctx, local.ID, model.StatusActive, "",
			auditEvent(actor, "skill.active", local.Namespace, local.Name, map[string]any{"reason": "listed again by the upstream"})); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
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
