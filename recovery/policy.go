// Package recovery decides what to do once a deadlock has been detected.
//
// Everything here sits behind the Policy interface so the benchmark harness
// can run the two baselines and our adaptive policy against identical
// workloads by swapping one value. That swappability is what makes the
// comparison in Section 8 of the report a fair test rather than a claim.
package recovery

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"adlmf/depmonitor"
	"adlmf/internal/types"
)

// ActionKind is what we do to the service we pick.
type ActionKind int

const (
	// Abort rolls the service's transaction back entirely.
	Abort ActionKind = iota
	// Preempt forces it to drop one lock but lets it continue.
	Preempt
	// DelayRetry makes it back off and retry with a priority boost.
	DelayRetry
)

func (a ActionKind) String() string {
	switch a {
	case Abort:
		return "ABORT"
	case Preempt:
		return "PREEMPT"
	case DelayRetry:
		return "DELAY_RETRY"
	}
	return "UNKNOWN"
}

// Candidate is one option available to the engine.
type Candidate struct {
	Service types.ServiceID
	Lock    types.LockID // the lock that would be given up
	Kind    ActionKind
}

// Snapshot is everything a policy is allowed to look at when deciding.
type Snapshot struct {
	// Cycle is the set of services that are mutually deadlocked.
	Cycle []types.ServiceID
	// Graph is the live wait-for graph, used for blast radius.
	Graph *depmonitor.WaitForGraph
	// Criticality says how much each service matters. Default 1.0.
	Criticality map[types.ServiceID]float64
	// RollbackCost estimates the work lost by aborting each service.
	RollbackCost map[types.ServiceID]float64
	// WaitTime is how long each service has been blocked, in arbitrary
	// consistent units. The timeout baseline uses this.
	WaitTime map[types.ServiceID]float64
	// AbortCount is how many times each service has already been sacrificed.
	// Without this the engine has no way to notice that it keeps choosing the
	// same victim, and a service that is cheap to abort gets aborted forever.
	AbortCount map[types.ServiceID]float64

	// PartitionSuspect marks services that look unreachable rather than
	// genuinely stuck. Sacrificing one of those does not fix anything,
	// because it may not even receive the instruction.
	PartitionSuspect map[types.ServiceID]bool
}

func (s Snapshot) criticality(svc types.ServiceID) float64 {
	if v, ok := s.Criticality[svc]; ok {
		return v
	}
	return 1.0
}

// Decision records both the chosen action and why, which is what Objective 4
// turns into a human-readable explanation. Recording it is cheap and happens
// on the critical path; turning it into prose happens elsewhere.
type Decision struct {
	Chosen Candidate
	Scores map[types.ServiceID]float64
	Reason string
}

// Policy chooses a recovery action. The three implementations below are the
// two baselines and our proposal.
type Policy interface {
	Name() string
	Decide(s Snapshot) (Decision, error)
}

// --------------------------------------------------------------------------
// Blast radius
// --------------------------------------------------------------------------

// BlastRadius is the criticality-weighted count of services hurt if `victim`
// is aborted: the victim itself plus everything queued up behind it from
// outside the deadlock.
//
// This is the quantity a single-database victim selector has no reason to
// compute, because inside one database there are no other services to affect.
// It is the input that makes Pillar 2 different from US 9,104,989 B2.
//
// Services inside the cycle are excluded from the count. They are all
// transitively blocked on each other by definition, so counting them would
// give every candidate the same score and discriminate between none of them.
// They are also the services that get *unblocked* by the abort, so counting
// them as damage would be wrong anyway.
func BlastRadius(s Snapshot, victim types.ServiceID) float64 {
	total := s.criticality(victim)
	if s.Graph != nil {
		inCycle := make(map[types.ServiceID]bool, len(s.Cycle))
		for _, c := range s.Cycle {
			inCycle[c] = true
		}
		for _, dep := range s.Graph.DependentsExcluding(victim, inCycle) {
			total += s.criticality(dep)
		}
	}
	return total
}

// --------------------------------------------------------------------------
// Baseline 1: timeout
// --------------------------------------------------------------------------

// ErrNotYet means the policy declines to act for now. The timeout baseline
// returns this until something has been blocked long enough to trip its
// threshold.
var ErrNotYet = errors.New("recovery: no action yet")

// TimeoutPolicy aborts whoever has been waiting longest, but only once some
// service has been blocked for at least Threshold.
//
// The threshold is what makes this a faithful baseline rather than a strawman.
// A timeout-based system does not know a deadlock has formed; it only knows
// that something has been stuck for a while. That waiting period is the cost
// of the approach, and leaving it out would hand our policy an unearned win on
// recovery time.
type TimeoutPolicy struct {
	Threshold float64
}

