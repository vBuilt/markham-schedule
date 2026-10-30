package auth

import (
	"sync"
	"time"
)

// Limiter is an in-memory sliding-window rate limiter keyed by string.
type Limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	now    func() time.Time
	hits   map[string][]time.Time
}

// NewLimiter allows max hits per key within window. A nil now uses time.Now.
func NewLimiter(max int, window time.Duration, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{max: max, window: window, now: now, hits: map[string][]time.Time{}}
}

// Allow records a hit for key and reports whether it is within the limit.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	cutoff := t.Add(-l.window)
	kept := l.hits[key][:0]
	for _, h := range l.hits[key] {
		if h.After(cutoff) {
			kept = append(kept, h)
		}
	}
	kept = append(kept, t)
	l.hits[key] = kept
	return len(kept) <= l.max
}

// Reset forgets all hits for key.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}
