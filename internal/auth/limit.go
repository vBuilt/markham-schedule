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

// live returns key's hits still inside the window ending at t.
func (l *Limiter) live(key string, t time.Time) []time.Time {
	cutoff := t.Add(-l.window)
	kept := l.hits[key][:0]
	for _, h := range l.hits[key] {
		if h.After(cutoff) {
			kept = append(kept, h)
		}
	}
	return kept
}

// Allow reports whether key is within the limit, recording a hit if so.
// Refused requests are not recorded, so a key never holds more than max hits.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	kept := l.live(key, t)
	if len(kept) >= l.max {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, t)
	return true
}

// Reset forgets all hits for key.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}

// Prune forgets keys with no hits inside the window, bounding memory.
func (l *Limiter) Prune() {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	for key := range l.hits {
		if kept := l.live(key, t); len(kept) == 0 {
			delete(l.hits, key)
		} else {
			l.hits[key] = kept
		}
	}
}
