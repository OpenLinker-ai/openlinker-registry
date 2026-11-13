// Package safehttp builds HTTP clients for requests whose destination is
// chosen by configuration or by remote content (upstream registries, index
// entries). The client refuses to connect to non-public addresses, checked on
// the address actually dialed, never follows redirects and ignores proxy
// settings. It holds no credentials and no registry policy.
package safehttp

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Stable error codes.
const (
	CodeURLInvalid          = "url_invalid"
	CodeSchemeNotAllowed    = "scheme_not_allowed"
	CodeAddressNotAllowed   = "address_not_allowed"
	CodeRedirectNotFollowed = "redirect_not_followed"
	CodeTooLarge            = "too_large"
	CodeTimeout             = "timeout"
	CodeUnreachable         = "unreachable"
	CodeStatus              = "status"
)

// Error is an outbound request failure with a stable Code. Status is the
// HTTP status for the codes "status" and "redirect_not_followed" and 0
// otherwise. Msg names at most the method, scheme and host of the request:
// never the path or query, header values, response bodies or credentials.
type Error struct {
	Code   string
	Msg    string
	Status int

	// cause is only ever context.Canceled or context.DeadlineExceeded, so
	// callers can tell their own cancellation apart with errors.Is. The
	// transport error is deliberately not kept: it embeds the full URL.
	cause error
}

func (e *Error) Error() string { return e.Msg }

// Unwrap returns context.Canceled or context.DeadlineExceeded when one of
// them ended the request, and nil otherwise.
func (e *Error) Unwrap() error { return e.cause }

// Policy describes where outbound requests may go. The zero value allows
// https to public addresses only.
type Policy struct {
	AllowHTTP   bool           // permit http:// (default https only)
	AllowCIDRs  []netip.Prefix // non-public ranges that are allowed anyway (e.g. an internal upstream)
	Timeout     time.Duration  // whole request including the body; default 30s
	DialTimeout time.Duration  // default 10s
}

const (
	defaultTimeout     = 30 * time.Second
	defaultDialTimeout = 10 * time.Second
	headerTimeout      = 15 * time.Second
	maxHeaderBytes     = 64 << 10
	// drainLimit bounds how much of a rejected response is read so the
	// connection can be reused.
	drainLimit = 4 << 10
)

