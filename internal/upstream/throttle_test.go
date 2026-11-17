package upstream_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/safehttp"
	"github.com/OpenLinker-ai/openlinker-registry/internal/upstream"
)

// A sync walks a whole catalogue and will meet the upstream's rate limit.
// Under WithThrottleRetry a 429 is waited out and the request repeated;
// without it (an interactive read) the 429 is reported at once.
func TestThrottleRetry(t *testing.T) {
	var calls atomic.Int64
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeJSON(w, `{"skills":[{"namespace":"io.github.alice","name":"demo"}]}`)
	})
	c := newClient(f.URL)

	_, _, err := c.ListSkills(ctx, "", 0)
	if code, status := scode(err); code != safehttp.CodeStatus || status != http.StatusTooManyRequests || calls.Load() != 1 {
		t.Fatalf("interactive read must not wait: %v after %d calls", err, calls.Load())
	}

	calls.Store(0)
	items, _, err := c.ListSkills(upstream.WithThrottleRetry(ctx), "", 0)
	if err != nil || len(items) != 1 || calls.Load() != 3 {
		t.Fatalf("sync read: %+v %v after %d calls", items, err, calls.Load())
	}

	// Retry-After may be an HTTP date; it is waited for like a number of
	// seconds, not hammered at once per second until a retry count runs out.
	var dated atomic.Int64
	until := time.Now().Add(1500 * time.Millisecond)
	d := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		dated.Add(1)
		if time.Now().Before(until) {
			w.Header().Set("Retry-After", until.Add(time.Second).UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeJSON(w, `{"skills":[]}`)
	})
	if _, _, err := newClient(d.URL).ListSkills(upstream.WithThrottleRetry(ctx), "", 0); err != nil || dated.Load() != 2 {
		t.Fatalf("dated Retry-After: %v after %d calls, want one wait and two calls", err, dated.Load())
	}

	// The wait ends with the context: a throttled run does not outlive its
	// deadline.
	always := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	short, cancel := context.WithTimeout(upstream.WithThrottleRetry(ctx), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err = newClient(always.URL).ListSkills(short, "", 0)
	if code, status := scode(err); code != safehttp.CodeStatus || status != http.StatusTooManyRequests || time.Since(start) > 5*time.Second {
		t.Fatalf("throttled past the deadline: %v after %v", err, time.Since(start))
	}
}
