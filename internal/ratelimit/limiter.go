// Package ratelimit provides a fixed-rate pacer for outbound API requests.
package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Limiter is a fixed-rate pacer. It enforces one request per interval
// (60s / ratePerMin), serializing concurrent callers. It is deliberately
// not a token bucket: there is no burst capacity.
type Limiter struct {
	mu       sync.Mutex
	interval time.Duration
	last     time.Time
}

// New creates a Limiter that permits ratePerMin requests per minute.
func New(ratePerMin int) *Limiter {
	return &Limiter{
		interval: time.Minute / time.Duration(ratePerMin),
	}
}

// Wait blocks until the caller's request slot is available, then returns nil.
// Returns ctx.Err() if the context is cancelled while waiting.
func (l *Limiter) Wait(ctx context.Context) error {
	l.mu.Lock()
	now := time.Now()
	next := l.last.Add(l.interval)
	var wait time.Duration
	if next.After(now) {
		wait = next.Sub(now)
		l.last = next
	} else {
		l.last = now
	}
	l.mu.Unlock()

	if wait <= 0 {
		return nil
	}
	select {
	case <-time.After(wait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
