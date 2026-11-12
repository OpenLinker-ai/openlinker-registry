package registry

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/OpenLinker-ai/openlinker-registry/internal/mcp"
	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/safehttp"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
	"github.com/OpenLinker-ai/openlinker-registry/internal/upstream"
)

// Upstreams are other registries this instance forwards reads to and mirrors
// from. Content only flows from an upstream to this instance: nothing stored
// here is ever pushed, and a request forwarded upstream carries only the
// public name of the entry being read (design §9).

// UpstreamInput creates an upstream.
type UpstreamInput struct {
	Name                string   `json:"name"`
	Kind                string   `json:"kind"`
	BaseURL             string   `json:"base_url"`
	Enabled             *bool    `json:"enabled"`
	Priority            *int     `json:"priority"`
	Visibility          string   `json:"visibility"`
	NamespaceAllow      []string `json:"namespace_allow"`
	NamespaceDeny       []string `json:"namespace_deny"`
	TargetNamespace     string   `json:"target_namespace"`
	SyncIntervalSeconds *int     `json:"sync_interval_seconds"`
	ProxyOnMiss         *bool    `json:"proxy_on_miss"`
	LatestOnly          *bool    `json:"latest_only"`
}

// UpstreamUpdate changes upstream settings. Name, kind and target namespace
// are fixed at creation.
type UpstreamUpdate struct {
	BaseURL             *string   `json:"base_url"`
	Enabled             *bool     `json:"enabled"`
	Priority            *int      `json:"priority"`
	Visibility          *string   `json:"visibility"`
	NamespaceAllow      *[]string `json:"namespace_allow"`
	NamespaceDeny       *[]string `json:"namespace_deny"`
	SyncIntervalSeconds *int      `json:"sync_interval_seconds"`
	ProxyOnMiss         *bool     `json:"proxy_on_miss"`
	LatestOnly          *bool     `json:"latest_only"`
}

const (
	maxUpstreamPatterns = 200
	minSyncInterval     = time.Minute
	maxSyncInterval     = 30 * 24 * time.Hour
	// syncTimeout bounds one sync run; a run that has not finished after
	// syncStale is assumed dead and may be taken over.
	syncTimeout = 30 * time.Minute
	syncStale   = 45 * time.Minute
	// syncOverlap re-reads a window before the stored watermark so entries
	// updated while the previous run was paging are not missed.
	syncOverlap  = 5 * time.Minute
	maxSyncPages = 5000
	syncPageSize = 100
	// maxIndexEntries bounds one agent-skills discovery index.
	maxIndexEntries = 2000
	missTTL         = time.Minute
	maxMisses       = 10000
)

func invalid(code, msg string) error { return fail(http.StatusUnprocessableEntity, code, msg) }

// checkBaseURL validates an upstream base URL against the outbound policy
// and returns it without a trailing slash.
func (s *Service) checkBaseURL(raw string) (string, error) {
	u, err := safehttp.CheckURL(raw, s.Cfg.UpstreamPolicy)
	if err != nil {
		var se *safehttp.Error
		if errors.As(err, &se) {
			return "", invalid("base_url_invalid", "base_url: "+se.Msg)
		}
		return "", invalid("base_url_invalid", "base_url must be an absolute https URL")
	}
	if u.RawQuery != "" || len(raw) > 2048 {
		return "", invalid("base_url_invalid", "base_url must not carry a query string and must be at most 2048 bytes")
	}
	out := strings.TrimRight(u.String(), "/")
	// Forwarding to itself would loop.
	if pub, err := url.Parse(s.Cfg.PublicURL); err == nil && pub.Host != "" && strings.EqualFold(pub.Host, u.Host) &&
		(out == s.Cfg.PublicURL || strings.HasPrefix(out+"/", s.Cfg.PublicURL+"/")) {
		return "", invalid("upstream_self", "an upstream cannot be this registry")
	}
	return out, nil
}

