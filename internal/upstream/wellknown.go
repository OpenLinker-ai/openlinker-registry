package upstream

import (
	"context"
	"encoding/json"
	"regexp"
)

// Index entry types of the agentskills.io discovery format.
const (
	IndexTypeSkillMD = "skill-md"
	IndexTypeArchive = "archive"
)

var sha256DigestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// IndexEntry is one usable entry of a discovery index. URL is absolute.
type IndexEntry struct {
	Name        string
	Type        string // "skill-md" | "archive"
	Description string
	URL         string
	Digest      string // "sha256:<64 lowercase hex>"
}

// Index is a discovery index as far as it could be read.
type Index struct {
	// Entries are the usable entries, in index order.
	Entries []IndexEntry
	// Dropped counts the entries that cannot be used. DroppedNames are the
	// names of those whose name could be read: the index lists them, although
	// nothing can be fetched for them now.
	Dropped      int
	DroppedNames []string
	// Complete is false when the name of a dropped entry could not be read.
	// The index then does not tell which skills it lists.
	Complete bool
}

// WellKnownIndex fetches {base}/.well-known/agent-skills/index.json. An
// entry is dropped when it is not an object of strings, has no name or url,
// an unknown type, a digest that is not sha256:<64 lowercase hex>, or a url
// that does not resolve — relative URLs are resolved against the index URL —
// to an absolute http(s) URL without credentials or fragment. Names are not
// otherwise validated and may repeat. Entry URLs may point to any origin;
// Fetch decides per request whether the token goes along.
func (c *Client) WellKnownIndex(ctx context.Context) (Index, error) {
	const op = "skill index"
	indexURL, err := c.apiURL("/.well-known/agent-skills/index.json", nil)
	if err != nil {
		return Index{}, wrapOp(op, err)
	}
	body, err := c.get(ctx, indexURL, "application/json", c.maxBody())
	if err != nil {
		return Index{}, wrapOp(op, err)
	}
	raws, _, err := listField(body, "skills", "discovery index")
	if err != nil {
		return Index{}, wrapOp(op, err)
	}
	index := Index{Complete: true}
	drop := func(raw json.RawMessage) {
		index.Dropped++
		if name := rawString(rawObject(raw), "name"); name != "" {
			index.DroppedNames = append(index.DroppedNames, name)
		} else {
			index.Complete = false
		}
	}
	for _, raw := range raws {
		var w struct {
			Name        string `json:"name"`
			Type        string `json:"type"`
			Description string `json:"description"`
			URL         string `json:"url"`
			Digest      string `json:"digest"`
		}
		if json.Unmarshal(raw, &w) != nil || w.Name == "" || w.URL == "" ||
			(w.Type != IndexTypeSkillMD && w.Type != IndexTypeArchive) || !sha256DigestRe.MatchString(w.Digest) {
			drop(raw)
			continue
		}
		u, err := indexURL.Parse(w.URL)
		if err != nil || !plainHTTPURL(u) {
			drop(raw)
			continue
		}
		index.Entries = append(index.Entries, IndexEntry{Name: w.Name, Type: w.Type, Description: w.Description, URL: u.String(), Digest: w.Digest})
	}
	return index, nil
}
