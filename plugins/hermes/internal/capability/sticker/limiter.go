package sticker

import (
	"context"
	"sync"
	"time"
)

// minuteLimiter is a small token bucket. It starts full, has a one-minute
// burst capacity, and replenishes continuously.
type minuteLimiter struct {
	mu       sync.Mutex
	capacity float64
	tokens   float64
	last     time.Time
	now      func() time.Time
}

func newMinuteLimiter(perMinute int) *minuteLimiter {
	if perMinute <= 0 {
		perMinute = 60
	}
	now := time.Now()
	return &minuteLimiter{
		capacity: float64(perMinute),
		tokens:   float64(perMinute),
		last:     now,
		now:      time.Now,
	}
}

func (l *minuteLimiter) acquire(ctx context.Context) error {
	for {
		l.mu.Lock()
		now := l.now()
		elapsed := now.Sub(l.last).Seconds()
		if elapsed > 0 {
			l.tokens = min(l.capacity, l.tokens+elapsed*l.capacity/60)
			l.last = now
		}
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		seconds := (1 - l.tokens) * 60 / l.capacity
		l.mu.Unlock()
		timer := time.NewTimer(time.Duration(seconds*float64(time.Second)) + time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}
