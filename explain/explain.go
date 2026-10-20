// Package explain implements Objective 4: recording why a recovery decision
// was made, and turning those records into plain language.
//
// The split matters and is the whole point of the design. Recording happens on
// the critical path, so it must be close to free: it appends a small struct to
// a pre-allocated buffer and does no formatting, no string building and no I/O.
// Generating readable text happens later, off the critical path, driven by
// whoever wants to read it. If those two were merged, every recovery decision
// would pay for prose nobody has asked for yet.
//
// Section 8's last hypothesis is a test of exactly this claim: recovery-path
// latency with explanations on versus off should not differ.
package explain

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"adlmf/internal/types"
	"adlmf/recovery"
)

// Record is one decision, captured in the cheapest form that still allows a
// full explanation to be reconstructed afterwards.
type Record struct {
	Tick      int
	Chosen    types.ServiceID
	Action    recovery.ActionKind
	Scores    map[types.ServiceID]float64
	Disrupted float64
	Reason    string
}

// Recorder is the on-critical-path half. It only appends.
type Recorder struct {
	mu      sync.Mutex
	records []Record
}

// NewRecorder pre-allocates so that steady-state recording does no allocation.
func NewRecorder(capacity int) *Recorder {
	return &Recorder{records: make([]Record, 0, capacity)}
}

// Record captures a decision. Deliberately trivial: no formatting here.
func (r *Recorder) Record(tick int, d recovery.Decision, disrupted float64) {
	r.mu.Lock()
	r.records = append(r.records, Record{
		Tick:      tick,
		Chosen:    d.Chosen.Service,
		Action:    d.Chosen.Kind,
		Scores:    d.Scores,
		Disrupted: disrupted,
		Reason:    d.Reason,
	})
	r.mu.Unlock()
}

// Records returns everything captured so far.
func (r *Recorder) Records() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Record, len(r.records))
	copy(out, r.records)
	return out
}

// Len reports how many decisions were recorded.
func (r *Recorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.records)
}

// Generator is the off-critical-path half: it turns records into text.
type Generator struct{}

// Explain produces a plain-language account of one decision.
func (Generator) Explain(rec Record) string {
	var b strings.Builder
	fmt.Fprintf(&b, "At tick %d the system broke a deadlock by choosing to %s %s.\n",
		rec.Tick, strings.ToLower(rec.Action.String()), rec.Chosen)
	fmt.Fprintf(&b, "  Why: %s\n", rec.Reason)
	fmt.Fprintf(&b, "  It actually disrupted %.0f service(s).\n", rec.Disrupted)
	return b.String()
}

// Contrastive answers the question a human actually asks, which is not "why
// this one" but "why this one rather than that one".
func (Generator) Contrastive(rec Record, alternative types.ServiceID) string {
	chosenScore, ok1 := rec.Scores[rec.Chosen]
	altScore, ok2 := rec.Scores[alternative]
	if !ok1 || !ok2 {
		return fmt.Sprintf("No score was recorded for %s, so the two cannot be compared.", alternative)
	}
	if altScore == chosenScore {
		return fmt.Sprintf("%s and %s scored the same (%.2f); %s was chosen by a stable tie-break.",
			rec.Chosen, alternative, chosenScore, rec.Chosen)
	}
	return fmt.Sprintf("%s was chosen over %s because it scored %.2f against %.2f, and the engine takes the lowest score. A lower score means less expected total disruption.",
		rec.Chosen, alternative, chosenScore, altScore)
}

// Summary describes a whole run.
func (g Generator) Summary(recs []Record) string {
	if len(recs) == 0 {
		return "No recovery decisions were taken."
	}
	counts := make(map[types.ServiceID]int)
	var total float64
	for _, r := range recs {
		counts[r.Chosen]++
		total += r.Disrupted
	}
	type kv struct {
		svc types.ServiceID
		n   int
	}
	var all []kv
	for s, n := range counts {
		all = append(all, kv{s, n})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].n != all[j].n {
			return all[i].n > all[j].n
		}
		return all[i].svc < all[j].svc
	})

	var b strings.Builder
	fmt.Fprintf(&b, "%d recovery decisions, disrupting %.1f services in total (%.2f per decision).\n",
		len(recs), total, total/float64(len(recs)))
	fmt.Fprintf(&b, "Most often chosen as victim: ")
	for i, e := range all {
		if i >= 3 {
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s (%d)", e.svc, e.n)
	}
	b.WriteString(".\n")
	return b.String()
}

// WhatIf answers the counterfactual: what would have happened had the engine
// chosen a different victim.
//
// This is the "stretch only" item in Section 7.3. It is possible at all only
// because the recorder keeps the score of every candidate, not just the winner.
// Had it stored only the chosen action, no counterfactual could be answered
// afterwards without re-running the decision.
func (Generator) WhatIf(rec Record, alternative types.ServiceID) string {
	chosen, ok1 := rec.Scores[rec.Chosen]
	alt, ok2 := rec.Scores[alternative]
	if !ok2 {
		return fmt.Sprintf("%s was not a candidate in that decision, so there is nothing to compare.", alternative)
	}
	if !ok1 {
		return fmt.Sprintf("No score was recorded for the chosen service, so the comparison is not available.")
	}

	diff := alt - chosen
	switch {
	case diff > 0:
		return fmt.Sprintf(
			"Had %s been aborted instead of %s, the engine's estimate of total disruption would have been %.2f rather than %.2f, i.e. %.2f worse. The deadlock would still have cleared; it would simply have cost more.",
			alternative, rec.Chosen, alt, chosen, diff)
	case diff < 0:
		return fmt.Sprintf(
			"Aborting %s would have scored %.2f against %.2f, i.e. %.2f better. That it was not chosen means it was ruled out before scoring, most likely as partition-suspect.",
			alternative, alt, chosen, -diff)
	default:
		return fmt.Sprintf("%s scored identically to %s (%.2f); the choice between them was a tie-break, not a judgement.",
			alternative, rec.Chosen, chosen)
	}
}

// Report renders everything known about one decision: what happened, why, the
// contrast with one alternative, and the counterfactual.
func (g Generator) Report(rec Record, alternative types.ServiceID) string {
	var b strings.Builder
	b.WriteString(g.Explain(rec))
	fmt.Fprintf(&b, "  Rather than %s: %s\n", alternative, g.Contrastive(rec, alternative))
	fmt.Fprintf(&b, "  What if: %s\n", g.WhatIf(rec, alternative))
	return b.String()
}
