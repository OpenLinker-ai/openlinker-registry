package storetest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

// serverJSON builds a document whose bytes a JSON normalizer would change
// (unsorted keys, the number 1e2): stores must return exactly what was stored.
func serverJSON(name, version, title, description string) json.RawMessage {
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	return json.RawMessage(`{"version":` + q(version) + `,"name":` + q(name) + `,"title":` + q(title) +
		`,"weight":1e2,"description":` + q(description) + `}`)
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// sameJSON reports whether a stored document is byte-identical to the one
// published: its digest is over these exact bytes.
func sameJSON(a, b []byte) bool { return bytes.Equal(a, b) }

// mcpInput publishes name ("<namespace>/<server>") as a public, listed,
// registered server.
func mcpInput(name, version string) model.MCPPublishInput {
	ns, _, _ := strings.Cut(name, "/")
	doc := serverJSON(name, version, "", "")
	return model.MCPPublishInput{Namespace: strings.ToLower(ns), Name: name, Mode: model.MCPModeRegistered,
		Visibility: model.VisibilityPublic, Listed: true,
		Version: model.MCPVersion{Version: version, ServerJSON: doc, Digest: digestOf(doc), CreatedBy: "publisher"}}
}

func publishMCP(t *testing.T, st store.Store, in model.MCPPublishInput) (model.MCPServer, model.MCPVersion) {
	t.Helper()
	srv, v, err := st.PublishMCPVersion(context.Background(), in, audit)
	if err != nil {
		t.Fatalf("publish %s@%s: %v", in.Name, in.Version.Version, err)
	}
	return srv, v
}

func mirrorInput(up model.Upstream, name, version string) model.MCPMirrorInput {
	ns, _, _ := strings.Cut(name, "/")
	doc := serverJSON(name, version, "", "")
	return model.MCPMirrorInput{UpstreamID: up.ID, Namespace: strings.ToLower(ns), Name: name, Visibility: up.Visibility, Listed: true,
		Version: model.MCPVersion{Version: version, ServerJSON: doc, Digest: digestOf(doc), Status: model.MCPStatusActive,
			CreatedBy: "upstream:" + up.Name}}
}

func mirrorMCP(t *testing.T, st store.Store, in model.MCPMirrorInput, want string) {
	t.Helper()
	if got, err := st.UpsertMirroredMCP(context.Background(), in, audit); err != nil || got != want {
		t.Fatalf("mirror %s@%s = %q, %v; want %q", in.Name, in.Version.Version, got, err, want)
	}
}

func mcpServer(t *testing.T, st store.Store, name string) model.MCPServer {
	t.Helper()
	srv, err := st.GetMCPServer(context.Background(), name)
	if err != nil {
		t.Fatalf("get server %s: %v", name, err)
	}
	return srv
}

func mcpVersion(t *testing.T, st store.Store, serverID int64, version string) model.MCPVersion {
	t.Helper()
	v, err := st.GetMCPVersion(context.Background(), serverID, version)
	if err != nil {
		t.Fatalf("get version %s: %v", version, err)
	}
	return v
}

// latestMCP returns the version flagged latest ("" when there is none) after
// checking that the flag, the "latest" lookup and the version list agree.
func latestMCP(t *testing.T, st store.Store, serverID int64) string {
	t.Helper()
	ctx := context.Background()
	all, err := st.ListMCPVersions(ctx, serverID)
	if err != nil {
		t.Fatal(err)
	}
	var flagged []string
	for _, v := range all {
		if v.IsLatest {
			flagged = append(flagged, v.Version)
			if v.Status == model.MCPStatusDeleted {
				t.Fatalf("deleted version %s is flagged latest", v.Version)
			}
		}
	}
	got, err := st.GetMCPVersion(ctx, serverID, "latest")
	switch {
	case len(flagged) > 1:
		t.Fatalf("several latest versions: %v", flagged)
	case len(flagged) == 0:
		if !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("no version is flagged but latest resolves: %v", err)
		}
		return ""
	case err != nil || got.Version != flagged[0] || !got.IsLatest:
		t.Fatalf("latest lookup = %q, %v; flagged %q", got.Version, err, flagged[0])
	}
	return flagged[0]
}

func setMCPStatus(t *testing.T, st store.Store, serverID int64, version, status, message string, at time.Time, want int) {
	t.Helper()
	if n, err := st.SetMCPStatus(context.Background(), serverID, version, status, message, at, audit); err != nil || n != want {
		t.Fatalf("set %q to %s: changed %d, %v; want %d", version, status, n, err, want)
	}
}

// isNow reports whether a store-assigned time is the current time, with
// room for clock differences between the test and a database server.
func isNow(at time.Time) bool {
	d := time.Since(at)
	return d > -time.Hour && d < time.Hour
}

func entryKeys(entries []model.MCPEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Server.Name+"@"+e.Version.Version)
	}
	return out
}

func searchMCP(t *testing.T, st store.Store, q model.MCPQuery) []string {
	t.Helper()
	entries, err := st.SearchMCP(context.Background(), q)
	if err != nil {
		t.Fatalf("search %+v: %v", q, err)
	}
	return entryKeys(entries)
}

