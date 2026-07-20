package output

import (
	"context"
	"strings"
	"sync"
	"time"
)

type Throttler struct {
	mu     sync.Mutex
	nextAt map[string]time.Time
}

func NewThrottler() *Throttler {
	return &Throttler{nextAt: make(map[string]time.Time)}
}

func (t *Throttler) Wait(
	ctx context.Context,
	receiverID string,
	interval time.Duration,
	now func() time.Time,
) error {
	if t == nil || interval <= 0 {
		return nil
	}
	key := strings.TrimSpace(receiverID)
	if key == "" {
		return nil
	}
	for {
		delay := t.reserveDelay(key, interval, now)
		if delay <= 0 {
			return nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (t *Throttler) reserveDelay(
	key string,
	interval time.Duration,
	now func() time.Time,
) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	current := now()
	next := t.nextAt[key]
	if next.IsZero() || !current.Before(next) {
		t.nextAt[key] = current.Add(interval)
		return 0
	}
	return next.Sub(current)
}

func firstThrottler(values []*Throttler) *Throttler {
	if len(values) == 0 {
		return nil
	}
	return values[0]
}
