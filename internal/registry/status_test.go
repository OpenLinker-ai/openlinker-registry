package registry

import (
	"context"
	"errors"
	"testing"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
)

// A deprecate request that read the skill before a yank completed must not
// overwrite the yank.
func TestDelayedDeprecateCannotOverwriteYank(t *testing.T) {
	svc := newGCService(t)
	if err := publishGC(svc, "1"); err != nil {
		t.Fatal(err)
	}
	stale, err := svc.Store.GetSkill(context.Background(), "com.example", "gc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetStatus(context.Background(), "a", stale, model.StatusYanked, "bad"); err != nil {
		t.Fatal(err)
	}
	_, err = svc.SetStatus(context.Background(), "b", stale, model.StatusDeprecated, "late")
	if e := AsError(err); e.Status != 409 || e.Code != "skill_yanked" {
		t.Fatalf("got %v", err)
	}
	cur, _ := svc.Store.GetSkill(context.Background(), "com.example", "gc")
	if cur.Status != model.StatusYanked {
		t.Fatalf("status = %s", cur.Status)
	}
}

// A visibility change validated against an earlier snapshot of versions is
// still rejected when a version with an unknown license now exists.
func TestStaleSettingsCannotPublishUnknownLicense(t *testing.T) {
	svc := newGCService(t)
	md := []byte("---\nname: gc\ndescription: no license\n---\nBody.\n")
	if _, _, err := svc.PublishUpload(context.Background(), PublishOptions{Actor: "t", Namespace: "com.example", Name: "gc",
		Version: "1", Visibility: model.VisibilityInternal}, UploadSkillMD, md); err != nil {
		t.Fatal(err)
	}
	sk, _ := svc.Store.GetSkill(context.Background(), "com.example", "gc")
	public := model.VisibilityPublic
	_, err := svc.UpdateSettings(context.Background(), "o", sk, model.SkillPatch{Visibility: &public})
	if e := AsError(err); e.Status != 422 || e.Code != model.CodeLicenseRequired {
		t.Fatalf("got %v", err)
	}
	var pe *model.PolicyError
	_, err = svc.Store.UpdateSkill(context.Background(), sk.ID, model.SkillPatch{Visibility: &public}, model.AuditEvent{})
	if !errors.As(err, &pe) {
		t.Fatalf("store must enforce the rule itself: %v", err)
	}
}
