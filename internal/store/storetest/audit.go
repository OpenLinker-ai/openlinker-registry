package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

// auditCount returns how many audit events carry the action.
func auditCount(t *testing.T, st store.Store, action string) int {
	t.Helper()
	events, err := st.ListAudit(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Action == action {
			n++
		}
	}
	return n
}

// Writes made on behalf of an upstream are audited like any other write:
// one event per change, none when nothing changed.
var auditTests = []testCase{{"mirror writes are audited", func(t *testing.T, st store.Store) {
	ctx := context.Background()
	up := newUpstream(t, st, "up")
	ev := func(action string) model.AuditEvent { return model.AuditEvent{Actor: "upstream:up", Action: action} }

	if _, err := st.EnsureUpstreamNamespace(ctx, "org.first", up.ID, ev("ns.ensure")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureUpstreamNamespace(ctx, "org.first", up.ID, ev("ns.ensure")); err != nil {
		t.Fatal(err)
	}
	if n := auditCount(t, st, "ns.ensure"); n != 1 {
		t.Fatalf("namespace creation audited %d times, want once (and not when it already exists)", n)
	}

	in := mirrorInput(up, "org.mirror/server", "1.0.0")
	for i, want := range []string{model.MirrorCreated, model.MirrorUnchanged} {
		if got, err := st.UpsertMirroredMCP(ctx, in, ev("mirror.write")); err != nil || got != want {
			t.Fatalf("upsert %d: %s, %v", i, got, err)
		}
	}
	if n := auditCount(t, st, "mirror.write"); n != 1 {
		t.Fatalf("a created version is audited once and an unchanged one not at all; got %d", n)
	}
	// The namespace the upsert created for itself is audited too.
	if n := auditCount(t, st, "namespace.mirror"); n != 1 {
		t.Fatalf("namespace created by a mirror write audited %d times", n)
	}
	in.Version.Status, in.Version.StatusMessage = model.MCPStatusDeprecated, "old"
	if got, err := st.UpsertMirroredMCP(ctx, in, ev("mirror.write")); err != nil || got != model.MirrorUpdated {
		t.Fatalf("status update: %s, %v", got, err)
	}
	if n := auditCount(t, st, "mirror.write"); n != 2 {
		t.Fatalf("an updated version must be audited; got %d events", n)
	}

	if err := st.FinishUpstreamSync(ctx, up.ID, model.UpstreamSyncResult{At: time.Now(), Status: model.SyncOK}, ev("sync.done")); err != nil {
		t.Fatal(err)
	}
	if n := auditCount(t, st, "sync.done"); n != 1 {
		t.Fatalf("sync result audited %d times", n)
	}
}}}
