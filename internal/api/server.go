// Package api is the HTTP layer: routing under an optional base path,
// authentication, per-route authorization, strict request decoding, security
// headers and JSON presentation.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/auth"
	"github.com/OpenLinker-ai/openlinker-registry/internal/ratelimit"
	"github.com/OpenLinker-ai/openlinker-registry/internal/registry"
)

// Server serves the registry API.
type Server struct {
	Svc            *registry.Service
	Auth           *auth.Authenticator
	BasePath       string
	AllowAnonRead  bool
	MaxUploadBytes int64
	Log            *slog.Logger

	// Per-client limits; nil limiters allow everything.
	ReadLimit    *ratelimit.Limiter
	WriteLimit   *ratelimit.Limiter
	ForwardLimit *ratelimit.Limiter
	// Clients resolves client addresses behind trusted proxies.
	Clients ratelimit.ClientAddr
	// UI serves the built-in web pages when enabled.
	UI bool
	// SecureCookies marks the UI session cookie Secure (https deployments).
	SecureCookies bool
	// Background is the lifetime of work started by a request but finished
	// after it (asynchronous syncs); nil means context.Background().
	Background context.Context
}

const maxJSONBody = 64 << 10

type ctxKey struct{}

func principal(r *http.Request) auth.Principal {
	if p, ok := r.Context().Value(ctxKey{}).(auth.Principal); ok {
		return p
	}
	return auth.Anonymous
}

// Handler returns the full HTTP handler.
func (s *Server) Handler() http.Handler {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	inner := http.NewServeMux()
	s.routes(inner)
	var h http.Handler = inner
	if s.BasePath != "" {
		outer := http.NewServeMux()
		outer.Handle(s.BasePath+"/", http.StripPrefix(s.BasePath, inner))
		outer.HandleFunc("GET /healthz", s.healthz)
		h = outer
	}
	return s.recoverer(s.headers(s.logger(s.authenticate(s.limit(h)))))
}

func (s *Server) routes(m *http.ServeMux) {
	m.HandleFunc("GET /healthz", s.healthz)
	m.HandleFunc("GET /readyz", s.readyz)

	// Discovery.
	m.HandleFunc("GET /.well-known/agent-skills/index.json", s.read(s.wellKnownSite))
	m.HandleFunc("GET /ns/{ns}/.well-known/agent-skills/index.json", s.read(s.wellKnownNamespace))

	// Read API.
	m.HandleFunc("GET /v1/skills", s.read(s.listSkills))
	m.HandleFunc("GET /v1/skills/{ns}/{name}", s.read(s.getSkill))
	m.HandleFunc("GET /v1/skills/{ns}/{name}/versions/{version}", s.read(s.getVersion))
	m.HandleFunc("GET /v1/skills/{ns}/{name}/versions/{version}/artifact", s.read(s.getArtifact))
	m.HandleFunc("GET /v1/skills/{ns}/{name}/versions/{version}/archive.tar.gz", s.read(s.getArchive))
	m.HandleFunc("GET /v1/skills/{ns}/{name}/versions/{version}/files/{path...}", s.read(s.getFile))

	// Publishing (publisher role).
	m.HandleFunc("POST /v1/skills/{ns}/{name}/versions", s.upload)
	m.HandleFunc("POST /v1/skills/{ns}/github-imports/preview", s.githubPreview)
	m.HandleFunc("POST /v1/skills/{ns}/github-imports", s.githubImport)
	m.HandleFunc("POST /v1/skills/{ns}/indexes/preview", s.githubPreview)
	m.HandleFunc("POST /v1/skills/{ns}/indexes", s.githubIndex)
	m.HandleFunc("POST /v1/skills/{ns}/{name}/deprecate", s.setStatus("deprecated"))
	m.HandleFunc("POST /v1/skills/{ns}/{name}/yank", s.setStatus("yanked"))
	m.HandleFunc("POST /v1/skills/{ns}/{name}/versions/{version}/yank", s.yankVersion)
	// Settings (owner role).
	m.HandleFunc("PATCH /v1/skills/{ns}/{name}", s.patchSkill)

	// Admin.
	m.HandleFunc("POST /v1/admin/namespaces", s.admin(s.createNamespace))
	m.HandleFunc("POST /v1/admin/namespaces/{ns}/roles", s.admin(s.setRole))
	m.HandleFunc("POST /v1/admin/tokens", s.admin(s.createToken))
	m.HandleFunc("POST /v1/admin/tokens/{id}/revoke", s.admin(s.revokeToken))
	m.HandleFunc("PUT /v1/admin/skills/{ns}/{name}/alias", s.admin(s.setAlias))
	m.HandleFunc("POST /v1/admin/skills/{ns}/{name}/versions/{version}/approve-license", s.admin(s.approveLicense))
	m.HandleFunc("GET /v1/admin/audit", s.admin(s.listAudit))

	s.mcpRoutes(m)
	s.adminRoutes(m)
	if s.UI {
		s.uiRoutes(m)
	}
}

