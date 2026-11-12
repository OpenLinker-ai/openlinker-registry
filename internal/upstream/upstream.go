// Package upstream holds typed read-only clients for the protocols another
// registry may speak: the MCP Registry read API, the openlinker-registry
// native skill API and the agentskills.io discovery index. It only fetches
// and decodes. What is returned is the upstream's claim: digests, names and
// manifests are not verified here, nothing is stored, and no registry policy
// (namespaces, visibility, mirroring rules) is applied.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/OpenLinker-ai/openlinker-registry/internal/safehttp"
)

// ErrNotFound reports that the upstream answered 404. Every method wraps it
// for a 404, so for the list and index calls it means the upstream does not
// serve that protocol. Other statuses (including 410 for yanked content) are
// a *safehttp.Error with code "status".
var ErrNotFound = errors.New("upstream: not found")

// Stable codes of *Error.
const (
	// CodeBadResponse: the upstream answered 2xx with something that is not
	// the expected document.
	CodeBadResponse = "bad_response"
	// CodeInvalidURL: BaseURL or a URL passed to Fetch is not an absolute
	// http(s) URL without credentials or fragment.
	CodeInvalidURL = "invalid_url"
	// CodeInvalidArgument: an empty or unusable name, namespace or version.
	CodeInvalidArgument = "invalid_argument"
)

// Error is a failure decided by this package rather than by the transport.
// Msg never contains response content, URLs or credentials.
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func badResponse(msg string) error    { return &Error{Code: CodeBadResponse, Msg: msg} }
func invalidArg(msg string) error     { return &Error{Code: CodeInvalidArgument, Msg: msg} }
func invalidURL(msg string) error     { return &Error{Code: CodeInvalidURL, Msg: msg} }
func wrapOp(op string, e error) error { return fmt.Errorf("upstream %s: %w", op, e) }

const (
	defaultMaxBody   = 4 << 20
	defaultUserAgent = "openlinker-registry"
)

// Client talks to one upstream. Token (optional) is sent as
// "Authorization: Bearer <token>" only with requests whose scheme, host and
// port equal BaseURL's; never to other origins, never in URLs, and it is
// never logged or put in errors. Whether a token may travel over plain http
// is decided by the safehttp policy of HTTP (AllowHTTP), not here.
//
// A Client is safe for concurrent use. Set the fields before the first call
// and use it by pointer.
type Client struct {
	HTTP      *http.Client // from safehttp.NewClient
	BaseURL   string       // e.g. "https://registry.modelcontextprotocol.io" or "https://example.com/registry"; a path prefix is kept, a trailing slash is ignored
	Token     string
	MaxBody   int64  // per JSON response; default 4 MiB. Artifact and Fetch calls take an explicit limit
	UserAgent string // default "openlinker-registry"

	mu        sync.Mutex
	mcpPrefix string // "/v0.1" or "/v0" once one of them has answered
}

func (c *Client) maxBody() int64 {
	if c.MaxBody > 0 {
		return c.MaxBody
	}
	return defaultMaxBody
}

// base parses BaseURL. The error never echoes the value: a misconfigured
// base URL may carry credentials.
func (c *Client) base() (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(c.BaseURL, "/"))
	if err != nil || !plainHTTPURL(u) || u.RawQuery != "" || u.ForceQuery {
		return nil, invalidURL("upstream base URL must be an absolute http(s) URL without credentials, query or fragment")
	}
	return u, nil
}

// plainHTTPURL reports whether u is an absolute http(s) URL with a host and
// without userinfo or fragment. The scheme and address policy proper is
// enforced by the safehttp client; this keeps credentials out of URLs and
// rejects what could never be requested.
func plainHTTPURL(u *url.URL) bool {
	return (u.Scheme == "https" || u.Scheme == "http") && u.Opaque == "" && u.Hostname() != "" &&
		u.User == nil && u.Fragment == "" && u.RawFragment == ""
}

// origin is scheme, lowercase host and effective port.
func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	return u.Scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port
}

