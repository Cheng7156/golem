package video

import (
	"sync"
	"time"
)

type minuteLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Time
	used   int
}

func newMinuteLimiter(limit int) *minuteLimiter {
	return &minuteLimiter{limit: limit}
}

func (l *minuteLimiter) Allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.window.IsZero() || now.Sub(l.window) >= time.Minute {
		l.window = now
		l.used = 0
	}
	if l.used >= l.limit {
		return false
	}
	l.used++
	return true
}
