package safehttp_test

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/safehttp"
)

var loopback = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}

func code(err error) string {
	var se *safehttp.Error
	if errors.As(err, &se) {
		return se.Code
	}
	return ""
}

func get(t *testing.T, c *http.Client, rawURL string, max int64) ([]byte, http.Header, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return safehttp.Do(c, req, max)
}

func TestAllowsDefaultPolicy(t *testing.T) {
	blocked := []string{
		// IPv4
		"127.0.0.1", "127.255.255.254", "10.0.0.1", "10.255.255.255", "172.16.0.1", "172.31.255.255",
		"192.168.1.1", "169.254.169.254", "169.254.0.1", "100.64.0.1", "100.127.255.255",
		"0.0.0.0", "0.1.2.3", "224.0.0.1", "239.255.255.250", "240.0.0.1", "255.255.255.255",
		"192.0.2.1", "198.51.100.7", "203.0.113.9", "198.18.0.1", "198.19.255.255", "192.0.0.8", "192.88.99.1",
		// IPv6
		"::", "::1", "fe80::1", "fe80::1%eth0", "fc00::1", "fd12:3456:789a::1", "fec0::1",
		"ff02::1", "ff0e::1", "2001:db8::1", "3fff::1", "2001::1", "2001:2::1", "2001:10::1",
		"100::1", "64:ff9b:1::1", "5f00::1", "4000::1", "::127.0.0.1", "::10.0.0.1",
		// IPv4-mapped, NAT64 and 6to4 forms of blocked IPv4 addresses
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", "::ffff:192.168.0.1", "::ffff:0.0.0.0",
		"64:ff9b::7f00:1", "64:ff9b::a00:1", "64:ff9b::a9fe:a9fe", "64:ff9b::c0a8:1",
		"2002:7f00:1::1", "2002:c0a8:101::1", "2002:a00:1::", "2002:a9fe:a9fe::1",
	}
	allowed := []string{
		"1.1.1.1", "8.8.8.8", "93.184.216.34", "100.63.255.255", "100.128.0.1", "172.15.255.255", "172.32.0.1",
		"192.167.255.255", "192.169.0.1", "169.253.255.255", "169.255.0.1", "198.17.255.255", "198.20.0.1",
		"126.255.255.255", "128.0.0.1", "223.255.255.255", "192.0.1.1", "192.0.3.1",
		"::ffff:8.8.8.8", "2606:4700:4700::1111", "2001:4860:4860::8888", "2a00:1450:4001::1", "2001:200::1",
		"64:ff9b::808:808", "2002:808:808::1",
	}
	var p safehttp.Policy
	for _, s := range blocked {
		if p.Allows(netip.MustParseAddr(s)) {
			t.Errorf("%s must be blocked", s)
		}
	}
	for _, s := range allowed {
		if !p.Allows(netip.MustParseAddr(s)) {
			t.Errorf("%s must be allowed", s)
		}
	}
	if p.Allows(netip.Addr{}) {
		t.Error("the zero address must be blocked")
	}
}

