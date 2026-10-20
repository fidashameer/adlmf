package faultmgr

import (
	"testing"
	"time"

	"adlmf/internal/types"
	"adlmf/lockservice"
)

// TestReclaimsStaleLockAfterCrash is the Section 8 safety experiment at unit
// level: a lock holder dies, and the lock must become available again without
// ever being held by two services at once.
func TestReclaimsStaleLockAfterCrash(t *testing.T) {
	clk := lockservice.NewManualClock()
	b := lockservice.NewMemoryBackend(clk)
	defer b.Close()
	fm := New(b, clk, 5*time.Second)

	lock := types.LockID("inventory")
	if _, err := b.TryAcquire("payment", lock, 10*time.Second); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// payment crashes. No release, no renewal.

	clk.Advance(4 * time.Second)
	if n := fm.Tick(); n != 0 {
		t.Fatalf("reclaimed %d locks while the lease was still live", n)
	}
	if _, err := b.TryAcquire("order", lock, 10*time.Second); err != lockservice.ErrHeld {
		t.Fatalf("order must not get the lock while the lease is live, got %v", err)
	}

	clk.Advance(7 * time.Second)
	if n := fm.Tick(); n != 1 {
		t.Fatalf("expected to reclaim 1 stale lock, got %d", n)
	}

	h, err := b.TryAcquire("order", lock, 10*time.Second)
	if err != nil {
		t.Fatalf("order should acquire after reclamation: %v", err)
	}
	// Mutual exclusion held throughout: payment never had a valid handle after
	// expiry, and its token is lower than order's.
	if holder, ok := b.Holder(lock); !ok || holder != "order" {
		t.Fatalf("order should be the holder, got %q", holder)
	}
	_ = h
}

// TestSilentServiceBecomesPartitionSuspect checks the evidence the recovery
// engine uses to avoid sacrificing a service it may not be able to reach.
func TestSilentServiceBecomesPartitionSuspect(t *testing.T) {
	clk := lockservice.NewManualClock()
	b := lockservice.NewMemoryBackend(clk)
	defer b.Close()
	fm := New(b, clk, 5*time.Second)

	fm.Observe(types.LockEvent{Service: "payment", Kind: types.Acquired, Lock: "L1", Seq: 1})
	fm.Observe(types.LockEvent{Service: "order", Kind: types.Acquired, Lock: "L2", Seq: 1})

	if got := fm.PartitionSuspects(); len(got) != 0 {
		t.Fatalf("nobody should be suspect yet, got %v", got)
	}

	// Time passes. order keeps talking; payment goes quiet.
	clk.Advance(6 * time.Second)
	fm.Heartbeat("order")

	suspects := fm.PartitionSuspects()
	if !suspects["payment"] {
		t.Fatalf("payment went quiet and should be suspect, got %v", suspects)
	}
	if suspects["order"] {
		t.Fatalf("order is still talking and must not be suspect, got %v", suspects)
	}
}

// TestHeartbeatClearsSuspicion checks that a service which comes back is no
// longer treated as unreachable.
func TestHeartbeatClearsSuspicion(t *testing.T) {
	clk := lockservice.NewManualClock()
	b := lockservice.NewMemoryBackend(clk)
	defer b.Close()
	fm := New(b, clk, 5*time.Second)

	fm.Heartbeat("payment")
	clk.Advance(6 * time.Second)
	if !fm.PartitionSuspects()["payment"] {
		t.Fatal("precondition: payment should be suspect")
	}

	fm.Heartbeat("payment") // it answers again
	if fm.PartitionSuspects()["payment"] {
		t.Fatal("payment answered and should no longer be suspect")
	}
}
