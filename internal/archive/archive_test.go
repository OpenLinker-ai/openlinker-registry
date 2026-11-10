package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
)

const md = "---\nname: demo\ndescription: Demo skill\n---\nDo things.\n"

func sampleFiles() []skill.File {
	return []skill.File{
		{Path: "SKILL.md", Data: []byte(md)},
		{Path: "scripts/run.sh", Data: []byte("#!/bin/sh\necho hi\n")},
		{Path: "docs/读我.md", Data: []byte("中文\n")},
		{Path: "a-b.txt", Data: bytes.Repeat([]byte("x"), 1000)},
	}
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func TestCanonicalTarDeterministic(t *testing.T) {
	files := sampleFiles()
	a, err := CanonicalTar(files)
	if err != nil {
		t.Fatal(err)
	}
	// Input order must not matter.
	rev := []skill.File{files[3], files[2], files[1], files[0]}
	b, err := CanonicalTar(rev)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("canonical tar depends on input order")
	}
	// Golden digest pins the byte layout across Go releases and refactors.
	const golden = "8dd48504e1eb548b93b3b70349e75cc2dd40914dfaa8223596f11d9f80e5be8f"
	if got := sum(a); got != golden {
		t.Logf("canonical tar digest = %s (update golden only for an intentional format change)", got)
		t.Fail()
	}
	if len(a)%512 != 0 || !bytes.Equal(a[len(a)-1024:], make([]byte, 1024)) {
		t.Fatal("must end with two zero blocks and be block aligned")
	}

	tr := tar.NewReader(bytes.NewReader(a))
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" || h.ModTime.Unix() != 0 || len(h.PAXRecords) != 0 {
			t.Fatalf("non-canonical header %+v", h)
		}
		// Go reports non-ASCII (UTF-8) ustar names as FormatUnknown; check ASCII ones.
		if isASCII(h.Name) && (h.Format&tar.FormatUSTAR == 0 || h.Format&tar.FormatGNU != 0) {
			t.Fatalf("format = %d, want USTAR", int(h.Format))
		}
		want := int64(0o644)
		if h.Typeflag == tar.TypeDir {
			want = 0o755
		}
		if h.Mode != want {
			t.Fatalf("%s mode %o", h.Name, h.Mode)
		}
	}
	wantNames := []string{"SKILL.md", "a-b.txt", "docs/", "docs/读我.md", "scripts/", "scripts/run.sh"}
	if strings.Join(names, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("order %v", names)
	}
	back, err := ReadCanonicalTar(a)
	if err != nil || len(back) != 4 {
		t.Fatalf("read back: %v %v", back, err)
	}
}

func TestCanonicalTarLongPaths(t *testing.T) {
	long := strings.Repeat("d", 90) + "/" + strings.Repeat("f", 90) + ".md"
	data, err := CanonicalTar([]skill.File{{Path: "SKILL.md"}, {Path: long, Data: []byte("x")}})
	if err != nil {
		t.Fatal(err)
	}
	files, err := ReadCanonicalTar(data)
	if err != nil || files[1].Path != long {
		t.Fatalf("ustar prefix split failed: %v %v", files, err)
	}
	tooLong := strings.Repeat("f", 120)
	if _, err := CanonicalTar([]skill.File{{Path: tooLong}}); !errors.Is(err, ErrPathTooLong) {
		t.Fatalf("want ErrPathTooLong, got %v", err)
	}
}

func TestStoredGzip(t *testing.T) {
	for _, n := range []int{0, 1, 65535, 65536, 200000} {
		data := bytes.Repeat([]byte{'z'}, n)
		gz := StoredGzip(data)
		if !bytes.Equal(gz, StoredGzip(data)) {
			t.Fatal("not deterministic")
		}
		zr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			t.Fatal(err)
		}
		out, err := io.ReadAll(zr)
		if err != nil || !bytes.Equal(out, data) {
			t.Fatalf("n=%d roundtrip failed: %v", n, err)
		}
	}
}

func makeTar(t *testing.T, entries []tar.Header, bodies []string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for i, h := range entries {
		h.Size = int64(len(bodies[i]))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(bodies[i]))
	}
	tw.Close()
	return buf.Bytes()
}

