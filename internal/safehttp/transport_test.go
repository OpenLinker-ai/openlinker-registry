package safehttp

import (
	"crypto/tls"
	"net/http"
	"testing"
	"time"
)

// Proxy use cannot be observed against a loopback test server (Go never
// proxies loopback), so the transport settings are asserted directly.
func TestTransportSettings(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	c := NewClient(Policy{})
	g, ok := c.Transport.(*guard)
	if !ok {
		t.Fatalf("transport is %T, want *guard", c.Transport)
	}
	tr, ok := g.base.(*http.Transport)
	if !ok {
		t.Fatalf("base transport is %T", g.base)
	}
	if tr.Proxy != nil {
		t.Error("the transport must not use a proxy")
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion < tls.VersionTLS12 || tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("TLS must verify certificates and require TLS 1.2 or later")
	}
	if tr.ForceAttemptHTTP2 {
		t.Error("HTTP/2 must stay off")
	}
	if c.Timeout != defaultTimeout || tr.TLSHandshakeTimeout != defaultDialTimeout {
		t.Errorf("default timeouts not applied: %v %v", c.Timeout, tr.TLSHandshakeTimeout)
	}
	if tr.ResponseHeaderTimeout <= 0 || tr.IdleConnTimeout <= 0 || tr.MaxIdleConns <= 0 || tr.MaxIdleConnsPerHost <= 0 || tr.MaxResponseHeaderBytes <= 0 {
		t.Error("header, idle and connection limits must be set")
	}
	if c.CheckRedirect == nil {
		t.Error("redirects must be disabled")
	}

	c = NewClient(Policy{Timeout: 2 * time.Second, DialTimeout: time.Second})
	tr = c.Transport.(*guard).base.(*http.Transport)
	if c.Timeout != 2*time.Second || tr.TLSHandshakeTimeout != time.Second || tr.ResponseHeaderTimeout != 2*time.Second {
		t.Errorf("configured timeouts not applied: %v %v %v", c.Timeout, tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout)
	}
}

// The dial check accepts only TCP to an allowed literal address.
func TestControl(t *testing.T) {
	var p Policy
	cases := []struct {
		network, address string
		ok               bool
	}{
		{"tcp4", "93.184.216.34:443", true},
		{"tcp6", "[2606:4700:4700::1111]:443", true},
		{"tcp4", "127.0.0.1:443", false},
		{"tcp6", "[::1]:443", false},
		{"tcp6", "[::ffff:169.254.169.254]:80", false},
		{"tcp6", "[fe80::1%lo0]:443", false},
		{"udp4", "93.184.216.34:443", false},
		{"unix", "/var/run/docker.sock", false},
		{"tcp4", "example.com:443", false},
		{"tcp4", "", false},
	}
	for _, c := range cases {
		err := p.control(c.network, c.address, nil)
		if (err == nil) != c.ok {
			t.Errorf("control(%s, %s) = %v, want ok=%v", c.network, c.address, err, c.ok)
		}
		if err != nil {
			if e, isErr := err.(*Error); !isErr || e.Code != CodeAddressNotAllowed {
				t.Errorf("control(%s, %s) returned %T %v", c.network, c.address, err, err)
			}
		}
	}
}
