package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

var reviewTests = []testCase{
	{"writes authorized before a claim cannot land after it", func(t *testing.T, st store.Store) {
		ctx := context.Background()
		newNamespace(t, st, "com.author")
		sk, v, err := publish(st, "moving", model.VisibilityPublic, "1", skill.LicenseSPDX)
		if err != nil {
			t.Fatal(err)
		}
		stale := model.WithSkillGuard(ctx, sk.ID, "com.example")
		moved, err := st.MoveSkill(model.WithSkillGuard(ctx, sk.ID, "com.example"), sk.ID, "com.author", "moving", audit)
		if err != nil || moved.Namespace != "com.author" || !moved.Claimed {
			t.Fatalf("move: %+v %v", moved, err)
		}
		listed := false
		if _, err := st.UpdateSkill(stale, sk.ID, model.SkillPatch{Listed: &listed}, audit); conflictCode(err) != "entry_moved" {
			t.Fatalf("patch after claim: %v", err)
		}
		if _, err := st.SetSkillStatus(stale, sk.ID, model.StatusYanked, "late", audit); conflictCode(err) != "entry_moved" {
			t.Fatalf("yank after claim: %v", err)
		}
		if err := st.YankVersion(stale, v.ID, "late", time.Now(), audit); conflictCode(err) != "entry_moved" {
			t.Fatalf("version yank after claim: %v", err)
		}
		if _, err := st.MoveSkill(stale, sk.ID, "com.author", "again", audit); conflictCode(err) != "entry_moved" {
			t.Fatalf("second claim from a stale view: %v", err)
		}
		if _, _, err := publish(st, "moving", model.VisibilityPublic, "2", skill.LicenseSPDX); conflictCode(err) != "name_reserved" {
			t.Fatalf("recreate the old name: %v", err)
		}
		got, err := st.GetSkill(ctx, "com.author", "moving")
		if err != nil || got.Status != model.StatusActive || !got.Listed {
			t.Fatalf("author entry changed: %+v %v", got, err)
		}
		if r, err := st.SkillRedirect(ctx, "com.example", "moving"); err != nil || r.ID != sk.ID {
			t.Fatalf("redirect: %+v %v", r, err)
		}
		if _, err := st.MoveSkill(ctx, sk.ID, "com.example", "moving", audit); conflictCode(err) != "claim_name_taken" {
			t.Fatalf("move back onto a reserved name: %v", err)
		}
	}},
	{"reports are kept per entry across a claim", func(t *testing.T, st store.Store) {
		ctx := context.Background()
		newNamespace(t, st, "com.author")
		sk, _, err := publish(st, "reported", model.VisibilityPublic, "1", skill.LicenseSPDX)
		if err != nil {
			t.Fatal(err)
		}
		rep := model.Report{Kind: "skill", EntryID: sk.ID, Namespace: "com.example", Name: "reported", Reporter: "a", Category: "spam"}
		if res, err := st.CreateReport(ctx, rep, 2, false, audit); err != nil || !res.Created || res.Reopened {
			t.Fatalf("first report: %+v %v", res, err)
		}
		if _, err := st.MoveSkill(ctx, sk.ID, "com.author", "reported", audit); err != nil {
			t.Fatal(err)
		}
		rep.Reporter = "b"
		res, err := st.CreateReport(ctx, rep, 2, false, audit)
		if err != nil || !res.Reopened {
			t.Fatalf("second report after the claim: %+v %v", res, err)
		}
		got, _ := st.GetSkill(ctx, "com.author", "reported")
		if got.ReviewState != model.ReviewPending {
			t.Fatalf("review state: %+v", got)
		}
		items, err := st.ListReview(ctx, "skill", model.ReviewPending, 10)
		if err != nil || len(items) != 1 || items[0].OpenReports != 2 || items[0].Namespace != "com.author" {
			t.Fatalf("queue: %+v %v", items, err)
		}
	}},
}
