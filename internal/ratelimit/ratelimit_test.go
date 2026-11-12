package ratelimit

import (
	"net/http"
	"net/netip"
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := New(60)
	l.now = func() time.Time { return now }
	for i := range 60 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("event %d within burst rejected", i)
		}
	}
	ok, wait := l.Allow("a")
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("over limit: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("keys must be independent")
	}
	now = now.Add(time.Second)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("one token should refill per second at 60/min")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("only one token refilled")
	}
	now = now.Add(time.Hour)
	for i := range 60 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("burst is capped at the per-minute rate, event %d", i)
		}
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("burst must not exceed the per-minute rate")
	}
}

func TestNilLimiterAllows(t *testing.T) {
	var l *Limiter
	if ok, _ := l.Allow("x"); !ok || New(0) != nil || New(-1) != nil {
		t.Fatal("a non-positive rate disables limiting")
	}
}

func TestPrune(t *testing.T) {
	now := time.Unix(1000, 0)
	l := New(1)
	l.now = func() time.Time { return now }
	l.Allow("busy")
	now = now.Add(2 * time.Minute)
	l.Allow("fresh") // fresh has 0 tokens now, busy has refilled
	l.prune(now)
	if _, ok := l.buckets["busy"]; ok {
		t.Fatal("refilled bucket should be pruned")
	}
	if _, ok := l.buckets["fresh"]; !ok {
		t.Fatal("bucket in use must survive pruning")
	}
}

func TestClientAddr(t *testing.T) {
	trusted, err := ParsePrefixes("10.0.0.0/8, 192.168.1.1, ::1")
	if err != nil {
		t.Fatal(err)
	}
	c := ClientAddr{Trusted: trusted}
	cases := []struct {
		remote string
		xff    []string
		want   string
	}{
		{"203.0.113.9:1234", nil, "203.0.113.9"},
		// An untrusted peer cannot choose its address.
		{"203.0.113.9:1234", []string{"1.2.3.4"}, "203.0.113.9"},
		{"10.0.0.1:80", []string{"198.51.100.7"}, "198.51.100.7"},
		// The right-most untrusted hop wins; anything left of it is client-supplied.
		{"10.0.0.1:80", []string{"1.1.1.1, 198.51.100.7, 10.0.0.2"}, "198.51.100.7"},
		{"10.0.0.1:80", []string{"1.1.1.1", "198.51.100.7"}, "198.51.100.7"},
		{"10.0.0.1:80", []string{"10.0.0.3, 192.168.1.1"}, "10.0.0.1"},
		{"10.0.0.1:80", []string{"garbage"}, "10.0.0.1"},
		{"10.0.0.1:80", []string{"198.51.100.7, garbage"}, "10.0.0.1"},
		{"[::1]:80", []string{"2001:db8::5"}, "2001:db8::5"},
		{"[::ffff:10.0.0.1]:80", []string{"198.51.100.7"}, "198.51.100.7"},
	}
	for _, tc := range cases {
		r := &http.Request{RemoteAddr: tc.remote, Header: http.Header{}}
		for _, v := range tc.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := c.From(r).String(); got != tc.want {
			t.Errorf("remote %s xff %v: got %s, want %s", tc.remote, tc.xff, got, tc.want)
		}
	}
	if a := (ClientAddr{}).From(&http.Request{RemoteAddr: "not-an-address"}); a.IsValid() {
		t.Fatal("unparseable peer must yield the zero address")
	}
}

func TestKey(t *testing.T) {
	if got := Key(netip.MustParseAddr("203.0.113.9")); got != "ip:203.0.113.9" {
		t.Fatal(got)
	}
	a, b := Key(netip.MustParseAddr("2001:db8:1:2::1")), Key(netip.MustParseAddr("2001:db8:1:2:ffff::9"))
	if a != b || a != "ip:2001:db8:1:2::/64" {
		t.Fatalf("IPv6 clients share their /64: %s %s", a, b)
	}
	if Key(netip.Addr{}) != "ip:unknown" {
		t.Fatal("invalid address key")
	}
	if _, err := ParsePrefixes("10.0.0.0/8, nope"); err == nil {
		t.Fatal("bad prefix accepted")
	}
}
