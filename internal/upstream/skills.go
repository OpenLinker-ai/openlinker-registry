package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
)

// The types below mirror the JSON written by internal/api/read.go
// (skillJSON, getSkill, versionJSON). Unknown fields are ignored; a field of
// the wrong type fails the whole document.

// SkillSummary is one entry of GET /v1/skills.
type SkillSummary struct {
	Namespace, Name, Description, Visibility, Status, StatusReason, Mode string
	LatestVersion                                                        string // "" when the skill has no usable version
	Listed                                                               bool
	UpdatedAt                                                            time.Time
}

// SkillVersionRef is one entry of the version list of a skill.
type SkillVersionRef struct {
	Version        string
	Yanked         bool
	ArtifactDigest string
}

// SkillDetail is GET /v1/skills/{ns}/{name}.
type SkillDetail struct {
	SkillSummary
	Versions []SkillVersionRef // newest first, as served
}

// SkillVersion is GET /v1/skills/{ns}/{name}/versions/{version}.
type SkillVersion struct {
	Namespace, Name, Version, Mode          string
	FrontmatterName, FrontmatterDescription string
	LicenseKind, LicenseID, LicenseText     string // LicenseText only when the upstream grants full-text access
	LicenseApproved                         bool
	CoreCompatible                          bool
	Files                                   []skill.ManifestEntry
	ArtifactType, ArtifactDigest            string
	ArtifactSize                            int64
	ArchiveDigest                           string // "" when the upstream has no archive
	AccessArtifact, AccessFullText          bool   // content_access.artifact / .full_text
	Yanked                                  bool
	YankReason                              string
	CreatedAt                               time.Time
}

type skillWire struct {
	Namespace     string    `json:"namespace"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	Visibility    string    `json:"visibility"`
	Listed        bool      `json:"listed"`
	Status        string    `json:"status"`
	StatusReason  string    `json:"status_reason"`
	Mode          string    `json:"mode"`
	LatestVersion *string   `json:"latest_version"` // null without a usable version
	UpdatedAt     time.Time `json:"updated_at"`
	Versions      []struct {
		Version        string `json:"version"`
		Yanked         bool   `json:"yanked"`
		ArtifactDigest string `json:"artifact_digest"`
	} `json:"versions"`
}

func (w skillWire) summary() SkillSummary {
	s := SkillSummary{Namespace: w.Namespace, Name: w.Name, Description: w.Description, Visibility: w.Visibility,
		Status: w.Status, StatusReason: w.StatusReason, Mode: w.Mode, Listed: w.Listed, UpdatedAt: w.UpdatedAt}
	if w.LatestVersion != nil {
		s.LatestVersion = *w.LatestVersion
	}
	return s
}

type versionWire struct {
	Namespace   string    `json:"namespace"`
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Mode        string    `json:"mode"`
	CreatedAt   time.Time `json:"created_at"`
	Frontmatter struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"frontmatter"`
	License struct {
		Kind     string `json:"kind"`
		ID       string `json:"id"`
		Approved bool   `json:"approved"`
		Text     string `json:"text"`
	} `json:"license"`
	CoreCompatible bool                  `json:"core_compatible"`
	Files          []skill.ManifestEntry `json:"files"`
	Artifact       struct {
		Type   string `json:"type"`
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	} `json:"artifact"`
	Archive *struct {
		Digest string `json:"digest"`
	} `json:"archive"`
	ContentAccess struct {
		FullText bool `json:"full_text"`
		Artifact bool `json:"artifact"`
	} `json:"content_access"`
	// null, or {"at": ..., "reason": ...} once yanked.
	Yanked json.RawMessage `json:"yanked"`
}

// ListSkills returns one page of GET /v1/skills, in upstream order. next is
// "" at the end. A limit of 0 leaves the page size to the upstream.
func (c *Client) ListSkills(ctx context.Context, cursor string, limit int) (items []SkillSummary, next string, err error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	body, err := c.getJSON(ctx, "/v1/skills", q)
	if err != nil {
		return nil, "", wrapOp("skill list", err)
	}
	raws, top, err := listField(body, "skills", "skill list")
	if err != nil {
		return nil, "", wrapOp("skill list", err)
	}
	for _, raw := range raws {
		var w skillWire
		if err := decode(raw, &w, "skill list"); err != nil {
			return nil, "", wrapOp("skill list", err)
		}
		if w.Namespace == "" || w.Name == "" {
			return nil, "", wrapOp("skill list", badResponse("upstream returned a skill without namespace or name"))
		}
		items = append(items, w.summary())
	}
	next = rawString(top, "next_cursor") // null at the end
	if next != "" && next == cursor {
		return nil, "", wrapOp("skill list", badResponse("upstream returned a cursor that does not advance"))
	}
	return items, next, nil
}

