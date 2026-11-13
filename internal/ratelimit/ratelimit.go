// Package ratelimit implements per-client token buckets and client address
// resolution behind trusted reverse proxies. State is in-process: each
// registry replica enforces its own limits.
package ratelimit

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// maxBuckets bounds memory; when reached, idle (full) buckets are dropped.
const maxBuckets = 100000

// Limiter allows perMinute events per key per minute, with a burst of the
// same size. A nil *Limiter allows everything.
type Limiter struct {
	perMinute float64
	now       func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New returns a limiter, or nil (unlimited) when perMinute is not positive.
func New(perMinute int) *Limiter {
	if perMinute <= 0 {
		return nil
	}
	return &Limiter{perMinute: float64(perMinute), now: time.Now, buckets: map[string]*bucket{}}
}

// Allow consumes one event for key. When the key is over its limit it
// returns false and how long until the next event is allowed.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= maxBuckets {
			l.prune(now)
		}
		b = &bucket{tokens: l.perMinute, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.perMinute, b.tokens+now.Sub(b.last).Minutes()*l.perMinute)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.perMinute * float64(time.Minute))
	return false, wait
}

// Exhausted reports whether key is over its limit without consuming an
// event, and how long until it is not. It lets a caller count only failures
// (Allow on failure) yet refuse work once too many have happened.
func (l *Limiter) Exhausted(key string) (bool, time.Duration) {
	if l == nil {
		return false, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		return false, 0
	}
	tokens := min(l.perMinute, b.tokens+now.Sub(b.last).Minutes()*l.perMinute)
	if tokens >= 1 {
		return false, 0
	}
	return true, time.Duration((1 - tokens) / l.perMinute * float64(time.Minute))
}

// prune drops buckets that have refilled completely. If every bucket is
// still in use the map is reset: forgetting limits briefly is preferable to
// unbounded growth.
func (l *Limiter) prune(now time.Time) {
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Minutes()*l.perMinute >= l.perMinute {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) >= maxBuckets {
		l.buckets = map[string]*bucket{}
	}
}

// ClientAddr resolves the client address of a request. X-Forwarded-For is
// honored only when the direct peer is a trusted proxy; the client is then
// the right-most address that is not itself a trusted proxy, which a client
// cannot forge.
type ClientAddr struct {
	Trusted []netip.Prefix
}

func (c ClientAddr) trusted(a netip.Addr) bool {
	for _, p := range c.Trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// From returns the client address ("" when the peer address is unusable).
func (c ClientAddr) From(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	peer = peer.Unmap()
	if !c.trusted(peer) {
		return peer
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		a = a.Unmap()
		if !c.trusted(a) {
			return a
		}
	}
	return peer
}

// Key returns the limiter key for an anonymous client: the IPv4 address or
// the IPv6 /64, since one host commonly controls a whole /64.
func Key(a netip.Addr) string {
	if !a.IsValid() {
		return "ip:unknown"
	}
	if a.Is6() {
		if p, err := a.Prefix(64); err == nil {
			return "ip:" + p.String()
		}
	}
	return "ip:" + a.String()
}

// ParsePrefixes parses a comma-separated list of CIDR prefixes or single
// addresses.
func ParsePrefixes(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if p, err := netip.ParsePrefix(item); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(item)
		if err != nil {
			return nil, err
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}
