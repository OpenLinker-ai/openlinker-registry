package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OpenLinker-ai/openlinker-registry/internal/mcp"
	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/registry"
)

// MCP servers are served twice: under /v1/mcp (native, same conventions as
// the skill API) and under /v0.1 and /v0, which follow the read API of the
// official MCP Registry so that its clients and downstream registries can
// use this instance unchanged. Both views apply the same visibility rules.

const maxServerJSON = 256 << 10

func (s *Server) mcpRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /v1/mcp/servers", s.read(s.listMCP))
	m.HandleFunc("GET /v1/mcp/servers/{ns}/{name}", s.read(s.getMCPServer))
	m.HandleFunc("GET /v1/mcp/servers/{ns}/{name}/versions/{version}", s.read(s.getMCPVersion))
	m.HandleFunc("POST /v1/mcp/servers/{ns}", s.publishMCP)
	m.HandleFunc("POST /v1/mcp/servers/{ns}/{name}/status", s.setMCPStatus)
	m.HandleFunc("POST /v1/mcp/servers/{ns}/{name}/versions/{version}/status", s.setMCPStatus)
	m.HandleFunc("PATCH /v1/mcp/servers/{ns}/{name}", s.patchMCP)

	for _, p := range []string{"/v0.1", "/v0"} {
		m.HandleFunc("GET "+p+"/servers", s.read(s.v0List))
		m.HandleFunc("GET "+p+"/servers/{rest...}", s.read(s.v0Route))
		m.HandleFunc("POST "+p+"/publish", s.v0Publish)
	}
}

// --- presentation ---

func mcpServerJSON(srv model.MCPServer) map[string]any {
	out := map[string]any{"name": srv.Name, "namespace": srv.Namespace, "mode": srv.Mode, "visibility": srv.Visibility,
		"listed": srv.Listed, "created_at": srv.CreatedAt, "updated_at": srv.UpdatedAt}
	if srv.Upstream != "" {
		out["upstream"] = srv.Upstream
	}
	return out
}

func mcpVersionJSON(srv model.MCPServer, v model.MCPVersion) map[string]any {
	out := mcpServerJSON(srv)
	out["version"] = v.Version
	out["digest"] = v.Digest
	out["status"] = v.Status
	out["status_changed_at"] = v.StatusChangedAt
	out["published_at"] = v.PublishedAt
	out["updated_at"] = v.UpdatedAt
	out["is_latest"] = v.IsLatest
	out["server"] = json.RawMessage(v.ServerJSON)
	if v.StatusMessage != "" {
		out["status_message"] = v.StatusMessage
	}
	return out
}

// v0Response is the official ServerResponse: the server.json plus registry
// metadata. This registry's own facts go under its own _meta key.
func v0Response(srv model.MCPServer, v model.MCPVersion) map[string]any {
	official := map[string]any{"status": v.Status, "statusChangedAt": v.StatusChangedAt, "publishedAt": v.PublishedAt,
		"updatedAt": v.UpdatedAt, "isLatest": v.IsLatest}
	if v.StatusMessage != "" {
		official["statusMessage"] = v.StatusMessage
	}
	entry := map[string]any{"mode": srv.Mode, "digest": v.Digest}
	if srv.Upstream != "" {
		entry["upstream"] = srv.Upstream
	}
	return map[string]any{"server": json.RawMessage(v.ServerJSON),
		"_meta": map[string]any{mcp.MetaOfficial: official, mcp.MetaRegistry: entry}}
}

// writeV0Error uses the official error body: {"error": "<message>"}.
func writeV0Error(w http.ResponseWriter, err error) {
	e := registry.AsError(err)
	writeJSON(w, e.Status, map[string]any{"error": e.Msg})
}

func encodeMCPCursor(name, version string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(name + "\x00" + version))
}

func decodeMCPCursor(c string) (string, string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || len(b) > 512 {
		return "", "", false
	}
	name, version, ok := strings.Cut(string(b), "\x00")
	return name, version, ok && name != "" && utf8.ValidString(name) && utf8.ValidString(version)
}

func badQuery(msg string) error {
	return &registry.Error{Status: http.StatusBadRequest, Code: "invalid_query", Msg: msg}
}

