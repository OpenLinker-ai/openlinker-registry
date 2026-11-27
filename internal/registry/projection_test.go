package registry

import (
	"testing"

	"github.com/OpenLinker-ai/openlinker-registry/internal/archive"
	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
)

func TestHiddenVersionIsNotDiscoverable(t *testing.T) {
	svc := &Service{Cfg: Config{PublicURL: "https://registry.example"}}
	c := model.IndexCandidate{
		Skill:   model.Skill{Namespace: "ai.openlinker.u0123456789ab", Name: "p-x", Visibility: model.VisibilityPublic, Listed: true, Status: model.StatusActive, ReviewState: model.ReviewApproved},
		Version: model.Version{Version: "1", Mode: skill.ModeHosted, ArtifactType: archive.TypeSkillMD, FrontmatterDescription: "Reads PDFs", ArtifactDigest: "sha256:x"},
	}
	if _, ok, _ := svc.Eligible(c, "p-x"); !ok {
		t.Fatal("a visible version must stay eligible")
	}
	c.Version.Hidden = true
	if _, ok, _ := svc.Eligible(c, "p-x"); ok {
		t.Fatal("a hidden version was eligible for discovery")
	}
}