func (TimeoutPolicy) Name() string { return "timeout" }

func (p TimeoutPolicy) Decide(s Snapshot) (Decision, error) {
	if len(s.Cycle) == 0 {
		return Decision{}, fmt.Errorf("recovery: empty cycle")
	}
	longest := 0.0
	for _, svc := range s.Cycle {
		if w := s.WaitTime[svc]; w > longest {
			longest = w
		}
	}
	if longest < p.Threshold {
		return Decision{}, ErrNotYet
	}
	scores := make(map[types.ServiceID]float64, len(s.Cycle))
	var best types.ServiceID
	bestWait := -1.0
	for _, svc := range order(s.Cycle) {
		w := s.WaitTime[svc]
		scores[svc] = w
		if w > bestWait {
			bestWait, best = w, svc
		}
	}
	return Decision{
		Chosen: Candidate{Service: best, Kind: Abort},
		Scores: scores,
		Reason: fmt.Sprintf("aborted %s because it had waited longest (%.1f)", best, bestWait),
	}, nil
}

// --------------------------------------------------------------------------
// Baseline 2: cost-only
// --------------------------------------------------------------------------

// CostOnlyPolicy aborts whichever service is cheapest to roll back. This is a
// faithful stand-in for the prior art: US 9,104,989 B2 and the victim
// selection that ships in production databases. It is deliberately a strong
// baseline, not a strawman — on a single machine it is the right answer.
type CostOnlyPolicy struct{}

func (CostOnlyPolicy) Name() string { return "cost-only" }

func (CostOnlyPolicy) Decide(s Snapshot) (Decision, error) {
	if len(s.Cycle) == 0 {
		return Decision{}, fmt.Errorf("recovery: empty cycle")
	}
	scores := make(map[types.ServiceID]float64, len(s.Cycle))
	var best types.ServiceID
	bestCost := -1.0
	for _, svc := range order(s.Cycle) {
		c := s.RollbackCost[svc]
		scores[svc] = c
		if bestCost < 0 || c < bestCost {
			bestCost, best = c, svc
		}
	}
	return Decision{
		Chosen: Candidate{Service: best, Kind: Abort},
		Scores: scores,
		Reason: fmt.Sprintf("aborted %s because it was cheapest to roll back (%.1f)", best, bestCost),
	}, nil
}

// YoungestPolicy aborts the candidate that became blocked most recently.
//
// This is the second baseline variant named in Section 7.3's "if we have time"
// tier, and it is the textbook rule rather than something invented here:
// classical transaction managers break a cycle by killing the youngest
// transaction, on the reasoning that it has the least invested work and the
// best chance of succeeding on retry. It is the same intuition behind the
// wound-wait / wait-die family.
//
// It is included because it is a genuinely different kind of baseline from the
// other two. Timeout uses no graph information at all; cost-only uses per
// service cost; this one uses *time*, which is the input most readily
// available in a real system and the one a practitioner would reach for first.
// If the adaptive policy cannot beat it, the extra machinery is not worth it.
type YoungestPolicy struct{}

func (YoungestPolicy) Name() string { return "youngest" }

func (YoungestPolicy) Decide(s Snapshot) (Decision, error) {
	if len(s.Cycle) == 0 {
		return Decision{}, fmt.Errorf("recovery: empty cycle")
	}
	scores := make(map[types.ServiceID]float64, len(s.Cycle))
	var best types.ServiceID
	bestWait := math.Inf(1)
	for _, svc := range order(s.Cycle) {
		// Shorter wait means more recently blocked, so it is the youngest.
		// Scores are negated waits so that, as with every other policy here,
		// lowest score wins and the Decision record stays comparable.
		w := s.WaitTime[svc]
		scores[svc] = -w
		if w < bestWait {
			bestWait, best = w, svc
		}
	}
	return Decision{
		Chosen: Candidate{Service: best, Kind: Abort},
		Scores: scores,
		Reason: fmt.Sprintf("aborted %s because it blocked most recently (waiting %.0f, the youngest in the cycle)", best, bestWait),
	}, nil
}

// --------------------------------------------------------------------------
// Pillar 2: adaptive, blast-radius-weighted
// --------------------------------------------------------------------------

// Weights are the three coefficients of the scoring function.
type Weights struct {
	BlastRadius  float64
	RollbackCost float64
	Partition    float64
	// Fairness penalises a service for having been sacrificed before. It is
	// not in the report's formula; it was added after measurement showed that
	// minimising blast radius alone starves the cheapest victims and collapses
	// throughput. See README.
	Fairness float64
}

