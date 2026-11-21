package providers

import (
	"context"
	"sync"
	"time"
)

// Limiter spaces calls at least `interval` apart (a serialising limiter:
// MusicBrainz asks for an average of one request per second per IP, so no
// bursts). Safe for concurrent use; waiters are served in lock order.
type Limiter struct {
	interval time.Duration
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error

	mu   sync.Mutex
	next time.Time
}

func NewLimiter(interval time.Duration) *Limiter {
	return &Limiter{interval: interval, now: time.Now, sleep: sleepCtx}
}

// SetClock replaces the clock and sleep function (tests).
func (l *Limiter) SetClock(now func() time.Time, sleep func(context.Context, time.Duration) error) {
	l.now, l.sleep = now, sleep
}

// Wait blocks until the caller may issue a request, or ctx ends.
func (l *Limiter) Wait(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	now := l.now()
	if d := l.next.Sub(now); d > 0 {
		if err := l.sleep(ctx, d); err != nil {
			return err
		}
		now = l.next
	}
	l.next = now.Add(l.interval)
	return nil
}

// Penalize pushes the next allowed call out to at least now+d (after a 503).
func (l *Limiter) Penalize(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t := l.now().Add(d); t.After(l.next) {
		l.next = t
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
