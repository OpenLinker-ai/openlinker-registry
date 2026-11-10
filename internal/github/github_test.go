package github_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenLinker-ai/openlinker-registry/internal/github"
	"github.com/OpenLinker-ai/openlinker-registry/internal/github/githubtest"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
)

const (
	c1 = "1111111111111111111111111111111111111111"
	c2 = "2222222222222222222222222222222222222222"
)

var md = []byte("---\nname: demo\ndescription: Demo\nlicense: MIT\n---\nBody\r\n名前 🚀\r\n")

func setup(t *testing.T, lim skill.Limits) (*githubtest.Fake, *github.Client) {
	t.Helper()
	f := githubtest.New("acme", "skills")
	t.Cleanup(f.Close)
	f.AddCommit(c1, map[string]githubtest.Entry{
		"skills/demo/SKILL.md":       {Data: md},
		"skills/demo/scripts/run.sh": {Data: []byte("#!/bin/sh\r\necho é\n"), Mode: "100755"},
		"README.md":                  {Data: []byte("root")},
	}, "main", "v1")
	c, err := github.New(github.Config{APIBase: f.API.URL, RawBase: f.Raw.URL, Token: "test-token", Limits: lim})
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func code(err error) string {
	var ge *github.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return ""
}

func TestParseURL(t *testing.T) {
	c, _ := github.New(github.Config{})
	r, err := c.ParseURL("https://github.com/acme/skills/tree/main/skills/demo", "")
	if err != nil || r.Owner != "acme" || r.Repo != "skills" || r.Ref != "main" || r.Dir != "skills/demo" || r.SingleFile {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = c.ParseURL("https://github.com/acme/skills/blob/"+c1+"/skills/demo/SKILL.md", "")
	if err != nil || !r.SingleFile || r.Dir != "skills/demo" || r.Ref != c1 {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err = c.ParseURL("https://github.com/acme/skills", ""); err != nil || r.Ref != "" || r.Dir != "" {
		t.Fatalf("%+v %v", r, err)
	}
	bad := map[string][2]string{
		"ref with slash":       {"https://github.com/acme/skills/tree/main", "feature/x"},
		"encoded slash in ref": {"https://github.com/acme/skills/tree/feature%2Fx/skills", ""},
		"http":                 {"http://github.com/acme/skills", ""},
		"other host":           {"https://evil.example/acme/skills", ""},
		"credentials":          {"https://u:p@github.com/acme/skills", ""},
		"query":                {"https://github.com/acme/skills?x=1", ""},
		"traversal":            {"https://github.com/acme/skills/tree/main/../x", ""},
		"blob not skill":       {"https://github.com/acme/skills/blob/main/README.md", ""},
		"issues":               {"https://github.com/acme/skills/issues/1", ""},
	}
	for name, in := range bad {
		if _, err := c.ParseURL(in[0], in[1]); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := c.ParseURL("https://github.com/acme/skills/tree/main", "feature/x"); code(err) != "ref_unsupported" {
		t.Errorf("ref with slash: got %v", err)
	}
}

func TestResolveAndFetch(t *testing.T) {
	f, c := setup(t, skill.DefaultLimits)
	ctx := context.Background()
	r, _ := c.ParseURL("https://github.com/acme/skills/tree/v1/skills/demo", "")
	commit, err := c.ResolveCommit(ctx, r)
	if err != nil || commit != c1 {
		t.Fatalf("resolve: %s %v", commit, err)
	}
	r0, _ := c.ParseURL("https://github.com/acme/skills", "")
	if commit, err := c.ResolveCommit(ctx, r0); err != nil || commit != c1 {
		t.Fatalf("default branch: %s %v", commit, err)
	}
	snap, err := c.Fetch(ctx, r, commit)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Files) != 2 || snap.Files[0].Path != "SKILL.md" || snap.Files[1].Path != "scripts/run.sh" {
		t.Fatalf("files %+v", snap.Files)
	}
	if snap.TreeSHA == "" || snap.Files[0].GitBlobSHA == "" {
		t.Fatal("tree sha / blob sha missing")
	}
	// Single-file mode returns only SKILL.md.
	rs, _ := c.ParseURL("https://github.com/acme/skills/blob/main/skills/demo/SKILL.md", "")
	snap, err = c.Fetch(ctx, rs, c1)
	if err != nil || len(snap.Files) != 1 {
		t.Fatalf("single: %+v %v", snap, err)
	}
	_ = f
}

func TestFetchRejections(t *testing.T) {
	ctx := context.Background()
	t.Run("unknown ref", func(t *testing.T) {
		_, c := setup(t, skill.DefaultLimits)
		r, _ := c.ParseURL("https://github.com/acme/skills/tree/feature/x/skills", "")
		if _, err := c.ResolveCommit(ctx, r); code(err) != "not_found" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		f, c := setup(t, skill.DefaultLimits)
		f.Truncate = true
		r, _ := c.ParseURL("https://github.com/acme/skills/tree/main/skills/demo", "")
		if _, err := c.Fetch(ctx, r, c1); code(err) != "tree_truncated" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		f, c := setup(t, skill.DefaultLimits)
		f.AddCommit(c2, map[string]githubtest.Entry{"SKILL.md": {Data: md}, "link": {Data: []byte("/etc/passwd"), Mode: "120000"}})
		r, _ := c.ParseURL("https://github.com/acme/skills", "")
		if _, err := c.Fetch(ctx, r, c2); code(err) != "symlink_rejected" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("submodule", func(t *testing.T) {
		f, c := setup(t, skill.DefaultLimits)
		f.AddCommit(c2, map[string]githubtest.Entry{"SKILL.md": {Data: md}, "vendor": {Data: []byte("x"), Mode: "160000"}})
		r, _ := c.ParseURL("https://github.com/acme/skills", "")
		if _, err := c.Fetch(ctx, r, c2); code(err) != "submodule_rejected" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("tampered raw bytes", func(t *testing.T) {
		f, c := setup(t, skill.DefaultLimits)
		// Same length, different content: only the blob SHA can catch it.
		tampered := []byte(strings.Replace(string(md), "Body", "Evil", 1))
		f.Tamper(c1, "skills/demo/SKILL.md", tampered)
		r, _ := c.ParseURL("https://github.com/acme/skills/tree/main/skills/demo", "")
		if _, err := c.Fetch(ctx, r, c1); code(err) != "integrity_mismatch" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("limits before download", func(t *testing.T) {
		f, c := setup(t, skill.Limits{MaxFiles: 1, MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 20})
		r, _ := c.ParseURL("https://github.com/acme/skills/tree/main/skills/demo", "")
		if _, err := c.Fetch(ctx, r, c1); code(err) != "too_many_files" {
			t.Fatalf("got %v", err)
		}
		if f.RawHits.Load() != 0 {
			t.Fatal("files were downloaded before limits were checked")
		}
	})
	t.Run("missing SKILL.md", func(t *testing.T) {
		f, c := setup(t, skill.DefaultLimits)
		f.AddCommit(c2, map[string]githubtest.Entry{"README.md": {Data: []byte("x")}})
		r, _ := c.ParseURL("https://github.com/acme/skills", "")
		if _, err := c.Fetch(ctx, r, c2); code(err) != "skill_md_missing" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("non-hex commit", func(t *testing.T) {
		_, c := setup(t, skill.DefaultLimits)
		r, _ := c.ParseURL("https://github.com/acme/skills", "")
		if _, err := c.Fetch(ctx, r, "main"); code(err) != "commit_invalid" {
			t.Fatalf("got %v", err)
		}
	})
}

func TestOutboundGuard(t *testing.T) {
	// A redirect to another host must not be followed.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("other host contacted") }))
	defer other.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusMovedPermanently)
	}))
	defer api.Close()
	c, err := github.New(github.Config{APIBase: api.URL, RawBase: api.URL})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := c.ParseURL("https://github.com/acme/skills/tree/main", "")
	if _, err := c.ResolveCommit(context.Background(), r); code(err) != "upstream_redirect" {
		t.Fatalf("got %v", err)
	}
	if _, err := github.New(github.Config{APIBase: "https://user:pw@api.example"}); err == nil {
		t.Fatal("credentials in base URL must be rejected")
	}
}
