package api

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/auth"
	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/ratelimit"
	"github.com/OpenLinker-ai/openlinker-registry/internal/registry"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
)

// The built-in pages are a read-only browser for private instances (design
// §10): server-rendered HTML, no scripts, no third-party resources. Writes
// stay in the API. The pages authenticate with a session cookie that holds
// an API token; the cookie is scoped to the page paths, is never read by API
// routes, and is SameSite=Strict, so the JSON API cannot be driven
// cross-site through it.

//go:embed ui/*.html ui/style.css
var uiFS embed.FS

var uiTmpl = template.Must(template.New("ui").Funcs(template.FuncMap{
	"date": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format("2006-01-02 15:04")
	},
	"datep": func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.UTC().Format("2006-01-02 15:04")
	},
	"short": func(d string) string {
		d = strings.TrimPrefix(d, "sha256:")
		if len(d) > 12 {
			return d[:12]
		}
		return d
	},
	"esc": url.PathEscape,
}).ParseFS(uiFS, "ui/*.html"))

const (
	sessionCookie = "olr_session"
	uiCSP         = "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
	uiPageSize    = 30
)

func (s *Server) uiRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.BasePath+"/ui/", http.StatusSeeOther)
	})
	m.HandleFunc("GET /ui/static/style.css", s.uiStyle)
	m.HandleFunc("GET /ui/login", s.uiLoginForm)
	m.HandleFunc("POST /ui/login", s.uiLogin)
	m.HandleFunc("POST /ui/logout", s.uiLogout)
	m.HandleFunc("GET /ui/{$}", s.uiGate(s.uiSkills))
	m.HandleFunc("GET /ui/skills/{ns}/{name}", s.uiGate(s.uiSkill))
	m.HandleFunc("GET /ui/skills/{ns}/{name}/versions/{version}", s.uiGate(s.uiSkill))
	// Downloads for signed-in visitors: the API routes do not read the
	// session cookie, so the pages link to these copies of them.
	m.HandleFunc("GET /ui/skills/{ns}/{name}/versions/{version}/artifact", s.uiGate(s.getArtifact))
	m.HandleFunc("GET /ui/skills/{ns}/{name}/versions/{version}/archive.tar.gz", s.uiGate(s.getArchive))
	m.HandleFunc("GET /ui/mcp", s.uiGate(s.uiMCPList))
	m.HandleFunc("GET /ui/mcp/{ns}/{name}", s.uiGate(s.uiMCPServer))
	m.HandleFunc("GET /ui/mcp/{ns}/{name}/versions/{version}", s.uiGate(s.uiMCPServer))
	m.HandleFunc("GET /ui/upstreams", s.uiGate(s.uiUpstreams))
}

// isUIPath reports whether the request targets the built-in pages; only
// those read the session cookie.
func (s *Server) isUIPath(r *http.Request) bool {
	return s.UI && strings.HasPrefix(r.URL.Path, s.BasePath+"/ui/")
}

// uiPage is the data every page template receives.
type uiPage struct {
	Lang   string
	Base   string
	Title  string
	Tab    string
	User   auth.Principal
	Mode   string
	Query  string
	Error  string
	Notice string
	Data   any
}

// T translates a UI string key.
func (p *uiPage) T(key string) string {
	if s, ok := uiStrings[p.Lang][key]; ok {
		return s
	}
	if s, ok := uiStrings["en"][key]; ok {
		return s
	}
	return key
}

func uiLang(r *http.Request) string {
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		switch {
		case strings.HasPrefix(tag, "zh"):
			return "zh"
		case strings.HasPrefix(tag, "en"):
			return "en"
		}
	}
	return "en"
}

