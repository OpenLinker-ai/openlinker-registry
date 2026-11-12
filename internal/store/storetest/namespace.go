package storetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

func newUpstream(t *testing.T, st store.Store, name string) model.Upstream {
	t.Helper()
	u, err := st.CreateUpstream(context.Background(), model.Upstream{Name: name, Kind: model.UpstreamMCP,
		BaseURL: "https://" + name + ".example", Enabled: true, Priority: 100, Visibility: model.VisibilityPublic}, audit)
	if err != nil {
		t.Fatalf("create upstream %s: %v", name, err)
	}
	return u
}

func newNamespace(t *testing.T, st store.Store, name string) model.Namespace {
	t.Helper()
	ns, err := st.CreateNamespace(context.Background(), model.Namespace{Name: name, Kind: "domain"}, audit)
	if err != nil {
		t.Fatalf("create namespace %s: %v", name, err)
	}
	return ns
}

var namespaceTests = []testCase{
	{"namespaces are local unless an upstream creates them", func(t *testing.T, st store.Store) {
		ctx := context.Background()
		up := newUpstream(t, st, "up")
		if _, err := st.CreateNamespace(ctx, model.Namespace{Name: "org.viakind", Kind: model.NSKindUpstream, UpstreamID: up.ID}, audit); policyCode(err) != "namespace_invalid" {
			t.Fatalf("CreateNamespace with kind upstream: %v", err)
		}
		if _, err := st.GetNamespace(ctx, "org.viakind"); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("rejected namespace was stored: %v", err)
		}
		// An upstream id on a local kind is ignored, never stored.
		local, err := st.CreateNamespace(ctx, model.Namespace{Name: "org.local", Kind: "domain", UpstreamID: up.ID}, audit)
		if err != nil || local.UpstreamID != 0 {
			t.Fatalf("local namespace: upstream %d, %v", local.UpstreamID, err)
		}
		if got, err := st.GetNamespace(ctx, "org.local"); err != nil || got.UpstreamID != 0 || got.Kind != "domain" {
			t.Fatalf("stored local namespace: %+v, %v", got, err)
		}
		if _, err := st.EnsureUpstreamNamespace(ctx, "a-b.example", up.ID); err != nil {
			t.Fatal(err)
		}
		newNamespace(t, st, "ab.example")
		all, err := st.ListNamespaces(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, ns := range all {
			names = append(names, ns.Name)
			if want := ns.Name == "a-b.example"; (ns.UpstreamID == up.ID) != want || (ns.Kind == model.NSKindUpstream) != want {
				t.Fatalf("listed namespace %+v", ns)
			}
		}
		// Byte order: '-' sorts before letters, whatever the database locale.
		if want := []string{"a-b.example", "ab.example", "com.example", "org.local"}; !slices.Equal(names, want) {
			t.Fatalf("namespaces = %v, want %v", names, want)
		}
	}},

	{"ensure upstream namespace", func(t *testing.T, st store.Store) {
		ctx := context.Background()
		up, other := newUpstream(t, st, "up"), newUpstream(t, st, "other")
		ns, err := st.EnsureUpstreamNamespace(ctx, "org.mirror", up.ID)
		if err != nil || ns.ID == 0 || ns.Name != "org.mirror" || ns.Kind != model.NSKindUpstream || ns.UpstreamID != up.ID {
			t.Fatalf("create: %+v, %v", ns, err)
		}
		again, err := st.EnsureUpstreamNamespace(ctx, "org.mirror", up.ID)
		if err != nil || again.ID != ns.ID || again.UpstreamID != up.ID {
			t.Fatalf("idempotent: %+v, %v", again, err)
		}
		if got, err := st.GetNamespace(ctx, "org.mirror"); err != nil || got.ID != ns.ID || got.UpstreamID != up.ID || got.Kind != model.NSKindUpstream {
			t.Fatalf("stored: %+v, %v", got, err)
		}
		if _, err := st.EnsureUpstreamNamespace(ctx, "org.mirror", other.ID); conflictCode(err) != "namespace_upstream_managed" {
			t.Fatalf("namespace of another upstream: %v", err)
		}
		if _, err := st.EnsureUpstreamNamespace(ctx, "com.example", up.ID); conflictCode(err) != "namespace_upstream_managed" {
			t.Fatalf("local namespace: %v", err)
		}
		if got, _ := st.GetNamespace(ctx, "com.example"); got.UpstreamID != 0 || got.Kind != "domain" {
			t.Fatalf("local namespace changed owner: %+v", got)
		}
		for _, id := range []int64{0, 1 << 40} {
			// Upstream id 0 must never match a local namespace.
			for _, name := range []string{"com.example", "org.fresh"} {
				if _, err := st.EnsureUpstreamNamespace(ctx, name, id); !errors.Is(err, model.ErrNotFound) {
					t.Fatalf("upstream %d, namespace %s: %v", id, name, err)
				}
			}
		}
		if _, err := st.GetNamespace(ctx, "org.fresh"); !errors.Is(err, model.ErrNotFound) {
			t.Fatal("namespace created for a missing upstream")
		}
		for _, bad := range []string{"", "single", "Org.Upper", "org..x", "org.x-", "org.under_score", strings.Repeat("a.", 64) + "b"} {
			if _, err := st.EnsureUpstreamNamespace(ctx, bad, up.ID); policyCode(err) != "namespace_invalid" {
				t.Fatalf("invalid name %q: %v", bad, err)
			}
		}
	}},

	{"concurrent ensure upstream namespace", func(t *testing.T, st store.Store) {
		ctx := context.Background()
		up := newUpstream(t, st, "up")
		ids := make([]int64, 20)
		for i, err := range parallel(len(ids), func(i int) error {
			ns, err := st.EnsureUpstreamNamespace(ctx, "org.raced", up.ID)
			ids[i] = ns.ID
			return err
		}) {
			if err != nil || ids[i] == 0 || ids[i] != ids[0] {
				t.Fatalf("caller %d: id %d (first %d), %v", i, ids[i], ids[0], err)
			}
		}
	}},

	{"skill publish checks the namespace owner", func(t *testing.T, st store.Store) {
		ctx := context.Background()
		up, other := newUpstream(t, st, "up"), newUpstream(t, st, "other")
		if _, err := st.EnsureUpstreamNamespace(ctx, "org.mirror", up.ID); err != nil {
			t.Fatal(err)
		}
		pub := func(ns string, upstreamID int64) (model.Skill, error) {
			sk, _, err := st.PublishVersion(ctx, model.PublishInput{Namespace: ns, Name: "s", Mode: skill.ModeHosted,
				Visibility: model.VisibilityPublic, Listed: true, UpstreamID: upstreamID, Version: version("1", skill.LicenseSPDX)}, audit)
			return sk, err
		}
		for _, c := range []struct {
			ns string
			id int64
		}{{"org.mirror", 0}, {"org.mirror", other.ID}, {"com.example", up.ID}} {
			if _, err := pub(c.ns, c.id); conflictCode(err) != "namespace_upstream_managed" {
				t.Fatalf("publish into %s as upstream %d: %v", c.ns, c.id, err)
			}
			if _, err := st.GetSkill(ctx, c.ns, "s"); !errors.Is(err, model.ErrNotFound) {
				t.Fatalf("rejected publish into %s created the skill", c.ns)
			}
		}
		if _, err := pub("org.missing", up.ID); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("missing namespace: %v", err)
		}
		local, err := pub("com.example", 0)
		if err != nil || local.Upstream != "" {
			t.Fatalf("local publish: upstream %q, %v", local.Upstream, err)
		}
		mirrored, err := pub("org.mirror", up.ID)
		if err != nil || mirrored.Upstream != "up" {
			t.Fatalf("mirror publish: upstream %q, %v", mirrored.Upstream, err)
		}

		// Every read path reports the managing upstream.
		want := map[string]string{"com.example": "", "org.mirror": "up"}
		for ns, upstream := range want {
			if got, err := st.GetSkill(ctx, ns, "s"); err != nil || got.Upstream != upstream {
				t.Fatalf("GetSkill %s: upstream %q, %v", ns, got.Upstream, err)
			}
		}
		listed := true
		if got, err := st.UpdateSkill(ctx, mirrored.ID, model.SkillPatch{Listed: &listed}, audit); err != nil || got.Upstream != "up" {
			t.Fatalf("UpdateSkill: upstream %q, %v", got.Upstream, err)
		}
		if got, err := st.SetSkillStatus(ctx, mirrored.ID, model.StatusActive, "", audit); err != nil || got.Upstream != "up" {
			t.Fatalf("SetSkillStatus: upstream %q, %v", got.Upstream, err)
		}
		found, err := st.Search(ctx, model.SearchQuery{})
		if err != nil || len(found) != 2 {
			t.Fatalf("Search: %d rows, %v", len(found), err)
		}
		for _, sum := range found {
			if sum.Upstream != want[sum.Namespace] {
				t.Fatalf("Search %s: upstream %q", sum.Namespace, sum.Upstream)
			}
		}
		cands, err := st.IndexCandidates(ctx, "", false)
		if err != nil || len(cands) != 2 {
			t.Fatalf("IndexCandidates: %d rows, %v", len(cands), err)
		}
		for _, c := range cands {
			if c.Skill.Upstream != want[c.Skill.Namespace] {
				t.Fatalf("IndexCandidates %s: upstream %q", c.Skill.Namespace, c.Skill.Upstream)
			}
		}
	}},

	{"tokens are listed newest first without secrets", func(t *testing.T, st store.Store) {
		ctx := context.Background()
		expires := time.Now().Add(time.Hour)
		for i := range 3 {
			tok := model.Token{ID: fmt.Sprintf("token%011d", i), SecretHash: make([]byte, 32), Name: fmt.Sprintf("t%d", i),
				Scopes: []string{"read", fmt.Sprintf("s%d", i)}, CreatedBy: "test"}
			if i == 1 {
				tok.ExpiresAt = &expires
			}
			if err := st.CreateToken(ctx, tok, nil, audit); err != nil {
				t.Fatal(err)
			}
			// Creation order must not depend on timestamps colliding.
			time.Sleep(2 * time.Millisecond)
		}
		if err := st.RevokeToken(ctx, "token00000000000", time.Now(), audit); err != nil {
			t.Fatal(err)
		}
		toks, err := st.ListTokens(ctx)
		if err != nil || len(toks) != 3 {
			t.Fatalf("ListTokens: %d, %v", len(toks), err)
		}
		for i, tok := range toks {
			n := 2 - i
			if tok.ID != fmt.Sprintf("token%011d", n) || tok.Name != fmt.Sprintf("t%d", n) || tok.CreatedBy != "test" ||
				!slices.Equal(tok.Scopes, []string{"read", fmt.Sprintf("s%d", n)}) || tok.CreatedAt.IsZero() {
				t.Fatalf("token %d = %+v", i, tok)
			}
			if tok.SecretHash != nil {
				t.Fatalf("token %s exposes its secret hash", tok.ID)
			}
			if (tok.RevokedAt != nil) != (n == 0) || (tok.ExpiresAt != nil) != (n == 1) {
				t.Fatalf("token %s: revoked %v, expires %v", tok.ID, tok.RevokedAt, tok.ExpiresAt)
			}
		}
		// The stored hash is untouched by listing.
		if got, err := st.GetToken(ctx, "token00000000002"); err != nil || len(got.SecretHash) != 32 {
			t.Fatalf("GetToken after list: %d-byte hash, %v", len(got.SecretHash), err)
		}
	}},
}