// mcpQuery reads the listing parameters shared by both API styles.
func (s *Server) mcpQuery(r *http.Request, searchParam string, defaultLimit int) (model.MCPQuery, int, error) {
	qv := r.URL.Query()
	q := model.MCPQuery{Viewer: principal(r).Viewer(), Search: strings.TrimSpace(qv.Get(searchParam)),
		Namespace: qv.Get("namespace"), Mode: qv.Get("mode"), Version: qv.Get("version")}
	if len(q.Search) > 200 {
		return q, 0, badQuery(searchParam + " is too long")
	}
	if q.Mode != "" && !slices.Contains([]string{model.MCPModeRegistered, model.MCPModeIndexed, model.MCPModeMirrored}, q.Mode) {
		return q, 0, badQuery("mode must be registered, indexed or mirrored")
	}
	if len(q.Version) > 255 || len(q.Namespace) > 128 {
		return q, 0, badQuery("version or namespace is too long")
	}
	if v := qv.Get("updated_since"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return q, 0, badQuery("updated_since must be an RFC 3339 timestamp")
		}
		// Incremental sync must see deletions (official registry behavior).
		q.UpdatedSince, q.IncludeDeleted = &t, true
	}
	if v := qv.Get("include_deleted"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return q, 0, badQuery("include_deleted must be a boolean")
		}
		q.IncludeDeleted = q.IncludeDeleted || b
	}
	limit := defaultLimit
	if l := qv.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 100 {
			return q, 0, badQuery("limit must be 1-100")
		}
		limit = n
	}
	if c := qv.Get("cursor"); c != "" {
		name, version, ok := decodeMCPCursor(c)
		if !ok {
			return q, 0, badQuery("invalid cursor")
		}
		q.AfterName, q.AfterVersion = name, version
	}
	q.Limit = limit + 1
	return q, limit, nil
}

// searchMCP runs the query and returns one page plus the cursor of the next.
func (s *Server) searchMCP(r *http.Request, q model.MCPQuery, limit int) ([]model.MCPEntry, string, error) {
	rows, err := s.Svc.Store.SearchMCP(r.Context(), q)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		next = encodeMCPCursor(last.Server.Name, last.Version.Version)
	}
	return rows, next, nil
}

// loadVisibleMCP resolves name@version for the caller. A local miss may be
// forwarded to the upstreams; an entry the caller may not see is
// indistinguishable from a missing one.
func (s *Server) loadVisibleMCP(r *http.Request, name, version string) (model.MCPServer, model.MCPVersion, error) {
	srv, v, err := s.Svc.ResolveMCP(r.Context(), name, version, false)
	if errors.Is(err, model.ErrNotFound) {
		if ferr := s.allowForward(r); ferr != nil {
			if ferr != errNoForward {
				return srv, v, ferr
			}
		} else {
			srv, v, err = s.Svc.ResolveMCP(r.Context(), name, version, true)
		}
	}
	if errors.Is(err, model.ErrNotFound) {
		return srv, v, errNotFound
	}
	if err != nil {
		return srv, v, err
	}
	if !principal(r).Viewer().CanSee(srv.Namespace, srv.Visibility) {
		return srv, v, errNotFound
	}
	return srv, v, nil
}

// serverName returns the full server name of a native API request.
func serverName(r *http.Request) string {
	return r.PathValue("ns") + "/" + r.PathValue("name")
}

// v0Target parses the part of an official-API path after "/servers/":
//
//	<serverName>/versions            <serverName>/versions/<version>
//
// serverName is normally one escaped segment ("io.github.a%2Fb"), but a
// proxy that decodes %2F delivers it as two segments; both forms are
// accepted. A full server name always contains a slash, which tells the two
// apart. list is true for the version listing.
func v0Target(r *http.Request) (name, version string, list, ok bool) {
	escaped := r.URL.EscapedPath()
	i := strings.LastIndex(escaped, "/servers/")
	if i < 0 {
		return "", "", false, false
	}
	var segs []string
	for _, raw := range strings.Split(escaped[i+len("/servers/"):], "/") {
		seg, err := url.PathUnescape(raw)
		if err != nil || seg == "" {
			return "", "", false, false
		}
		segs = append(segs, seg)
	}
	if len(segs) >= 2 && !strings.Contains(segs[0], "/") {
		segs = append([]string{segs[0] + "/" + segs[1]}, segs[2:]...)
	}
	switch {
	case len(segs) == 2 && segs[1] == "versions":
		return segs[0], "", true, true
	case len(segs) == 3 && segs[1] == "versions":
		return segs[0], segs[2], false, true
	}
	return "", "", false, false
}