// seg escapes one path segment so that it stays one segment on the wire:
// "/" becomes %2F and, beyond url.PathEscape, "+" becomes %2B so servers that
// treat "+" as a space in paths still see the version "1.0.0+build".
func seg(s string) string {
	return strings.ReplaceAll(url.PathEscape(s), "+", "%2B")
}

// checkSegs rejects path parameters that would address a different resource:
// empty ones and the dot segments that servers and proxies normalize away.
func checkSegs(what string, vals ...string) error {
	for _, v := range vals {
		if v == "" || v == "." || v == ".." {
			return invalidArg(what + " must not be empty or a dot segment")
		}
	}
	return nil
}

// apiURL builds base + escapedPath (+ query) without re-encoding the path,
// so escaped slashes inside a segment survive.
func (c *Client) apiURL(escapedPath string, q url.Values) (*url.URL, error) {
	b, err := c.base()
	if err != nil {
		return nil, err
	}
	raw := b.Scheme + "://" + b.Host + b.EscapedPath() + escapedPath
	if len(q) > 0 {
		raw += "?" + q.Encode()
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, invalidURL("upstream request URL cannot be built")
	}
	return u, nil
}

// get is the single place a request leaves this package, and therefore the
// single place that decides whether the token goes with it.
func (c *Client) get(ctx context.Context, u *url.URL, accept string, max int64) ([]byte, error) {
	if c.HTTP == nil {
		return nil, invalidArg("upstream client has no HTTP client")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, invalidURL("upstream request cannot be built")
	}
	ua := c.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", accept)
	if c.Token != "" {
		b, err := c.base()
		if err != nil {
			return nil, err
		}
		if origin(u) == origin(b) {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
	}
	body, _, err := safehttp.Do(c.HTTP, req, max)
	if err != nil {
		var se *safehttp.Error
		if errors.As(err, &se) && se.Code == safehttp.CodeStatus && se.Status == http.StatusNotFound {
			// Both ErrNotFound (errors.Is) and the *safehttp.Error
			// (errors.As) stay reachable.
			return nil, fmt.Errorf("%w (%w)", ErrNotFound, err)
		}
		return nil, err
	}
	return body, nil
}

// getJSON fetches an API path under BaseURL as JSON, capped by MaxBody.
func (c *Client) getJSON(ctx context.Context, escapedPath string, q url.Values) ([]byte, error) {
	u, err := c.apiURL(escapedPath, q)
	if err != nil {
		return nil, err
	}
	return c.get(ctx, u, "application/json", c.maxBody())
}

// decode is strict about JSON (one value, right types) and lenient about
// unknown fields. The parser's message is dropped: it can quote the body.
func decode(body []byte, out any, what string) error {
	if err := json.Unmarshal(body, out); err != nil {
		return badResponse("upstream returned an invalid " + what)
	}
	return nil
}

// listField decodes body as an object and returns the array under key. The
// key must be present (null counts as empty): an object without it is some
// other document, and reading it as an empty list would look like "the
// upstream has nothing".
func listField(body []byte, key, what string) (items []json.RawMessage, top map[string]json.RawMessage, err error) {
	if err := decode(body, &top, what); err != nil {
		return nil, nil, err
	}
	raw, ok := top[key]
	if !ok {
		return nil, nil, badResponse("upstream returned an invalid " + what)
	}
	if err := decode(raw, &items, what); err != nil {
		return nil, nil, err
	}
	return items, top, nil
}

// Fetch downloads rawURL — typically an index entry URL, on any origin the
// client's safehttp policy permits — with a size cap. The token is sent only
// if rawURL has the same origin as BaseURL. Redirects are not followed.
func (c *Client) Fetch(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || !plainHTTPURL(u) {
		return nil, wrapOp("fetch", invalidURL("URL must be an absolute http(s) URL without credentials or fragment"))
	}
	body, err := c.get(ctx, u, "*/*", maxBytes)
	if err != nil {
		return nil, wrapOp("fetch", err)
	}
	return body, nil
}