// Ranges that are never public. Sources: IANA IPv4 and IPv6 special-purpose
// address registries.
var (
	blocked4 = mustPrefixes(
		"0.0.0.0/8",       // "this network", includes the unspecified address
		"10.0.0.0/8",      // RFC 1918
		"100.64.0.0/10",   // CGNAT (RFC 6598)
		"127.0.0.0/8",     // loopback
		"169.254.0.0/16",  // link-local, includes 169.254.169.254 metadata
		"172.16.0.0/12",   // RFC 1918
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation (TEST-NET-1)
		"192.88.99.0/24",  // deprecated 6to4 relay anycast
		"192.168.0.0/16",  // RFC 1918
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation (TEST-NET-2)
		"203.0.113.0/24",  // documentation (TEST-NET-3)
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved, includes 255.255.255.255 broadcast
	)
	// IPv6 is allowed only inside global unicast 2000::/3 (and the NAT64
	// well-known prefix, judged by its embedded IPv4 address), so loopback,
	// unspecified, IPv4-compatible, ULA fc00::/7, link-local fe80::/10,
	// site-local fec0::/10, multicast ff00::/8, discard 100::/64, local NAT64
	// 64:ff9b:1::/48 and SRv6 5f00::/16 are all outside without being listed.
	global6 = netip.MustParsePrefix("2000::/3")
	nat64   = netip.MustParsePrefix("64:ff9b::/96")
	sixToFo = netip.MustParsePrefix("2002::/16")
	// Special ranges inside 2000::/3.
	blocked6 = mustPrefixes(
		"2001::/23",     // IETF protocol assignments: Teredo, benchmarking, ORCHID
		"2001:db8::/32", // documentation
		"3fff::/20",     // documentation (RFC 9637)
	)
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

func inAny(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// public reports whether a (no zone, already unmapped) is a public unicast
// address. NAT64 and 6to4 addresses are judged by the IPv4 address they
// embed, because that is where the packets end up.
func public(a netip.Addr) bool {
	if a.Is4() {
		return !inAny(a, blocked4)
	}
	b := a.As16()
	switch {
	case nat64.Contains(a):
		return public(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	case !global6.Contains(a):
		return false
	case sixToFo.Contains(a):
		return public(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
	}
	return !inAny(a, blocked6)
}

// Allows reports whether the policy permits connecting to addr: it is a
// public unicast address or lies inside one of AllowCIDRs. IPv4-mapped IPv6
// addresses are treated as the IPv4 address they carry, on both sides of the
// comparison; zones are ignored.
func (p Policy) Allows(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	a := addr.WithZone("").Unmap()
	for _, pre := range p.AllowCIDRs {
		if !pre.IsValid() {
			continue
		}
		if pa := pre.Addr(); pa.Is4In6() && pre.Bits() >= 96 {
			pre = netip.PrefixFrom(pa.Unmap(), pre.Bits()-96)
		}
		if pre.Contains(a) {
			return true
		}
	}
	return public(a)
}

func errAddress() *Error {
	return &Error{Code: CodeAddressNotAllowed, Msg: "the destination address is not allowed by the outbound policy"}
}

// control runs after DNS resolution, on the socket about to connect, for
// every address the dialer tries. Checking here rather than on the hostname
// is what defeats DNS rebinding and alternative spellings of an address.
func (p Policy) control(network, address string, _ syscall.RawConn) error {
	if network != "tcp4" && network != "tcp6" {
		return errAddress()
	}
	ap, err := netip.ParseAddrPort(address)
	if err != nil || !p.Allows(ap.Addr()) {
		return errAddress()
	}
	return nil
}

// checkParsed applies the URL rules shared by CheckURL and the transport.
func checkParsed(u *url.URL, p Policy) *Error {
	bad := func(msg string) *Error { return &Error{Code: CodeURLInvalid, Msg: msg} }
	switch u.Scheme {
	case "https":
	case "http":
		if !p.AllowHTTP {
			return &Error{Code: CodeSchemeNotAllowed, Msg: "http URLs are not allowed; use https"}
		}
	case "":
		return bad("URL must be absolute")
	default:
		return &Error{Code: CodeSchemeNotAllowed, Msg: "only https URLs are allowed"}
	}
	host := u.Hostname()
	switch {
	case u.Opaque != "" || host == "":
		return bad("URL must have a host")
	case u.User != nil:
		return bad("URL must not contain credentials")
	case u.Fragment != "" || u.RawFragment != "":
		return bad("URL must not contain a fragment")
	}
	if ps := u.Port(); ps != "" {
		if n, err := strconv.Atoi(ps); err != nil || n < 1 || n > 65535 {
			return bad("URL has an invalid port")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return bad("URL has an invalid port")
	}
	// A literal address can be judged before dialing. Hostnames cannot: they
	// are judged by the dialer on the address they resolve to.
	if a, err := netip.ParseAddr(host); err == nil && !p.Allows(a) {
		return errAddress()
	}
	return nil
}

// CheckURL parses and validates an absolute URL for the policy: scheme https
// (or http when AllowHTTP), non-empty host, no userinfo, no fragment, a valid
// port, and — when the host is a literal IP address — an address the policy
// allows. It returns a *Error. Hostnames are not resolved here; the client
// checks what they resolve to when it connects.
func CheckURL(raw string, p Policy) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, &Error{Code: CodeURLInvalid, Msg: "URL cannot be parsed"}
	}
	if strings.Contains(raw, "#") {
		return nil, &Error{Code: CodeURLInvalid, Msg: "URL must not contain a fragment"}
	}
	if e := checkParsed(u, p); e != nil {
		return nil, e
	}
	return u, nil
}

// guard enforces the URL rules on every request, so a client from NewClient
// is safe to hand a URL taken from remote content without the caller having
// the Policy at hand.
type guard struct {
	policy Policy
	base   http.RoundTripper
}

func (g *guard) RoundTrip(r *http.Request) (*http.Response, error) {
	if e := checkParsed(r.URL, g.policy); e != nil {
		return nil, e
	}
	return g.base.RoundTrip(r)
}

// NewClient returns an *http.Client that
//   - never follows redirects (a 3xx is returned to the caller as a response),
//   - ignores proxy environment variables,
//   - refuses URLs that CheckURL would refuse for the same policy,
//   - validates the address actually being connected to at dial time (after
//     DNS resolution, so DNS rebinding cannot bypass it): loopback, private,
//     link-local, CGNAT, multicast, unspecified, broadcast, documentation,
//     benchmark and reserved ranges are rejected, as are IPv4-mapped, NAT64
//     and 6to4 forms of them, unless the address is inside AllowCIDRs,
//   - only dials TCP, to the host and port of the request URL,
//   - requires TLS 1.2 or later and speaks HTTP/1.1 only,
//   - bounds the dial, TLS handshake, response headers, whole request and
//     idle connections.
//
// The dialer tries every address a name resolves to and connects only to
// allowed ones.
func NewClient(p Policy) *http.Client {
	if p.Timeout <= 0 {
		p.Timeout = defaultTimeout
	}
	if p.DialTimeout <= 0 {
		p.DialTimeout = defaultDialTimeout
	}
	p.AllowCIDRs = append([]netip.Prefix(nil), p.AllowCIDRs...)
	dialer := &net.Dialer{Timeout: p.DialTimeout, KeepAlive: 30 * time.Second, Control: p.control}
	tr := &http.Transport{
		Proxy: nil, // never a proxy: the dial check must see the real destination
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if network != "tcp" {
				return nil, errAddress()
			}
			return dialer.DialContext(ctx, network, addr)
		},
		// A custom TLS config without ForceAttemptHTTP2 keeps the transport
		// on HTTP/1.1, which is all the registry protocols need.
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    p.DialTimeout,
		ResponseHeaderTimeout:  min(headerTimeout, p.Timeout),
		ExpectContinueTimeout:  time.Second,
		IdleConnTimeout:        60 * time.Second,
		MaxIdleConns:           16,
		MaxIdleConnsPerHost:    2,
		MaxResponseHeaderBytes: maxHeaderBytes,
	}
	return &http.Client{
		Timeout:   p.Timeout,
		Transport: &guard{policy: p, base: tr},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// target is the only part of a request that error messages mention.
func target(req *http.Request) string {
	if req == nil || req.URL == nil {
		return "request"
	}
	return req.Method + " " + req.URL.Scheme + "://" + req.URL.Host
}

// classify turns a transport or body-read error into a *Error without
// carrying its text, which contains the full request URL.
func classify(err error, req *http.Request, during string) *Error {
	var se *Error
	if errors.As(err, &se) {
		return &Error{Code: se.Code, Msg: target(req) + ": " + se.Msg, Status: se.Status}
	}
	if errors.Is(err, context.Canceled) {
		return &Error{Code: CodeUnreachable, Msg: target(req) + ": " + during + " was canceled", cause: context.Canceled}
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return &Error{Code: CodeTimeout, Msg: target(req) + ": " + during + " timed out", cause: context.DeadlineExceeded}
	}
	return &Error{Code: CodeUnreachable, Msg: target(req) + ": " + during + " failed"}
}

// Do sends req with client and returns the body if the status is 2xx and the
// body is at most maxBytes (after any transparent decompression). Every
// failure is a *Error: non-2xx is "status" with Status set, except 3xx which
// is "redirect_not_followed"; a larger body is "too_large". hdr is the
// response header whenever a response arrived, including on status errors
// (for Retry-After and the like). Error messages never include response
// bodies, header values, the path or query, or credentials.
func Do(client *http.Client, req *http.Request, maxBytes int64) (body []byte, hdr http.Header, err error) {
	if maxBytes < 0 {
		maxBytes = 0
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, classify(err, req, "request")
	}
	defer resp.Body.Close()
	hdr = resp.Header
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		_, _ = io.CopyN(io.Discard, resp.Body, drainLimit)
		return nil, hdr, &Error{Code: CodeRedirectNotFollowed, Status: resp.StatusCode,
			Msg: target(req) + ": redirect (status " + strconv.Itoa(resp.StatusCode) + ") was not followed"}
	default:
		_, _ = io.CopyN(io.Discard, resp.Body, drainLimit)
		return nil, hdr, &Error{Code: CodeStatus, Status: resp.StatusCode,
			Msg: target(req) + ": responded with status " + strconv.Itoa(resp.StatusCode)}
	}
	tooLarge := &Error{Code: CodeTooLarge, Msg: target(req) + ": response exceeds the size limit"}
	if resp.ContentLength > maxBytes {
		return nil, hdr, tooLarge
	}
	limit := maxBytes
	if limit < math.MaxInt64 {
		limit++ // one byte more than allowed tells "exactly maxBytes" from "more"
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, hdr, classify(err, req, "reading the response")
	}
	if int64(len(data)) > maxBytes {
		return nil, hdr, tooLarge
	}
	return data, hdr, nil
}