func (s *Server) v0Route(w http.ResponseWriter, r *http.Request) {
	name, version, list, ok := v0Target(r)
	switch {
	case !ok:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "Not found"})
	case list:
		s.v0Versions(w, r, name)
	default:
		s.v0Get(w, r, name, version)
	}
}

// --- native read API ---

func (s *Server) listMCP(w http.ResponseWriter, r *http.Request) {
	q, limit, err := s.mcpQuery(r, "q", 50)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if q.Version == "" {
		q.Version = "latest" // one row per server
	} else if q.Version == "all" {
		q.Version = ""
	}
	rows, next, err := s.searchMCP(r, q, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, e := range rows {
		items = append(items, mcpVersionJSON(e.Server, e.Version))
	}
	s.setCache(w, r, false)
	writeJSON(w, http.StatusOK, map[string]any{"servers": items, "next_cursor": nullable(next)})
}

func (s *Server) getMCPServer(w http.ResponseWriter, r *http.Request) {
	srv, latest, err := s.loadVisibleMCP(r, serverName(r), "latest")
	if err != nil {
		// A server whose versions are all deleted has no latest version but
		// still exists.
		var lerr error
		if srv, lerr = s.Svc.Store.GetMCPServer(r.Context(), serverName(r)); lerr != nil ||
			!principal(r).Viewer().CanSee(srv.Namespace, srv.Visibility) {
			s.fail(w, r, err)
			return
		}
		latest = model.MCPVersion{}
	}
	versions, err := s.Svc.Store.ListMCPVersions(r.Context(), srv.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	list := make([]map[string]any, 0, len(versions))
	for _, v := range versions {
		list = append(list, map[string]any{"version": v.Version, "status": v.Status, "is_latest": v.IsLatest,
			"published_at": v.PublishedAt, "updated_at": v.UpdatedAt, "digest": v.Digest})
	}
	out := mcpServerJSON(srv)
	out["latest_version"] = nullable(latest.Version)
	out["versions"] = list
	s.setCache(w, r, false)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getMCPVersion(w http.ResponseWriter, r *http.Request) {
	srv, v, err := s.loadVisibleMCP(r, serverName(r), r.PathValue("version"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.setCache(w, r, srv.Visibility == model.VisibilityPublic)
	writeJSON(w, http.StatusOK, mcpVersionJSON(srv, v))
}

// --- native write API ---

func readServerJSON(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return nil, &registry.Error{Status: http.StatusUnsupportedMediaType, Code: "content_type", Msg: "expected application/json"}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxServerJSON))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, &registry.Error{Status: http.StatusRequestEntityTooLarge, Code: "body_too_large", Msg: "server.json is too large"}
		}
		return nil, &registry.Error{Status: http.StatusBadRequest, Code: "body_unreadable", Msg: "could not read request body"}
	}
	return body, nil
}

// publishMCP handles POST /v1/mcp/servers/{ns} with a server.json body and
// optional query parameters visibility, listed and indexed.
func (s *Server) publishMCP(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	if err := requireRole(r, ns, model.RolePublisher); err != nil {
		s.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	o := registry.MCPPublishOptions{Actor: principal(r).Subject, Namespace: ns, Visibility: q.Get("visibility")}
	for k := range q {
		if !slices.Contains([]string{"visibility", "listed", "indexed"}, k) {
			s.fail(w, r, badQuery("unknown query parameter "+strconv.Quote(k)))
			return
		}
	}
	if v := q.Get("indexed"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			s.fail(w, r, badQuery("indexed must be a boolean"))
			return
		}
		o.Indexed = b
	}
	if v := q.Get("listed"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			s.fail(w, r, badQuery("listed must be a boolean"))
			return
		}
		o.Listed = &b
	}
	body, err := readServerJSON(w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	srv, v, err := s.Svc.PublishMCP(r.Context(), o, body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, mcpVersionJSON(srv, v))
}

// loadMCPForWrite authorizes then loads the server, so callers without a
// role learn nothing about private entries.
func (s *Server) loadMCPForWrite(r *http.Request, min model.Role) (model.MCPServer, error) {
	if err := requireRole(r, strings.ToLower(r.PathValue("ns")), min); err != nil {
		return model.MCPServer{}, err
	}
	srv, err := s.Svc.Store.GetMCPServer(r.Context(), serverName(r))
	if errors.Is(err, model.ErrNotFound) {
		return srv, errNotFound
	}
	return srv, err
}

type mcpStatusRequest struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

func (s *Server) setMCPStatus(w http.ResponseWriter, r *http.Request) {
	srv, err := s.loadMCPForWrite(r, model.RolePublisher)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var req mcpStatusRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	n, err := s.Svc.SetMCPStatus(r.Context(), principal(r).Subject, srv, r.PathValue("version"), req.Status, req.Message)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": srv.Name, "status": req.Status, "updated_count": n})
}

func (s *Server) patchMCP(w http.ResponseWriter, r *http.Request) {
	srv, err := s.loadMCPForWrite(r, model.RoleOwner)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var req patchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.Svc.UpdateMCPSettings(r.Context(), principal(r).Subject, srv, model.MCPPatch{Visibility: req.Visibility, Listed: req.Listed})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, mcpServerJSON(out))
}