func TestAllowsCIDRs(t *testing.T) {
	p := safehttp.Policy{AllowCIDRs: []netip.Prefix{
		{}, // invalid prefixes are ignored
		netip.MustParsePrefix("10.1.0.0/16"),
		netip.MustParsePrefix("fd00::/8"),
		netip.MustParsePrefix("::ffff:192.168.7.0/120"), // IPv4-mapped spelling of 192.168.7.0/24
	}}
	cases := map[string]bool{
		"10.1.2.3":           true,
		"::ffff:10.1.2.3":    true,
		"10.2.0.1":           false,
		"10.0.255.255":       false,
		"fd12::1":            true,
		"fd12::1%eth0":       true,
		"fc00::1":            false,
		"192.168.7.9":        true,
		"::ffff:192.168.7.9": true,
		"192.168.8.1":        false,
		"127.0.0.1":          false,
		"8.8.8.8":            true,
		// An allowed IPv4 range does not open its NAT64 or 6to4 spellings.
		"64:ff9b::a01:203": false,
		"2002:a01:203::1":  false,
	}
	for s, want := range cases {
		if got := p.Allows(netip.MustParseAddr(s)); got != want {
			t.Errorf("Allows(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestCheckURL(t *testing.T) {
	cases := []struct {
		raw  string
		p    safehttp.Policy
		code string
	}{
		{"https://example.com", safehttp.Policy{}, ""},
		{"https://example.com:8443/registry/v1?x=1", safehttp.Policy{}, ""},
		{"HTTPS://Example.COM/a", safehttp.Policy{}, ""},
		{"https://[2606:4700:4700::1111]/", safehttp.Policy{}, ""},
		{"http://example.com/", safehttp.Policy{AllowHTTP: true}, ""},
		{"http://127.0.0.1:8080/", safehttp.Policy{AllowHTTP: true, AllowCIDRs: loopback}, ""},
		{"https://10.2.3.4/", safehttp.Policy{AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}, ""},

		{"http://example.com/", safehttp.Policy{}, safehttp.CodeSchemeNotAllowed},
		{"ftp://example.com/", safehttp.Policy{AllowHTTP: true}, safehttp.CodeSchemeNotAllowed},
		{"file:///etc/passwd", safehttp.Policy{AllowHTTP: true}, safehttp.CodeSchemeNotAllowed},
		{"gopher://example.com/", safehttp.Policy{}, safehttp.CodeSchemeNotAllowed},
		{"javascript:alert(1)", safehttp.Policy{}, safehttp.CodeSchemeNotAllowed},

		{"", safehttp.Policy{}, safehttp.CodeURLInvalid},
		{"example.com/path", safehttp.Policy{}, safehttp.CodeURLInvalid},
		{"/just/a/path", safehttp.Policy{}, safehttp.CodeURLInvalid},
		{"//example.com/path", safehttp.Policy{}, safehttp.CodeURLInvalid},
		{"https://", safehttp.Policy{}, safehttp.CodeURLInvalid},
		{"https:///path", safehttp.Policy{}, safehttp.CodeURLInvalid},
		{"https://:443/", safehttp.Policy{}, safehttp.CodeURLInvalid},
		{"https:example.com", safehttp.Policy{}, safehttp.CodeURLInvalid},
		{"https://user@example.com/", safehttp.Policy{}, safehttp.CodeURLInvalid},
		{"https://user:pw@example.com/", safehttp.Policy{}, safehttp.CodeURLInvalid},
		{"https://example.com/a#frag", safehttp.Policy{}, safehttp.CodeURLInvalid},
		{"https://example.com/a#", safehttp.Policy{}, safehttp.CodeURLInvalid},
		{"https://example.com:0/", safehttp.Policy{}, safehttp.CodeURLInvalid},
		{"https://example.com:99999/", safehttp.Policy{}, safehttp.CodeURLInvalid},
		{"https://example.com:/", safehttp.Policy{}, safehttp.CodeURLInvalid},
		{"https://exa mple.com/", safehttp.Policy{}, safehttp.CodeURLInvalid},
		{"https://example.com/\x00", safehttp.Policy{}, safehttp.CodeURLInvalid},

		{"https://127.0.0.1/", safehttp.Policy{}, safehttp.CodeAddressNotAllowed},
		{"https://169.254.169.254/latest/meta-data", safehttp.Policy{}, safehttp.CodeAddressNotAllowed},
		{"https://[::1]:8443/", safehttp.Policy{}, safehttp.CodeAddressNotAllowed},
		{"https://[::ffff:10.0.0.1]/", safehttp.Policy{}, safehttp.CodeAddressNotAllowed},
		{"https://[fe80::1%25eth0]/", safehttp.Policy{}, safehttp.CodeAddressNotAllowed},
	}
	for _, c := range cases {
		u, err := safehttp.CheckURL(c.raw, c.p)
		if c.code == "" {
			if err != nil || u == nil {
				t.Errorf("CheckURL(%q) = %v, want ok", c.raw, err)
			}
			continue
		}
		var se *safehttp.Error
		if !errors.As(err, &se) || se.Code != c.code || u != nil {
			t.Errorf("CheckURL(%q) = %v (%q), want code %q", c.raw, err, code(err), c.code)
		}
	}
}

// A literal loopback URL is refused before dialing; a hostname that resolves
// to loopback passes the URL check and is refused when the socket connects.
func TestLoopbackBlockedAtDialTime(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	byName := "http://localhost:" + port + "/x"
	if addrs, err := net.LookupHost("localhost"); err != nil || len(addrs) == 0 {
		t.Skipf("localhost does not resolve here: %v", err)
	}

	blocked := safehttp.Policy{AllowHTTP: true}
	if _, err := safehttp.CheckURL(byName, blocked); err != nil {
		t.Fatalf("a hostname must pass the URL check: %v", err)
	}
	c := safehttp.NewClient(blocked)
	for _, u := range []string{srv.URL + "/x", byName} {
		if _, _, err := get(t, c, u, 1<<10); code(err) != safehttp.CodeAddressNotAllowed {
			t.Errorf("GET %s: err = %v (%q), want address_not_allowed", u, err, code(err))
		}
	}
	// The plain client API is guarded as well, not only Do.
	if resp, err := c.Get(byName); err == nil {
		resp.Body.Close()
		t.Error("client.Get to loopback must fail")
	} else if code(err) != safehttp.CodeAddressNotAllowed {
		t.Errorf("client.Get: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("server received %d requests through a blocking policy", n)
	}

	// Only the IPv4 range is allowed: the dialer skips ::1 and still connects.
	for _, cidrs := range [][]netip.Prefix{loopback, loopback[:1]} {
		c = safehttp.NewClient(safehttp.Policy{AllowHTTP: true, AllowCIDRs: cidrs})
		for _, u := range []string{srv.URL + "/x", byName} {
			body, _, err := get(t, c, u, 1<<10)
			if err != nil || string(body) != "ok" {
				t.Errorf("GET %s with %v: %q, %v", u, cidrs, body, err)
			}
		}
	}
	if n := hits.Load(); n != 4 {
		t.Fatalf("server received %d requests, want 4", n)
	}
}

func TestSchemeEnforcedByClient(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	c := safehttp.NewClient(safehttp.Policy{AllowCIDRs: loopback}) // https only
	if _, _, err := get(t, c, srv.URL, 10); code(err) != safehttp.CodeSchemeNotAllowed {
		t.Fatalf("err = %v, want scheme_not_allowed", err)
	}
	c = safehttp.NewClient(safehttp.Policy{AllowHTTP: true, AllowCIDRs: loopback})
	u, _ := url.Parse(srv.URL)
	u.User = url.UserPassword("alice", "hunter2-password")
	_, _, err := get(t, c, u.String(), 10)
	if code(err) != safehttp.CodeURLInvalid || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "alice") {
		t.Fatalf("userinfo: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("request was sent")
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	var targetHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			targetHits.Add(1)
			return
		}
		http.Redirect(w, r, "/target?from="+r.URL.Query().Get("code"), http.StatusFound)
	}))
	defer srv.Close()
	c := safehttp.NewClient(safehttp.Policy{AllowHTTP: true, AllowCIDRs: loopback})

	for _, status := range []int{301, 302, 303, 307, 308} {
		redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, srv.URL+"/target", status)
		}))
		_, hdr, err := get(t, c, redir.URL, 1<<10)
		redir.Close()
		var se *safehttp.Error
		if !errors.As(err, &se) || se.Code != safehttp.CodeRedirectNotFollowed || se.Status != status {
			t.Fatalf("status %d: err = %v", status, err)
		}
		if hdr.Get("Location") == "" {
			t.Fatalf("status %d: response header not returned", status)
		}
		if strings.Contains(err.Error(), "/target") {
			t.Fatalf("error exposes the Location header: %v", err)
		}
	}
	// The raw client hands the 3xx back as a response.
	resp, err := c.Get(srv.URL + "/start")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if n := targetHits.Load(); n != 0 {
		t.Fatalf("redirect target was requested %d times", n)
	}
}

func TestSizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chunked" {
			// No Content-Length: the cap must be enforced while reading.
			for i := 0; i < 10; i++ {
				_, _ = w.Write([]byte("x"))
				w.(http.Flusher).Flush()
			}
			return
		}
		_, _ = w.Write([]byte("0123456789"))
	}))
	defer srv.Close()
	c := safehttp.NewClient(safehttp.Policy{AllowHTTP: true, AllowCIDRs: loopback})
	for _, p := range []string{"/sized", "/chunked"} {
		for max, wantErr := range map[int64]bool{0: true, 9: true, 10: false, 11: false, 1 << 62: false, -5: true} {
			body, _, err := get(t, c, srv.URL+p, max)
			if wantErr {
				if code(err) != safehttp.CodeTooLarge || body != nil {
					t.Errorf("%s max=%d: body=%q err=%v, want too_large", p, max, body, err)
				}
			} else if err != nil || len(body) != 10 {
				t.Errorf("%s max=%d: body=%q err=%v", p, max, body, err)
			}
		}
	}
}

func TestStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/204":
			w.WriteHeader(http.StatusNoContent)
		case "/404":
			http.NotFound(w, r)
		case "/429":
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := safehttp.NewClient(safehttp.Policy{AllowHTTP: true, AllowCIDRs: loopback})
	if body, _, err := get(t, c, srv.URL+"/204", 10); err != nil || len(body) != 0 {
		t.Fatalf("204: %q %v", body, err)
	}
	for p, status := range map[string]int{"/404": 404, "/429": 429, "/500": 500} {
		body, hdr, err := get(t, c, srv.URL+p, 10)
		var se *safehttp.Error
		if !errors.As(err, &se) || se.Code != safehttp.CodeStatus || se.Status != status || body != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if status == 429 && hdr.Get("Retry-After") != "7" {
			t.Fatalf("header not returned with a status error: %v", hdr)
		}
	}
}

func TestTimeoutAndCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow-body" {
			w.Header().Set("Content-Length", "10")
			_, _ = w.Write([]byte("abc"))
			w.(http.Flusher).Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	c := safehttp.NewClient(safehttp.Policy{AllowHTTP: true, AllowCIDRs: loopback, Timeout: 150 * time.Millisecond})
	for _, p := range []string{"/slow-headers", "/slow-body"} {
		_, _, err := get(t, c, srv.URL+p, 100)
		if code(err) != safehttp.CodeTimeout || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s: err = %v (%q), want timeout", p, err, code(err))
		}
	}

	c = safehttp.NewClient(safehttp.Policy{AllowHTTP: true, AllowCIDRs: loopback})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/slow-headers", nil)
	_, _, err := safehttp.Do(c, req, 100)
	if code(err) != safehttp.CodeUnreachable || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: err = %v (%q)", err, code(err))
	}

	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/slow-headers", nil)
	if _, _, err = safehttp.Do(c, req, 100); code(err) != safehttp.CodeTimeout {
		t.Fatalf("context deadline: err = %v (%q)", err, code(err))
	}
}

func TestUnreachable(t *testing.T) {
	// A port that was just closed refuses connections.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	c := safehttp.NewClient(safehttp.Policy{AllowHTTP: true, AllowCIDRs: loopback})
	if _, _, err := get(t, c, "http://"+addr+"/", 10); code(err) != safehttp.CodeUnreachable {
		t.Fatalf("err = %v (%q), want unreachable", err, code(err))
	}
	// A certificate the system does not trust is a failed connection too.
	tlsSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("request must not reach a server with an untrusted certificate")
	}))
	tlsSrv.Config.ErrorLog = log.New(io.Discard, "", 0) // the failed handshake is expected
	tlsSrv.StartTLS()
	defer tlsSrv.Close()
	if _, _, err := get(t, c, tlsSrv.URL, 10); code(err) != safehttp.CodeUnreachable {
		t.Fatalf("untrusted TLS: err = %v (%q), want unreachable", err, code(err))
	}
}