func TestExtractFormats(t *testing.T) {
	lim := skill.DefaultLimits
	raw := makeTar(t, []tar.Header{{Name: "demo/", Typeflag: tar.TypeDir}, {Name: "demo/SKILL.md", Mode: 0o600, Typeflag: tar.TypeReg}, {Name: "demo/x.txt", Mode: 0o755, Typeflag: tar.TypeReg}}, []string{"", md, "x"})
	files, err := Extract(raw, FormatTar, lim)
	if err != nil || len(files) != 2 || files[0].Path != "SKILL.md" {
		t.Fatalf("tar: %v %v", files, err)
	}
	canon1, _ := CanonicalTar(files)

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(raw)
	zw.Close()
	files, err = Extract(gz.Bytes(), FormatTarGz, lim)
	if err != nil {
		t.Fatal(err)
	}
	canon2, _ := CanonicalTar(files)

	var zb bytes.Buffer
	zwr := zip.NewWriter(&zb)
	for _, f := range []struct{ n, b string }{{"SKILL.md", md}, {"x.txt", "x"}} {
		w, _ := zwr.Create(f.n)
		w.Write([]byte(f.b))
	}
	zwr.Close()
	files, err = Extract(zb.Bytes(), FormatZip, lim)
	if err != nil {
		t.Fatal(err)
	}
	canon3, _ := CanonicalTar(files)
	if !bytes.Equal(canon1, canon2) || !bytes.Equal(canon1, canon3) {
		t.Fatal("same files in different archive formats must give the same canonical tar")
	}
}

func TestExtractRejects(t *testing.T) {
	lim := skill.Limits{MaxFiles: 4, MaxFileBytes: 100, MaxTotalBytes: 150}
	cases := map[string][]byte{
		"symlink":   makeTar(t, []tar.Header{{Name: "SKILL.md", Typeflag: tar.TypeReg}, {Name: "l", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}}, []string{md, ""}),
		"hardlink":  makeTar(t, []tar.Header{{Name: "SKILL.md", Typeflag: tar.TypeReg}, {Name: "l", Typeflag: tar.TypeLink, Linkname: "SKILL.md"}}, []string{md, ""}),
		"device":    makeTar(t, []tar.Header{{Name: "SKILL.md", Typeflag: tar.TypeReg}, {Name: "d", Typeflag: tar.TypeChar}}, []string{md, ""}),
		"traversal": makeTar(t, []tar.Header{{Name: "SKILL.md", Typeflag: tar.TypeReg}, {Name: "../x", Typeflag: tar.TypeReg}}, []string{md, "x"}),
		"absolute":  makeTar(t, []tar.Header{{Name: "/SKILL.md", Typeflag: tar.TypeReg}}, []string{md}),
		"too big":   makeTar(t, []tar.Header{{Name: "SKILL.md", Typeflag: tar.TypeReg}, {Name: "big", Typeflag: tar.TypeReg}}, []string{md, strings.Repeat("a", 101)}),
		"no skill":  makeTar(t, []tar.Header{{Name: "README.md", Typeflag: tar.TypeReg}}, []string{"x"}),
	}
	for name, data := range cases {
		if _, err := Extract(data, FormatTar, lim); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
	// Gzip bomb: declared small, expands beyond the total limit.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(makeTar(t, []tar.Header{{Name: "SKILL.md", Typeflag: tar.TypeReg}}, []string{strings.Repeat("a", 10000)}))
	zw.Close()
	if _, err := Extract(gz.Bytes(), FormatTarGz, lim); err == nil {
		t.Error("gzip bomb: expected rejection")
	}
}

func TestParseBundleCanonical(t *testing.T) {
	b := Bundle{RequiredCommands: []string{}, Name: "demo", Description: "Demo skill", Files: map[string]string{"SKILL.md": md, "a.txt": "<&>"},
		CapabilityIDs: []string{}, Providers: []string{"claude"}}
	raw, _ := json.Marshal(b)
	if _, files, err := ParseBundle(raw, skill.DefaultLimits); err != nil || len(files) != 2 {
		t.Fatalf("canonical bundle rejected: %v", err)
	}
	pretty, _ := json.MarshalIndent(b, "", " ")
	if _, _, err := ParseBundle(pretty, skill.DefaultLimits); err == nil {
		t.Fatal("non-canonical bundle must be rejected")
	}
	extra := bytes.Replace(raw, []byte(`{"required_commands"`), []byte(`{"x":1,"required_commands"`), 1)
	if _, _, err := ParseBundle(extra, skill.DefaultLimits); err == nil {
		t.Fatal("unknown fields must be rejected")
	}
	files, err := FilesFromArtifact(TypeBundle, raw)
	if err != nil || len(files) != 2 {
		t.Fatalf("FilesFromArtifact: %v", err)
	}
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