func (s *Server) uiRender(w http.ResponseWriter, r *http.Request, status int, name string, p *uiPage) {
	p.Lang, p.Base, p.User, p.Mode = uiLang(r), s.BasePath, principal(r), s.Svc.Cfg.Mode
	var buf bytes.Buffer
	if err := uiTmpl.ExecuteTemplate(&buf, name, p); err != nil {
		s.Log.Error("page rendering failed", "page", name, "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Security-Policy", uiCSP)
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "private, no-store")
	h.Add("Vary", "Cookie")
	h.Add("Vary", "Accept-Language")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) uiError(w http.ResponseWriter, r *http.Request, err error) {
	e := registry.AsError(err)
	if e.Status >= 500 && e.Code == "internal" {
		s.Log.Error("page failed", "path", r.URL.Path, "err", err.Error())
	}
	s.uiRender(w, r, e.Status, "error", &uiPage{Title: http.StatusText(e.Status), Error: e.Msg, Data: e.Status})
}

// uiGate sends callers who may not read to the login page.
func (s *Server) uiGate(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !principal(r).Authenticated && !s.AllowAnonRead {
			http.Redirect(w, r, s.BasePath+"/ui/login", http.StatusSeeOther)
			return
		}
		h(w, r)
	}
}

func (s *Server) uiStyle(w http.ResponseWriter, _ *http.Request) {
	css, _ := uiFS.ReadFile("ui/style.css")
	h := w.Header()
	h.Set("Content-Type", "text/css; charset=utf-8")
	h.Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(css)
}

// --- session ---

// sameOrigin rejects cross-site form posts. Browsers send Sec-Fetch-Site on
// every request; older ones are checked through Origin.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func (s *Server) setSession(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: value, Path: s.BasePath + "/ui/", MaxAge: maxAge,
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteStrictMode})
}

func (s *Server) uiLoginForm(w http.ResponseWriter, r *http.Request) {
	s.uiRender(w, r, http.StatusOK, "login", &uiPage{Title: "Sign in", Tab: "login"})
}

func (s *Server) uiLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		s.uiRender(w, r, http.StatusForbidden, "login", &uiPage{Title: "Sign in", Tab: "login", Error: "cross-site request refused"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	token := strings.TrimSpace(r.PostFormValue("token"))
	addr := ratelimit.Key(s.Clients.From(r))
	if over, _ := s.AuthFailLimit.Exhausted(addr); over {
		s.uiRender(w, r, http.StatusTooManyRequests, "login", &uiPage{Title: "Sign in", Tab: "login", Error: "too many failed attempts; retry later"})
		return
	}
	if _, err := s.Auth.Authenticate(r.Context(), token); err != nil {
		status, msg := http.StatusUnauthorized, "invalid or expired token"
		if !errors.Is(err, auth.ErrInvalidToken) {
			status, msg = http.StatusServiceUnavailable, "authentication unavailable"
		} else {
			s.AuthFailLimit.Allow(addr)
		}
		s.uiRender(w, r, status, "login", &uiPage{Title: "Sign in", Tab: "login", Error: msg})
		return
	}
	s.setSession(w, token, 12*60*60)
	http.Redirect(w, r, s.BasePath+"/ui/", http.StatusSeeOther)
}

func (s *Server) uiLogout(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		s.uiError(w, r, &registry.Error{Status: http.StatusForbidden, Code: "forbidden", Msg: "cross-site request refused"})
		return
	}
	s.setSession(w, "", -1)
	http.Redirect(w, r, s.BasePath+"/ui/login", http.StatusSeeOther)
}

// --- skills ---

type uiSkillRow struct {
	model.SkillSummary
}

type uiSkillList struct {
	Rows []uiSkillRow
	Mode string
	Next string
}

func (s *Server) uiSkills(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	q := model.SearchQuery{Viewer: principal(r).Viewer(), Q: strings.TrimSpace(qv.Get("q")), Limit: uiPageSize + 1}
	if len(q.Q) > 200 {
		q.Q = q.Q[:200]
	}
	if m := qv.Get("mode"); m == skill.ModeHosted || m == skill.ModeIndexed {
		q.Mode = m
	}
	if ns, name, ok := decodeCursor(qv.Get("cursor")); ok {
		q.AfterNS, q.AfterName = ns, name
	}
	rows, err := s.Svc.Store.Search(r.Context(), q)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	data := uiSkillList{Mode: q.Mode}
	if len(rows) > uiPageSize {
		rows = rows[:uiPageSize]
		last := rows[len(rows)-1]
		next := url.Values{"cursor": {encodeCursor(last.Namespace, last.Name)}}
		if q.Q != "" {
			next.Set("q", q.Q)
		}
		if q.Mode != "" {
			next.Set("mode", q.Mode)
		}
		// A complete URL: the template escapes a value placed after "?" as
		// one query component, which would swallow the "=" and "&".
		data.Next = s.BasePath + "/ui/?" + next.Encode()
	}
	for _, row := range rows {
		data.Rows = append(data.Rows, uiSkillRow{row})
	}
	s.uiRender(w, r, http.StatusOK, "skills", &uiPage{Title: "Skills", Tab: "skills", Query: q.Q, Data: data})
}

