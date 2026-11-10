package skill

import (
	"strings"
	"testing"
)

func TestValidSlug(t *testing.T) {
	good := []string{"a", "pdf", "my-skill", "a1-b2-c3", strings.Repeat("a", 64)}
	bad := []string{"", "-a", "a-", "a--b", "A", "my_skill", "my.skill", "über", strings.Repeat("a", 65), "a b"}
	for _, s := range good {
		if !ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = false", s)
		}
	}
	for _, s := range bad {
		if ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = true", s)
		}
	}
	if ValidSkillName("indexes") || ValidSkillName("github-imports") {
		t.Error("reserved names must be rejected")
	}
}

func TestValidVersion(t *testing.T) {
	for _, v := range []string{"1.0.0", "v2", "git-0123456789ab", "1.0.0-rc.1"} {
		if !ValidVersion(v) {
			t.Errorf("ValidVersion(%q) = false", v)
		}
	}
	for _, v := range []string{"", ".1", "-1", "1/2", "1 0", strings.Repeat("1", 65)} {
		if ValidVersion(v) {
			t.Errorf("ValidVersion(%q) = true", v)
		}
	}
}

func TestValidateNamespace(t *testing.T) {
	cases := []struct {
		name, kind, mode string
		ok               bool
	}{
		{"io.github.alice", NSKindGitHub, "public", true},
		{"io.github.alice-b", NSKindGitHub, "private", true},
		{"io.github.Alice", NSKindGitHub, "public", false},
		{"io.github.alice.extra", NSKindGitHub, "public", false},
		{"ai.openlinker.bob", NSKindOpenLinker, "public", true},
		{"ai.openlinker.bob", NSKindOpenLinker, "private", false},
		{"internal.team", NSKindInternal, "private", true},
		{"internal.team", NSKindInternal, "public", false},
		{"com.example", NSKindDomain, "public", true},
		{"io.github.mallory", NSKindDomain, "public", false},
		{"internal.x", NSKindDomain, "private", false},
		{"single", NSKindDomain, "public", false},
		{"com..example", NSKindDomain, "public", false},
		{"com.example", "bogus", "public", false},
	}
	for _, c := range cases {
		err := ValidateNamespace(c.name, c.kind, c.mode)
		if (err == nil) != c.ok {
			t.Errorf("ValidateNamespace(%q,%q,%q) err=%v want ok=%v", c.name, c.kind, c.mode, err, c.ok)
		}
	}
}