func (s *Service) checkMirrorVisibility(v string) error {
	if v != model.VisibilityPublic && v != model.VisibilityInternal {
		return invalid("visibility_invalid", "mirrored entries are public or internal")
	}
	return s.checkVisibility(v)
}

func checkPatterns(field string, ps []string) error {
	if len(ps) > maxUpstreamPatterns {
		return invalid("pattern_invalid", field+" has too many patterns")
	}
	for _, p := range ps {
		if !model.ValidNamespacePattern(p) {
			return invalid("pattern_invalid", field+` patterns are a namespace, a prefix ending in ".*", or "*"`)
		}
	}
	return nil
}

func checkInterval(seconds int) (time.Duration, error) {
	d := time.Duration(seconds) * time.Second
	if seconds < 0 || (d != 0 && (d < minSyncInterval || d > maxSyncInterval)) {
		return 0, invalid("sync_interval_invalid", "sync_interval_seconds must be 0 (manual) or between 60 and 2592000")
	}
	return d, nil
}

// CreateUpstream validates and stores an upstream. For an agent-skills
// upstream the target namespace is reserved for it immediately.
func (s *Service) CreateUpstream(ctx context.Context, actor string, in UpstreamInput) (model.Upstream, error) {
	if !skill.ValidSlug(in.Name) || len(in.Name) > 40 {
		return model.Upstream{}, invalid("name_invalid", "upstream name must be a lowercase slug of at most 40 characters")
	}
	if !model.ValidUpstreamKind(in.Kind) {
		return model.Upstream{}, invalid("kind_invalid", "kind must be openlinker-registry, mcp-registry or agent-skills")
	}
	base, err := s.checkBaseURL(in.BaseURL)
	if err != nil {
		return model.Upstream{}, err
	}
	u := model.Upstream{Name: in.Name, Kind: in.Kind, BaseURL: base, Enabled: true, Priority: 100,
		Visibility: in.Visibility, NamespaceAllow: in.NamespaceAllow, NamespaceDeny: in.NamespaceDeny,
		TargetNamespace: in.TargetNamespace}
	if u.Visibility == "" {
		u.Visibility = s.DefaultVisibility()
	}
	if err := s.checkMirrorVisibility(u.Visibility); err != nil {
		return model.Upstream{}, err
	}
	if err := checkPatterns("namespace_allow", u.NamespaceAllow); err != nil {
		return model.Upstream{}, err
	}
	if err := checkPatterns("namespace_deny", u.NamespaceDeny); err != nil {
		return model.Upstream{}, err
	}
	if (in.Kind == model.UpstreamAgentSkills) != (in.TargetNamespace != "") {
		return model.Upstream{}, invalid("target_namespace_invalid", "target_namespace is required for agent-skills upstreams and not allowed otherwise")
	}
	if in.TargetNamespace != "" && !skill.ValidNamespaceName(in.TargetNamespace) {
		return model.Upstream{}, invalid("target_namespace_invalid", "target_namespace must be a lowercase reverse-DNS namespace")
	}
	if in.Enabled != nil {
		u.Enabled = *in.Enabled
	}
	if in.Priority != nil {
		if *in.Priority < 0 || *in.Priority > 100000 {
			return model.Upstream{}, invalid("priority_invalid", "priority must be between 0 and 100000")
		}
		u.Priority = *in.Priority
	}
	if in.SyncIntervalSeconds != nil {
		if u.SyncInterval, err = checkInterval(*in.SyncIntervalSeconds); err != nil {
			return model.Upstream{}, err
		}
	}
	if in.ProxyOnMiss != nil {
		u.ProxyOnMiss = *in.ProxyOnMiss
	}
	if in.LatestOnly != nil {
		u.LatestOnly = *in.LatestOnly
	}
	if u.ProxyOnMiss && u.Kind == model.UpstreamAgentSkills {
		return model.Upstream{}, invalid("proxy_on_miss_invalid", "agent-skills upstreams have no per-entry lookup; use a sync interval")
	}
	out, err := s.Store.CreateUpstream(ctx, u, auditEvent(actor, "upstream.create", "", u.Name,
		map[string]any{"kind": u.Kind, "base_url": u.BaseURL, "visibility": u.Visibility}))
	if err != nil {
		return model.Upstream{}, err
	}
	s.resetForwarding()
	if out.TargetNamespace != "" {
		if _, err := s.Store.EnsureUpstreamNamespace(ctx, out.TargetNamespace, out.ID, namespaceAudit(out, out.TargetNamespace)); err != nil {
			_ = s.Store.DeleteUpstream(ctx, out.Name, auditEvent(actor, "upstream.delete", "", out.Name,
				map[string]any{"reason": "target namespace unavailable"}))
			return model.Upstream{}, err
		}
	}
	return out, nil
}

