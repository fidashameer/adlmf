package depmonitor

import (
	"testing"

	"adlmf/internal/types"
)

// ev is a terse helper for building event streams in tests.
func ev(kind types.EventKind, svc types.ServiceID, lock types.LockID, seq uint64) types.LockEvent {
	return types.LockEvent{Kind: kind, Service: svc, Lock: lock, Seq: seq}
}

// --------------------------------------------------------------------------
// Wait-for graph (reactive detection — cited infrastructure, not a claim)
// --------------------------------------------------------------------------

// TestDetectsThreeServiceCycle reproduces the exact scenario used throughout
// the report: Payment holds L1 wants L2, Inventory holds L2 wants L3, Order
// holds L3 wants L1.
func TestDetectsThreeServiceCycle(t *testing.T) {
	g := NewWaitForGraph()
	stream := []types.LockEvent{
		ev(types.Acquired, "payment", "L1", 1),
		ev(types.Acquired, "inventory", "L2", 1),
		ev(types.Acquired, "order", "L3", 1),
		ev(types.Waiting, "payment", "L2", 2),
		ev(types.Waiting, "inventory", "L3", 2),
		ev(types.Waiting, "order", "L1", 2),
	}
	for _, e := range stream {
		g.Apply(e)
	}

	cycles := g.Cycles()
	if len(cycles) != 1 {
		t.Fatalf("expected exactly 1 deadlock cycle, got %d: %v", len(cycles), cycles)
	}
	if len(cycles[0]) != 3 {
		t.Fatalf("expected all 3 services in the cycle, got %v", cycles[0])
	}

	// The engine needs to know which lock to break for a given edge.
	if lock, ok := g.BlockedOn("payment", "inventory"); !ok || lock != "L2" {
		t.Fatalf("payment should be blocked on inventory via L2, got %q ok=%v", lock, ok)
	}
}

// TestNoCycleWhenChainIsAcyclic guards against the detector inventing
// deadlocks. A chain of waiters is not a deadlock.
func TestNoCycleWhenChainIsAcyclic(t *testing.T) {
	g := NewWaitForGraph()
	for _, e := range []types.LockEvent{
		ev(types.Acquired, "a", "L1", 1),
		ev(types.Acquired, "b", "L2", 1),
		ev(types.Waiting, "b", "L1", 2), // b -> a
		ev(types.Waiting, "c", "L2", 1), // c -> b
	} {
		g.Apply(e)
	}
	if cycles := g.Cycles(); len(cycles) != 0 {
		t.Fatalf("a waiting chain is not a deadlock, got %v", cycles)
	}
}

// TestReleaseBreaksCycle checks that the graph is genuinely live: once the
// lock is released the deadlock is gone.
func TestReleaseBreaksCycle(t *testing.T) {
	g := NewWaitForGraph()
	for _, e := range []types.LockEvent{
		ev(types.Acquired, "a", "L1", 1),
		ev(types.Acquired, "b", "L2", 1),
		ev(types.Waiting, "a", "L2", 2),
		ev(types.Waiting, "b", "L1", 2),
	} {
		g.Apply(e)
	}
	if len(g.Cycles()) != 1 {
		t.Fatal("expected a 2-service deadlock")
	}

	g.Apply(ev(types.Released, "a", "L1", 3))
	if cycles := g.Cycles(); len(cycles) != 0 {
		t.Fatalf("releasing L1 should break the cycle, got %v", cycles)
	}
}

// TestLateReleaseDoesNotEraseCurrentHolder is a partial-information case. A
// Released event from a previous holder can arrive after the next holder has
// already acquired. Applying it naively would blank out the holder and hide a
// real deadlock.
func TestLateReleaseDoesNotEraseCurrentHolder(t *testing.T) {
	g := NewWaitForGraph()
	g.Apply(ev(types.Acquired, "a", "L1", 1))
	g.Apply(ev(types.Released, "a", "L1", 2))
	g.Apply(ev(types.Acquired, "b", "L1", 1))
	// Straggler from a's earlier tenure, arriving out of order.
	g.Apply(ev(types.Released, "a", "L1", 2))

	if h, ok := g.Holder("L1"); !ok || h != "b" {
		t.Fatalf("b should still hold L1, got %q ok=%v", h, ok)
	}
}

// --------------------------------------------------------------------------
// Lock-order graph (Pillar 1 — the predictive claim)
// --------------------------------------------------------------------------

