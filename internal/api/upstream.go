package api

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/ratelimit"
	"github.com/OpenLinker-ai/openlinker-registry/internal/registry"
)

func (s *Server) adminRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /v1/whoami", s.whoami)
	m.HandleFunc("GET /v1/admin/namespaces", s.admin(s.listNamespaces))
	m.HandleFunc("GET /v1/admin/tokens", s.admin(s.listTokens))
	m.HandleFunc("GET /v1/admin/upstreams", s.admin(s.listUpstreams))
	m.HandleFunc("POST /v1/admin/upstreams", s.admin(s.createUpstream))
	m.HandleFunc("GET /v1/admin/upstreams/{name}", s.admin(s.getUpstream))
	m.HandleFunc("PATCH /v1/admin/upstreams/{name}", s.admin(s.patchUpstream))
	m.HandleFunc("DELETE /v1/admin/upstreams/{name}", s.admin(s.deleteUpstream))
	m.HandleFunc("POST /v1/admin/upstreams/{name}/sync", s.admin(s.syncUpstream))
}

// --- rate limiting ---

// clientKey identifies the caller for rate limiting: the token subject when
// authenticated, the client network address otherwise.
func (s *Server) clientKey(r *http.Request) string {
	if p := principal(r); p.Authenticated {
		return "sub:" + p.Subject
	}
	return ratelimit.Key(s.Clients.From(r))
}

func tooManyRequests(wait time.Duration) *registry.Error {
	return &registry.Error{Status: http.StatusTooManyRequests, Code: "rate_limited", Msg: "too many requests; retry later",
		RetryAfter: int(math.Ceil(wait.Seconds()))}
}

// limit applies the read or write limit of the caller. Only the health
// probes themselves are exempt.
func (s *Server) limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", s.BasePath + "/healthz", s.BasePath + "/readyz":
			next.ServeHTTP(w, r)
			return
		}
		l := s.WriteLimit
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			l = s.ReadLimit
		}
		if ok, wait := l.Allow(s.clientKey(r)); !ok {
			s.reject(w, r, tooManyRequests(wait))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// chargeForward draws one forwarded read from the caller's budget. Callers
// first establish that an upstream could serve this caller at all.
func (s *Server) chargeForward(r *http.Request) error {
	if ok, wait := s.ForwardLimit.Allow(s.clientKey(r)); !ok {
		return tooManyRequests(wait)
	}
	return nil
}

// --- identity and listings ---

func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	roles := map[string]model.Role{}
	for ns, role := range p.Roles {
		roles[ns] = role
	}
	scopes := p.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	s.setCache(w, r, false)
	writeJSON(w, http.StatusOK, map[string]any{"subject": p.Subject, "authenticated": p.Authenticated, "admin": p.Admin,
		"scopes": scopes, "roles": roles, "mode": s.Svc.Cfg.Mode, "anonymous_read": s.AllowAnonRead})
}

func (s *Server) listNamespaces(w http.ResponseWriter, r *http.Request) {
	all, err := s.Svc.Store.ListNamespaces(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ups, err := s.Svc.Store.ListUpstreams(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	names := map[int64]string{}
	for _, u := range ups {
		names[u.ID] = u.Name
	}
	out := make([]map[string]any, 0, len(all))
	for _, ns := range all {
		item := map[string]any{"name": ns.Name, "kind": ns.Kind, "github_id": ns.GitHubID, "display_name": ns.DisplayName, "created_at": ns.CreatedAt}
		if ns.UpstreamID != 0 {
			item["upstream"] = names[ns.UpstreamID]
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"namespaces": out})
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	all, err := s.Svc.Store.ListTokens(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(all))
	for _, t := range all {
		out = append(out, map[string]any{"id": t.ID, "subject": "token:" + t.ID, "name": t.Name, "scopes": t.Scopes,
			"created_by": t.CreatedBy, "created_at": t.CreatedAt, "expires_at": t.ExpiresAt, "revoked_at": t.RevokedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// --- upstreams ---

func (s *Server) upstreamJSON(u model.Upstream) map[string]any {
	stats := u.LastSyncStats
	if stats == nil {
		stats = map[string]int{}
	}
	out := map[string]any{
		"name": u.Name, "kind": u.Kind, "base_url": u.BaseURL, "enabled": u.Enabled, "priority": u.Priority,
		"visibility": u.Visibility, "namespace_allow": orEmpty(u.NamespaceAllow), "namespace_deny": orEmpty(u.NamespaceDeny),
		"sync_interval_seconds": int(u.SyncInterval / time.Second), "proxy_on_miss": u.ProxyOnMiss, "latest_only": u.LatestOnly,
		// Whether a token is configured, never the token.
		"token_configured": s.Svc.Cfg.UpstreamToken != nil && s.Svc.Cfg.UpstreamToken(u.Name) != "",
		"sync": map[string]any{"state": u.SyncState, "started_at": u.SyncStartedAt, "last_at": u.LastSyncAt,
			"last_status": nullable(u.LastSyncStatus), "last_error": nullable(u.LastSyncError), "last_stats": stats,
			"cursor": nullable(u.Cursor)},
		"created_at": u.CreatedAt, "updated_at": u.UpdatedAt,
	}
	if u.TargetNamespace != "" {
		out["target_namespace"] = u.TargetNamespace
	}
	return out
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (s *Server) listUpstreams(w http.ResponseWriter, r *http.Request) {
	all, err := s.Svc.Store.ListUpstreams(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(all))
	for _, u := range all {
		out = append(out, s.upstreamJSON(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"upstreams": out})
}

func (s *Server) getUpstream(w http.ResponseWriter, r *http.Request) {
	u, err := s.Svc.Store.GetUpstream(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.upstreamJSON(u))
}

func (s *Server) createUpstream(w http.ResponseWriter, r *http.Request) {
	var req registry.UpstreamInput
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	u, err := s.Svc.CreateUpstream(r.Context(), principal(r).Subject, req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.upstreamJSON(u))
}

func (s *Server) patchUpstream(w http.ResponseWriter, r *http.Request) {
	var req registry.UpstreamUpdate
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	u, err := s.Svc.UpdateUpstream(r.Context(), principal(r).Subject, r.PathValue("name"), req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.upstreamJSON(u))
}

func (s *Server) deleteUpstream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.Svc.DeleteUpstream(r.Context(), principal(r).Subject, name); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "deleted": true})
}

// syncUpstream starts a sync. With ?wait=true the request returns when the
// run has finished; otherwise the run continues in the background and its
// result is read from the upstream's sync state.
func (s *Server) syncUpstream(w http.ResponseWriter, r *http.Request) {
	name, actor := r.PathValue("name"), principal(r).Subject
	wait := false
	if v := r.URL.Query().Get("wait"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			s.fail(w, r, badQuery("wait must be a boolean"))
			return
		}
		wait = b
	}
	if wait {
		u, err := s.Svc.SyncUpstream(r.Context(), actor, name)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, s.upstreamJSON(u))
		return
	}
	u, err := s.Svc.Store.GetUpstream(r.Context(), name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !u.Enabled {
		s.fail(w, r, &registry.Error{Status: http.StatusConflict, Code: "upstream_disabled", Msg: "this upstream is disabled"})
		return
	}
	bg := s.Background
	if bg == nil {
		bg = context.Background()
	}
	go func() {
		if _, err := s.Svc.SyncUpstream(bg, actor, name); err != nil {
			s.Log.Warn("upstream sync failed", "upstream", name, "err", err.Error())
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"name": name, "sync": "started"})
}
