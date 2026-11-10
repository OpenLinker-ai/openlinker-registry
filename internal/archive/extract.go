package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"

	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
)

// Upload formats.
const (
	FormatTar    = "tar"
	FormatTarGz  = "tar.gz"
	FormatZip    = "zip"
	FormatBundle = "openlinker-bundle-v1"
)

// Error is an archive validation failure with a stable code.
type Error struct{ Code, Msg string }

func (e *Error) Error() string { return e.Msg }

func aerr(code, msg string) error { return &Error{Code: code, Msg: msg} }

// maxEntries bounds raw archive entries (including directories) to stop
// header-only bombs before file limits apply.
func maxEntries(lim skill.Limits) int { return lim.MaxFiles*4 + 64 }

// Extract reads an uploaded tar, tar.gz or zip archive into regular files,
// enforcing limits while decompressing. Symlinks, hard links, devices, FIFOs
// and unsafe paths are rejected. If SKILL.md is not at the root and every file
// lives under one top-level directory that contains SKILL.md, that directory
// is stripped. The result is validated and sorted.
func Extract(data []byte, format string, lim skill.Limits) ([]skill.File, error) {
	var files []skill.File
	var err error
	switch format {
	case FormatTar:
		files, err = readTar(bytes.NewReader(data), lim)
	case FormatTarGz:
		zr, gerr := gzip.NewReader(bytes.NewReader(data))
		if gerr != nil {
			return nil, aerr("archive_invalid", "invalid gzip stream")
		}
		zr.Multistream(false)
		files, err = readTar(zr, lim)
	case FormatZip:
		files, err = readZip(data, lim)
	default:
		return nil, aerr("archive_format", "unsupported archive format")
	}
	if err != nil {
		return nil, err
	}
	files = stripSingleRoot(files)
	return skill.ValidateFiles(files, lim)
}

func readTar(r io.Reader, lim skill.Limits) ([]skill.File, error) {
	tr := tar.NewReader(r)
	var files []skill.File
	var total int64
	for n := 0; ; n++ {
		if n > maxEntries(lim) {
			return nil, aerr("too_many_files", "archive has too many entries")
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, aerr("archive_invalid", "invalid tar archive")
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		switch hdr.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // accept legacy regular-file flag
		case tar.TypeSymlink, tar.TypeLink:
			return nil, aerr("link_rejected", "archives must not contain links")
		case tar.TypeXGlobalHeader:
			continue
		default:
			return nil, aerr("entry_rejected", "archives may only contain regular files and directories")
		}
		if err := skill.ValidatePath(name); err != nil {
			return nil, err
		}
		if hdr.Size < 0 || hdr.Size > lim.MaxFileBytes {
			return nil, aerr("file_too_large", "file exceeds the per-file size limit")
		}
		total += hdr.Size
		if total > lim.MaxTotalBytes {
			return nil, aerr("total_too_large", "archive exceeds the total size limit")
		}
		buf, err := io.ReadAll(io.LimitReader(tr, hdr.Size+1))
		if err != nil || int64(len(buf)) != hdr.Size {
			return nil, aerr("archive_invalid", "truncated tar entry")
		}
		files = append(files, skill.File{Path: name, Data: buf})
		if len(files) > lim.MaxFiles {
			return nil, aerr("too_many_files", "too many files")
		}
	}
	return files, nil
}

func readZip(data []byte, lim skill.Limits) ([]skill.File, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, aerr("archive_invalid", "invalid zip archive")
	}
	if len(zr.File) > maxEntries(lim) {
		return nil, aerr("too_many_files", "archive has too many entries")
	}
	var files []skill.File
	var total int64
	for _, zf := range zr.File {
		mode := zf.Mode()
		if mode.IsDir() {
			continue
		}
		if !mode.IsRegular() {
			return nil, aerr("entry_rejected", "archives may only contain regular files and directories")
		}
		name := strings.TrimPrefix(zf.Name, "./")
		if err := skill.ValidatePath(name); err != nil {
			return nil, err
		}
		if zf.UncompressedSize64 > uint64(lim.MaxFileBytes) {
			return nil, aerr("file_too_large", "file exceeds the per-file size limit")
		}
		rc, err := zf.Open()
		if err != nil {
			return nil, aerr("archive_invalid", "invalid zip entry")
		}
		// Never trust the declared size: read at most the limit plus one.
		buf, err := io.ReadAll(io.LimitReader(rc, lim.MaxFileBytes+1))
		rc.Close()
		if err != nil {
			return nil, aerr("archive_invalid", "invalid zip entry")
		}
		if int64(len(buf)) > lim.MaxFileBytes {
			return nil, aerr("file_too_large", "file exceeds the per-file size limit")
		}
		total += int64(len(buf))
		if total > lim.MaxTotalBytes {
			return nil, aerr("total_too_large", "archive exceeds the total size limit")
		}
		files = append(files, skill.File{Path: name, Data: buf})
		if len(files) > lim.MaxFiles {
			return nil, aerr("too_many_files", "too many files")
		}
	}
	return files, nil
}

