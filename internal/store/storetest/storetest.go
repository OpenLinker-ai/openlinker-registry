// Package storetest holds store conformance tests for rules that each
// store.Store implementation must enforce atomically with the mutation,
// independent of any snapshot the caller read earlier.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

var audit = model.AuditEvent{Actor: "test", Action: "test"}

func version(v, license string) model.Version {
	return model.Version{Version: v, FrontmatterName: "x", FrontmatterDescription: "y", LicenseKind: license,
		Files: []skill.ManifestEntry{}, ArtifactDigest: "sha256:" + strings.Repeat("a", 64), ArtifactType: "skill-md",
		Source: &model.Source{Kind: "upload"}, CreatedBy: "test"}
}

func publish(st store.Store, name, visibility, ver, license string) (model.Skill, model.Version, error) {
	return st.PublishVersion(context.Background(), model.PublishInput{Namespace: "com.example", Name: name, Mode: skill.ModeHosted,
		Visibility: visibility, Listed: true, Version: version(ver, license)}, audit)
}

func isPolicy(err error) bool {
	var pe *model.PolicyError
	return errors.As(err, &pe) && pe.Code == model.CodeLicenseRequired
}

// conflictCode is the code of a ConflictError, "" for any other error.
func conflictCode(err error) string {
	var ce *model.ConflictError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

// policyCode is the code of a PolicyError, "" for any other error.
func policyCode(err error) string {
	var pe *model.PolicyError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// sameTime compares at the precision every store keeps (microseconds).
func sameTime(a, b time.Time) bool {
	return a.Truncate(time.Microsecond).Equal(b.Truncate(time.Microsecond))
}

// parallel runs f n times concurrently and returns the errors by index.
func parallel(n int, f func(i int) error) []error {
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = f(i)
		}()
	}
	wg.Wait()
	return errs
}

// testCase is one conformance test; run receives a store that holds only the
// local namespace com.example.
type testCase struct {
	name string
	run  func(t *testing.T, st store.Store)
}

// Run executes the conformance tests; newStore must return an empty store.
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	setup := func(t *testing.T) store.Store {
		st := newStore(t)
		if _, err := st.CreateNamespace(context.Background(), model.Namespace{Name: "com.example", Kind: "domain"}, audit); err != nil {
			t.Fatal(err)
		}
		return st
	}

	t.Run("yank is terminal against stale snapshots", func(t *testing.T) {
		st := setup(t)
		ctx := context.Background()
		stale, _, err := publish(st, "y", model.VisibilityPublic, "1", skill.LicenseSPDX)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetSkillStatus(ctx, stale.ID, model.StatusYanked, "bad", audit); err != nil {
			t.Fatal(err)
		}
		// A delayed deprecate built from the pre-yank snapshot.
		var ce *model.ConflictError
		if _, err := st.SetSkillStatus(ctx, stale.ID, model.StatusDeprecated, "late", audit); !errors.As(err, &ce) || ce.Code != "skill_yanked" {
			t.Fatalf("deprecate after yank: %v", err)
		}
		if _, err := st.SetSkillStatus(ctx, stale.ID, model.StatusActive, "", audit); !errors.As(err, &ce) {
			t.Fatalf("reactivate after yank: %v", err)
		}
		cur, err := st.GetSkill(ctx, "com.example", "y")
		if err != nil || cur.Status != model.StatusYanked || cur.StatusReason != "bad" {
			t.Fatalf("status = %s (%q), %v", cur.Status, cur.StatusReason, err)
		}
		if _, err := st.SetSkillStatus(ctx, 1<<40, model.StatusDeprecated, "", audit); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("missing skill: %v", err)
		}
	})

	t.Run("license rule enforced by the store", func(t *testing.T) {
		st := setup(t)
		ctx := context.Background()
		if _, _, err := publish(st, "pubnew", model.VisibilityPublic, "1", skill.LicenseUnknown); !isPolicy(err) {
			t.Fatalf("new public skill with unknown license: %v", err)
		}
		if _, err := st.GetSkill(ctx, "com.example", "pubnew"); !errors.Is(err, model.ErrNotFound) {
			t.Fatal("rejected publish created the skill")
		}
		pub, _, err := publish(st, "pub", model.VisibilityPublic, "1", skill.LicenseSPDX)
		if err != nil {
			t.Fatal(err)
		}
		// The caller's visibility input is ignored for an existing skill; the
		// stored, current visibility decides.
		if _, _, err := publish(st, "pub", model.VisibilityInternal, "2", skill.LicenseUnknown); !isPolicy(err) {
			t.Fatalf("unknown license into public skill: %v", err)
		}
		if _, err := st.GetVersion(ctx, pub.ID, "2"); !errors.Is(err, model.ErrNotFound) {
			t.Fatal("rejected version was stored")
		}
		in, v, err := publish(st, "int", model.VisibilityInternal, "1", skill.LicenseUnknown)
		if err != nil {
			t.Fatal(err)
		}
		public := model.VisibilityPublic
		if _, err := st.UpdateSkill(ctx, in.ID, model.SkillPatch{Visibility: &public}, audit); !isPolicy(err) {
			t.Fatalf("make public with unknown license: %v", err)
		}
		if cur, _ := st.GetSkill(ctx, "com.example", "int"); cur.Visibility != model.VisibilityInternal {
			t.Fatal("rejected visibility change was applied")
		}
		if err := st.YankVersion(ctx, v.ID, "r", time.Now(), audit); err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpdateSkill(ctx, in.ID, model.SkillPatch{Visibility: &public}, audit); err != nil {
			t.Fatalf("yanked unknown versions do not block: %v", err)
		}
	})

	t.Run("concurrent publish and visibility change", func(t *testing.T) {
		st := setup(t)
		ctx := context.Background()
		public := model.VisibilityPublic
		for i := range 20 {
			name := fmt.Sprintf("race-%d", i)
			sk, _, err := publish(st, name, model.VisibilityInternal, "1", skill.LicenseSPDX)
			if err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, _, _ = publish(st, name, model.VisibilityInternal, "2", skill.LicenseUnknown)
			}()
			go func() {
				defer wg.Done()
				_, _ = st.UpdateSkill(ctx, sk.ID, model.SkillPatch{Visibility: &public}, audit)
			}()
			wg.Wait()
			cur, err := st.GetSkill(ctx, "com.example", name)
			if err != nil {
				t.Fatal(err)
			}
			versions, err := st.ListVersions(ctx, sk.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range versions {
				if cur.Visibility == model.VisibilityPublic && v.YankedAt == nil && v.LicenseKind == skill.LicenseUnknown {
					t.Fatalf("round %d: public hosted skill has an unknown-license version", i)
				}
			}
		}
	})

	for _, cases := range [][]testCase{namespaceTests, mcpTests, upstreamTests, auditTests} {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) { c.run(t, setup(t)) })
		}
	}
}
