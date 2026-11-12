package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"time"
)

// officialMeta is the key of the registry-managed block in "_meta".
const officialMeta = "io.modelcontextprotocol.registry/official"

// MCP version statuses, as in the official MCP Registry.
const (
	mcpActive     = "active"
	mcpDeprecated = "deprecated"
	mcpDeleted    = "deleted"
)

// MCPDoc is one (server, version) as an upstream reports it.
type MCPDoc struct {
	Server          json.RawMessage // the server.json object bytes exactly as received
	Name, Version   string          // from server.name / server.version
	Status          string          // active, deprecated or deleted; "active" when absent or unknown
	StatusMessage   string
	StatusChangedAt time.Time // zero when absent or unparseable, like the two below
	PublishedAt     time.Time
	UpdatedAt       time.Time
	IsLatest        bool
}

// MCPListOptions selects a page of GET /servers. Zero fields are not sent.
type MCPListOptions struct {
	Cursor       string
	Limit        int
	UpdatedSince *time.Time
	LatestOnly   bool // version=latest
	Search       string
}

// mcpGet requests an MCP path under the API prefix. Until a prefix has
// answered, "/v0.1" is tried first and a 404 from it is retried under "/v0";
// the first prefix that answers 2xx is used for the life of the Client. A 404
// from both (a server that simply does not exist) leaves the question open
// for the next call.
func (c *Client) mcpGet(ctx context.Context, suffix string, q url.Values) ([]byte, error) {
	c.mu.Lock()
	prefix := c.mcpPrefix
	c.mu.Unlock()
	if prefix != "" {
		return c.getJSON(ctx, prefix+suffix, q)
	}
	body, err := c.getJSON(ctx, "/v0.1"+suffix, q)
	if err == nil {
		c.rememberMCPPrefix("/v0.1")
		return body, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	// A failure of the fallback that is not itself a 404 is reported as
	// that failure: "not found" must stay a definite answer.
	body, err = c.getJSON(ctx, "/v0"+suffix, q)
	if err != nil {
		return nil, err
	}
	c.rememberMCPPrefix("/v0")
	return body, nil
}

func (c *Client) rememberMCPPrefix(p string) {
	c.mu.Lock()
	if c.mcpPrefix == "" { // concurrent first calls: the first answer wins
		c.mcpPrefix = p
	}
	c.mu.Unlock()
}

// rawString returns the JSON string under key, or "" when it is absent or
// not a string.
func rawString(m map[string]json.RawMessage, key string) string {
	var s string
	if raw, ok := m[key]; !ok || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

func rawTime(m map[string]json.RawMessage, key string) time.Time {
	t, err := time.Parse(time.RFC3339, rawString(m, key))
	if err != nil {
		return time.Time{}
	}
	return t
}

// rawObject decodes a JSON object; anything else (including null) is nil.
func rawObject(raw json.RawMessage) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// parseMCPDoc reads one ServerResponse. Two shapes are accepted: the current
// {"server": {...}, "_meta": {...}} and the older flat server object carrying
// "_meta" itself. ok is false when the server is not an object or lacks a
// non-empty string name or version. Registry metadata is read field by field
// so one odd field does not discard the entry.
func parseMCPDoc(raw json.RawMessage) (doc MCPDoc, ok bool) {
	top := rawObject(raw)
	if top == nil {
		return doc, false
	}
	serverRaw, wrapped := top["server"]
	server := top
	if wrapped {
		if server = rawObject(serverRaw); server == nil {
			return doc, false
		}
	} else {
		serverRaw = raw
	}
	doc.Name, doc.Version = rawString(server, "name"), rawString(server, "version")
	if doc.Name == "" || doc.Version == "" {
		return doc, false
	}
	doc.Server = bytes.Clone(bytes.TrimSpace(serverRaw))
	doc.Status = mcpActive

	// The flat shape has "_meta" in the only place it can be (top is the
	// server); the wrapped shape has it next to "server".
	official := rawObject(rawObject(top["_meta"])[officialMeta])
	switch s := rawString(official, "status"); s {
	case mcpDeprecated, mcpDeleted:
		doc.Status = s
	}
	doc.StatusMessage = rawString(official, "statusMessage")
	doc.StatusChangedAt = rawTime(official, "statusChangedAt")
	doc.PublishedAt = rawTime(official, "publishedAt")
	doc.UpdatedAt = rawTime(official, "updatedAt")
	var latest bool
	if json.Unmarshal(official["isLatest"], &latest) == nil {
		doc.IsLatest = latest
	}
	return doc, true
}

// ListMCP returns one page of GET {base}/v0.1/servers. Entries whose server
// is not an object or lacks a string name or version are skipped, so a page
// can be empty while nextCursor is not. nextCursor is "" at the end.
// UpdatedSince is sent in UTC with second precision, truncated, so the
// upstream may repeat an entry but cannot skip one.
func (c *Client) ListMCP(ctx context.Context, o MCPListOptions) (docs []MCPDoc, nextCursor string, err error) {
	q := url.Values{}
	if o.Cursor != "" {
		q.Set("cursor", o.Cursor)
	}
	if o.Limit > 0 {
		q.Set("limit", strconv.Itoa(o.Limit))
	}
	if o.Search != "" {
		q.Set("search", o.Search)
	}
	if o.UpdatedSince != nil {
		q.Set("updated_since", o.UpdatedSince.UTC().Format(time.RFC3339))
	}
	if o.LatestOnly {
		q.Set("version", "latest")
	}
	body, err := c.mcpGet(ctx, "/servers", q)
	if err != nil {
		return nil, "", wrapOp("mcp list", err)
	}
	items, top, err := listField(body, "servers", "server list")
	if err != nil {
		return nil, "", wrapOp("mcp list", err)
	}
	for _, it := range items {
		if d, ok := parseMCPDoc(it); ok {
			docs = append(docs, d)
		}
	}
	// nextCursor null, empty or missing all mean the end.
	nextCursor = rawString(rawObject(top["metadata"]), "nextCursor")
	if nextCursor != "" && nextCursor == o.Cursor {
		// Following it would loop forever.
		return nil, "", wrapOp("mcp list", badResponse("upstream returned a cursor that does not advance"))
	}
	return docs, nextCursor, nil
}

// GetMCP returns one version of a server ("latest" is allowed). The answer
// must be for the server that was asked for, and for the version unless that
// was "latest"; anything else is a bad_response.
func (c *Client) GetMCP(ctx context.Context, name, version string) (MCPDoc, error) {
	if err := checkSegs("server name and version", name, version); err != nil {
		return MCPDoc{}, wrapOp("mcp get", err)
	}
	body, err := c.mcpGet(ctx, "/servers/"+seg(name)+"/versions/"+seg(version), nil)
	if err != nil {
		return MCPDoc{}, wrapOp("mcp get", err)
	}
	doc, ok := parseMCPDoc(body)
	if !ok {
		return MCPDoc{}, wrapOp("mcp get", badResponse("upstream returned an invalid server document"))
	}
	if doc.Name != name || (version != "latest" && doc.Version != version) {
		return MCPDoc{}, wrapOp("mcp get", badResponse("upstream returned a different server or version than requested"))
	}
	return doc, nil
}

// ListMCPVersions returns every version of a server, in upstream order.
// Malformed entries and entries for another server name are skipped.
func (c *Client) ListMCPVersions(ctx context.Context, name string) ([]MCPDoc, error) {
	if err := checkSegs("server name", name); err != nil {
		return nil, wrapOp("mcp versions", err)
	}
	body, err := c.mcpGet(ctx, "/servers/"+seg(name)+"/versions", nil)
	if err != nil {
		return nil, wrapOp("mcp versions", err)
	}
	items, _, err := listField(body, "servers", "version list")
	if err != nil {
		return nil, wrapOp("mcp versions", err)
	}
	var docs []MCPDoc
	for _, it := range items {
		if d, ok := parseMCPDoc(it); ok && d.Name == name {
			docs = append(docs, d)
		}
	}
	return docs, nil
}
