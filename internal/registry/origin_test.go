package registry

import (
	"errors"
	"net/http"
	"testing"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/safehttp"
)

// A download the upstream's own origin refuses is a failure of the upstream,
// however the entry URL spells that origin.
func TestUpstreamFailureComparesOrigins(t *testing.T) {
	refused := &safehttp.Error{Code: safehttp.CodeStatus, Status: http.StatusUnauthorized}
	unreachable := &safehttp.Error{Code: safehttp.CodeUnreachable}
	u := model.Upstream{BaseURL: "https://site.example"}
	for entry, want := range map[string]bool{
		"https://site.example/a/SKILL.md":      true,
		"https://site.example:443/a/SKILL.md":  true,
		"https://SITE.Example:443/a/SKILL.md":  true,
		"https://site.example:8443/a/SKILL.md": false,
		"http://site.example/a/SKILL.md":       false,
		"https://cdn.example/a/SKILL.md":       false,
		"https://site.example.evil/a/SKILL.md": false,
	} {
		for _, err := range []error{refused, unreachable} {
			if got := upstreamFailure(u, entry, err); got != want {
				t.Errorf("%s (%v): upstream failure = %v, want %v", entry, err, got, want)
			}
		}
	}
	plain := model.Upstream{BaseURL: "http://site.example:80/skills"}
	if !upstreamFailure(plain, "http://site.example/a/SKILL.md", refused) {
		t.Error("the default port of http is 80")
	}
	// An answer about the entry itself is never a failure of the upstream.
	for _, err := range []error{
		&safehttp.Error{Code: safehttp.CodeStatus, Status: http.StatusInternalServerError},
		&safehttp.Error{Code: safehttp.CodeRedirectNotFollowed},
		&safehttp.Error{Code: safehttp.CodeTooLarge},
	} {
		if upstreamFailure(u, "https://site.example/a/SKILL.md", err) {
			t.Errorf("%v on the upstream's origin is about the entry", err)
		}
	}
	if !upstreamFailure(u, "https://site.example/a/SKILL.md", errors.New("connection reset")) {
		t.Error("a failure that is no answer at all is a failure of the upstream")
	}
}