// --- middleware ---

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.Log.Error("panic", "path", r.URL.Path, "stack", string(debug.Stack()))
				writeError(w, &registry.Error{Status: 500, Code: "internal", Msg: "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// headers applies same-origin hardening (design §7.3) to every response.
func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	n      int64
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.n += int64(n)
	return n, err
}

// logger logs method, path, status, size and duration. Headers, query
// strings and bodies are never logged.
func (s *Server) logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		s.Log.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "bytes", sw.n,
			"duration_ms", time.Since(start).Milliseconds())
	})
}

// authenticate resolves an optional bearer token. A present but invalid
// token is rejected rather than downgraded to anonymous.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get("Authorization")
		if hdr == "" {
			// The built-in pages (and only they) authenticate by session
			// cookie. A stale cookie means "signed out", not an error.
			if s.isUIPath(r) {
				if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
					if p, err := s.Auth.Authenticate(r.Context(), c.Value); err == nil {
						r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, p))
					}
				}
			}
			next.ServeHTTP(w, r)
			return
		}
		scheme, tok, ok := strings.Cut(hdr, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(tok) == "" {
			s.reject(w, r, &registry.Error{Status: http.StatusUnauthorized, Code: "unauthorized", Msg: "invalid authorization header"})
			return
		}
		p, err := s.Auth.Authenticate(r.Context(), strings.TrimSpace(tok))
		if err != nil {
			if !errors.Is(err, auth.ErrInvalidToken) {
				s.Log.Error("authentication backend error", "err", err.Error())
				s.reject(w, r, &registry.Error{Status: http.StatusServiceUnavailable, Code: "unavailable", Msg: "authentication unavailable"})
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			s.reject(w, r, &registry.Error{Status: http.StatusUnauthorized, Code: "unauthorized", Msg: "invalid or expired token"})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}

// read gates read routes: anonymous callers are allowed only when the
// instance permits anonymous reads (public mode by default).
func (s *Server) read(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !principal(r).Authenticated && !s.AllowAnonRead {
			w.Header().Set("WWW-Authenticate", "Bearer")
			s.reject(w, r, &registry.Error{Status: http.StatusUnauthorized, Code: "unauthorized", Msg: "authentication required"})
			return
		}
		h(w, r)
	}
}

func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principal(r)
		if !p.Authenticated {
			writeError(w, &registry.Error{Status: http.StatusUnauthorized, Code: "unauthorized", Msg: "authentication required"})
			return
		}
		if !p.Admin {
			writeError(w, &registry.Error{Status: http.StatusForbidden, Code: "forbidden", Msg: "admin access required"})
			return
		}
		h(w, r)
	}
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(v); err != nil {
		http.Error(w, `{"error":{"code":"internal","message":"encoding failed"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func writeError(w http.ResponseWriter, e *registry.Error) {
	body := map[string]any{"code": e.Code, "message": e.Msg}
	if e.SourceURL != "" {
		body["source_url"] = e.SourceURL
	}
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	writeJSON(w, e.Status, map[string]any{"error": body})
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	e := registry.AsError(err)
	if e.Status >= 500 && e.Code == "internal" {
		s.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err.Error())
	}
	writeError(w, e)
}

// decodeJSON decodes exactly one JSON object with unknown fields rejected.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return &registry.Error{Status: http.StatusUnsupportedMediaType, Code: "content_type", Msg: "expected application/json"}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return &registry.Error{Status: http.StatusRequestEntityTooLarge, Code: "body_too_large", Msg: "request body too large"}
		}
		return &registry.Error{Status: http.StatusBadRequest, Code: "invalid_json", Msg: "invalid JSON body: " + sanitizeJSONErr(err)}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return &registry.Error{Status: http.StatusBadRequest, Code: "invalid_json", Msg: "request body must contain a single JSON object"}
	}
	return nil
}

func sanitizeJSONErr(err error) string {
	msg := err.Error()
	if len(msg) > 120 {
		msg = msg[:120]
	}
	return msg
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Svc.Store.Ping(ctx); err != nil {
		writeError(w, &registry.Error{Status: http.StatusServiceUnavailable, Code: "unavailable", Msg: "database unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
