package auth

import (
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewLimiter(3, time.Minute, func() time.Time { return now })
	for i := 0; i < 3; i++ {
		if !l.Allow("a") {
			t.Fatalf("hit %d denied", i+1)
		}
	}
	if l.Allow("a") {
		t.Error("4th allowed")
	}
	if !l.Allow("b") {
		t.Error("other key denied")
	}
	now = now.Add(61 * time.Second)
	if !l.Allow("a") {
		t.Error("not allowed after window")
	}
	for i := 0; i < 5; i++ {
		l.Allow("a")
	}
	l.Reset("a")
	if !l.Allow("a") {
		t.Error("denied after reset")
	}
}

func TestLimiterRefusedHitsAreNotRecorded(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewLimiter(3, time.Minute, func() time.Time { return now })
	for i := 0; i < 1000; i++ {
		l.Allow("a")
	}
	if n := len(l.hits["a"]); n != 3 {
		t.Fatalf("stored hits = %d, want 3", n)
	}
	// Hammering must not extend the lockout past the first window.
	now = now.Add(61 * time.Second)
	if !l.Allow("a") {
		t.Error("denied after window despite refused hits")
	}
}

func TestLimiterPrune(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewLimiter(3, time.Minute, func() time.Time { return now })
	l.Allow("stale")
	now = now.Add(45 * time.Second)
	l.Allow("live")
	now = now.Add(30 * time.Second) // stale's hit is 75s old, live's 30s
	l.Prune()
	if _, ok := l.hits["stale"]; ok {
		t.Error("stale key kept")
	}
	if n := len(l.hits["live"]); n != 1 {
		t.Errorf("live key hits = %d, want 1", n)
	}
}
