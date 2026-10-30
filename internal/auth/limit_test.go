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
