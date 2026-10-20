package depmonitor

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"adlmf/internal/types"
)

// pair is a directed lock-ordering edge: "someone held From, then took To".
type pair struct {
	From types.LockID
	To   types.LockID
}

// Flag is a warning that two services are acquiring the same two locks in
// opposite orders. If both ever hold their first lock at the same time, they
// deadlock. Crucially this can be reported before anybody is blocked.
type Flag struct {
	LockA, LockB types.LockID    // LockA < LockB, so the pair has one name
	ServiceA     types.ServiceID // established LockA -> LockB
	ServiceB     types.ServiceID // established LockB -> LockA
	Confirmed    bool
}

func (f Flag) String() string {
	return fmt.Sprintf("order-violation locks=(%s,%s) %s took %s->%s, %s took %s->%s",
		f.LockA, f.LockB, f.ServiceA, f.LockA, f.LockB, f.ServiceB, f.LockB, f.LockA)
}

// key gives the unordered lock pair a stable identity.
func (f Flag) key() pair { return pair{f.LockA, f.LockB} }

// LockOrderGraph is the persistent, historical half of the dependency monitor:
// Pillar 1.
//
// Unlike the wait-for graph it is never cleared when a lock is released. It
// records that a service once took lock A and then lock B, because that habit
// stays dangerous long after both locks are free.
//
// Two things make the distributed case harder than lockdep's single-process
// one, and both are handled here:
//
//   - Observations arrive out of order. A service's own events carry a
//     monotonic sequence number, so they are buffered and replayed in order
//     rather than trusted as they land.
//   - Observations go missing. A gap in the sequence is skipped after one
//     reporting interval rather than blocking detection forever, and a flag is
//     only confirmed once it has survived a second interval, so a warning
//     produced by a temporarily incomplete picture gets dropped instead of
//     reported.
type LockOrderGraph struct {
	// edges[{A,B}][S] = the sequence number at which service S was seen
	// holding A and then acquiring B.
	edges map[pair]map[types.ServiceID]uint64

	// held[S] = locks service S currently holds, in acquisition order.
	held map[types.ServiceID][]types.LockID

	// reorder buffering, per service
	nextSeq  map[types.ServiceID]uint64
	pending  map[types.ServiceID]map[uint64]types.LockEvent
	gapSince map[types.ServiceID]int // tick at which a gap was first noticed

	// candidate flags awaiting confirmation: pair -> tick first raised
	candidates map[pair]Flag
	raisedAt   map[pair]int
	confirmed  map[pair]Flag

	tick int
}

// NewLockOrderGraph returns an empty lock-order graph.
func NewLockOrderGraph() *LockOrderGraph {
	return &LockOrderGraph{
		edges:      make(map[pair]map[types.ServiceID]uint64),
		held:       make(map[types.ServiceID][]types.LockID),
		nextSeq:    make(map[types.ServiceID]uint64),
		pending:    make(map[types.ServiceID]map[uint64]types.LockEvent),
		gapSince:   make(map[types.ServiceID]int),
		candidates: make(map[pair]Flag),
		raisedAt:   make(map[pair]int),
		confirmed:  make(map[pair]Flag),
	}
}

// Observe takes one event off the wire. It does not apply it immediately:
// events are replayed in per-service sequence order, so an observation that
// overtakes an earlier one does not corrupt the recorded ordering.
func (g *LockOrderGraph) Observe(ev types.LockEvent) {
	if g.pending[ev.Service] == nil {
		g.pending[ev.Service] = make(map[uint64]types.LockEvent)
	}
	if g.nextSeq[ev.Service] == 0 {
		g.nextSeq[ev.Service] = 1
	}
	if ev.Seq < g.nextSeq[ev.Service] {
		return // already applied; a duplicate or a very late straggler
	}
	g.pending[ev.Service][ev.Seq] = ev
	g.drain(ev.Service)
}

// drain applies as many buffered events as are contiguous from nextSeq.
func (g *LockOrderGraph) drain(svc types.ServiceID) {
	for {
		want := g.nextSeq[svc]
		ev, ok := g.pending[svc][want]
		if !ok {
			return
		}
		delete(g.pending[svc], want)
		g.nextSeq[svc] = want + 1
		delete(g.gapSince, svc)
		g.apply(ev)
	}
}