// GetSkill returns a skill with its version list.
func (c *Client) GetSkill(ctx context.Context, ns, name string) (SkillDetail, error) {
	if err := checkSegs("namespace and name", ns, name); err != nil {
		return SkillDetail{}, wrapOp("skill get", err)
	}
	body, err := c.getJSON(ctx, "/v1/skills/"+seg(ns)+"/"+seg(name), nil)
	if err != nil {
		return SkillDetail{}, wrapOp("skill get", err)
	}
	var w skillWire
	if err := decode(body, &w, "skill document"); err != nil {
		return SkillDetail{}, wrapOp("skill get", err)
	}
	if w.Namespace != ns || w.Name != name {
		return SkillDetail{}, wrapOp("skill get", badResponse("upstream returned a different skill than requested"))
	}
	d := SkillDetail{SkillSummary: w.summary()}
	for _, v := range w.Versions {
		if v.Version == "" {
			return SkillDetail{}, wrapOp("skill get", badResponse("upstream returned a version without a name"))
		}
		d.Versions = append(d.Versions, SkillVersionRef{Version: v.Version, Yanked: v.Yanked, ArtifactDigest: v.ArtifactDigest})
	}
	return d, nil
}

// GetSkillVersion returns the metadata of one version. Files and digests are
// what the upstream states; verify them against the artifact.
func (c *Client) GetSkillVersion(ctx context.Context, ns, name, version string) (SkillVersion, error) {
	const op = "skill version"
	if err := checkSegs("namespace, name and version", ns, name, version); err != nil {
		return SkillVersion{}, wrapOp(op, err)
	}
	body, err := c.getJSON(ctx, "/v1/skills/"+seg(ns)+"/"+seg(name)+"/versions/"+seg(version), nil)
	if err != nil {
		return SkillVersion{}, wrapOp(op, err)
	}
	var w versionWire
	if err := decode(body, &w, "skill version document"); err != nil {
		return SkillVersion{}, wrapOp(op, err)
	}
	if w.Namespace != ns || w.Name != name || w.Version != version {
		return SkillVersion{}, wrapOp(op, badResponse("upstream returned a different skill version than requested"))
	}
	v := SkillVersion{
		Namespace: w.Namespace, Name: w.Name, Version: w.Version, Mode: w.Mode,
		FrontmatterName: w.Frontmatter.Name, FrontmatterDescription: w.Frontmatter.Description,
		LicenseKind: w.License.Kind, LicenseID: w.License.ID, LicenseText: w.License.Text, LicenseApproved: w.License.Approved,
		CoreCompatible: w.CoreCompatible, Files: w.Files,
		ArtifactType: w.Artifact.Type, ArtifactDigest: w.Artifact.Digest, ArtifactSize: w.Artifact.Size,
		AccessArtifact: w.ContentAccess.Artifact, AccessFullText: w.ContentAccess.FullText,
		CreatedAt: w.CreatedAt,
	}
	if w.Archive != nil {
		v.ArchiveDigest = w.Archive.Digest
	}
	// The handler writes null or an object; a plain boolean is accepted too.
	switch y := string(bytes.TrimSpace(w.Yanked)); y {
	case "", "null", "false":
	case "true":
		v.Yanked = true
	default:
		var yank struct {
			Reason string `json:"reason"`
		}
		if err := decode([]byte(y), &yank, "skill version document"); err != nil {
			return SkillVersion{}, wrapOp(op, err)
		}
		v.Yanked, v.YankReason = true, yank.Reason
	}
	return v, nil
}

// GetSkillArtifact downloads the artifact of one version, at most maxBytes.
// The bytes are not checked against the version's digest here.
func (c *Client) GetSkillArtifact(ctx context.Context, ns, name, version string, maxBytes int64) ([]byte, error) {
	const op = "skill artifact"
	if err := checkSegs("namespace, name and version", ns, name, version); err != nil {
		return nil, wrapOp(op, err)
	}
	u, err := c.apiURL("/v1/skills/"+seg(ns)+"/"+seg(name)+"/versions/"+seg(version)+"/artifact", nil)
	if err != nil {
		return nil, wrapOp(op, err)
	}
	body, err := c.get(ctx, u, "*/*", maxBytes)
	if err != nil {
		return nil, wrapOp(op, err)
	}
	return body, nil
}