type uiSkillDetail struct {
	Skill    model.Skill
	Version  model.Version
	Versions []model.Version
	Access   skill.Access
	// SkillMD is the text of SKILL.md when it may be shown.
	SkillMD     string
	ContentNote string
	SourceURL   string
	ArtifactURL string
	ArchiveURL  string
	// Install is the skills CLI command, set when the version is in the
	// namespace discovery index.
	Install string
}

func (s *Server) uiSkill(w http.ResponseWriter, r *http.Request) {
	sk, err := s.loadVisible(r)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	versions, err := s.Svc.Store.ListVersions(r.Context(), sk.ID)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	d := uiSkillDetail{Skill: sk, Versions: versions}
	want := r.PathValue("version")
	found := false
	if want == "" {
		d.Version, found = model.LatestVersion(sk.CurrentVersion, versions)
	}
	for _, v := range versions {
		if want != "" && v.Version == want {
			d.Version, found = v, true
			break
		}
	}
	if !found && want == "" && len(versions) > 0 {
		d.Version, found = versions[0], true
	}
	if !found {
		s.uiError(w, r, errNotFound)
		return
	}
	v := d.Version
	d.Access, d.SourceURL = registry.Access(v), s.Svc.SourceURL(v)
	if d.Access.Artifact && v.YankedAt == nil && sk.Status != model.StatusYanked {
		base := s.BasePath + "/ui/skills/" + url.PathEscape(sk.Namespace) + "/" + url.PathEscape(sk.Name) +
			"/versions/" + url.PathEscape(v.Version)
		d.ArtifactURL = base + "/artifact"
		if v.ArchiveDigest != "" {
			d.ArchiveURL = base + "/archive.tar.gz"
		}
	}
	if d.Access.FullText {
		files, _, err := s.Svc.Materialize(r.Context(), sk, v, true)
		if err != nil {
			d.ContentNote = registry.AsError(err).Msg
		}
		for _, f := range files {
			if f.Path == skill.SkillFile {
				d.SkillMD = string(f.Data)
			}
		}
	} else {
		d.ContentNote = "content is not redistributed by this registry; use the source link"
	}
	if _, ok, _ := s.Svc.Eligible(model.IndexCandidate{Skill: sk, Version: v}, sk.Name); ok && want == "" {
		d.Install = "npx skills add " + s.Svc.Cfg.PublicURL + "/ns/" + sk.Namespace + " --skill " + sk.Name
	}
	s.uiRender(w, r, http.StatusOK, "skill", &uiPage{Title: sk.Namespace + "/" + sk.Name, Tab: "skills", Data: d})
}

// --- MCP servers ---

type uiMCPRow struct {
	Server      model.MCPServer
	Version     model.MCPVersion
	Title       string
	Description string
	Path        string
}

type uiMCPList struct {
	Rows []uiMCPRow
	Next string
}

