package skill

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SkillFile is the required entry point at the root of every skill.
const SkillFile = "SKILL.md"

// File is one regular file of a skill, addressed by a slash-separated path
// relative to the skill root.
type File struct {
	Path string
	Data []byte
	// GitBlobSHA is set for files fetched from GitHub.
	GitBlobSHA string
}

// ManifestEntry describes one file in an immutable version.
type ManifestEntry struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	GitBlobSHA string `json:"git_blob_sha,omitempty"`
}

// Limits bound file sets accepted from uploads and GitHub.
type Limits struct {
	MaxFiles      int
	MaxFileBytes  int64
	MaxTotalBytes int64
}

// DefaultLimits are conservative R1 defaults.
var DefaultLimits = Limits{MaxFiles: 256, MaxFileBytes: 2 << 20, MaxTotalBytes: 16 << 20}

// MaxPathBytes caps a single relative path. Paths that do not fit a ustar
// header (name 100 / prefix 155) are rejected separately by the tar writer.
const MaxPathBytes = 255

// ValidatePath checks a relative skill path: clean, slash-separated, UTF-8,
// no traversal, no absolute or drive paths, no backslashes or control
// characters and no .git segments.
func ValidatePath(p string) error {
	bad := func() error { return verr("path_unsafe", "unsafe file path: "+quoteShort(p)) }
	if p == "" || len(p) > MaxPathBytes || !utf8.ValidString(p) {
		return bad()
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || path.Clean(p) != p || p == "." {
		return bad()
	}
	if len(p) >= 2 && p[1] == ':' {
		return bad()
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return bad()
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.EqualFold(seg, ".git") {
			return bad()
		}
	}
	return nil
}

// ValidateFiles validates paths, limits, uniqueness (including case-folded
// collisions) and the presence of SKILL.md, and returns the files sorted by
// path in byte order.
func ValidateFiles(files []File, lim Limits) ([]File, error) {
	if len(files) == 0 {
		return nil, verr("skill_md_missing", "a skill must contain SKILL.md at its root")
	}
	if len(files) > lim.MaxFiles {
		return nil, verr("too_many_files", "too many files")
	}
	out := slices.Clone(files)
	slices.SortFunc(out, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	var total int64
	folded := map[string]bool{}
	paths := map[string]bool{}
	hasSkill := false
	for _, f := range out {
		if err := ValidatePath(f.Path); err != nil {
			return nil, err
		}
		key := strings.ToLower(f.Path)
		if folded[key] {
			return nil, verr("path_duplicate", "duplicate or case-colliding path: "+quoteShort(f.Path))
		}
		folded[key] = true
		paths[f.Path] = true
		if int64(len(f.Data)) > lim.MaxFileBytes {
			return nil, verr("file_too_large", "file exceeds the per-file size limit: "+quoteShort(f.Path))
		}
		total += int64(len(f.Data))
		if total > lim.MaxTotalBytes {
			return nil, verr("total_too_large", "skill exceeds the total size limit")
		}
		if f.Path == SkillFile {
			hasSkill = true
		}
	}
	for p := range paths {
		for parent := path.Dir(p); parent != "."; parent = path.Dir(parent) {
			if paths[parent] {
				return nil, verr("path_conflict", "a file cannot also be a directory: "+quoteShort(parent))
			}
		}
	}
	if !hasSkill {
		return nil, verr("skill_md_missing", "a skill must contain SKILL.md at its root")
	}
	return out, nil
}

// SHA256Hex returns the lowercase hex sha256 of data.
func SHA256Hex(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

// Digest returns "sha256:<hex>" for data.
func Digest(data []byte) string { return "sha256:" + SHA256Hex(data) }

// BuildManifest returns manifest entries in the order of files (callers pass
// files sorted by ValidateFiles).
func BuildManifest(files []File) []ManifestEntry {
	out := make([]ManifestEntry, 0, len(files))
	for _, f := range files {
		out = append(out, ManifestEntry{Path: f.Path, Size: int64(len(f.Data)), SHA256: SHA256Hex(f.Data), GitBlobSHA: f.GitBlobSHA})
	}
	return out
}

// Core compatibility (OpenLinker Core skill_packages.v1 import limits).
var corePathRe = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)

const (
	coreMaxFiles   = 32
	coreMaxPayload = 65536
)

// CoreCompatible reports whether the file set meets the import constraints of
// OpenLinker Core skill packages v1: at most 32 UTF-8 text files without NUL,
// Core-safe paths (no dot-files), a frontmatter name/description and body,
// and an encoded bundle within 64 KiB. The payload size is an estimate made
// with empty capability/command lists and a single provider; Core performs
// the authoritative check at import time.
func CoreCompatible(fm Frontmatter, files []File) (bool, []string) {
	var reasons []string
	if len(files) > coreMaxFiles {
		reasons = append(reasons, "more than 32 files")
	}
	m := map[string]string{}
	for _, f := range files {
		if !utf8.Valid(f.Data) || bytes.IndexByte(f.Data, 0) >= 0 {
			reasons = append(reasons, "non-text file: "+f.Path)
			continue
		}
		if len(f.Path) > 180 || !corePathRe.MatchString(f.Path) || strings.HasPrefix(f.Path, ".") || strings.Contains(f.Path, "/.") {
			reasons = append(reasons, "path not accepted by Core: "+f.Path)
		}
		m[f.Path] = string(f.Data)
	}
	if fm.Name == "" || fm.Description == "" || !fm.HasBody {
		reasons = append(reasons, "SKILL.md needs name, description and instructions")
	}
	if len(reasons) == 0 {
		raw, _ := json.Marshal(struct {
			RequiredCommands []string          `json:"required_commands"`
			Name             string            `json:"name"`
			Description      string            `json:"description"`
			Files            map[string]string `json:"files"`
			CapabilityIDs    []string          `json:"capability_ids"`
			Providers        []string          `json:"providers"`
		}{[]string{}, fm.Name, fm.Description, m, []string{}, []string{"claude"}})
		if len(raw) > coreMaxPayload {
			reasons = append(reasons, "encoded bundle exceeds 64 KiB")
		}
	}
	return len(reasons) == 0, reasons
}

func quoteShort(s string) string {
	if len(s) > 80 {
		s = s[:80] + "..."
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}
