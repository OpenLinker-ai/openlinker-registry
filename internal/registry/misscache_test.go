package registry

import (
	"fmt"
	"testing"
	"time"
)

// Misses that have expired are dropped even when nobody asks for them
// again, and the cache never holds more than maxMisses.
func TestMissCacheIsBounded(t *testing.T) {
	var c missCache
	t0 := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	for i := range 100 {
		c.add(fmt.Sprint("a", i), t0)
	}
	if !c.recent("a1", t0.Add(missTTL/2)) {
		t.Fatal("a fresh miss must be remembered")
	}
	c.add("b", t0.Add(missTTL+time.Second))
	if len(c.m) != 1 {
		t.Fatalf("expired misses were kept: %d entries", len(c.m))
	}
	for i := range 3 * maxMisses {
		c.add(fmt.Sprint("c", i), t0.Add(2*missTTL))
	}
	if len(c.m) > maxMisses {
		t.Fatalf("the cache holds %d entries, more than %d", len(c.m), maxMisses)
	}
}