var mcpTests = []testCase{
	{"mcp publish", func(t *testing.T, st store.Store) {
		ctx := context.Background()
		up := newUpstream(t, st, "up")
		newNamespace(t, st, "org.other")
		if _, err := st.EnsureUpstreamNamespace(ctx, "org.mirror", up.ID, audit); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.PublishMCPVersion(ctx, mcpInput("org.missing/server", "1.0.0"), audit); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("missing namespace: %v", err)
		}
		if _, _, err := st.PublishMCPVersion(ctx, mcpInput("org.mirror/server", "1.0.0"), audit); conflictCode(err) != "namespace_upstream_managed" {
			t.Fatalf("upstream-managed namespace: %v", err)
		}
		if _, err := st.GetMCPServer(ctx, "org.mirror/server"); !errors.Is(err, model.ErrNotFound) {
			t.Fatal("rejected publish created the server")
		}

		in := mcpInput("com.example/Server", "1.0.0")
		in.Visibility, in.Listed = model.VisibilityInternal, false
		in.Version.Status, in.Version.StatusMessage, in.Version.IsLatest = model.MCPStatusDeleted, "ignored", false
		srv, v := publishMCP(t, st, in)
		if srv.ID == 0 || srv.Namespace != "com.example" || srv.Name != "com.example/Server" || srv.Mode != model.MCPModeRegistered ||
			srv.Visibility != model.VisibilityInternal || srv.Listed || srv.Upstream != "" || srv.CreatedBy != "publisher" ||
			!isNow(srv.CreatedAt) || !isNow(srv.UpdatedAt) {
			t.Fatalf("server = %+v", srv)
		}
		// Status fields of the input are not the publisher's to set.
		if v.ID == 0 || v.ServerID != srv.ID || v.Version != "1.0.0" || v.Digest != in.Version.Digest || v.CreatedBy != "publisher" ||
			v.Status != model.MCPStatusActive || v.StatusMessage != "" || !v.IsLatest || !sameJSON(v.ServerJSON, in.Version.ServerJSON) ||
			!isNow(v.PublishedAt) || !sameTime(v.UpdatedAt, v.PublishedAt) || !sameTime(v.StatusChangedAt, v.PublishedAt) {
			t.Fatalf("version = %+v", v)
		}
		if got := mcpServer(t, st, "com.example/Server"); got.ID != srv.ID || got.Namespace != "com.example" || got.Visibility != model.VisibilityInternal {
			t.Fatalf("stored server = %+v", got)
		}
		if got := mcpVersion(t, st, srv.ID, "1.0.0"); got.ID != v.ID || !sameJSON(got.ServerJSON, in.Version.ServerJSON) || !sameTime(got.PublishedAt, v.PublishedAt) {
			t.Fatalf("stored version = %+v", got)
		}
		// The name lookup is exact.
		if _, err := st.GetMCPServer(ctx, "com.example/server"); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("lookup by another letter case: %v", err)
		}

		// A later publish keeps the server settings and takes the given time.
		at := time.Date(2031, 5, 6, 7, 8, 9, 123456000, time.UTC)
		in2 := mcpInput("com.example/Server", "1.1.0")
		in2.Version.PublishedAt = at
		srv2, v2 := publishMCP(t, st, in2)
		if srv2.ID != srv.ID || srv2.Visibility != model.VisibilityInternal || srv2.Listed || srv2.UpdatedAt.Before(srv.UpdatedAt) {
			t.Fatalf("server after second publish = %+v", srv2)
		}
		if !sameTime(v2.PublishedAt, at) || !sameTime(v2.UpdatedAt, at) || !sameTime(v2.StatusChangedAt, at) || !v2.IsLatest {
			t.Fatalf("second version = %+v", v2)
		}

		conflicts := []struct {
			code string
			in   func() model.MCPPublishInput
		}{
			{"version_exists", func() model.MCPPublishInput { return mcpInput("com.example/Server", "1.0.0") }},
			{"version_exists", func() model.MCPPublishInput { return mcpInput("com.example/Server", "1.1.0") }},
			{"mode_mismatch", func() model.MCPPublishInput {
				in := mcpInput("com.example/Server", "2.0.0")
				in.Mode = model.MCPModeIndexed
				return in
			}},
			{"server_name_conflict", func() model.MCPPublishInput { return mcpInput("com.example/server", "2.0.0") }},
			{"server_name_conflict", func() model.MCPPublishInput { return mcpInput("com.example/SERVER", "1.0.0") }},
			// The same name claimed from another namespace.
			{"server_name_conflict", func() model.MCPPublishInput {
				in := mcpInput("com.example/Server", "2.0.0")
				in.Namespace = "org.other"
				return in
			}},
		}
		for _, c := range conflicts {
			in := c.in()
			if _, _, err := st.PublishMCPVersion(ctx, in, audit); conflictCode(err) != c.code {
				t.Fatalf("publish %s@%s (%s into %s): %v, want %s", in.Name, in.Version.Version, in.Mode, in.Namespace, err, c.code)
			}
		}
		all, err := st.ListMCPVersions(ctx, srv.ID)
		if err != nil || len(all) != 2 || all[0].Version != "1.1.0" || all[1].Version != "1.0.0" {
			t.Fatalf("versions after rejected publishes: %v, %v", all, err)
		}
		if got := latestMCP(t, st, srv.ID); got != "1.1.0" {
			t.Fatalf("latest = %q", got)
		}
		if _, err := st.GetMCPVersion(ctx, srv.ID, "9.9.9"); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("missing version: %v", err)
		}
		if _, err := st.GetMCPVersion(ctx, 1<<40, "latest"); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("latest of a missing server: %v", err)
		}

		// An indexed server is a separate entry with its own mode.
		idx := mcpInput("org.other/indexed", "1.0.0")
		idx.Mode = model.MCPModeIndexed
		if got, _ := publishMCP(t, st, idx); got.Mode != model.MCPModeIndexed || got.Namespace != "org.other" {
			t.Fatalf("indexed server = %+v", got)
		}

		private := model.VisibilityPrivate
		listed := true
		patched, err := st.UpdateMCPServer(ctx, srv.ID, model.MCPPatch{Visibility: &private}, audit)
		if err != nil || patched.Visibility != private || patched.Listed || patched.Namespace != "com.example" || patched.UpdatedAt.Before(srv2.UpdatedAt) {
			t.Fatalf("patch visibility: %+v, %v", patched, err)
		}
		patched, err = st.UpdateMCPServer(ctx, srv.ID, model.MCPPatch{Listed: &listed}, audit)
		if err != nil || patched.Visibility != private || !patched.Listed {
			t.Fatalf("patch listed: %+v, %v", patched, err)
		}
		if got := mcpServer(t, st, "com.example/Server"); got.Visibility != private || !got.Listed {
			t.Fatalf("stored patch = %+v", got)
		}
		if _, err := st.UpdateMCPServer(ctx, 1<<40, model.MCPPatch{Listed: &listed}, audit); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("patch a missing server: %v", err)
		}
	}},

	{"mcp latest follows semantic versions", func(t *testing.T, st store.Store) {
		old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
		var srv model.MCPServer
		steps := []struct{ publish, latest string }{
			{"1.10.0", "1.10.0"},
			{"1.2.0", "1.10.0"}, // lower, although published later
			{"1.10.0-rc.1", "1.10.0"},
			{"2.0.0-rc.1", "2.0.0-rc.1"},
			{"2.0.0-rc.2", "2.0.0-rc.2"},
			{"2.0.0", "2.0.0"},
			{"1.99.0", "2.0.0"},
			{"v2.0.1", "v2.0.1"},
		}
		for i, s := range steps {
			in := mcpInput("com.example/semver", s.publish)
			in.Version.PublishedAt = old.Add(time.Duration(i) * time.Hour)
			var v model.MCPVersion
			srv, v = publishMCP(t, st, in)
			if v.IsLatest != (s.publish == s.latest) {
				t.Fatalf("publish %s: returned IsLatest %v", s.publish, v.IsLatest)
			}
			if got := latestMCP(t, st, srv.ID); got != s.latest {
				t.Fatalf("after %s: latest = %q, want %q", s.publish, got, s.latest)
			}
		}
		// A version that lost the flag is stamped so incremental readers see
		// the flip; one that kept its state is not touched.
		if v := mcpVersion(t, st, srv.ID, "2.0.0"); v.IsLatest || !isNow(v.UpdatedAt) || !sameTime(v.PublishedAt, old.Add(5*time.Hour)) {
			t.Fatalf("former latest = %+v", v)
		}
		for _, untouched := range []string{"1.2.0", "1.99.0"} {
			if v := mcpVersion(t, st, srv.ID, untouched); v.IsLatest || !sameTime(v.UpdatedAt, v.PublishedAt) {
				t.Fatalf("never-latest version = %+v", v)
			}
		}

		// Versions that are not semantic versions do not compare: newest wins.
		for _, ver := range []string{"2024-06-01", "nightly", "alpha", "1.0"} {
			srv, _ = publishMCP(t, st, mcpInput("com.example/dated", ver))
			if got := latestMCP(t, st, srv.ID); got != ver {
				t.Fatalf("after %s: latest = %q", ver, got)
			}
		}
	}},

	{"concurrent mcp publish", func(t *testing.T, st store.Store) {
		ctx := context.Background()
		const n = 20
		for i, err := range parallel(n, func(i int) error {
			_, _, err := st.PublishMCPVersion(ctx, mcpInput("com.example/distinct", fmt.Sprintf("1.%d.0", i)), audit)
			return err
		}) {
			if err != nil {
				t.Fatalf("publisher %d: %v", i, err)
			}
		}
		srv := mcpServer(t, st, "com.example/distinct")
		if all, err := st.ListMCPVersions(ctx, srv.ID); err != nil || len(all) != n {
			t.Fatalf("%d versions, %v", len(all), err)
		}
		// Whatever the interleaving, the highest version holds the flag.
		if got := latestMCP(t, st, srv.ID); got != fmt.Sprintf("1.%d.0", n-1) {
			t.Fatalf("latest = %q", got)
		}

		won := 0
		for i, err := range parallel(n, func(int) error {
			_, _, err := st.PublishMCPVersion(ctx, mcpInput("com.example/same", "1.0.0"), audit)
			return err
		}) {
			switch {
			case err == nil:
				won++
			case conflictCode(err) != "version_exists":
				t.Fatalf("publisher %d: %v", i, err)
			}
		}
		same := mcpServer(t, st, "com.example/same")
		if all, err := st.ListMCPVersions(ctx, same.ID); won != 1 || err != nil || len(all) != 1 {
			t.Fatalf("%d publishers won, %d versions, %v", won, len(all), err)
		}
		if got := latestMCP(t, st, same.ID); got != "1.0.0" {
			t.Fatalf("latest = %q", got)
		}
	}},

	{"mcp status", func(t *testing.T, st store.Store) {
		ctx := context.Background()
		base := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		var srv model.MCPServer
		for i, ver := range []string{"1.0.0", "1.1.0", "1.2.0"} {
			in := mcpInput("com.example/status", ver)
			in.Version.PublishedAt = base.Add(time.Duration(i) * time.Hour)
			srv, _ = publishMCP(t, st, in)
		}
		bystander := mcpVersion(t, st, srv.ID, "1.0.0").UpdatedAt
		events := func() int {
			a, err := st.ListAudit(ctx, 1000)
			if err != nil {
				t.Fatal(err)
			}
			return len(a)
		}
		audited := events()
		at := base.Add(24 * time.Hour)
		if _, err := st.SetMCPStatus(ctx, 1<<40, "", model.MCPStatusDeprecated, "x", at, audit); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("missing server: %v", err)
		}
		for _, missing := range []string{"9.9.9", "latest"} {
			if _, err := st.SetMCPStatus(ctx, srv.ID, missing, model.MCPStatusDeprecated, "x", at, audit); !errors.Is(err, model.ErrNotFound) {
				t.Fatalf("version %q: %v", missing, err)
			}
		}
		if events() != audited {
			t.Fatal("a rejected status change wrote an audit event")
		}

		setMCPStatus(t, st, srv.ID, "1.1.0", model.MCPStatusDeprecated, "use 1.2.0", at, 1)
		if events() != audited+1 {
			t.Fatal("status change wrote no audit event")
		}
		v := mcpVersion(t, st, srv.ID, "1.1.0")
		if v.Status != model.MCPStatusDeprecated || v.StatusMessage != "use 1.2.0" || !sameTime(v.StatusChangedAt, at) ||
			!sameTime(v.UpdatedAt, at) || !sameTime(v.PublishedAt, base.Add(time.Hour)) || v.IsLatest {
			t.Fatalf("deprecated version = %+v", v)
		}
		if got := latestMCP(t, st, srv.ID); got != "1.2.0" {
			t.Fatalf("latest = %q", got)
		}
		// Nothing differs: nothing changes, and the timestamps stay.
		setMCPStatus(t, st, srv.ID, "1.1.0", model.MCPStatusDeprecated, "use 1.2.0", at.Add(time.Hour), 0)
		if v := mcpVersion(t, st, srv.ID, "1.1.0"); !sameTime(v.UpdatedAt, at) || !sameTime(v.StatusChangedAt, at) {
			t.Fatalf("unchanged version was stamped: %+v", v)
		}
		// Only the message differs.
		setMCPStatus(t, st, srv.ID, "1.1.0", model.MCPStatusDeprecated, "use 1.2.0 instead", at, 1)

		// Deleting the latest version moves the flag to the best remaining
		// one, even a deprecated one, and stamps both rows.
		at2 := at.Add(2 * time.Hour)
		setMCPStatus(t, st, srv.ID, "1.2.0", model.MCPStatusDeleted, "broken", at2, 1)
		if got := latestMCP(t, st, srv.ID); got != "1.1.0" {
			t.Fatalf("latest after delete = %q", got)
		}
		if v := mcpVersion(t, st, srv.ID, "1.1.0"); !sameTime(v.UpdatedAt, at2) || !sameTime(v.StatusChangedAt, at) {
			t.Fatalf("new latest = %+v", v)
		}
		if v := mcpVersion(t, st, srv.ID, "1.2.0"); v.Status != model.MCPStatusDeleted || v.StatusMessage != "broken" || !sameTime(v.UpdatedAt, at2) {
			t.Fatalf("deleted version = %+v", v)
		}
		if v := mcpVersion(t, st, srv.ID, "1.0.0"); !sameTime(v.UpdatedAt, bystander) {
			t.Fatalf("bystander was stamped: %+v", v)
		}

		// Restoring it moves the flag back; active carries no message.
		at3 := at.Add(3 * time.Hour)
		setMCPStatus(t, st, srv.ID, "1.2.0", model.MCPStatusActive, "dropped", at3, 1)
		if v := mcpVersion(t, st, srv.ID, "1.2.0"); v.Status != model.MCPStatusActive || v.StatusMessage != "" || !v.IsLatest {
			t.Fatalf("restored version = %+v", v)
		}
		if got := latestMCP(t, st, srv.ID); got != "1.2.0" {
			t.Fatalf("latest after restore = %q", got)
		}
		if v := mcpVersion(t, st, srv.ID, "1.1.0"); v.IsLatest || !sameTime(v.UpdatedAt, at3) {
			t.Fatalf("former latest = %+v", v)
		}
		setMCPStatus(t, st, srv.ID, "1.2.0", model.MCPStatusActive, "still dropped", at3, 0)

		// Every version at once; only those that differ count.
		at4 := at.Add(4 * time.Hour)
		setMCPStatus(t, st, srv.ID, "1.0.0", model.MCPStatusDeleted, "gone", at4, 1)
		setMCPStatus(t, st, srv.ID, "", model.MCPStatusDeleted, "gone", at4.Add(time.Hour), 2)
		if got := latestMCP(t, st, srv.ID); got != "" {
			t.Fatalf("latest of a fully deleted server = %q", got)
		}
		if v := mcpVersion(t, st, srv.ID, "1.0.0"); !sameTime(v.UpdatedAt, at4) {
			t.Fatalf("already deleted version was stamped: %+v", v)
		}
		setMCPStatus(t, st, srv.ID, "", model.MCPStatusDeleted, "gone", at4.Add(2*time.Hour), 0)

		// A publish into a fully deleted server takes the flag, and gives it
		// up to a higher version once that one is restored.
		low := mcpInput("com.example/status", "0.9.0")
		low.Version.PublishedAt = base.Add(100 * time.Hour)
		if _, v := publishMCP(t, st, low); !v.IsLatest {
			t.Fatal("first live version is not latest")
		}
		at5 := at.Add(200 * time.Hour)
		setMCPStatus(t, st, srv.ID, "", model.MCPStatusActive, "", at5, 3)
		if got := latestMCP(t, st, srv.ID); got != "1.2.0" {
			t.Fatalf("latest after restoring all = %q", got)
		}
		all, err := st.ListMCPVersions(ctx, srv.ID)
		if err != nil || len(all) != 4 {
			t.Fatalf("%d versions, %v", len(all), err)
		}
		for _, v := range all {
			if v.Status != model.MCPStatusActive || v.StatusMessage != "" {
				t.Fatalf("version %s = %s (%q)", v.Version, v.Status, v.StatusMessage)
			}
		}

		// Without semantic versions the newest live version is latest.
		var dated model.MCPServer
		for i, ver := range []string{"mon", "tue", "wed"} {
			in := mcpInput("com.example/dated", ver)
			in.Version.PublishedAt = base.Add(time.Duration(i) * time.Hour)
			dated, _ = publishMCP(t, st, in)
		}
		setMCPStatus(t, st, dated.ID, "wed", model.MCPStatusDeleted, "", at, 1)
		if got := latestMCP(t, st, dated.ID); got != "tue" {
			t.Fatalf("latest = %q", got)
		}
		setMCPStatus(t, st, dated.ID, "tue", model.MCPStatusDeleted, "", at, 1)
		if got := latestMCP(t, st, dated.ID); got != "mon" {
			t.Fatalf("latest = %q", got)
		}
		setMCPStatus(t, st, dated.ID, "tue", model.MCPStatusDeprecated, "", at, 1)
		if got := latestMCP(t, st, dated.ID); got != "tue" {
			t.Fatalf("latest = %q", got)
		}
	}},

	{"mcp mirror upsert", func(t *testing.T, st store.Store) {
		ctx := context.Background()
		up, other := newUpstream(t, st, "up"), newUpstream(t, st, "other")
		publishMCP(t, st, mcpInput("com.example/local", "1.0.0"))
		published := time.Date(2029, 3, 4, 5, 6, 7, 0, time.UTC)

		in := mirrorInput(up, "org.mirror/Tool", "1.0.0")
		in.Visibility = model.VisibilityInternal
		in.Version.Status, in.Version.StatusMessage, in.Version.IsLatest = model.MCPStatusDeprecated, "old", true
		in.Version.PublishedAt, in.Version.StatusChangedAt, in.Version.UpdatedAt = published, published.Add(time.Hour), published.Add(2*time.Hour)
		mirrorMCP(t, st, in, model.MirrorCreated)
		if ns, err := st.GetNamespace(ctx, "org.mirror"); err != nil || ns.Kind != model.NSKindUpstream || ns.UpstreamID != up.ID {
			t.Fatalf("namespace = %+v, %v", ns, err)
		}
		srv := mcpServer(t, st, "org.mirror/Tool")
		if srv.Namespace != "org.mirror" || srv.Mode != model.MCPModeMirrored || srv.Visibility != model.VisibilityInternal ||
			!srv.Listed || srv.Upstream != "up" || srv.CreatedBy != "upstream:up" {
			t.Fatalf("server = %+v", srv)
		}
		v := mcpVersion(t, st, srv.ID, "1.0.0")
		if v.Status != model.MCPStatusDeprecated || v.StatusMessage != "old" || !v.IsLatest || v.Digest != in.Version.Digest ||
			!sameJSON(v.ServerJSON, in.Version.ServerJSON) || v.CreatedBy != "upstream:up" || !sameTime(v.PublishedAt, published) ||
			!sameTime(v.StatusChangedAt, published.Add(time.Hour)) || !sameTime(v.UpdatedAt, published.Add(2*time.Hour)) {
			t.Fatalf("version = %+v", v)
		}

		// The same input again writes nothing, even with newer timestamps.
		same := in
		same.Version.UpdatedAt, same.Version.StatusChangedAt = published.Add(50*time.Hour), published.Add(50*time.Hour)
		mirrorMCP(t, st, same, model.MirrorUnchanged)
		if got := mcpVersion(t, st, srv.ID, "1.0.0"); !sameTime(got.UpdatedAt, v.UpdatedAt) || !sameTime(got.StatusChangedAt, v.StatusChangedAt) {
			t.Fatalf("unchanged upsert stamped the row: %+v", got)
		}
		if got := mcpServer(t, st, "org.mirror/Tool"); !sameTime(got.UpdatedAt, srv.UpdatedAt) {
			t.Fatal("unchanged upsert stamped the server")
		}

		// Status, message and latest are mutable; the rest never changes.
		upd := in
		upd.Visibility = model.VisibilityPublic // only used at creation
		upd.Version.Status, upd.Version.StatusMessage = model.MCPStatusActive, "dropped for active"
		upd.Version.StatusChangedAt, upd.Version.UpdatedAt = published.Add(3*time.Hour), published.Add(4*time.Hour)
		upd.Version.PublishedAt, upd.Version.CreatedBy = published.Add(99*time.Hour), "someone else"
		mirrorMCP(t, st, upd, model.MirrorUpdated)
		v = mcpVersion(t, st, srv.ID, "1.0.0")
		if v.Status != model.MCPStatusActive || v.StatusMessage != "" || !v.IsLatest || !sameTime(v.StatusChangedAt, published.Add(3*time.Hour)) ||
			!sameTime(v.UpdatedAt, published.Add(4*time.Hour)) || !sameTime(v.PublishedAt, published) || v.CreatedBy != "upstream:up" {
			t.Fatalf("updated version = %+v", v)
		}
		if got := mcpServer(t, st, "org.mirror/Tool"); got.Visibility != model.VisibilityInternal {
			t.Fatalf("update changed the server: %+v", got)
		}
		mirrorMCP(t, st, upd, model.MirrorUnchanged)
		flag := upd
		flag.Version.IsLatest = false
		flag.Version.StatusChangedAt, flag.Version.UpdatedAt = time.Time{}, time.Time{}
		mirrorMCP(t, st, flag, model.MirrorUpdated)
		if got := latestMCP(t, st, srv.ID); got != "" {
			t.Fatalf("latest = %q", got)
		}
		// Zero times mean now.
		if v := mcpVersion(t, st, srv.ID, "1.0.0"); !isNow(v.UpdatedAt) || !isNow(v.StatusChangedAt) {
			t.Fatalf("zero times were not defaulted: %+v", v)
		}
		mirrorMCP(t, st, upd, model.MirrorUpdated)

		// The flag moves between versions; the one that lost it is stamped.
		v2 := mirrorInput(up, "org.mirror/Tool", "2.0.0")
		v2.Version.IsLatest = true
		mirrorMCP(t, st, v2, model.MirrorCreated)
		if got := latestMCP(t, st, srv.ID); got != "2.0.0" {
			t.Fatalf("latest = %q", got)
		}
		if v := mcpVersion(t, st, srv.ID, "1.0.0"); v.IsLatest || !isNow(v.UpdatedAt) {
			t.Fatalf("former latest = %+v", v)
		}
		if v := mcpVersion(t, st, srv.ID, "2.0.0"); !isNow(v.PublishedAt) || !isNow(v.UpdatedAt) || !isNow(v.StatusChangedAt) || v.Status != model.MCPStatusActive {
			t.Fatalf("version with defaulted times = %+v", v)
		}
		mirrorMCP(t, st, upd, model.MirrorUpdated) // 1.0.0 takes the flag back
		if got := latestMCP(t, st, srv.ID); got != "1.0.0" {
			t.Fatalf("latest = %q", got)
		}
		v3 := mirrorInput(up, "org.mirror/Tool", "3.0.0")
		mirrorMCP(t, st, v3, model.MirrorCreated) // not latest upstream: the flag stays
		if got := latestMCP(t, st, srv.ID); got != "1.0.0" {
			t.Fatalf("latest = %q", got)
		}

		// A deleted version is never latest, at creation or on update.
		del := mirrorInput(up, "org.mirror/Tool", "4.0.0")
		del.Version.Status, del.Version.StatusMessage, del.Version.IsLatest = model.MCPStatusDeleted, "withdrawn", true
		mirrorMCP(t, st, del, model.MirrorCreated)
		if v := mcpVersion(t, st, srv.ID, "4.0.0"); v.IsLatest || v.Status != model.MCPStatusDeleted || v.StatusMessage != "withdrawn" {
			t.Fatalf("deleted version = %+v", v)
		}
		if got := latestMCP(t, st, srv.ID); got != "1.0.0" {
			t.Fatalf("latest = %q", got)
		}
		mirrorMCP(t, st, del, model.MirrorUnchanged)
		gone := upd
		gone.Version.Status, gone.Version.IsLatest = model.MCPStatusDeleted, true
		mirrorMCP(t, st, gone, model.MirrorUpdated)
		if got := latestMCP(t, st, srv.ID); got != "" {
			t.Fatalf("latest = %q", got)
		}

		// A different document under a stored version is refused untouched.
		forged := mirrorInput(up, "org.mirror/Tool", "2.0.0")
		forged.Version.ServerJSON = serverJSON("org.mirror/Tool", "2.0.0", "forged", "")
		forged.Version.Digest = digestOf(forged.Version.ServerJSON)
		forged.Version.Status, forged.Version.StatusMessage = model.MCPStatusDeprecated, "forged"
		beforeForge := mcpVersion(t, st, srv.ID, "2.0.0")
		if _, err := st.UpsertMirroredMCP(ctx, forged, audit); conflictCode(err) != "digest_mismatch" {
			t.Fatalf("different digest: %v", err)
		}
		if got := mcpVersion(t, st, srv.ID, "2.0.0"); got.Digest != v2.Version.Digest || !sameJSON(got.ServerJSON, v2.Version.ServerJSON) ||
			got.Status != model.MCPStatusActive || got.StatusMessage != "" || !sameTime(got.UpdatedAt, beforeForge.UpdatedAt) {
			t.Fatalf("version after refused upsert = %+v", got)
		}

		// Namespace ownership.
		for name, in := range map[string]model.MCPMirrorInput{
			"local namespace":                 mirrorInput(up, "com.example/mirrored", "1.0.0"),
			"local namespace, existing local": mirrorInput(up, "com.example/local", "1.0.0"),
			"namespace of another upstream":   mirrorInput(other, "org.mirror/Tool", "9.0.0"),
			"new server, another upstream":    mirrorInput(other, "org.mirror/fresh", "1.0.0"),
		} {
			if _, err := st.UpsertMirroredMCP(ctx, in, audit); conflictCode(err) != "namespace_upstream_managed" {
				t.Fatalf("%s: %v", name, err)
			}
		}
		missing := mirrorInput(up, "org.mirror/Tool", "9.0.0")
		missing.UpstreamID = 1 << 40
		if _, err := st.UpsertMirroredMCP(ctx, missing, audit); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("missing upstream: %v", err)
		}
		invalid := mirrorInput(up, "org.mirror/Tool", "9.0.0")
		invalid.Namespace = "Not_A_Namespace"
		if _, err := st.UpsertMirroredMCP(ctx, invalid, audit); policyCode(err) != "namespace_invalid" {
			t.Fatalf("invalid namespace: %v", err)
		}

		// Server names: another letter case, a local server, another
		// namespace of the same upstream. A refused upsert must not leave the
		// namespace it would have created.
		caseDiff := mirrorInput(up, "org.mirror/tool", "1.0.0")
		localName := mirrorInput(up, "com.example/local", "5.0.0")
		localName.Namespace = "org.fresh"
		moved := mirrorInput(up, "org.mirror/Tool", "9.0.0")
		moved.Namespace = "org.moved"
		for name, in := range map[string]model.MCPMirrorInput{"letter case": caseDiff, "local server": localName, "other namespace": moved} {
			if _, err := st.UpsertMirroredMCP(ctx, in, audit); conflictCode(err) != "server_name_conflict" {
				t.Fatalf("%s: %v", name, err)
			}
		}
		for _, ns := range []string{"org.fresh", "org.moved"} {
			if _, err := st.GetNamespace(ctx, ns); !errors.Is(err, model.ErrNotFound) {
				t.Fatalf("refused upsert created namespace %s: %v", ns, err)
			}
		}
		if all, err := st.ListMCPVersions(ctx, srv.ID); err != nil || len(all) != 4 {
			t.Fatalf("%d versions after refused upserts, %v", len(all), err)
		}
		local := mcpServer(t, st, "com.example/local")
		if all, err := st.ListMCPVersions(ctx, local.ID); err != nil || len(all) != 1 || local.Mode != model.MCPModeRegistered {
			t.Fatalf("local server after refused upserts: %d versions, %+v, %v", len(all), local, err)
		}
		// A mirrored name cannot be published locally either.
		taken := mcpInput("org.mirror/Tool", "9.0.0")
		taken.Namespace = "com.example"
		if _, _, err := st.PublishMCPVersion(ctx, taken, audit); conflictCode(err) != "mode_mismatch" {
			t.Fatalf("local publish of a mirrored name: %v", err)
		}
	}},

	{"concurrent mcp mirror upsert", func(t *testing.T, st store.Store) {
		ctx := context.Background()
		up := newUpstream(t, st, "up")
		const n = 20
		outcomes := make([]string, n)
		for i, err := range parallel(n, func(i int) error {
			in := mirrorInput(up, "org.raced/tool", fmt.Sprintf("1.%d.0", i))
			in.Version.IsLatest = true
			var err error
			outcomes[i], err = st.UpsertMirroredMCP(ctx, in, audit)
			return err
		}) {
			if err != nil || outcomes[i] != model.MirrorCreated {
				t.Fatalf("mirror %d = %q, %v", i, outcomes[i], err)
			}
		}
		srv := mcpServer(t, st, "org.raced/tool")
		if all, err := st.ListMCPVersions(ctx, srv.ID); err != nil || len(all) != n {
			t.Fatalf("%d versions, %v", len(all), err)
		}
		if got := latestMCP(t, st, srv.ID); got == "" {
			t.Fatal("no version is latest")
		}
		// The same version from every caller: one creates it, the others
		// find it as they would have written it.
		created := 0
		for i, err := range parallel(n, func(i int) error {
			var err error
			outcomes[i], err = st.UpsertMirroredMCP(ctx, mirrorInput(up, "org.raced/same", "1.0.0"), audit)
			return err
		}) {
			switch {
			case err != nil:
				t.Fatalf("mirror %d: %v", i, err)
			case outcomes[i] == model.MirrorCreated:
				created++
			case outcomes[i] != model.MirrorUnchanged:
				t.Fatalf("mirror %d = %q", i, outcomes[i])
			}
		}
		same := mcpServer(t, st, "org.raced/same")
		if all, err := st.ListMCPVersions(ctx, same.ID); created != 1 || err != nil || len(all) != 1 {
			t.Fatalf("%d creators, %d versions, %v", created, len(all), err)
		}
	}},

	{"mcp search", func(t *testing.T, st store.Store) {
		ctx := context.Background()
		up := newUpstream(t, st, "up")
		newNamespace(t, st, "org.other")
		base := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		clock := base
		pub := func(name, version, visibility string, listed bool, title, description string) {
			in := mcpInput(name, version)
			in.Visibility, in.Listed = visibility, listed
			in.Version.ServerJSON = serverJSON(name, version, title, description)
			in.Version.Digest = digestOf(in.Version.ServerJSON)
			clock = clock.Add(time.Minute)
			in.Version.PublishedAt = clock
			publishMCP(t, st, in)
		}
		pub("com.example/Zeta", "1.0.0", model.VisibilityPublic, true, "Zeta", "plain")
		pub("com.example/alpha", "1.0.0", model.VisibilityPublic, true, "Weather 100% Tool", "fetch_data now")
		pub("com.example/alpha", "1.10.0", model.VisibilityPublic, true, "Weather", "forecast")
		pub("com.example/alpha", "1.2.0", model.VisibilityPublic, true, "Weather", "forecast")
		pub("com.example/internal-one", "1.0.0", model.VisibilityInternal, true, "Internal", "")
		pub("com.example/private-one", "1.0.0", model.VisibilityPrivate, true, "Private", "")
		pub("org.other/private-two", "1.2.0", model.VisibilityPrivate, true, "Private", "")
		pub("com.example/unlisted", "1.0.0", model.VisibilityPublic, false, "Weather", "hidden")
		mir := mirrorInput(up, "org.mirror/tool", "0.1.0")
		mir.Version.IsLatest = true
		mir.Version.PublishedAt, mir.Version.UpdatedAt, mir.Version.StatusChangedAt = base, base, base
		mirrorMCP(t, st, mir, model.MirrorCreated)

		// Byte order: uppercase before lowercase, versions as strings.
		public := []string{"com.example/Zeta@1.0.0", "com.example/alpha@1.0.0", "com.example/alpha@1.10.0", "com.example/alpha@1.2.0",
			"org.mirror/tool@0.1.0"}
		withInternal := slices.Insert(slices.Clone(public), 4, "com.example/internal-one@1.0.0")
		member := slices.Insert(slices.Clone(withInternal), 5, "com.example/private-one@1.0.0")
		everything := slices.Insert(slices.Clone(member), 7, "org.other/private-two@1.2.0")
		admin := model.Viewer{Admin: true}
		check := func(name string, q model.MCPQuery, want ...string) {
			t.Helper()
			if got := searchMCP(t, st, q); !slices.Equal(got, want) {
				t.Fatalf("%s:\n got %v\nwant %v", name, got, want)
			}
		}
		check("anonymous", model.MCPQuery{}, public...)
		check("authenticated", model.MCPQuery{Viewer: model.Viewer{Authenticated: true}}, withInternal...)
		check("member", model.MCPQuery{Viewer: model.Viewer{Authenticated: true, Namespaces: []string{"com.example"}}}, member...)
		check("member of both", model.MCPQuery{Viewer: model.Viewer{Authenticated: true, Namespaces: []string{"com.example", "org.other"}}}, everything...)
		// Membership alone grants private entries, not internal ones.
		check("unauthenticated member", model.MCPQuery{Viewer: model.Viewer{Namespaces: []string{"org.other"}}},
			append(slices.Clone(public), "org.other/private-two@1.2.0")...)
		check("admin", model.MCPQuery{Viewer: admin}, everything...)

		entries, err := st.SearchMCP(ctx, model.MCPQuery{Viewer: admin})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			wantNS, _, _ := strings.Cut(e.Server.Name, "/")
			wantUp := ""
			if wantNS == "org.mirror" {
				wantUp = "up"
			}
			var doc struct{ Name, Version string }
			if err := json.Unmarshal(e.Version.ServerJSON, &doc); err != nil || doc.Name != e.Server.Name || doc.Version != e.Version.Version {
				t.Fatalf("entry document = %s, %v", e.Version.ServerJSON, err)
			}
			if e.Server.Namespace != wantNS || e.Server.Upstream != wantUp || e.Version.ServerID != e.Server.ID {
				t.Fatalf("entry = %+v", e)
			}
		}

		// Pages chained by cursor cover every row once, in order.
		for _, size := range []int{1, 2, 3, 100} {
			var got []string
			q := model.MCPQuery{Viewer: admin, Limit: size}
			for {
				page := searchMCP(t, st, q)
				if len(page) == 0 {
					break
				}
				if len(page) > size {
					t.Fatalf("page of %d rows with limit %d", len(page), size)
				}
				got = append(got, page...)
				if len(got) > len(everything) {
					break // a cursor that does not advance must not loop forever
				}
				q.AfterName, q.AfterVersion, _ = strings.Cut(page[len(page)-1], "@")
			}
			if !slices.Equal(got, everything) {
				t.Fatalf("pages of %d:\n got %v\nwant %v", size, got, everything)
			}
		}
		check("cursor inside a server", model.MCPQuery{AfterName: "com.example/alpha", AfterVersion: "1.0.0", Limit: 2},
			"com.example/alpha@1.10.0", "com.example/alpha@1.2.0")
		check("cursor between names", model.MCPQuery{AfterName: "com.example/a"}, public[1:]...)

		check("latest", model.MCPQuery{Viewer: admin, Version: "latest"}, "com.example/Zeta@1.0.0", "com.example/alpha@1.10.0",
			"com.example/internal-one@1.0.0", "com.example/private-one@1.0.0", "org.mirror/tool@0.1.0", "org.other/private-two@1.2.0")
		check("exact version", model.MCPQuery{Viewer: admin, Version: "1.2.0"}, "com.example/alpha@1.2.0", "org.other/private-two@1.2.0")
		check("unknown version", model.MCPQuery{Viewer: admin, Version: "7.7.7"})
		check("namespace", model.MCPQuery{Viewer: admin, Namespace: "org.other"}, "org.other/private-two@1.2.0")
		check("namespace is exact", model.MCPQuery{Viewer: admin, Namespace: "org"})
		check("mode", model.MCPQuery{Viewer: admin, Mode: model.MCPModeMirrored}, "org.mirror/tool@0.1.0")
		check("mode and namespace", model.MCPQuery{Mode: model.MCPModeRegistered, Namespace: "com.example", Version: "latest"},
			"com.example/Zeta@1.0.0", "com.example/alpha@1.10.0")

		// Substring search over name, title and description, any letter
		// case; LIKE wildcards are literals; unlisted entries never match.
		check("name", model.MCPQuery{Search: "ZETA"}, "com.example/Zeta@1.0.0")
		check("name across namespaces", model.MCPQuery{Viewer: admin, Search: "private-"}, "com.example/private-one@1.0.0", "org.other/private-two@1.2.0")
		check("title", model.MCPQuery{Search: "weather"}, "com.example/alpha@1.0.0", "com.example/alpha@1.10.0", "com.example/alpha@1.2.0")
		check("description", model.MCPQuery{Search: "FoReCaSt"}, "com.example/alpha@1.10.0", "com.example/alpha@1.2.0")
		check("percent", model.MCPQuery{Viewer: admin, Search: "%"}, "com.example/alpha@1.0.0")
		check("percent in context", model.MCPQuery{Viewer: admin, Search: "100% t"}, "com.example/alpha@1.0.0")
		check("underscore", model.MCPQuery{Viewer: admin, Search: "_"}, "com.example/alpha@1.0.0")
		check("underscore in context", model.MCPQuery{Viewer: admin, Search: "fetch_data"}, "com.example/alpha@1.0.0")
		check("wildcards do not match", model.MCPQuery{Viewer: admin, Search: "w%r"})
		check("backslash", model.MCPQuery{Viewer: admin, Search: `\`})
		check("no match", model.MCPQuery{Viewer: admin, Search: "hidden"})
		check("search and latest", model.MCPQuery{Search: "weather", Version: "latest"}, "com.example/alpha@1.10.0")

		// Incremental listing by update time, inclusive.
		alpha := mcpServer(t, st, "com.example/alpha")
		later := base.Add(1000 * time.Hour)
		setMCPStatus(t, st, alpha.ID, "1.0.0", model.MCPStatusDeprecated, "old", later, 1)
		after := later.Add(time.Microsecond)
		check("updated since", model.MCPQuery{Viewer: admin, UpdatedSince: &later}, "com.example/alpha@1.0.0")
		check("updated since, later", model.MCPQuery{Viewer: admin, UpdatedSince: &after})
		check("updated since the start", model.MCPQuery{Viewer: admin, UpdatedSince: &base}, everything...)

		// Deleted versions are listed only on request.
		zeta := mcpServer(t, st, "com.example/Zeta")
		setMCPStatus(t, st, zeta.ID, "", model.MCPStatusDeleted, "gone", later, 1)
		setMCPStatus(t, st, alpha.ID, "1.10.0", model.MCPStatusDeleted, "gone", later, 1)
		check("deleted are hidden", model.MCPQuery{}, "com.example/alpha@1.0.0", "com.example/alpha@1.2.0", "org.mirror/tool@0.1.0")
		check("include deleted", model.MCPQuery{IncludeDeleted: true}, public...)
		check("latest moved off the deleted version", model.MCPQuery{Version: "latest", IncludeDeleted: true},
			"com.example/alpha@1.2.0", "org.mirror/tool@0.1.0")
		check("deleted exact version", model.MCPQuery{Version: "1.10.0"})
		check("deleted exact version on request", model.MCPQuery{Version: "1.10.0", IncludeDeleted: true}, "com.example/alpha@1.10.0")

		// Unlisting hides every version; listing again brings them back.
		off, on := false, true
		if _, err := st.UpdateMCPServer(ctx, alpha.ID, model.MCPPatch{Listed: &off}, audit); err != nil {
			t.Fatal(err)
		}
		check("unlisted", model.MCPQuery{Viewer: admin, Namespace: "com.example", IncludeDeleted: true},
			"com.example/Zeta@1.0.0", "com.example/internal-one@1.0.0", "com.example/private-one@1.0.0")
		if _, err := st.UpdateMCPServer(ctx, alpha.ID, model.MCPPatch{Listed: &on}, audit); err != nil {
			t.Fatal(err)
		}
		check("listed again", model.MCPQuery{Search: "weather"}, "com.example/alpha@1.0.0", "com.example/alpha@1.2.0")
	}},
}