// No failure mode may put the Authorization header, the path or the query
// into the error text.
func TestErrorsDoNotLeak(t *testing.T) {
	const secret = "s3cr3t-token-value"
	const pathSecret = "path-part-7781"
	const querySecret = "sig-9f3a77"
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.Header().Set("X-Echo", auth)
		switch r.URL.Query().Get("mode") {
		case "status":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("denied for " + auth))
		case "redirect":
			w.Header().Set("Location", "/elsewhere?token="+auth)
			w.WriteHeader(http.StatusFound)
		case "large":
			_, _ = w.Write([]byte(strings.Repeat(auth, 100)))
		case "slow":
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
	}))
	defer srv.Close()
	defer close(release)

	open := safehttp.NewClient(safehttp.Policy{AllowHTTP: true, AllowCIDRs: loopback, Timeout: 150 * time.Millisecond})
	closed := safehttp.NewClient(safehttp.Policy{AllowHTTP: true})
	httpsOnly := safehttp.NewClient(safehttp.Policy{AllowCIDRs: loopback})
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	cases := []struct {
		name   string
		client *http.Client
		url    string
		code   string
	}{
		{"status", open, srv.URL + "/" + pathSecret + "?mode=status&sig=" + querySecret, safehttp.CodeStatus},
		{"redirect", open, srv.URL + "/" + pathSecret + "?mode=redirect&sig=" + querySecret, safehttp.CodeRedirectNotFollowed},
		{"too large", open, srv.URL + "/" + pathSecret + "?mode=large&sig=" + querySecret, safehttp.CodeTooLarge},
		{"timeout", open, srv.URL + "/" + pathSecret + "?mode=slow&sig=" + querySecret, safehttp.CodeTimeout},
		{"blocked literal", closed, srv.URL + "/" + pathSecret + "?sig=" + querySecret, safehttp.CodeAddressNotAllowed},
		{"blocked at dial", closed, "http://localhost:" + port + "/" + pathSecret + "?sig=" + querySecret, safehttp.CodeAddressNotAllowed},
		{"scheme", httpsOnly, srv.URL + "/" + pathSecret + "?sig=" + querySecret, safehttp.CodeSchemeNotAllowed},
		{"unreachable", open, "http://127.0.0.1:1/" + pathSecret + "?sig=" + querySecret, safehttp.CodeUnreachable},
	}
	for _, c := range cases {
		req, err := http.NewRequest(http.MethodGet, c.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		_, _, err = safehttp.Do(c.client, req, 64)
		if code(err) != c.code {
			t.Errorf("%s: err = %v (%q), want %q", c.name, err, code(err), c.code)
			continue
		}
		for _, s := range []string{secret, "Bearer", pathSecret, querySecret, "denied", "elsewhere"} {
			if strings.Contains(err.Error(), s) {
				t.Errorf("%s: error %q contains %q", c.name, err.Error(), s)
			}
		}
		// Nothing may be reachable through the chain either.
		for e := errors.Unwrap(err); e != nil; e = errors.Unwrap(e) {
			if strings.Contains(e.Error(), secret) || strings.Contains(e.Error(), querySecret) {
				t.Errorf("%s: wrapped error %q leaks", c.name, e.Error())
			}
		}
	}
}