// --- official MCP Registry API ---

func (s *Server) v0List(w http.ResponseWriter, r *http.Request) {
	q, limit, err := s.mcpQuery(r, "search", 30)
	if err != nil {
		writeV0Error(w, err)
		return
	}
	rows, next, err := s.searchMCP(r, q, limit)
	if err != nil {
		s.v0Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, e := range rows {
		items = append(items, v0Response(e.Server, e.Version))
	}
	meta := map[string]any{"count": len(items)}
	if next != "" {
		meta["nextCursor"] = next
	}
	s.setCache(w, r, false)
	writeJSON(w, http.StatusOK, map[string]any{"servers": items, "metadata": meta})
}

func (s *Server) v0Fail(w http.ResponseWriter, r *http.Request, err error) {
	if e := registry.AsError(err); e.Status >= 500 && e.Code == "internal" {
		s.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err.Error())
	}
	writeV0Error(w, err)
}

func (s *Server) v0Get(w http.ResponseWriter, r *http.Request, name, version string) {
	srv, v, err := s.loadVisibleMCP(r, name, version)
	if err != nil {
		if err == errNotFound {
			err = &registry.Error{Status: http.StatusNotFound, Code: "not_found", Msg: "Server not found"}
		}
		s.v0Fail(w, r, err)
		return
	}
	s.setCache(w, r, srv.Visibility == model.VisibilityPublic)
	writeJSON(w, http.StatusOK, v0Response(srv, v))
}

func (s *Server) v0Versions(w http.ResponseWriter, r *http.Request, name string) {
	srv, _, err := s.loadVisibleMCP(r, name, "latest")
	if err != nil {
		var lerr error
		if srv, lerr = s.Svc.Store.GetMCPServer(r.Context(), name); lerr != nil ||
			!principal(r).Viewer().CanSee(srv.Namespace, srv.Visibility) {
			if err == errNotFound {
				err = &registry.Error{Status: http.StatusNotFound, Code: "not_found", Msg: "Server not found"}
			}
			s.v0Fail(w, r, err)
			return
		}
	}
	versions, err := s.Svc.Store.ListMCPVersions(r.Context(), srv.ID)
	if err != nil {
		s.v0Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(versions))
	for _, v := range versions {
		items = append(items, v0Response(srv, v))
	}
	s.setCache(w, r, false)
	writeJSON(w, http.StatusOK, map[string]any{"servers": items, "metadata": map[string]any{"count": len(items)}})
}

// v0Publish accepts a server.json like the official registry's publish
// endpoint, authorized by a registry token with the publisher role in the
// namespace of the server name.
func (s *Server) v0Publish(w http.ResponseWriter, r *http.Request) {
	body, err := readServerJSON(w, r)
	if err != nil {
		writeV0Error(w, err)
		return
	}
	var head struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(body, &head)
	ns, _, ok := mcp.SplitName(head.Name)
	if !ok {
		writeV0Error(w, &registry.Error{Status: http.StatusUnprocessableEntity, Code: "name_invalid", Msg: "server name must be <namespace>/<name>"})
		return
	}
	if err := requireRole(r, ns, model.RolePublisher); err != nil {
		writeV0Error(w, err)
		return
	}
	srv, v, err := s.Svc.PublishMCP(r.Context(), registry.MCPPublishOptions{Actor: principal(r).Subject, Namespace: ns}, body)
	if err != nil {
		s.v0Fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v0Response(srv, v))
}