func stripSingleRoot(files []skill.File) []skill.File {
	for _, f := range files {
		if f.Path == skill.SkillFile {
			return files
		}
	}
	root := ""
	for _, f := range files {
		first, _, ok := strings.Cut(f.Path, "/")
		if !ok || (root != "" && first != root) {
			return files
		}
		root = first
	}
	if root == "" {
		return files
	}
	out := make([]skill.File, 0, len(files))
	found := false
	for _, f := range files {
		rel := strings.TrimPrefix(f.Path, root+"/")
		if rel == skill.SkillFile {
			found = true
		}
		out = append(out, skill.File{Path: rel, Data: f.Data, GitBlobSHA: f.GitBlobSHA})
	}
	if !found {
		return files
	}
	return out
}

// ReadCanonicalTar decodes a canonical tar produced by CanonicalTar back into
// files (directories are implied by paths).
func ReadCanonicalTar(data []byte) ([]skill.File, error) {
	tr := tar.NewReader(bytes.NewReader(data))
	var files []skill.File
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, errors.New("unexpected entry in canonical tar")
		}
		buf, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files = append(files, skill.File{Path: path.Clean(hdr.Name), Data: buf})
	}
}

// Bundle mirrors the JSON layout of an OpenLinker Core skill package bundle
// (skill_packages.v1). Field order matters: canonical bytes are produced with
// encoding/json in this order, which is how Core computes its digest.
type Bundle struct {
	RequiredCommands []string          `json:"required_commands"`
	Name             string            `json:"name"`
	Description      string            `json:"description"`
	Files            map[string]string `json:"files"`
	CapabilityIDs    []string          `json:"capability_ids"`
	Providers        []string          `json:"providers"`
}

// ParseBundle accepts only canonical Core bundle bytes: decoding with unknown
// fields rejected and re-encoding must reproduce the input exactly, so the
// stored artifact digest equals the digest Core computed for the same bundle.
func ParseBundle(data []byte, lim skill.Limits) (Bundle, []skill.File, error) {
	var b Bundle
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil || dec.More() {
		return Bundle{}, nil, aerr("bundle_invalid", "invalid OpenLinker skill bundle JSON")
	}
	again, err := json.Marshal(b)
	if err != nil || !bytes.Equal(again, data) {
		return Bundle{}, nil, aerr("bundle_not_canonical", "bundle bytes must be the canonical Core encoding")
	}
	files := make([]skill.File, 0, len(b.Files))
	for p, content := range b.Files {
		files = append(files, skill.File{Path: p, Data: []byte(content)})
	}
	files, err = skill.ValidateFiles(files, lim)
	if err != nil {
		return Bundle{}, nil, err
	}
	return b, files, nil
}

// FilesFromArtifact decodes stored artifact bytes of the given type.
func FilesFromArtifact(artifactType string, data []byte) ([]skill.File, error) {
	switch artifactType {
	case TypeSkillMD:
		return []skill.File{{Path: skill.SkillFile, Data: data}}, nil
	case TypeTar:
		return ReadCanonicalTar(data)
	case TypeBundle:
		var b Bundle
		if err := json.Unmarshal(data, &b); err != nil {
			return nil, err
		}
		files := make([]skill.File, 0, len(b.Files))
		for p, c := range b.Files {
			files = append(files, skill.File{Path: p, Data: []byte(c)})
		}
		return skill.ValidateFiles(files, skill.Limits{MaxFiles: 1 << 20, MaxFileBytes: 1 << 40, MaxTotalBytes: 1 << 40})
	}
	return nil, errors.New("unknown artifact type")
}

// Artifact types stored on versions.
const (
	TypeSkillMD = "skill-md"
	TypeTar     = "tar"
	TypeBundle  = "openlinker-bundle-v1"
)