// mcpDoc is the subset of server.json the pages display.
type mcpDoc struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	WebsiteURL  string `json:"websiteUrl"`
	Repository  struct {
		URL string `json:"url"`
	} `json:"repository"`
	Remotes []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"remotes"`
	Packages []struct {
		RegistryType string `json:"registryType"`
		Identifier   string `json:"identifier"`
		Version      string `json:"version"`
		RuntimeHint  string `json:"runtimeHint"`
		Transport    struct {
			Type string `json:"type"`
		} `json:"transport"`
	} `json:"packages"`
}

// mcpPath is the page path of a server: its name with each part escaped.
func mcpPath(name string) string {
	ns, server, _ := strings.Cut(name, "/")
	return url.PathEscape(ns) + "/" + url.PathEscape(server)
}

// safeLink returns u only when it is an http(s) URL, so untrusted metadata
// can never produce a javascript: or data: link.
func safeLink(u string) string {
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" {
		return ""
	}
	return u
}

func (s *Server) uiMCPList(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	q := model.MCPQuery{Viewer: principal(r).Viewer(), Search: strings.TrimSpace(qv.Get("q")), Version: "latest", Limit: uiPageSize + 1}
	if len(q.Search) > 200 {
		q.Search = q.Search[:200]
	}
	if name, version, ok := decodeMCPCursor(qv.Get("cursor")); ok {
		q.AfterName, q.AfterVersion = name, version
	}
	rows, err := s.Svc.Store.SearchMCP(r.Context(), q)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	var data uiMCPList
	if len(rows) > uiPageSize {
		rows = rows[:uiPageSize]
		last := rows[len(rows)-1]
		next := url.Values{"cursor": {encodeMCPCursor(last.Server.Name, last.Version.Version)}}
		if q.Search != "" {
			next.Set("q", q.Search)
		}
		data.Next = s.BasePath + "/ui/mcp?" + next.Encode()
	}
	for _, e := range rows {
		var doc mcpDoc
		_ = json.Unmarshal(e.Version.ServerJSON, &doc)
		data.Rows = append(data.Rows, uiMCPRow{Server: e.Server, Version: e.Version, Title: doc.Title,
			Description: doc.Description, Path: mcpPath(e.Server.Name)})
	}
	s.uiRender(w, r, http.StatusOK, "mcp", &uiPage{Title: "MCP servers", Tab: "mcp", Query: q.Search, Data: data})
}

type uiMCPDetail struct {
	Server     model.MCPServer
	Version    model.MCPVersion
	Versions   []model.MCPVersion
	Doc        mcpDoc
	Path       string
	Website    string
	Repository string
	JSON       string
	// Config is a client configuration snippet for the first remote.
	Config string
}

func (s *Server) uiMCPServer(w http.ResponseWriter, r *http.Request) {
	version := r.PathValue("version")
	if version == "" {
		version = "latest"
	}
	srv, v, err := s.loadVisibleMCP(r, serverName(r), version)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	versions, err := s.Svc.Store.ListMCPVersions(r.Context(), srv.ID)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	d := uiMCPDetail{Server: srv, Version: v, Versions: versions, Path: mcpPath(srv.Name)}
	_ = json.Unmarshal(v.ServerJSON, &d.Doc)
	d.Website, d.Repository = safeLink(d.Doc.WebsiteURL), safeLink(d.Doc.Repository.URL)
	var pretty bytes.Buffer
	if json.Indent(&pretty, v.ServerJSON, "", "  ") == nil {
		d.JSON = pretty.String()
	}
	if len(d.Doc.Remotes) > 0 {
		_, short, _ := strings.Cut(srv.Name, "/")
		transport := "http"
		if d.Doc.Remotes[0].Type == "sse" {
			transport = "sse"
		}
		cfg, _ := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{
			short: map[string]string{"type": transport, "url": d.Doc.Remotes[0].URL}}}, "", "  ")
		d.Config = string(cfg)
	}
	s.uiRender(w, r, http.StatusOK, "mcpserver", &uiPage{Title: srv.Name, Tab: "mcp", Data: d})
}

// --- upstreams (admin) ---

func (s *Server) uiUpstreams(w http.ResponseWriter, r *http.Request) {
	if !principal(r).Admin {
		s.uiError(w, r, &registry.Error{Status: http.StatusForbidden, Code: "forbidden", Msg: "admin access required"})
		return
	}
	ups, err := s.Svc.Store.ListUpstreams(r.Context())
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	s.uiRender(w, r, http.StatusOK, "upstreams", &uiPage{Title: "Upstreams", Tab: "upstreams", Data: ups})
}