func TestParseFrontmatter(t *testing.T) {
	fm, err := ParseFrontmatter([]byte("---\nname: pdf\ndescription: Work with PDFs\nlicense: MIT\n---\n# Body\n"))
	if err != nil || fm.Name != "pdf" || fm.Description != "Work with PDFs" || fm.License != "MIT" || !fm.HasBody {
		t.Fatalf("got %+v, %v", fm, err)
	}
	// CRLF and BOM are tolerated for parsing.
	fm, err = ParseFrontmatter([]byte("\uFEFF---\r\nname: x\r\ndescription: y\r\n---\r\nbody\r\n"))
	if err != nil || fm.Name != "x" || fm.Description != "y" {
		t.Fatalf("crlf: %+v, %v", fm, err)
	}
	fm, err = ParseFrontmatter([]byte("---\nname: x\ndescription: y\n---"))
	if err != nil || fm.HasBody {
		t.Fatalf("no body: %+v %v", fm, err)
	}
	bad := map[string]string{
		"missing":      "# no frontmatter\n",
		"unterminated": "---\nname: x\ndescription: y\n",
		"no name":      "---\ndescription: y\n---\nb",
		"no desc":      "---\nname: x\n---\nb",
		"non-string":   "---\nname: [a]\ndescription: y\n---\nb",
		"bad yaml":     "---\nname: : :\n  - x\n---\nb",
		"nul":          "---\nname: x\x00\ndescription: y\n---\nb",
		"control":      "---\nname: \"x\\u0007\"\ndescription: y\n---\nb",
		"long name":    "---\nname: " + strings.Repeat("n", 121) + "\ndescription: y\n---\nb",
		"invalid utf8": "---\nname: \xff\ndescription: y\n---\nb",
	}
	for name, md := range bad {
		if _, err := ParseFrontmatter([]byte(md)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestValidatePath(t *testing.T) {
	for _, p := range []string{"SKILL.md", "scripts/run.sh", "docs/读我.md", ".gitignore"} {
		if err := ValidatePath(p); err != nil {
			t.Errorf("ValidatePath(%q) = %v", p, err)
		}
	}
	for _, p := range []string{"", "/abs", "../x", "a/../b", "a//b", "./a", "a/", `a\b`, "C:x", ".git/config", "a/.GIT/x", "a\x01b", strings.Repeat("a", 256)} {
		if ValidatePath(p) == nil {
			t.Errorf("ValidatePath(%q) = nil", p)
		}
	}
}

func TestValidateFiles(t *testing.T) {
	lim := Limits{MaxFiles: 3, MaxFileBytes: 10, MaxTotalBytes: 15}
	ok := []File{{Path: "b.txt", Data: []byte("b")}, {Path: "SKILL.md", Data: []byte("s")}}
	out, err := ValidateFiles(ok, lim)
	if err != nil || out[0].Path != "SKILL.md" {
		t.Fatalf("got %v %v", out, err)
	}
	bad := map[string][]File{
		"missing skill": {{Path: "a", Data: nil}},
		"too many":      {{Path: "SKILL.md"}, {Path: "a"}, {Path: "b"}, {Path: "c"}},
		"file too big":  {{Path: "SKILL.md", Data: make([]byte, 11)}},
		"total too big": {{Path: "SKILL.md", Data: make([]byte, 8)}, {Path: "a", Data: make([]byte, 8)}},
		"case collide":  {{Path: "SKILL.md"}, {Path: "A.txt"}, {Path: "a.txt"}},
		"file and dir":  {{Path: "SKILL.md"}, {Path: "a"}, {Path: "a/b"}},
	}
	for name, files := range bad {
		if _, err := ValidateFiles(files, lim); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestCoreCompatible(t *testing.T) {
	fm := Frontmatter{Name: "x", Description: "y", HasBody: true}
	if ok, r := CoreCompatible(fm, []File{{Path: "SKILL.md", Data: []byte("text")}}); !ok {
		t.Fatalf("expected compatible: %v", r)
	}
	if ok, _ := CoreCompatible(fm, []File{{Path: "SKILL.md", Data: []byte{0xff}}}); ok {
		t.Fatal("binary must be incompatible")
	}
	if ok, _ := CoreCompatible(fm, []File{{Path: "SKILL.md"}, {Path: ".hidden"}}); ok {
		t.Fatal("dot files must be incompatible")
	}
	if ok, _ := CoreCompatible(fm, []File{{Path: "SKILL.md", Data: []byte(strings.Repeat("a", 70000))}}); ok {
		t.Fatal("oversized bundle must be incompatible")
	}
	if ok, _ := CoreCompatible(Frontmatter{Name: "x", Description: "y"}, []File{{Path: "SKILL.md"}}); ok {
		t.Fatal("missing body must be incompatible")
	}
}

func TestLicenseRules(t *testing.T) {
	cases := map[string]License{
		"":                  {Kind: LicenseUnknown},
		"NOASSERTION":       {Kind: LicenseUnknown},
		"mit":               {Kind: LicenseSPDX, ID: "MIT"},
		"Apache-2.0":        {Kind: LicenseSPDX, ID: "Apache-2.0"},
		"custom":            {Kind: LicenseCustom, ID: "custom"},
		"Proprietary":       {Kind: LicenseCustom, ID: "Proprietary"},
		"MIT OR Apache-2.0": {Kind: LicenseCustom, ID: "MIT OR Apache-2.0"},
	}
	for in, want := range cases {
		if got := ClassifyLicense(in); got != want {
			t.Errorf("ClassifyLicense(%q) = %+v, want %+v", in, got, want)
		}
	}
	type acc struct{ full, art bool }
	table := []struct {
		mode, kind string
		approved   bool
		want       acc
	}{
		{ModeHosted, LicenseUnknown, false, acc{true, true}},
		{ModeIndexed, LicenseSPDX, false, acc{true, true}},
		{ModeIndexed, LicenseCustom, false, acc{false, false}},
		{ModeIndexed, LicenseCustom, true, acc{true, true}},
		{ModeIndexed, LicenseUnknown, false, acc{false, false}},
		{ModeIndexed, LicenseUnknown, true, acc{false, false}},
	}
	for _, c := range table {
		a := ContentAccess(c.mode, c.kind, c.approved)
		if (acc{a.FullText, a.Artifact}) != c.want {
			t.Errorf("ContentAccess(%s,%s,%v) = %+v", c.mode, c.kind, c.approved, a)
		}
	}
}
