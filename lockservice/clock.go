package lockservice

import (
	"sync"
	"time"
)

// Clock abstracts time so that lease expiry can be tested deterministically
// instead of by sleeping. Production code uses SystemClock; tests use
// ManualClock and step time forward explicitly.
type Clock interface {
	Now() time.Time
}

// SystemClock is the real clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

// ManualClock is a clock that only moves when the test tells it to.
type ManualClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewManualClock starts a manual clock at a fixed, arbitrary instant.
func NewManualClock() *ManualClock {
	return &ManualClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