// TestPillar1FlagsViolationBeforeAnyDeadlock is the central claim of Pillar 1.
// Two services use the same two locks in opposite orders, but never at the
// same time, so no deadlock ever forms and the wait-for graph stays empty.
// The ordering violation is still reported.
func TestPillar1FlagsViolationBeforeAnyDeadlock(t *testing.T) {
	lo := NewLockOrderGraph()
	wf := NewWaitForGraph()

	stream := []types.LockEvent{
		// payment: L1 then L2, then lets both go.
		ev(types.Acquired, "payment", "L1", 1),
		ev(types.Acquired, "payment", "L2", 2),
		ev(types.Released, "payment", "L2", 3),
		ev(types.Released, "payment", "L1", 4),
		// order: the opposite order, safely after payment finished.
		ev(types.Acquired, "order", "L2", 1),
		ev(types.Acquired, "order", "L1", 2),
	}
	for _, e := range stream {
		lo.Observe(e)
		wf.Apply(e)
	}

	// Nobody is blocked. A reactive detector has nothing to report.
	if cycles := wf.Cycles(); len(cycles) != 0 {
		t.Fatalf("no deadlock should exist, got %v", cycles)
	}

	// First interval: candidate raised but deliberately not reported yet.
	if flags := lo.Tick(); len(flags) != 0 {
		t.Fatalf("flag should not be confirmed in its first interval, got %v", flags)
	}
	// Second interval: evidence survived, so report it.
	flags := lo.Tick()
	if len(flags) != 1 {
		t.Fatalf("expected 1 confirmed flag, got %d: %v", len(flags), flags)
	}
	f := flags[0]
	if f.LockA != "L1" || f.LockB != "L2" {
		t.Fatalf("flag should name L1 and L2, got %v", f)
	}
	if !f.Confirmed {
		t.Fatal("flag should be marked confirmed")
	}
}

// TestPillar1IgnoresConsistentNesting is the main false-positive guard. One
// service that always takes L1 before L2 is well behaved no matter how often
// it does it, and so are two services that agree on the order.
func TestPillar1IgnoresConsistentNesting(t *testing.T) {
	lo := NewLockOrderGraph()
	for _, e := range []types.LockEvent{
		ev(types.Acquired, "payment", "L1", 1),
		ev(types.Acquired, "payment", "L2", 2),
		ev(types.Released, "payment", "L2", 3),
		ev(types.Released, "payment", "L1", 4),
		// same service, same order, again
		ev(types.Acquired, "payment", "L1", 5),
		ev(types.Acquired, "payment", "L2", 6),
		ev(types.Released, "payment", "L2", 7),
		ev(types.Released, "payment", "L1", 8),
		// a different service agreeing on the order
		ev(types.Acquired, "order", "L1", 1),
		ev(types.Acquired, "order", "L2", 2),
	} {
		lo.Observe(e)
	}
	lo.Tick()
	if flags := lo.Tick(); len(flags) != 0 {
		t.Fatalf("consistent ordering must not be flagged, got %v", flags)
	}
}

// TestPillar1SurvivesOutOfOrderObservation checks the reorder buffer. The two
// Acquired events for one service arrive the wrong way round. Taken at face
// value they look like the reverse ordering and would produce a false alarm.
func TestPillar1SurvivesOutOfOrderObservation(t *testing.T) {
	lo := NewLockOrderGraph()

	// payment really did L1 then L2, but we hear about L2 first.
	lo.Observe(ev(types.Acquired, "payment", "L2", 2))
	lo.Observe(ev(types.Acquired, "payment", "L1", 1))
	lo.Observe(ev(types.Released, "payment", "L2", 3))
	lo.Observe(ev(types.Released, "payment", "L1", 4))

	// order does the same thing, also in L1 -> L2 order.
	lo.Observe(ev(types.Acquired, "order", "L1", 1))
	lo.Observe(ev(types.Acquired, "order", "L2", 2))

	lo.Tick()
	if flags := lo.Tick(); len(flags) != 0 {
		t.Fatalf("reordered observations must not produce a false violation, got %v", flags)
	}
}

// TestPillar1RecoversFromLostObservation checks that a permanently missing
// event does not stall detection. The gap is skipped after one interval and
// the genuine violation still gets reported.
func TestPillar1RecoversFromLostObservation(t *testing.T) {
	lo := NewLockOrderGraph()

	lo.Observe(ev(types.Acquired, "payment", "L1", 1))
	lo.Observe(ev(types.Acquired, "payment", "L2", 2))

	// order's seq 1 is lost in the network; only seq 2 and 3 arrive.
	lo.Observe(ev(types.Acquired, "order", "L2", 2))
	lo.Observe(ev(types.Acquired, "order", "L1", 3))

	// Interval 1: order's stream is stuck behind the missing seq 1.
	lo.Tick()
	// Interval 2: the gap is given up on, buffered events are replayed, and
	// the candidate is raised.
	lo.Tick()
	// Interval 3: candidate survives and is reported.
	flags := lo.Tick()
	if len(flags) != 1 {
		t.Fatalf("expected the violation to be reported despite a lost event, got %v", flags)
	}
}

// TestPillar1GraphGrowsWithPairsNotContention backs the overhead claim in the
// report: the lock-order graph is sized by how many distinct lock pairs are
// used together, not by how much contention there is.
func TestPillar1GraphGrowsWithPairsNotContention(t *testing.T) {
	lo := NewLockOrderGraph()

	// One service takes the same two locks 100 times over.
	seq := uint64(0)
	for i := 0; i < 100; i++ {
		seq++
		lo.Observe(ev(types.Acquired, "payment", "L1", seq))
		seq++
		lo.Observe(ev(types.Acquired, "payment", "L2", seq))
		seq++
		lo.Observe(ev(types.Released, "payment", "L2", seq))
		seq++
		lo.Observe(ev(types.Released, "payment", "L1", seq))
	}
	if got := lo.Size(); got != 1 {
		t.Fatalf("100 repetitions of one pair should leave 1 edge, got %d", got)
	}
}
