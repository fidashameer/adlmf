package recovery

import (
	"testing"

	"adlmf/depmonitor"
	"adlmf/internal/types"
)

// threeServiceDeadlock builds the running example from the report: payment,
// inventory and order in a three-way cycle. `extraWaiters` services are queued
// behind payment's lock L1, which is what makes payment an expensive victim
// despite being cheap to roll back.
func threeServiceDeadlock(extraWaiters int) *depmonitor.WaitForGraph {
	g := depmonitor.NewWaitForGraph()
	apply := func(k types.EventKind, s types.ServiceID, l types.LockID, seq uint64) {
		g.Apply(types.LockEvent{Kind: k, Service: s, Lock: l, Seq: seq})
	}
	apply(types.Acquired, "payment", "L1", 1)
	apply(types.Acquired, "inventory", "L2", 1)
	apply(types.Acquired, "order", "L3", 1)
	apply(types.Waiting, "payment", "L2", 2)
	apply(types.Waiting, "inventory", "L3", 2)
	apply(types.Waiting, "order", "L1", 2)

	for i := 0; i < extraWaiters; i++ {
		svc := types.ServiceID("w" + string(rune('1'+i)))
		apply(types.Waiting, svc, "L1", 1)
	}
	return g
}

func baseSnapshot(extraWaiters int) Snapshot {
	return Snapshot{
		Cycle: []types.ServiceID{"payment", "inventory", "order"},
		Graph: threeServiceDeadlock(extraWaiters),
		RollbackCost: map[types.ServiceID]float64{
			"payment":   1, // cheapest to roll back...
			"inventory": 5,
			"order":     8,
		},
		WaitTime: map[types.ServiceID]float64{
			"payment":   10,
			"inventory": 30, // waited longest
			"order":     20,
		},
	}
}

// TestBlastRadiusCountsOnlyOutsideTheCycle pins down the definition. Inside the
// cycle everyone is blocked on everyone, so only the services queued behind a
// victim from outside should count.
func TestBlastRadiusCountsOnlyOutsideTheCycle(t *testing.T) {
	s := baseSnapshot(8)

	// payment holds L1, with 8 outside services queued behind it, plus itself.
	if got := BlastRadius(s, "payment"); got != 9 {
		t.Fatalf("payment blast radius: want 9, got %.1f", got)
	}
	// Nothing outside the cycle is queued behind inventory or order.
	if got := BlastRadius(s, "inventory"); got != 1 {
		t.Fatalf("inventory blast radius: want 1, got %.1f", got)
	}
	if got := BlastRadius(s, "order"); got != 1 {
		t.Fatalf("order blast radius: want 1, got %.1f", got)
	}
}

// TestAdaptiveBeatsCostOnlyWhenCheapVictimIsUpstream is the central hypothesis
// of the whole project, stated as a test.
//
// payment is the cheapest service to roll back, so the cost-only policy — the
// faithful stand-in for the prior art — picks it. But payment has eight other
// services queued behind it. The adaptive policy sees that and sacrifices
// inventory instead, which costs more to roll back but strands nobody.
func TestAdaptiveBeatsCostOnlyWhenCheapVictimIsUpstream(t *testing.T) {
	s := baseSnapshot(8)

	costOnly, err := CostOnlyPolicy{}.Decide(s)
	if err != nil {
		t.Fatalf("cost-only: %v", err)
	}
	if costOnly.Chosen.Service != "payment" {
		t.Fatalf("cost-only should pick the cheapest rollback (payment), got %s", costOnly.Chosen.Service)
	}

	adaptive, err := NewAdaptivePolicy(0.1).Decide(s)
	if err != nil {
		t.Fatalf("adaptive: %v", err)
	}
	if adaptive.Chosen.Service != "inventory" {
		t.Fatalf("adaptive should avoid the upstream victim and pick inventory, got %s (scores %v)",
			adaptive.Chosen.Service, adaptive.Scores)
	}

	// The point of the exercise: measured in services disrupted, the adaptive
	// choice is cheaper even though its rollback cost is higher.
	disruptedByCostOnly := BlastRadius(s, costOnly.Chosen.Service)
	disruptedByAdaptive := BlastRadius(s, adaptive.Chosen.Service)
	if !(disruptedByAdaptive < disruptedByCostOnly) {
		t.Fatalf("adaptive should disrupt fewer services: adaptive=%.1f cost-only=%.1f",
			disruptedByAdaptive, disruptedByCostOnly)
	}
	t.Logf("cost-only disrupted %.0f services, adaptive disrupted %.0f",
		disruptedByCostOnly, disruptedByAdaptive)
}

// TestAdaptiveAgreesWithCostOnlyWhenNothingIsUpstream is the honesty check. If
// no service has anything queued behind it, blast radius is equal across the
// board and our policy should fall back to the same answer as the prior art.
// A policy that disagreed here would just be noise.
func TestAdaptiveAgreesWithCostOnlyWhenNothingIsUpstream(t *testing.T) {
	s := baseSnapshot(0)

	costOnly, _ := CostOnlyPolicy{}.Decide(s)
	adaptive, _ := NewAdaptivePolicy(0.1).Decide(s)

	if costOnly.Chosen.Service != adaptive.Chosen.Service {
		t.Fatalf("with no downstream difference both policies should agree: cost-only=%s adaptive=%s",
			costOnly.Chosen.Service, adaptive.Chosen.Service)
	}
}