// UpdateUpstream changes upstream settings.
func (s *Service) UpdateUpstream(ctx context.Context, actor, name string, in UpstreamUpdate) (model.Upstream, error) {
	cur, err := s.Store.GetUpstream(ctx, name)
	if err != nil {
		return model.Upstream{}, err
	}
	p := model.UpstreamPatch{Enabled: in.Enabled, ProxyOnMiss: in.ProxyOnMiss, LatestOnly: in.LatestOnly,
		NamespaceAllow: in.NamespaceAllow, NamespaceDeny: in.NamespaceDeny}
	details := map[string]any{}
	if in.BaseURL != nil {
		base, err := s.checkBaseURL(*in.BaseURL)
		if err != nil {
			return model.Upstream{}, err
		}
		p.BaseURL, details["base_url"] = &base, base
	}
	if in.Visibility != nil {
		if err := s.checkMirrorVisibility(*in.Visibility); err != nil {
			return model.Upstream{}, err
		}
		p.Visibility, details["visibility"] = in.Visibility, *in.Visibility
	}
	if in.Priority != nil {
		if *in.Priority < 0 || *in.Priority > 100000 {
			return model.Upstream{}, invalid("priority_invalid", "priority must be between 0 and 100000")
		}
		p.Priority = in.Priority
	}
	if in.NamespaceAllow != nil {
		if err := checkPatterns("namespace_allow", *in.NamespaceAllow); err != nil {
			return model.Upstream{}, err
		}
		details["namespace_allow"] = *in.NamespaceAllow
	}
	if in.NamespaceDeny != nil {
		if err := checkPatterns("namespace_deny", *in.NamespaceDeny); err != nil {
			return model.Upstream{}, err
		}
		details["namespace_deny"] = *in.NamespaceDeny
	}
	if in.SyncIntervalSeconds != nil {
		d, err := checkInterval(*in.SyncIntervalSeconds)
		if err != nil {
			return model.Upstream{}, err
		}
		p.SyncInterval = &d
	}
	if in.ProxyOnMiss != nil && *in.ProxyOnMiss && cur.Kind == model.UpstreamAgentSkills {
		return model.Upstream{}, invalid("proxy_on_miss_invalid", "agent-skills upstreams have no per-entry lookup; use a sync interval")
	}
	if in.Enabled != nil {
		details["enabled"] = *in.Enabled
	}
	out, err := s.Store.UpdateUpstream(ctx, name, p, auditEvent(actor, "upstream.update", "", name, details))
	if err == nil {
		s.clients.drop(name)
		s.resetForwarding()
	}
	return out, err
}

// DeleteUpstream removes an upstream that never mirrored anything.
func (s *Service) DeleteUpstream(ctx context.Context, actor, name string) error {
	err := s.Store.DeleteUpstream(ctx, name, auditEvent(actor, "upstream.delete", "", name, nil))
	if err == nil {
		s.clients.drop(name)
		s.resetForwarding()
	}
	return err
}