// DefaultWeights are the starting values, chosen before any experiment is run
// so that the adaptive policy cannot be accused of being tuned to the test.
// DefaultAlpha is the learning rate the framework ships with, and it is zero:
// self-calibration is OFF by default.
//
// This is a result, not an oversight. Experiment H9 measures calibration on a
// workload where rollback cost genuinely varies, and it loses on all three
// metrics -- including the very cost it is trained to predict. The cause is
// localised in TestCalibrationFailureCause: the observed quantity available
// after an abort (work destroyed) does not depend on blast radius, so any
// honest regression drives the blast-radius weight to zero; but blast radius
// is what governs throughput. Freezing that weight at zero with no learning
// at all reproduces nearly the whole loss.
//
// So the mechanism is mis-specified rather than mistuned: a victim-selection
// score cannot be calibrated by regressing it onto one observed cost component
// when it has to balance costs that component does not contain. The code is
// kept, and reachable by passing a non-zero alpha, because the negative result
// is worth being able to reproduce.
const DefaultAlpha = 0.0

func DefaultWeights() Weights {
	return Weights{BlastRadius: 1.0, RollbackCost: 1.0, Partition: 2.0, Fairness: 1.0}
}

// AdaptivePolicy scores each candidate and picks the lowest.
//
//	score(a) = w1*blast_radius(a) + w2*rollback_cost(a) + w3*partition_penalty(a)
//
// Note on the sign of the partition term: the draft report writes this term as
// subtracted. That is inconsistent with picking the lowest-scoring action — a
// subtracted penalty would make a service that is merely unreachable *more*
// likely to be sacrificed, which is the opposite of the stated intent. It is
// added here so that a partition-suspect service is pushed away from being
// chosen, which is what the surrounding prose actually describes.
type AdaptivePolicy struct {
	W Weights
	// DisablePartitionTier turns off the preference for reachable candidates,
	// leaving only the weighted partition term from the report's formula. Used
	// by the ablation to show which of the two is actually doing the work.
	DisablePartitionTier bool

	// AuditGap, when set, is called on every decision in which some candidates
	// are partition-suspect and some are not, with the best score among the
	// suspects and the best among the reachable ones, each computed WITHOUT the
	// partition term. It exists only to measure how much headroom the weighted
	// term actually has; it is nil in normal use.
	AuditGap func(suspectBest, reachableBest float64)

	// Alpha is the learning rate for weight recalibration. Small and fixed;
	// the report quotes 0.05-0.1.
	Alpha float64
	// history records the last decision so the outcome can be fed back.
	lastVictim    types.ServiceID
	lastPredicted float64
	lastFeatures  Weights
}

// NewAdaptivePolicy builds the policy with the default starting weights.
func NewAdaptivePolicy(alpha float64) *AdaptivePolicy {
	return &AdaptivePolicy{W: DefaultWeights(), Alpha: alpha}
}

func (*AdaptivePolicy) Name() string { return "adaptive" }

// partitionPenalty is 1 for a service that looks partitioned away, 0 otherwise.
func partitionPenalty(s Snapshot, svc types.ServiceID) float64 {
	if s.PartitionSuspect[svc] {
		return 1.0
	}
	return 0.0
}

