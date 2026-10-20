package lockservice

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"adlmf/internal/types"
)

// TestMutualExclusion is the property the whole project rests on: no two
// services may hold the same lock at the same time. Section 8 of the report
// treats any violation here as a correctness bug rather than a tunable, so
// this test is deliberately aggressive.
func TestMutualExclusion(t *testing.T) {
	b := NewMemoryBackend(SystemClock{})
	defer b.Close()

	const (
		services   = 16
		iterations = 400
		lock       = types.LockID("shared/resource")
	)

	var inside int32 // how many goroutines believe they hold the lock
	var violations int32
	var wg sync.WaitGroup

	for i := 0; i < services; i++ {
		svc := types.ServiceID(string(rune('a' + i)))
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < iterations; n++ {
				h, err := b.TryAcquire(svc, lock, time.Minute)
				if err != nil {
					continue // held by someone else; try again
				}
				// Critical section.
				if atomic.AddInt32(&inside, 1) != 1 {
					atomic.AddInt32(&violations, 1)
				}
				atomic.AddInt32(&inside, -1)
				if err := b.Release(h); err != nil {
					t.Errorf("release failed: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if violations != 0 {
		t.Fatalf("mutual exclusion violated %d times", violations)
	}
}

// TestLeaseExpiryReclaimsLock covers the crash case: a service takes a lock
// and dies without releasing it. Nobody should be stuck forever.
func TestLeaseExpiryReclaimsLock(t *testing.T) {
	clk := NewManualClock()
	b := NewMemoryBackend(clk)
	defer b.Close()

	lock := types.LockID("inventory")

	if _, err := b.TryAcquire("payment", lock, 10*time.Second); err != nil {
		t.Fatalf("payment should have acquired: %v", err)
	}
	// "payment" now crashes: it never releases and never renews.

	if _, err := b.TryAcquire("order", lock, 10*time.Second); err != ErrHeld {
		t.Fatalf("order should be blocked while the lease is live, got %v", err)
	}

	// Before expiry, reaping must do nothing.
	clk.Advance(9 * time.Second)
	if n := b.ReapExpired(); n != 0 {
		t.Fatalf("reaped %d locks before the lease expired", n)
	}

	// After expiry the fault manager reclaims it.
	clk.Advance(2 * time.Second)
	if n := b.ReapExpired(); n != 1 {
		t.Fatalf("expected to reclaim 1 lock, reclaimed %d", n)
	}

	if _, err := b.TryAcquire("order", lock, 10*time.Second); err != nil {
		t.Fatalf("order should acquire after reclamation: %v", err)
	}
}

// TestFencingTokensStrictlyIncrease is the Kleppmann safety property. A
// service whose lease lapsed must not be able to pass itself off as the
// current holder, and the resource must be able to tell the difference.
func TestFencingTokensStrictlyIncrease(t *testing.T) {
	clk := NewManualClock()
	b := NewMemoryBackend(clk)
	defer b.Close()

	lock := types.LockID("config")

	stale, err := b.TryAcquire("payment", lock, 5*time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// payment pauses (GC pause, VM freeze, whatever) past its lease.
	clk.Advance(6 * time.Second)
	b.ReapExpired()

	fresh, err := b.TryAcquire("order", lock, 5*time.Second)
	if err != nil {
		t.Fatalf("order should acquire the lapsed lock: %v", err)
	}

	if !(fresh.Token > stale.Token) {
		t.Fatalf("fencing token did not increase: stale=%d fresh=%d", stale.Token, fresh.Token)
	}

	// payment wakes up and tries to carry on as though nothing happened.
	if err := b.Release(stale); err != ErrNotHolder {
		t.Fatalf("stale holder should not be able to release, got %v", err)
	}
	if _, err := b.Renew(stale, 5*time.Second); err != ErrNotHolder {
		t.Fatalf("stale holder should not be able to renew, got %v", err)
	}
}

// TestRenewKeepsLockAndRejectsLapsedLease checks that a well-behaved holder
// keeps its lock, and a lapsed one cannot renew its way back.
func TestRenewKeepsLockAndRejectsLapsedLease(t *testing.T) {
	clk := NewManualClock()
	b := NewMemoryBackend(clk)
	defer b.Close()

	lock := types.LockID("rate-limiter")
	h, err := b.TryAcquire("payment", lock, 10*time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Renew twice, well inside the lease each time.
	for i := 0; i < 2; i++ {
		clk.Advance(5 * time.Second)
		h, err = b.Renew(h, 10*time.Second)
		if err != nil {
			t.Fatalf("renew %d: %v", i, err)
		}
	}
	if holder, ok := b.Holder(lock); !ok || holder != "payment" {
		t.Fatalf("payment should still hold the lock, got holder=%q ok=%v", holder, ok)
	}

	// Now let it lapse and try to renew.
	clk.Advance(11 * time.Second)
	if _, err := b.Renew(h, 10*time.Second); err != ErrLeaseExpired {
		t.Fatalf("expected ErrLeaseExpired renewing a lapsed lease, got %v", err)
	}
}

// TestWaitingEventsFeedTheGraph checks that blocking on a lock is observable.
// The dependency monitor is built entirely out of these events, so if they are
// not emitted there is no wait-for graph to reason about.
func TestWaitingEventsFeedTheGraph(t *testing.T) {
	b := NewMemoryBackend(SystemClock{})
	defer b.Close()
	sub := b.Subscribe()

	lock := types.LockID("L1")
	if _, err := b.TryAcquire("payment", lock, time.Minute); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := b.TryAcquire("order", lock, time.Minute); err != ErrHeld {
		t.Fatalf("expected ErrHeld, got %v", err)
	}

	got := drain(sub, 2)
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d: %v", len(got), got)
	}
	if got[0].Kind != types.Acquired || got[0].Service != "payment" {
		t.Fatalf("first event should be payment ACQUIRED, got %v", got[0])
	}
	if got[1].Kind != types.Waiting || got[1].Service != "order" {
		t.Fatalf("second event should be order WAITING, got %v", got[1])
	}

	if w := b.Waiters(lock); len(w) != 1 || w[0] != "order" {
		t.Fatalf("expected order to be recorded as a waiter, got %v", w)
	}
}

// TestPerServiceSequenceIsMonotonic checks the ordering guarantee the monitor
// is allowed to rely on: within one service, sequence numbers increase.
func TestPerServiceSequenceIsMonotonic(t *testing.T) {
	b := NewMemoryBackend(SystemClock{})
	defer b.Close()
	sub := b.Subscribe()

	for i := 0; i < 5; i++ {
		h, err := b.TryAcquire("payment", types.LockID("L"), time.Minute)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		if err := b.Release(h); err != nil {
			t.Fatalf("release %d: %v", i, err)
		}
	}

	events := drain(sub, 10)
	var last uint64
	for _, ev := range events {
		if ev.Service != "payment" {
			continue
		}
		if ev.Seq <= last {
			t.Fatalf("sequence went backwards: %d after %d", ev.Seq, last)
		}
		last = ev.Seq
	}
	if last == 0 {
		t.Fatal("no events observed")
	}
}

// drain pulls up to n events that are already buffered on the channel.
func drain(ch <-chan types.LockEvent, n int) []types.LockEvent {
	var out []types.LockEvent
	for i := 0; i < n; i++ {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-time.After(200 * time.Millisecond):
			return out
		}
	}
	return out
}