// clientCache keeps one protocol client per upstream so what a client learns
// about its upstream (the MCP API prefix) survives across requests.
type clientCache struct {
	mu sync.Mutex
	m  map[string]*upstream.Client
}

func (c *clientCache) drop(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, name)
}

func (s *Service) client(u model.Upstream) *upstream.Client {
	token := ""
	if s.Cfg.UpstreamToken != nil {
		token = s.Cfg.UpstreamToken(u.Name)
	}
	s.clients.mu.Lock()
	defer s.clients.mu.Unlock()
	if c := s.clients.m[u.Name]; c != nil && c.BaseURL == u.BaseURL && c.Token == token {
		return c
	}
	if s.clients.m == nil {
		s.clients.m = map[string]*upstream.Client{}
	}
	c := &upstream.Client{HTTP: s.HTTP, BaseURL: u.BaseURL, Token: token, UserAgent: "openlinker-registry"}
	s.clients.m[u.Name] = c
	return c
}

// missCache remembers recent upstream misses so repeated reads of a name
// that exists nowhere do not turn into repeated outbound requests.
type missCache struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func (c *missCache) recent(key string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.m[key]
	if ok && now.After(until) {
		delete(c.m, key)
		return false
	}
	return ok
}

func (c *missCache) add(key string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= maxMisses {
		c.m = map[string]time.Time{}
	}
	c.m[key] = now.Add(missTTL)
}

func errUpstreamUnavailable() error {
	return fail(http.StatusBadGateway, "upstream_unavailable", "an upstream registry could not be reached")
}

