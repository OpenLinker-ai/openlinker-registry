package skill

import "strings"

// License kinds stored on every version.
const (
	LicenseSPDX    = "spdx"
	LicenseCustom  = "custom"
	LicenseUnknown = "unknown"
)

// License is the classified license of a version.
type License struct {
	Kind string // spdx | custom | unknown
	// ID is the canonical SPDX identifier, or the declared string for custom.
	ID string
}

// redistributable lists SPDX identifiers that permit redistribution of
// unmodified copies. Matching is case-insensitive; the value is the canonical
// spelling. Compound SPDX expressions are not parsed and classify as custom.
var redistributable = func() map[string]string {
	ids := []string{
		"0BSD", "AFL-3.0", "AGPL-3.0-only", "AGPL-3.0-or-later", "Apache-2.0", "Artistic-2.0",
		"BSD-2-Clause", "BSD-3-Clause", "BSL-1.0", "CC-BY-4.0", "CC-BY-SA-4.0", "CC0-1.0",
		"EPL-2.0", "EUPL-1.2", "GPL-2.0-only", "GPL-2.0-or-later", "GPL-3.0-only", "GPL-3.0-or-later",
		"ISC", "LGPL-2.1-only", "LGPL-2.1-or-later", "LGPL-3.0-only", "LGPL-3.0-or-later",
		"MIT", "MIT-0", "MPL-2.0", "PSF-2.0", "Python-2.0", "Unlicense", "UPL-1.0", "Zlib",
		// Deprecated short forms still common in package metadata.
		"GPL-2.0", "GPL-3.0", "LGPL-2.1", "LGPL-3.0", "AGPL-3.0",
	}
	m := make(map[string]string, len(ids))
	for _, id := range ids {
		m[strings.ToLower(id)] = id
	}
	return m
}()

// ClassifyLicense maps a declared license string to a License. Empty or
// "unknown" is unknown; a recognized SPDX id is spdx; anything else is custom.
func ClassifyLicense(declared string) License {
	d := strings.TrimSpace(declared)
	switch strings.ToLower(d) {
	case "", "unknown", "noassertion", "none":
		return License{Kind: LicenseUnknown}
	case "custom":
		return License{Kind: LicenseCustom, ID: "custom"}
	}
	if id, ok := redistributable[strings.ToLower(d)]; ok {
		return License{Kind: LicenseSPDX, ID: id}
	}
	return License{Kind: LicenseCustom, ID: d}
}

// Modes of a skill entry.
const (
	ModeHosted  = "hosted"
	ModeIndexed = "indexed"
)

// Access says what of a version's content may be served (design §6).
type Access struct {
	// FullText allows single-file reads (detail page rendering).
	FullText bool
	// Artifact allows serving artifact bytes (hosted artifact, or the
	// evictable cached artifact of an indexed version).
	Artifact bool
	// Reason is a stable code when access is restricted.
	Reason string
}

// ContentAccess applies the redistribution table of design §6. Hosted content
// is published by its owner and is always servable to callers who can see the
// entry. Indexed content follows the license: redistributable SPDX is served,
// custom only after reviewer approval, unknown never (metadata and source
// link only).
func ContentAccess(mode, licenseKind string, customApproved bool) Access {
	if mode == ModeHosted {
		return Access{FullText: true, Artifact: true}
	}
	switch licenseKind {
	case LicenseSPDX:
		return Access{FullText: true, Artifact: true}
	case LicenseCustom:
		if customApproved {
			return Access{FullText: true, Artifact: true}
		}
		return Access{Reason: "license_pending_review"}
	default:
		return Access{Reason: "license_unknown"}
	}
}