func (p *AdaptivePolicy) Decide(s Snapshot) (Decision, error) {
	if len(s.Cycle) == 0 {
		return Decision{}, fmt.Errorf("recovery: empty cycle")
	}

	// Partition suspicion is handled as a preference between tiers rather than
	// purely as a term in the sum.
	//
	// Reason: the penalty is a 0/1 indicator while blast radius and rollback
	// cost are unbounded, so in a plain weighted sum *any* fixed partition
	// weight can be outweighed by a large enough cost difference. That makes
	// the stated intent ("do not sacrifice a service just because messages are
	// slow") impossible to express reliably. It is also not really a cost
	// trade-off: if a service is unreachable, aborting it may simply not take
	// effect, so the action fails rather than costing more.
	//
	// So: if any candidate looks reachable, choose among those. Only if every
	// candidate is partition-suspect do we score them all and accept the
	// least-bad option. The penalty term stays in the score either way, so
	// ranking within a tier still reflects it.
	candidates := order(s.Cycle)
	var reachable []types.ServiceID
	for _, svc := range candidates {
		if !s.PartitionSuspect[svc] {
			reachable = append(reachable, svc)
		}
	}
	eligible := candidates
	if len(reachable) > 0 && !p.DisablePartitionTier {
		eligible = reachable
	}

	// Scores are reported for every candidate, so the decision record shows
	// what the rejected options looked like.
	// The score has two halves and only one of them is a prediction.
	//
	// predicted(svc) estimates how much damage aborting svc would actually do,
	// from blast radius and rollback cost. That is a claim about the world, so
	// it is the only part calibration is allowed to adjust.
	//
	// The partition and fairness terms are preferences, not predictions: they
	// say which victim we would rather pick given equal damage. Folding them
	// into the regression target was a bug -- the fairness term grows without
	// bound as abort counts accumulate, so the error never settles and the two
	// genuine cost weights get driven to zero.
	predicted := make(map[types.ServiceID]float64, len(s.Cycle))
	scores := make(map[types.ServiceID]float64, len(s.Cycle))
	for _, svc := range candidates {
		predicted[svc] = p.W.BlastRadius*BlastRadius(s, svc) +
			p.W.RollbackCost*s.RollbackCost[svc]
		scores[svc] = predicted[svc] +
			p.W.Partition*partitionPenalty(s, svc) +
			p.W.Fairness*s.AbortCount[svc]
	}

	if p.AuditGap != nil && len(reachable) > 0 && len(reachable) < len(candidates) {
		sBest, rBest := math.Inf(1), math.Inf(1)
		for _, svc := range candidates {
			bare := scores[svc] - p.W.Partition*partitionPenalty(s, svc)
			if s.PartitionSuspect[svc] {
				if bare < sBest {
					sBest = bare
				}
			} else if bare < rBest {
				rBest = bare
			}
		}
		p.AuditGap(sBest, rBest)
	}

	var best types.ServiceID
	bestScore := 0.0
	var bestFeatures Weights
	for _, svc := range eligible {
		if best == "" || scores[svc] < bestScore {
			best, bestScore = svc, scores[svc]
			bestFeatures = Weights{
				BlastRadius:  BlastRadius(s, svc),
				RollbackCost: s.RollbackCost[svc],
				Partition:    partitionPenalty(s, svc),
			}
		}
	}

	p.lastVictim = best
	p.lastPredicted = predicted[best]
	p.lastFeatures = bestFeatures

	skipped := len(candidates) - len(eligible)
	reason := fmt.Sprintf(
		"aborted %s: blast_radius=%.1f rollback_cost=%.1f partition=%.0f -> score %.2f (lowest of %d)",
		best, bestFeatures.BlastRadius, bestFeatures.RollbackCost, bestFeatures.Partition,
		bestScore, len(eligible))
	if skipped > 0 {
		reason += fmt.Sprintf("; %d candidate(s) skipped as partition-suspect", skipped)
	}

	return Decision{
		Chosen: Candidate{Service: best, Kind: Abort},
		Scores: scores,
		Reason: reason,
	}, nil
}

// Observe feeds the actual disruption back so the weights can be recalibrated.
//
//	w_i(t+1) = w_i(t) + alpha * (observed - predicted) * feature_i / ||feature||
//
// Each weight is corrected in proportion to how much its own feature
// contributed to the prediction that turned out wrong. This is the standard
// least-mean-squares form.
//
// An earlier version used sign(feature_i) instead of the feature value. That
// applied an identical delta to every weight, so they moved in lockstep, the
// ranking of candidates never changed, and recalibration had no effect on a
// single decision despite the weights visibly drifting. Weights that move
// without changing any decision are not adaptation, and the ablation in
// cmd/ablate is what exposed it.
//
// Normalising by the feature vector's magnitude keeps the step size stable when
// the features are large; without it a single high-blast-radius decision can
// swamp the weights.
func (p *AdaptivePolicy) Observe(observedCost float64) {
	if p.lastVictim == "" {
		return
	}
	f := p.lastFeatures
	// Only the predictive features take part. The partition weight is a
	// preference and is deliberately left alone.
	norm := f.BlastRadius*f.BlastRadius + f.RollbackCost*f.RollbackCost
	if norm <= 0 {
		p.lastVictim = ""
		return
	}

	err := observedCost - p.lastPredicted
	step := p.Alpha * err / norm

	p.W.BlastRadius += step * f.BlastRadius
	p.W.RollbackCost += step * f.RollbackCost

	// Weights are kept non-negative: a negative weight would invert the
	// meaning of a term (making a bigger blast radius look better), which no
	// amount of observed data should be allowed to do.
	p.W.BlastRadius = clampLow(p.W.BlastRadius, 0)
	p.W.RollbackCost = clampLow(p.W.RollbackCost, 0)

	p.lastVictim = ""
}

func clampLow(v, low float64) float64 {
	if v < low {
		return low
	}
	return v
}

// order sorts a service list so decisions are deterministic given equal scores.
func order(in []types.ServiceID) []types.ServiceID {
	out := make([]types.ServiceID, len(in))
	copy(out, in)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

var (
	_ Policy = TimeoutPolicy{}
	_ Policy = CostOnlyPolicy{}
	_ Policy = (*AdaptivePolicy)(nil)
)