// forwardTargets returns the upstreams a miss in namespace ns may be
// forwarded to for this caller, in priority order.
//
// A namespace that exists locally is never forwarded: local ownership wins,
// and a namespace mirrored from one upstream is only ever completed from
// that upstream. An upstream is only consulted for callers who could see
// what it mirrors, so nobody can make this instance fetch (with its
// credentials) or store entries that would then be hidden from them.
func (s *Service) forwardTargets(ctx context.Context, viewer model.Viewer, ns string, kinds ...string) ([]model.Upstream, error) {
	var owner int64 = -1
	if n, err := s.Store.GetNamespace(ctx, ns); err == nil {
		if n.UpstreamID == 0 {
			return nil, nil
		}
		owner = n.UpstreamID
	} else if !errors.Is(err, model.ErrNotFound) {
		return nil, err
	}
	all, err := s.Store.ListUpstreams(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.Upstream
	for _, u := range all {
		if u.Enabled && u.ProxyOnMiss && slices.Contains(kinds, u.Kind) && u.Allows(ns) &&
			(owner == -1 || owner == u.ID) && viewer.CanSee(ns, u.Visibility) {
			out = append(out, u)
		}
	}
	return out, nil
}

// CanForwardSkill reports whether a miss on a skill in ns could be forwarded
// for this caller. The API asks before it charges the caller's forwarding
// budget, so a caller who cannot be served by any upstream gets the same
// answer for a hidden entry as for a missing one.
func (s *Service) CanForwardSkill(ctx context.Context, viewer model.Viewer, ns string) bool {
	if !skill.ValidNamespaceName(ns) || !s.Forwarding(ctx) {
		return false
	}
	targets, err := s.forwardTargets(ctx, viewer, ns, model.UpstreamRegistry)
	return err == nil && len(targets) > 0
}

// CanForwardMCP is CanForwardSkill for an MCP server name.
func (s *Service) CanForwardMCP(ctx context.Context, viewer model.Viewer, name string) bool {
	ns, _, ok := mcp.SplitName(name)
	if !ok || !s.Forwarding(ctx) {
		return false
	}
	targets, err := s.forwardTargets(ctx, viewer, ns, model.UpstreamMCP, model.UpstreamRegistry)
	return err == nil && len(targets) > 0
}

// forwardMCP looks name@version up on the upstreams and mirrors the first
// answer. It returns nil when the version is now stored locally.
func (s *Service) forwardMCP(ctx context.Context, viewer model.Viewer, name, version string) error {
	ns, _, ok := mcp.SplitName(name)
	if !ok {
		return model.ErrNotFound
	}
	key := "mcp\x00" + name + "\x00" + version
	if s.misses.recent(key, s.Now()) {
		return model.ErrNotFound
	}
	targets, err := s.forwardTargets(ctx, viewer, ns, model.UpstreamMCP, model.UpstreamRegistry)
	if err != nil {
		return err
	}
	unavailable := false
	for _, u := range targets {
		doc, err := s.client(u).GetMCP(ctx, name, version)
		if errors.Is(err, upstream.ErrNotFound) {
			continue
		}
		if err != nil {
			unavailable = true
			s.Log.Warn("upstream read failed", "upstream", u.Name, "kind", "mcp", "err", err.Error())
			continue
		}
		if doc.Name != name {
			continue
		}
		if _, err := s.mirrorMCP(ctx, u, doc, false); err != nil {
			if isStoreFailure(err) {
				return err
			}
			s.Log.Warn("upstream entry not mirrored", "upstream", u.Name, "name", name, "err", err.Error())
			continue
		}
		return nil
	}
	if unavailable {
		return errUpstreamUnavailable()
	}
	s.misses.add(key, s.Now())
	return model.ErrNotFound
}

// isStoreFailure separates infrastructure failures (which abort a sync or a
// forwarded read) from per-entry rejections (which are counted and skipped).
func isStoreFailure(err error) bool {
	var ce *model.ConflictError
	var pe *model.PolicyError
	var me *mcp.Error
	var re *Error
	var ve *skill.ValidationError
	return !(errors.As(err, &ce) || errors.As(err, &pe) || errors.As(err, &me) || errors.As(err, &re) ||
		errors.As(err, &ve) || errors.Is(err, model.ErrNotFound))
}

// clip makes text from an upstream storable: valid UTF-8 without NUL bytes
// (PostgreSQL text accepts neither), at most max bytes, cut on a rune
// boundary. One malformed string must not fail a whole sync.
func clip(s string, max int) string {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\x00", "")
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// mirrorMCP stores one upstream MCP document as a mirrored version. The
// document is validated leniently and credentials are redacted; the stored
// bytes are the canonical form, so the digest is this registry's own.
//
// An openlinker-registry upstream states how widely it shows each entry, and
// the mirror never shows it more widely (see mirrorVisibility). With
// onlyExisting the document is applied only to a version that is already
// mirrored; the outcome is then "ignored" for anything else.
func (s *Service) mirrorMCP(ctx context.Context, u model.Upstream, doc upstream.MCPDoc, onlyExisting bool) (string, error) {
	p, err := mcp.Parse(doc.Server, mcp.Options{})
	if err != nil {
		return "", err
	}
	if p.Name != doc.Name || p.Version != doc.Version {
		return "", &mcp.Error{Code: "name_invalid", Msg: "upstream document does not match its listing"}
	}
	if !u.Allows(p.Namespace) {
		return "", fail(http.StatusForbidden, "namespace_not_allowed", "namespace is not allowed for this upstream")
	}
	actor := "upstream:" + u.Name
	visibility, listed, stated := u.Visibility, true, false
	// The official registry states no visibility (everything it serves is
	// public). An openlinker-registry always does, whichever kind it was
	// configured as, and one that does not is not trusted to be public.
	if u.Kind == model.UpstreamRegistry || doc.Visibility != "" {
		var ok bool
		if visibility, ok = mirrorVisibility(u, doc.Visibility); !ok {
			if err := s.restrictMCP(ctx, u, p.Name); err != nil {
				return "", err
			}
			return "", fail(http.StatusForbidden, "upstream_private", "the upstream does not share this entry")
		}
		listed, stated = !doc.Unlisted, true
	}
	if onlyExisting {
		srv, err := s.Store.GetMCPServer(ctx, p.Name)
		if err == nil {
			_, err = s.Store.GetMCPVersion(ctx, srv.ID, p.Version)
		}
		if errors.Is(err, model.ErrNotFound) {
			return "ignored", nil
		}
		if err != nil {
			return "", err
		}
	}
	status, msg := doc.Status, clip(doc.StatusMessage, model.MaxMCPStatusMessage)
	if !model.ValidMCPStatus(status) {
		status = model.MCPStatusActive
	}
	if status == model.MCPStatusActive {
		msg = ""
	}
	outcome, err := s.Store.UpsertMirroredMCP(ctx, model.MCPMirrorInput{UpstreamID: u.ID, Namespace: p.Namespace, Name: p.Name,
		Visibility: visibility, Listed: listed, Version: model.MCPVersion{Version: p.Version, ServerJSON: p.Canonical, Digest: p.Digest,
			Status: status, StatusMessage: msg, StatusChangedAt: doc.StatusChangedAt, PublishedAt: doc.PublishedAt,
			IsLatest: doc.IsLatest && status != model.MCPStatusDeleted, CreatedBy: actor}},
		auditEvent(actor, "mcp.mirror", p.Namespace, p.Name+"@"+p.Version, map[string]any{"digest": p.Digest, "status": status, "upstream": u.Name}))
	if err != nil || !stated || outcome == model.MirrorCreated {
		return outcome, err
	}
	// Visibility and listing are set when the server is created; follow later
	// changes upstream.
	srv, err := s.Store.GetMCPServer(ctx, p.Name)
	if err != nil {
		return "", err
	}
	if srv.Visibility != visibility || srv.Listed != listed {
		if _, err := s.Store.UpdateMCPServer(ctx, srv.ID, model.MCPPatch{Visibility: &visibility, Listed: &listed},
			auditEvent(actor, "mcp.update", srv.Namespace, srv.Name, map[string]any{"visibility": visibility, "listed": listed})); err != nil {
			return "", err
		}
		if outcome == model.MirrorUnchanged {
			outcome = model.MirrorUpdated
		}
	}
	return outcome, nil
}

// restrictMCP withdraws a mirrored server the upstream stopped sharing: it
// leaves listings and anonymous reach and its versions are marked deleted.
func (s *Service) restrictMCP(ctx context.Context, u model.Upstream, name string) error {
	srv, err := s.Store.GetMCPServer(ctx, name)
	if errors.Is(err, model.ErrNotFound) || (err == nil && srv.Upstream != u.Name) {
		return nil
	}
	if err != nil {
		return err
	}
	actor := "upstream:" + u.Name
	const reason = "no longer shared by the upstream registry"
	if srv.Listed || srv.Visibility == model.VisibilityPublic {
		internal, unlisted := model.VisibilityInternal, false
		if _, err := s.Store.UpdateMCPServer(ctx, srv.ID, model.MCPPatch{Visibility: &internal, Listed: &unlisted},
			auditEvent(actor, "mcp.update", srv.Namespace, srv.Name, map[string]any{"visibility": internal, "listed": false, "reason": reason})); err != nil {
			return err
		}
	}
	_, err = s.Store.SetMCPStatus(ctx, srv.ID, "", model.MCPStatusDeleted, reason, s.Now(),
		auditEvent(actor, "mcp.status", srv.Namespace, srv.Name, map[string]any{"status": model.MCPStatusDeleted, "message": reason}))
	return err
}

// ResolveSkill returns a skill, forwarding a local miss to the upstream
// registries that serve this caller. Visibility of the result is the caller's
// concern.
func (s *Service) ResolveSkill(ctx context.Context, viewer model.Viewer, ns, name string, forward bool) (model.Skill, error) {
	sk, err := s.Store.GetSkill(ctx, ns, name)
	if err == nil || !errors.Is(err, model.ErrNotFound) || !forward {
		return sk, err
	}
	if err := s.forwardSkill(ctx, viewer, ns, name); err != nil {
		return model.Skill{}, err
	}
	return s.Store.GetSkill(ctx, ns, name)
}

// RefreshMirroredSkill re-reads a mirrored skill from its own upstream, for
// a caller asking for a version that is not here yet: the upstream may have
// published it since the last sync. It reports whether a refresh ran.
func (s *Service) RefreshMirroredSkill(ctx context.Context, viewer model.Viewer, sk model.Skill, version string) (bool, error) {
	if sk.Upstream == "" || !skill.ValidVersion(version) {
		return false, nil
	}
	key := "skillversion\x00" + sk.Namespace + "\x00" + sk.Name + "\x00" + version
	if s.misses.recent(key, s.Now()) {
		return false, nil
	}
	u, err := s.Store.GetUpstream(ctx, sk.Upstream)
	if err != nil || !u.Enabled || !u.ProxyOnMiss || u.Kind != model.UpstreamRegistry || !viewer.CanSee(sk.Namespace, u.Visibility) {
		return false, nil
	}
	if err := s.refreshSkill(ctx, u, s.client(u), sk.Namespace, sk.Name, syncStats{}); err != nil {
		if isStoreFailure(err) && !isUpstreamError(err) {
			return false, err
		}
		s.Log.Warn("upstream read failed", "upstream", u.Name, "kind", "skill", "err", err.Error())
		return false, errUpstreamUnavailable()
	}
	s.misses.add(key, s.Now())
	return true, nil
}

func (s *Service) forwardSkill(ctx context.Context, viewer model.Viewer, ns, name string) error {
	if !skill.ValidNamespaceName(ns) || !skill.ValidSkillName(name) {
		return model.ErrNotFound
	}
	key := "skill\x00" + ns + "\x00" + name
	if s.misses.recent(key, s.Now()) {
		return model.ErrNotFound
	}
	targets, err := s.forwardTargets(ctx, viewer, ns, model.UpstreamRegistry)
	if err != nil {
		return err
	}
	unavailable := false
	for _, u := range targets {
		if err := s.refreshSkill(ctx, u, s.client(u), ns, name, syncStats{}); err != nil {
			if isStoreFailure(err) && !isUpstreamError(err) {
				return err
			}
			unavailable = true
			s.Log.Warn("upstream read failed", "upstream", u.Name, "kind", "skill", "err", err.Error())
			continue
		}
		if _, err := s.Store.GetSkill(ctx, ns, name); err == nil {
			return nil
		}
	}
	if unavailable {
		return errUpstreamUnavailable()
	}
	s.misses.add(key, s.Now())
	return model.ErrNotFound
}

// forwardingTTL bounds how stale the cached answer of Forwarding may be.
const forwardingTTL = 15 * time.Second

type forwardingCache struct {
	mu    sync.Mutex
	until time.Time
	on    bool
}

// Forwarding reports whether any enabled upstream forwards local misses.
// The answer is cached briefly: it is consulted on every miss.
func (s *Service) Forwarding(ctx context.Context) bool {
	now := s.Now()
	s.forwarding.mu.Lock()
	defer s.forwarding.mu.Unlock()
	if now.Before(s.forwarding.until) {
		return s.forwarding.on
	}
	ups, err := s.Store.ListUpstreams(ctx)
	if err != nil {
		return s.forwarding.on
	}
	on := false
	for _, u := range ups {
		on = on || (u.Enabled && u.ProxyOnMiss)
	}
	s.forwarding.on, s.forwarding.until = on, now.Add(forwardingTTL)
	return on
}

func (s *Service) resetForwarding() {
	s.forwarding.mu.Lock()
	defer s.forwarding.mu.Unlock()
	s.forwarding.until = time.Time{}
}
