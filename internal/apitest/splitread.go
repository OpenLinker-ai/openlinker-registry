package apitest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

// hookStore runs a function once, just before a named read is passed on to
// the store. The function stands for whatever else happens between two
// reads of one request: a sync applying its upstream, an owner changing a
// setting.
type hookStore struct {
	store.Store
	mu    sync.Mutex
	hooks map[string]func()
}

func (h *hookStore) before(method string, f func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hooks[method] = f
}

// fire runs and forgets the hook of method, so that the requests the hook
// itself sends pass through.
func (h *hookStore) fire(method string) {
	h.mu.Lock()
	f := h.hooks[method]
	delete(h.hooks, method)
	h.mu.Unlock()
	if f != nil {
		f()
	}
}

func (h *hookStore) waiting(method string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hooks[method] != nil
}

func (h *hookStore) GetVersion(ctx context.Context, skillID int64, version string) (model.Version, error) {
	h.fire("GetVersion")
	return h.Store.GetVersion(ctx, skillID, version)
}

func (h *hookStore) ListVersions(ctx context.Context, skillID int64) ([]model.Version, error) {
	h.fire("ListVersions")
	return h.Store.ListVersions(ctx, skillID)
}

func (h *hookStore) ListVersionSummaries(ctx context.Context, skillID int64) ([]model.Version, error) {
	h.fire("ListVersions")
	return h.Store.ListVersionSummaries(ctx, skillID)
}

func (h *hookStore) GetMCPVersion(ctx context.Context, serverID int64, version string) (model.MCPVersion, error) {
	h.fire("GetMCPVersion")
	return h.Store.GetMCPVersion(ctx, serverID, version)
}

func (h *hookStore) ListMCPVersionsPage(ctx context.Context, serverID int64, q model.MCPVersionPage) ([]model.MCPVersion, error) {
	if q.After == nil {
		h.fire("ListMCPVersions")
	} else {
		h.fire("ListMCPVersions, next page")
	}
	return h.Store.ListMCPVersionsPage(ctx, serverID, q)
}