// apply folds an in-order event into the graph.
func (g *LockOrderGraph) apply(ev types.LockEvent) {
	switch ev.Kind {
	case types.Acquired:
		// Every lock this service already holds precedes the new one.
		for _, prior := range g.held[ev.Service] {
			if prior == ev.Lock {
				continue
			}
			g.addEdge(prior, ev.Lock, ev.Service, ev.Seq)
		}
		g.held[ev.Service] = append(g.held[ev.Service], ev.Lock)

	case types.Released:
		cur := g.held[ev.Service]
		for i, l := range cur {
			if l == ev.Lock {
				g.held[ev.Service] = append(cur[:i:i], cur[i+1:]...)
				break
			}
		}
		if len(g.held[ev.Service]) == 0 {
			delete(g.held, ev.Service)
		}
	}
}

// addEdge records "service took from, then to" and raises a candidate flag if
// somebody else has been seen doing the opposite.
func (g *LockOrderGraph) addEdge(from, to types.LockID, svc types.ServiceID, seq uint64) {
	p := pair{from, to}
	if g.edges[p] == nil {
		g.edges[p] = make(map[types.ServiceID]uint64)
	}
	g.edges[p][svc] = seq

	// Has anyone else established the reverse ordering?
	rev := pair{to, from}
	for other := range g.edges[rev] {
		if other == svc {
			continue // one service consistently nesting its own locks is fine
		}
		f := normaliseFlag(from, to, svc, other)
		if _, already := g.confirmed[f.key()]; already {
			return
		}
		if _, already := g.candidates[f.key()]; already {
			return
		}
		g.candidates[f.key()] = f
		g.raisedAt[f.key()] = g.tick
		return
	}
}

// normaliseFlag orders the lock pair so the same violation always gets the
// same identity regardless of which direction was observed first.
func normaliseFlag(from, to types.LockID, svc, other types.ServiceID) Flag {
	if from < to {
		return Flag{LockA: from, LockB: to, ServiceA: svc, ServiceB: other}
	}
	return Flag{LockA: to, LockB: from, ServiceA: other, ServiceB: svc}
}

// Tick closes the current reporting interval and returns any flags confirmed
// by it.
//
// This is where the latency-for-accuracy trade described in the report is
// actually made. A candidate raised during interval N is not reported at the
// end of N. It is re-checked at the end of N+1, by which time late events have
// had a chance to arrive, and only reported if the evidence still holds.
// Gaps in a service's sequence are skipped after one interval so that a single
// lost observation cannot stall detection permanently.
func (g *LockOrderGraph) Tick() []Flag {
	// Skip sequence gaps that have been outstanding since an earlier interval.
	for svc, buf := range g.pending {
		if len(buf) == 0 {
			continue
		}
		if first, seen := g.gapSince[svc]; !seen {
			g.gapSince[svc] = g.tick
		} else if g.tick > first {
			// Give up on the missing event and resume from the lowest
			// sequence number we do have.
			//
			// Crucially, we also forget what this service was holding. A lost
			// event may well have been an Acquired or a Released, so our view
			// of its held set is no longer trustworthy. Continuing to infer
			// "held X, then took Y" across that hole fabricates orderings that
			// never happened — and because the fabricated edge can be the
			// reverse of a real one, it produces confident warnings about
			// services that are behaving correctly. Dropping the held set
			// costs us the edges that span the gap and keeps the ones we can
			// still justify.
			lowest := ^uint64(0)
			for s := range buf {
				if s < lowest {
					lowest = s
				}
			}
			delete(g.held, svc)
			g.nextSeq[svc] = lowest
			delete(g.gapSince, svc)
			g.drain(svc)
		}
	}

	var out []Flag
	for key, f := range g.candidates {
		// g.tick is the interval now being closed. A candidate raised during
		// this same interval has not yet survived one, so it waits.
		if g.raisedAt[key] >= g.tick {
			continue
		}
		// Re-verify against everything known now, including anything that
		// arrived late. If the reverse edge no longer stands, drop it.
		if !g.stillViolating(f) {
			delete(g.candidates, key)
			delete(g.raisedAt, key)
			continue
		}
		f.Confirmed = true
		g.confirmed[key] = f
		delete(g.candidates, key)
		delete(g.raisedAt, key)
		out = append(out, f)
	}

	g.tick++

	sort.Slice(out, func(i, j int) bool {
		if out[i].LockA != out[j].LockA {
			return out[i].LockA < out[j].LockA
		}
		return out[i].LockB < out[j].LockB
	})
	return out
}