// TestTimeoutBaselinePicksLongestWaiter checks the other baseline behaves as
// advertised, so the comparison in Section 8 is against a real alternative.
func TestTimeoutBaselinePicksLongestWaiter(t *testing.T) {
	s := baseSnapshot(8)
	d, err := TimeoutPolicy{}.Decide(s)
	if err != nil {
		t.Fatalf("timeout: %v", err)
	}
	if d.Chosen.Service != "inventory" {
		t.Fatalf("timeout should pick the longest waiter (inventory), got %s", d.Chosen.Service)
	}
}

// TestYoungestBaselinePicksMostRecentlyBlocked pins the third baseline. The
// classical rule kills the youngest transaction, so with payment blocked most
// recently it must choose payment -- the exact opposite of what the timeout
// baseline does on the same snapshot, which is why the two are not redundant.
func TestYoungestBaselinePicksMostRecentlyBlocked(t *testing.T) {
	s := baseSnapshot(8)
	d, err := YoungestPolicy{}.Decide(s)
	if err != nil {
		t.Fatalf("youngest: %v", err)
	}
	if d.Chosen.Service != "payment" {
		t.Fatalf("youngest should pick the shortest waiter (payment), got %s", d.Chosen.Service)
	}

	// The two time-based baselines must disagree here, or one of them is not
	// telling us anything the other does not.
	td, err := TimeoutPolicy{}.Decide(s)
	if err != nil {
		t.Fatalf("timeout: %v", err)
	}
	if td.Chosen.Service == d.Chosen.Service {
		t.Fatalf("timeout and youngest both chose %s; they should be opposites", d.Chosen.Service)
	}
}

// TestYoungestIgnoresBlastRadius is the point of having this baseline. Eight
// services are queued behind payment's lock, making it by far the worst victim
// in blast-radius terms, and the youngest rule picks it anyway because it only
// looks at time. The adaptive policy must not.
func TestYoungestIgnoresBlastRadius(t *testing.T) {
	s := baseSnapshot(8)
	y, err := YoungestPolicy{}.Decide(s)
	if err != nil {
		t.Fatalf("youngest: %v", err)
	}
	a, err := NewAdaptivePolicy(0).Decide(s)
	if err != nil {
		t.Fatalf("adaptive: %v", err)
	}
	if y.Chosen.Service == a.Chosen.Service {
		t.Fatalf("adaptive matched the youngest rule (%s) despite 8 services queued behind it", y.Chosen.Service)
	}
}

// TestPartitionSuspectIsAvoided checks the partition term. A service that looks
// unreachable rather than genuinely stuck is a bad victim, because aborting it
// may not even take effect. The penalty must push the choice elsewhere.
func TestPartitionSuspectIsAvoided(t *testing.T) {
	s := baseSnapshot(0)
	// With no downstream waiters the scores are driven by rollback cost, so
	// payment (cost 1) would normally win.
	plain, _ := NewAdaptivePolicy(0.1).Decide(s)
	if plain.Chosen.Service != "payment" {
		t.Fatalf("precondition failed: expected payment, got %s", plain.Chosen.Service)
	}

	// Now mark payment as possibly partitioned away.
	s.PartitionSuspect = map[types.ServiceID]bool{"payment": true}
	guarded, _ := NewAdaptivePolicy(0.1).Decide(s)
	if guarded.Chosen.Service == "payment" {
		t.Fatalf("a partition-suspect service should not be chosen, scores=%v", guarded.Scores)
	}
}

// TestWeightRecalibrationRespondsToOutcome checks the self-calibration loop.
// If a decision turns out to disrupt far more than predicted, the weights on
// the features that drove it must increase, so the same mistake scores worse
// next time.
func TestWeightRecalibrationRespondsToOutcome(t *testing.T) {
	p := NewAdaptivePolicy(0.1)
	before := p.W

	s := baseSnapshot(8)
	if _, err := p.Decide(s); err != nil {
		t.Fatalf("decide: %v", err)
	}
	// The decision turned out much worse than the score predicted.
	p.Observe(50.0)

	if !(p.W.BlastRadius > before.BlastRadius) {
		t.Fatalf("blast-radius weight should have increased: before=%.3f after=%.3f",
			before.BlastRadius, p.W.BlastRadius)
	}
	if !(p.W.RollbackCost > before.RollbackCost) {
		t.Fatalf("rollback-cost weight should have increased: before=%.3f after=%.3f",
			before.RollbackCost, p.W.RollbackCost)
	}
}

// TestWeightsNeverGoNegative guards the one thing no amount of observed data
// should be allowed to do: invert the meaning of a term, so that a larger
// blast radius starts looking preferable.
func TestWeightsNeverGoNegative(t *testing.T) {
	p := NewAdaptivePolicy(0.5)
	s := baseSnapshot(8)
	for i := 0; i < 50; i++ {
		if _, err := p.Decide(s); err != nil {
			t.Fatalf("decide: %v", err)
		}
		p.Observe(0) // every decision looks far better than predicted
	}
	if p.W.BlastRadius < 0 || p.W.RollbackCost < 0 || p.W.Partition < 0 {
		t.Fatalf("weights went negative: %+v", p.W)
	}
}

// TestEmptyCycleIsAnError makes sure no policy silently invents a victim.
func TestEmptyCycleIsAnError(t *testing.T) {
	for _, p := range []Policy{TimeoutPolicy{}, CostOnlyPolicy{}, NewAdaptivePolicy(0.1)} {
		if _, err := p.Decide(Snapshot{}); err == nil {
			t.Fatalf("%s should reject an empty cycle", p.Name())
		}
	}
}
