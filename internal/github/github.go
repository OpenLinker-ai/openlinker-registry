// Package github resolves GitHub skill URLs to fixed commits and fetches skill
// files with integrity checks. It only talks to the configured API and raw
// hosts, never follows redirects and verifies every file's git blob SHA-1
// over the raw bytes.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/gitblob"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
)

// Config configures the client. Base URLs are configurable so tests (and GHE
// later) can point to other hosts.
type Config struct {
	APIBase string // default https://api.github.com
	RawBase string // default https://raw.githubusercontent.com
	WebHost string // host accepted in user URLs, default github.com
	Token   string // optional read-only token, sent to the API host only
	Timeout time.Duration
	Limits  skill.Limits
}

// Error is a client-facing failure with a stable code and HTTP status.
type Error struct {
	Code   string
	Status int
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

func gerr(status int, code, msg string) error { return &Error{Code: code, Status: status, Msg: msg} }

// Client talks to GitHub.
type Client struct {
	cfg  Config
	api  *url.URL
	raw  *url.URL
	http *http.Client
}

// New validates cfg and builds a client whose transport refuses any host
// other than the configured API and raw hosts.
func New(cfg Config) (*Client, error) {
	if cfg.APIBase == "" {
		cfg.APIBase = "https://api.github.com"
	}
	if cfg.RawBase == "" {
		cfg.RawBase = "https://raw.githubusercontent.com"
	}
	if cfg.WebHost == "" {
		cfg.WebHost = "github.com"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 20 * time.Second
	}
	if cfg.Limits.MaxFiles == 0 {
		cfg.Limits = skill.DefaultLimits
	}
	api, err := parseBase(cfg.APIBase)
	if err != nil {
		return nil, fmt.Errorf("github api base: %w", err)
	}
	raw, err := parseBase(cfg.RawBase)
	if err != nil {
		return nil, fmt.Errorf("github raw base: %w", err)
	}
	allowed := map[string]bool{api.Scheme + "://" + api.Host: true, raw.Scheme + "://" + raw.Host: true}
	hc := &http.Client{
		Timeout:   cfg.Timeout,
		Transport: &hostGuard{base: http.DefaultTransport, allowed: allowed},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Client{cfg: cfg, api: api, raw: raw, http: hc}, nil
}

func parseBase(s string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(s, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("must be an http(s) URL without credentials, query or fragment")
	}
	return u, nil
}

type hostGuard struct {
	base    http.RoundTripper
	allowed map[string]bool
}

func (g *hostGuard) RoundTrip(r *http.Request) (*http.Response, error) {
	if !g.allowed[r.URL.Scheme+"://"+r.URL.Host] {
		return nil, fmt.Errorf("outbound request to %s is not allowed", r.URL.Host)
	}
	return g.base.RoundTrip(r)
}

// Ref describes a parsed GitHub skill URL.
type Ref struct {
	Owner string
	Repo  string
	// Ref is the branch, tag or commit from the URL ("" for the default branch).
	Ref string
	// Dir is the skill directory relative to the repository root ("" = root).
	Dir string
	// SingleFile is true for blob URLs pointing at SKILL.md.
	SingleFile bool
}

// SourceKey is the normalized identity of the source used to reject duplicate
// indexed entries.
func (r Ref) SourceKey() string {
	return "github:" + strings.ToLower(r.Owner) + "/" + strings.ToLower(r.Repo) + ":/" + r.Dir
}

var (
	ownerRe  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	repoRe   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	refRe    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}$`)
	CommitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// ParseURL parses https://<webhost>/<owner>/<repo>[/tree/<ref>[/<dir>]] or
// .../blob/<ref>/<dir>/SKILL.md. The ref is the single path segment after
// tree/ or blob/; refs that contain "/" cannot be expressed and must be given
// as a commit SHA. explicitRef, when set, overrides the URL ref and must not
// contain "/".
func (c *Client) ParseURL(raw, explicitRef string) (Ref, error) {
	bad := func(msg string) (Ref, error) { return Ref{}, gerr(http.StatusBadRequest, "invalid_github_url", msg) }
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, c.cfg.WebHost) || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return bad("expected https://" + c.cfg.WebHost + "/<owner>/<repo>/tree/<ref>/<path>")
	}
	// Split the escaped path so an encoded slash (%2F) stays inside its
	// segment and is then rejected as part of a ref.
	segs := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	for i, s := range segs {
		if segs[i], err = url.PathUnescape(s); err != nil {
			return bad("invalid URL escaping")
		}
	}
	if len(segs) < 2 || !ownerRe.MatchString(segs[0]) || !repoRe.MatchString(segs[1]) || segs[1] == "." || segs[1] == ".." {
		return bad("invalid owner or repository")
	}
	r := Ref{Owner: segs[0], Repo: strings.TrimSuffix(segs[1], ".git")}
	rest := segs[2:]
	if len(rest) > 0 {
		if len(rest) < 2 || (rest[0] != "tree" && rest[0] != "blob") {
			return bad("URL must use /tree/<ref>/<path> or /blob/<ref>/<path>/SKILL.md")
		}
		r.Ref = rest[1]
		p := strings.Join(rest[2:], "/")
		if rest[0] == "blob" {
			if path.Base(p) != skill.SkillFile {
				return bad("blob URLs must point at SKILL.md")
			}
			r.SingleFile = true
			p = path.Dir(p)
			if p == "." {
				p = ""
			}
		}
		if p != "" {
			if err := skill.ValidatePath(p); err != nil {
				return bad("invalid path in URL")
			}
		}
		r.Dir = p
	}
	if explicitRef != "" {
		r.Ref = explicitRef
	}
	if strings.Contains(r.Ref, "/") {
		return Ref{}, gerr(http.StatusBadRequest, "ref_unsupported", "refs containing '/' are not supported; use a 40-character commit SHA")
	}
	if r.Ref != "" && !refRe.MatchString(r.Ref) {
		return Ref{}, gerr(http.StatusBadRequest, "ref_unsupported", "unsupported ref; use a branch, tag or commit SHA")
	}
	return r, nil
}

// ResolveCommit returns the 40-hex commit for ref (or the default branch).
func (c *Client) ResolveCommit(ctx context.Context, r Ref) (string, error) {
	ref := r.Ref
	if CommitRe.MatchString(ref) {
		return ref, nil
	}
	if ref == "" {
		var repo struct {
			DefaultBranch string `json:"default_branch"`
		}
		if err := c.apiJSON(ctx, "/repos/"+esc(r.Owner)+"/"+esc(r.Repo), &repo); err != nil {
			return "", err
		}
		if repo.DefaultBranch == "" || strings.Contains(repo.DefaultBranch, "/") || !refRe.MatchString(repo.DefaultBranch) {
			return "", gerr(http.StatusBadRequest, "ref_unsupported", "the default branch name cannot be resolved; use a commit SHA")
		}
		ref = repo.DefaultBranch
	}
	body, err := c.get(ctx, c.api, "/repos/"+esc(r.Owner)+"/"+esc(r.Repo)+"/commits/"+esc(ref), "application/vnd.github.sha", 128)
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(body))
	if !CommitRe.MatchString(sha) {
		return "", gerr(http.StatusBadGateway, "upstream_invalid", "GitHub returned an invalid commit id")
	}
	return sha, nil
}

// Snapshot is a verified set of files at a fixed commit.
type Snapshot struct {
	Ref     Ref
	Commit  string
	TreeSHA string
	Files   []skill.File // sorted, validated, GitBlobSHA set
}

type treeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size *int64 `json:"size"`
}

type treeResponse struct {
	SHA       string      `json:"sha"`
	Tree      []treeEntry `json:"tree"`
	Truncated bool        `json:"truncated"`
}

// Fetch lists the skill directory at commit, enforces limits before
// downloading, then downloads every file from the raw host and verifies its
// size and git blob SHA-1 over the raw bytes.
func (c *Client) Fetch(ctx context.Context, r Ref, commit string) (*Snapshot, error) {
	if !CommitRe.MatchString(commit) {
		return nil, gerr(http.StatusBadRequest, "commit_invalid", "commit must be a 40-character lowercase hex SHA")
	}
	treeish := commit
	if r.Dir != "" {
		treeish += ":" + escPath(r.Dir)
	}
	p := "/repos/" + esc(r.Owner) + "/" + esc(r.Repo) + "/git/trees/" + treeish
	if !r.SingleFile {
		p += "?recursive=1"
	}
	var tree treeResponse
	if err := c.apiJSON(ctx, p, &tree); err != nil {
		return nil, err
	}
	if tree.Truncated {
		return nil, gerr(http.StatusUnprocessableEntity, "tree_truncated", "the GitHub tree listing is truncated; the directory is too large")
	}
	if !CommitRe.MatchString(tree.SHA) {
		return nil, gerr(http.StatusBadGateway, "upstream_invalid", "GitHub returned an invalid tree")
	}
	lim := c.cfg.Limits
	var entries []treeEntry
	var total int64
	for _, e := range tree.Tree {
		switch {
		case e.Type == "tree":
			continue
		case e.Type == "commit" || e.Mode == "160000":
			if r.SingleFile && e.Path != skill.SkillFile {
				continue
			}
			return nil, gerr(http.StatusUnprocessableEntity, "submodule_rejected", "skills must not contain git submodules: "+e.Path)
		case e.Mode == "120000":
			if r.SingleFile && e.Path != skill.SkillFile {
				continue
			}
			return nil, gerr(http.StatusUnprocessableEntity, "symlink_rejected", "skills must not contain symbolic links: "+e.Path)
		case e.Type != "blob" || (e.Mode != "100644" && e.Mode != "100755"):
			return nil, gerr(http.StatusUnprocessableEntity, "entry_rejected", "unsupported tree entry: "+e.Path)
		}
		if r.SingleFile && e.Path != skill.SkillFile {
			continue
		}
		if err := skill.ValidatePath(e.Path); err != nil {
			return nil, gerr(http.StatusUnprocessableEntity, "path_unsafe", err.Error())
		}
		if !CommitRe.MatchString(e.SHA) || e.Size == nil || *e.Size < 0 {
			return nil, gerr(http.StatusBadGateway, "upstream_invalid", "GitHub returned an invalid tree entry")
		}
		if *e.Size > lim.MaxFileBytes {
			return nil, gerr(http.StatusUnprocessableEntity, "file_too_large", "file exceeds the per-file size limit: "+e.Path)
		}
		total += *e.Size
		if total > lim.MaxTotalBytes {
			return nil, gerr(http.StatusUnprocessableEntity, "total_too_large", "skill exceeds the total size limit")
		}
		entries = append(entries, e)
		if len(entries) > lim.MaxFiles {
			return nil, gerr(http.StatusUnprocessableEntity, "too_many_files", "too many files")
		}
	}
	files := make([]skill.File, 0, len(entries))
	for _, e := range entries {
		full := e.Path
		if r.Dir != "" {
			full = r.Dir + "/" + e.Path
		}
		data, err := c.get(ctx, c.raw, "/"+esc(r.Owner)+"/"+esc(r.Repo)+"/"+commit+"/"+escPath(full), "", *e.Size)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) != *e.Size || gitblob.SHA1(data) != e.SHA {
			return nil, gerr(http.StatusBadGateway, "integrity_mismatch", "downloaded bytes do not match the git blob: "+e.Path)
		}
		files = append(files, skill.File{Path: e.Path, Data: data, GitBlobSHA: e.SHA})
	}
	files, err := skill.ValidateFiles(files, lim)
	if err != nil {
		var ve *skill.ValidationError
		if errors.As(err, &ve) {
			return nil, gerr(http.StatusUnprocessableEntity, ve.Code, ve.Msg)
		}
		return nil, err
	}
	return &Snapshot{Ref: r, Commit: commit, TreeSHA: tree.SHA, Files: files}, nil
}

func esc(s string) string { return url.PathEscape(s) }

func escPath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

func (c *Client) apiJSON(ctx context.Context, p string, out any) error {
	body, err := c.get(ctx, c.api, p, "application/vnd.github+json", 8<<20)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return gerr(http.StatusBadGateway, "upstream_invalid", "GitHub returned invalid JSON")
	}
	return nil
}

// get performs a GET against base+p and reads at most max bytes; a larger
// body is an error.
func (c *Client) get(ctx context.Context, base *url.URL, p, accept string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String()+p, nil)
	if err != nil {
		return nil, gerr(http.StatusBadRequest, "invalid_github_url", "could not build GitHub request")
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("User-Agent", "openlinker-registry")
	if base == c.api {
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if c.cfg.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, gerr(http.StatusBadGateway, "upstream_unavailable", "GitHub is unreachable")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnprocessableEntity:
		return nil, gerr(http.StatusNotFound, "not_found", "repository, ref or path not found on GitHub")
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, gerr(http.StatusBadGateway, "upstream_redirect", "GitHub redirected the request (renamed or moved repository); use the canonical URL")
	default:
		return nil, gerr(http.StatusBadGateway, "upstream_unavailable", fmt.Sprintf("GitHub responded with status %d", resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, gerr(http.StatusBadGateway, "upstream_unavailable", "reading GitHub response failed")
	}
	if int64(len(data)) > max {
		return nil, gerr(http.StatusBadGateway, "upstream_too_large", "GitHub response exceeds the expected size")
	}
	return data, nil
}

// RawURL is the public raw URL of a file at a fixed commit (no credentials).
func (c *Client) RawURL(owner, repo, commit, p string) string {
	return c.raw.String() + "/" + esc(owner) + "/" + esc(repo) + "/" + commit + "/" + escPath(p)
}

// WebURL is the browsable GitHub URL of the skill at a fixed commit.
func (c *Client) WebURL(owner, repo, commit, dir string, singleFile bool) string {
	kind, p := "tree", dir
	if singleFile {
		kind = "blob"
		p = path.Join(dir, skill.SkillFile)
	}
	u := "https://" + c.cfg.WebHost + "/" + esc(owner) + "/" + esc(repo) + "/" + kind + "/" + commit
	if p != "" {
		u += "/" + escPath(p)
	}
	return u
}