// stillViolating re-checks that two different services really do hold opposite
// orderings for this pair.
func (g *LockOrderGraph) stillViolating(f Flag) bool {
	fwd := g.edges[pair{f.LockA, f.LockB}]
	rev := g.edges[pair{f.LockB, f.LockA}]
	for a := range fwd {
		for b := range rev {
			if a != b {
				return true
			}
		}
	}
	return false
}

// ConfirmedFlags returns every violation confirmed so far.
func (g *LockOrderGraph) ConfirmedFlags() []Flag {
	out := make([]Flag, 0, len(g.confirmed))
	for _, f := range g.confirmed {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LockA != out[j].LockA {
			return out[i].LockA < out[j].LockA
		}
		return out[i].LockB < out[j].LockB
	})
	return out
}

// Size reports how many ordering edges are being tracked. The report claims
// this grows with the number of distinct lock pairs ever used together, not
// with live contention, and the benchmark measures that claim.
func (g *LockOrderGraph) Size() int { return len(g.edges) }

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

// snapshot is the on-disk form of the lock-order graph.
type snapshot struct {
	Edges     map[string]map[string]uint64 `json:"edges"`
	Confirmed []Flag                       `json:"confirmed"`
	Tick      int                          `json:"tick"`
}

func edgeKey(p pair) string { return string(p.From) + "\x00" + string(p.To) }

func parseEdgeKey(k string) (pair, bool) {
	for i := 0; i < len(k); i++ {
		if k[i] == 0 {
			return pair{types.LockID(k[:i]), types.LockID(k[i+1:])}, true
		}
	}
	return pair{}, false
}

// Save writes the lock-order graph to a file.
//
// This graph is defined as persistent, historical knowledge: it records that
// some service once took lock A before lock B, and that stays true after the
// locks are released and after the monitor restarts. Keeping it only in memory
// would mean a monitor restart silently throws away every ordering learned so
// far and has to rediscover it, which is exactly the window in which an
// unflagged deadlock can happen.
//
// The live wait-for graph is deliberately NOT persisted: it describes who is
// blocked right now, and after a restart that is no longer knowable from an
// old file. It has to be rebuilt from fresh observations.
func (g *LockOrderGraph) Save(path string) error {
	s := snapshot{
		Edges: make(map[string]map[string]uint64, len(g.edges)),
		Tick:  g.tick,
	}
	for p, svcs := range g.edges {
		m := make(map[string]uint64, len(svcs))
		for svc, seq := range svcs {
			m[string(svc)] = seq
		}
		s.Edges[edgeKey(p)] = m
	}
	for _, f := range g.confirmed {
		s.Confirmed = append(s.Confirmed, f)
	}
	sort.Slice(s.Confirmed, func(i, j int) bool {
		if s.Confirmed[i].LockA != s.Confirmed[j].LockA {
			return s.Confirmed[i].LockA < s.Confirmed[j].LockA
		}
		return s.Confirmed[i].LockB < s.Confirmed[j].LockB
	})

	blob, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, blob, 0o644)
}

// Load restores a previously saved lock-order graph.
//
// Note what is deliberately not restored: the held-lock set and the per-service
// sequence positions. After a restart we have no idea what any service is
// currently holding or how far through its event stream we were, and guessing
// would fabricate orderings. Those are rebuilt from live observations; only the
// historical edges and already-confirmed flags carry over.
func (g *LockOrderGraph) Load(path string) error {
	blob, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var s snapshot
	if err := json.Unmarshal(blob, &s); err != nil {
		return err
	}

	g.edges = make(map[pair]map[types.ServiceID]uint64, len(s.Edges))
	for k, m := range s.Edges {
		p, ok := parseEdgeKey(k)
		if !ok {
			continue
		}
		svcs := make(map[types.ServiceID]uint64, len(m))
		for svc, seq := range m {
			svcs[types.ServiceID(svc)] = seq
		}
		g.edges[p] = svcs
	}
	g.confirmed = make(map[pair]Flag, len(s.Confirmed))
	for _, f := range s.Confirmed {
		g.confirmed[f.key()] = f
	}
	g.tick = s.Tick
	return nil
}