// runSplitReads covers a request that reads an entry's settings and then its
// versions, with a change in between: the entry becomes private and gets a
// second version. The request saw the entry while it was public, but the
// second version never was; it must not be served, in any form.
func runSplitReads(t *testing.T, newStore func(t *testing.T) store.Store) {
	h := &hookStore{Store: newStore(t), hooks: map[string]func(){}}
	e := NewEnv(t, h, Options{UI: true})
	e.namespace("com.acme", "domain")
	pub := e.publisher("com.acme")
	const marker = "never-public-0123456789"
	cases := []struct {
		what, method, path string
		mcp                bool
	}{
		{"a skill version", "GetVersion", "/v1/skills/com.acme/%s/versions/v2", false},
		{"a skill's artifact", "GetVersion", "/v1/skills/com.acme/%s/versions/v2/artifact", false},
		{"a skill's file", "GetVersion", "/v1/skills/com.acme/%s/versions/v2/files/SKILL.md", false},
		{"a skill's versions", "ListVersions", "/v1/skills/com.acme/%s", false},
		{"a skill's page", "ListVersions", "/ui/skills/com.acme/%s", false},
		{"a server version", "GetMCPVersion", "/v0.1/servers/com.acme%%2F%s/versions/2.0.0", true},
		{"a server's latest version", "GetMCPVersion", "/v0.1/servers/com.acme%%2F%s/versions/latest", true},
		{"a server version, older API path", "GetMCPVersion", "/v0/servers/com.acme%%2F%s/versions/latest", true},
		{"a server's versions", "ListMCPVersions", "/v0.1/servers/com.acme%%2F%s/versions", true},
		{"a server's detail", "ListMCPVersions", "/v1/mcp/servers/com.acme/%s", true},
		{"a server version, native API", "GetMCPVersion", "/v1/mcp/servers/com.acme/%s/versions/2.0.0", true},
		{"a server's page", "ListMCPVersions", "/ui/mcp/com.acme/%s", true},
	}
	for i, c := range cases {
		name := fmt.Sprintf("entry%d", i)
		path := fmt.Sprintf(c.path, name)
		if c.mcp {
			e.Expect(e.publishMCP(pub, "com.acme", serverJSON("com.acme/"+name, "1.0.0", "https://first.acme.example/mcp")), 201, c.what+": publish")
		} else {
			e.Expect(e.Do("POST", "/v1/skills/com.acme/"+name+"/versions?version=v1", pub, "text/markdown", md(name, "First", "MIT")), 201, c.what+": publish")
		}
		h.before(c.method, func() {
			if c.mcp {
				e.Expect(e.Do("PATCH", "/v1/mcp/servers/com.acme/"+name, AdminToken, "", map[string]any{"visibility": "private"}), 200, c.what+": restrict")
				e.Expect(e.publishMCP(pub, "com.acme", serverJSON("com.acme/"+name, "2.0.0", "https://"+marker+".acme.example/mcp")), 201, c.what+": second version")
				return
			}
			e.Expect(e.Do("PATCH", "/v1/skills/com.acme/"+name, AdminToken, "", map[string]any{"visibility": "private"}), 200, c.what+": restrict")
			e.Expect(e.Do("POST", "/v1/skills/com.acme/"+name+"/versions?version=v2", pub, "text/markdown", md(name, marker, "MIT")), 201, c.what+": second version")
		})
		r := e.Do("GET", path, "", "", nil)
		if h.waiting(c.method) {
			t.Fatalf("%s: GET %s never read %s; the case tests nothing", c.what, path, c.method)
		}
		if r.Status != 404 || strings.Contains(string(r.Body), marker) || strings.Contains(string(r.Body), "2.0.0") {
			t.Errorf("%s: an anonymous GET %s that began while the entry was public = %d %s", c.what, path, r.Status, r.Body)
		}
		if cache := r.Header.Get("Cache-Control"); strings.Contains(cache, "public") {
			t.Errorf("%s: the answer may be cached publicly: %s", c.what, cache)
		}
		// Its members still read it.
		if strings.HasPrefix(path, "/ui/") {
			continue
		}
		e.Expect(e.Do("GET", path, pub, "", nil), 200, c.what+": a member's read")
	}

	// A long version list is written a page at a time. When the server is
	// restricted between two pages, the answer cannot be taken back; it is
	// cut short, and nothing read after the restriction is in it.
	const pages = 60 // more than one page
	for i := 1; i <= pages; i++ {
		e.Expect(e.publishMCP(pub, "com.acme", serverJSON("com.acme/paged", fmt.Sprintf("nightly-%02d", i), "https://first.acme.example/mcp")), 201, "publish")
	}
	h.before("ListMCPVersions, next page", func() {
		e.Expect(e.Do("PATCH", "/v1/mcp/servers/com.acme/paged", AdminToken, "", map[string]any{"visibility": "private"}), 200, "restrict")
		e.Expect(e.publishMCP(pub, "com.acme", serverJSON("com.acme/paged", "nightly-99", "https://"+marker+".acme.example/mcp")), 201, "second version")
	})
	res, err := http.Get(e.Base + "/v0.1/servers/com.acme%2Fpaged/versions")
	if err != nil {
		t.Fatalf("version list: %v", err)
	}
	body, readErr := io.ReadAll(res.Body)
	res.Body.Close()
	if h.waiting("ListMCPVersions, next page") {
		t.Fatal("the version list was read in one piece; the case tests nothing")
	}
	if strings.Contains(string(body), marker) || strings.Contains(string(body), "nightly-01") {
		t.Fatalf("a version list went on after its server was restricted: %d bytes", len(body))
	}
	if readErr == nil && json.Valid(body) {
		t.Fatalf("a version list that was cut short reads as a complete answer: %d bytes", len(body))
	}
	e.Expect(e.Do("GET", "/v0.1/servers/com.acme%2Fpaged/versions", "", "", nil), 404, "the list after the restriction")
}
