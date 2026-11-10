// Package githubtest provides an in-process fake of the GitHub REST API and
// raw content host for tests. No real network is used.
package githubtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/OpenLinker-ai/openlinker-registry/internal/gitblob"
)

// Entry is a file in a fake commit.
type Entry struct {
	Data []byte
	Mode string // default 100644; 120000 symlink; 160000 submodule
}

// Fake serves one repository.
type Fake struct {
	Owner, Repo   string
	DefaultBranch string

	mu       sync.Mutex
	refs     map[string]string           // ref -> commit
	commits  map[string]map[string]Entry // commit -> path -> entry
	rawOver  map[string][]byte           // "commit/path" -> served bytes (tamper)
	Truncate bool
	RawHits  atomic.Int64
	API      *httptest.Server
	Raw      *httptest.Server
	// Down makes every request fail with 503.
	Down atomic.Bool
}

// New starts API and raw servers.
func New(owner, repo string) *Fake {
	f := &Fake{Owner: owner, Repo: repo, DefaultBranch: "main", refs: map[string]string{}, commits: map[string]map[string]Entry{}, rawOver: map[string][]byte{}}
	f.API = httptest.NewServer(http.HandlerFunc(f.serveAPI))
	f.Raw = httptest.NewServer(http.HandlerFunc(f.serveRaw))
	return f
}

// Close stops the servers.
func (f *Fake) Close() { f.API.Close(); f.Raw.Close() }

// AddCommit registers a commit with files and points refs at it.
func (f *Fake) AddCommit(commit string, files map[string]Entry, refs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits[commit] = files
	for _, r := range refs {
		f.refs[r] = commit
	}
}

// Tamper makes the raw host serve different bytes for commit/path.
func (f *Fake) Tamper(commit, p string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rawOver[commit+"/"+p] = data
}

// TreeSHA computes the fake (but stable) tree id of dir in commit.
func TreeSHA(files map[string]Entry, dir string) string {
	var keys []string
	for p, e := range files {
		if dir == "" || strings.HasPrefix(p, dir+"/") {
			keys = append(keys, p+"\x00"+e.Mode+"\x00"+gitblob.SHA1(e.Data))
		}
	}
	sort.Strings(keys)
	return gitblob.SHA1([]byte(strings.Join(keys, "\n")))
}

func (f *Fake) serveAPI(w http.ResponseWriter, r *http.Request) {
	if f.Down.Load() {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := "/repos/" + f.Owner + "/" + f.Repo
	p := r.URL.EscapedPath()
	if !strings.HasPrefix(p, prefix) {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(p, prefix)
	switch {
	case rest == "":
		_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": f.DefaultBranch})
	case strings.HasPrefix(rest, "/commits/"):
		ref := strings.TrimPrefix(rest, "/commits/")
		c, ok := f.refs[ref]
		if !ok {
			if _, isCommit := f.commits[ref]; isCommit {
				c = ref
			} else {
				http.Error(w, `{"message":"No commit found"}`, http.StatusUnprocessableEntity)
				return
			}
		}
		if r.Header.Get("Accept") != "application/vnd.github.sha" {
			http.Error(w, "bad accept", 400)
			return
		}
		_, _ = w.Write([]byte(c))
	case strings.HasPrefix(rest, "/git/trees/"):
		treeish := strings.TrimPrefix(rest, "/git/trees/")
		commit, dir, _ := strings.Cut(treeish, ":")
		files, ok := f.commits[commit]
		if !ok {
			http.NotFound(w, r)
			return
		}
		recursive := r.URL.Query().Get("recursive") == "1"
		type te struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
			Size *int64 `json:"size,omitempty"`
		}
		var out []te
		seenDir := map[string]bool{}
		found := dir == ""
		for fp, e := range files {
			rel := fp
			if dir != "" {
				if !strings.HasPrefix(fp, dir+"/") {
					continue
				}
				rel = strings.TrimPrefix(fp, dir+"/")
			}
			found = true
			if !recursive && strings.Contains(rel, "/") {
				d := strings.SplitN(rel, "/", 2)[0]
				if !seenDir[d] {
					seenDir[d] = true
					out = append(out, te{Path: d, Mode: "040000", Type: "tree", SHA: TreeSHA(files, path.Join(dir, d))})
				}
				continue
			}
			if recursive {
				for d := path.Dir(rel); d != "."; d = path.Dir(d) {
					if !seenDir[d] {
						seenDir[d] = true
						out = append(out, te{Path: d, Mode: "040000", Type: "tree", SHA: TreeSHA(files, path.Join(dir, d))})
					}
				}
			}
			mode, typ := e.Mode, "blob"
			if mode == "" {
				mode = "100644"
			}
			if mode == "160000" {
				typ = "commit"
			}
			n := int64(len(e.Data))
			out = append(out, te{Path: rel, Mode: mode, Type: typ, SHA: gitblob.SHA1(e.Data), Size: &n})
		}
		if !found {
			http.NotFound(w, r)
			return
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": TreeSHA(files, dir), "tree": out, "truncated": f.Truncate})
	default:
		http.NotFound(w, r)
	}
}

func (f *Fake) serveRaw(w http.ResponseWriter, r *http.Request) {
	if f.Down.Load() {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	f.RawHits.Add(1)
	if r.Header.Get("Authorization") != "" {
		http.Error(w, "unexpected credentials on raw host", 400)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := "/" + f.Owner + "/" + f.Repo + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	commit, fp, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if data, ok := f.rawOver[commit+"/"+fp]; ok {
		_, _ = w.Write(data)
		return
	}
	e, ok := f.commits[commit][fp]
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(e.Data)
}
