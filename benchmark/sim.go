// Package benchmark is the harness from Step 7: it generates contention,
// injects faults, degrades observation, and measures what each recovery policy
// actually does.
//
// How the simulation works, and why it is built this way:
//
// Time is discrete. On each tick every service takes at most one step of its
// transaction. This removes wall-clock noise entirely, which matters because
// the quantity under test is *decision quality*, not lock throughput — two
// policies given identical workloads must differ only in which victim they
// pick, or the comparison means nothing.
//
// There are deliberately two views of the world:
//
//   - The TRUE state: who really holds what and who is really blocked. Used
//     only for measurement, never for decisions. This is how we know when a
//     deadlock actually formed, so recovery time and lead time are measured
//     against reality rather than against what the monitor happened to notice.
//   - The OBSERVED state: what the dependency monitor has been told, after
//     events have been delayed and dropped. Every policy decision is made from
//     this view, because that is the information a real deployment would have.
//
// Keeping those apart is what lets the harness honestly report that a policy
// acted on a stale picture, instead of quietly grading it against the stale
// picture it used.
package benchmark

import (
	"math/rand"
	"sort"

	"adlmf/depmonitor"
	"adlmf/explain"
	"adlmf/internal/types"
	"adlmf/recovery"
)

// Config describes one experimental run.
type Config struct {
	Services    int     // number of simulated microservices
	LockPool    int     // distinct locks they contend over; fewer means more contention
	HoldTicks   int     // how long a service holds its locks once it has them all
	Ticks       int     // length of the run
	Seed        int64   // makes a run reproducible
	ReverseFrac float64 // fraction of services that acquire their two locks in reverse order

	// Observation degradation, used for the partial-information experiments.
	ObsDelay int     // ticks an event is held back before the monitor sees it
	ObsLoss  float64 // probability an event never arrives at all

	// CheckInterval is how often the monitor closes a reporting interval,
	// which is what gates Pillar 1 flag confirmation.
	CheckInterval int

	// CrashAt, if non-zero, kills the service holding the most locks at that
	// tick without releasing anything. Used for the stale-lock experiment.
	CrashAt int

	// Framework off means no monitor and no engine at all. Used to isolate
	// monitoring cost; pair it with ReverseFrac=0 so the two runs differ only
	// by the framework and not by whether deadlocks ever get resolved.
	FrameworkOff bool

	// ThinkTicks staggers services: after finishing or being aborted, a
	// service idles for a random 0..ThinkTicks before trying again. Without
	// this the system thrashes, every transaction collides immediately, and
	// almost nothing ever completes - which is not a realistic workload.
	ThinkTicks int

	// ReverseStartTick delays the services that use the reversed lock order,
	// modelling the realistic case: a system already running happily, into
	// which a service with the opposite acquisition order is introduced. With
	// everything starting at once the deadlock forms before any ordering
	// history exists, which leaves prediction nothing to work from.
	ReverseStartTick int

	// PartitionAt / PartitionFor simulate a network partition rather than a
	// crash: the service keeps running and keeps holding its locks, but its
	// events stop reaching the monitor and it stops being reachable. From
	// outside this looks exactly like a crash, which is the confusion Section
	// 9 lists as a risk.
	PartitionAt  int
	PartitionFor int

	// CriticalitySkew makes services differ in importance. 0 means uniform.
	// With skew, one service in every group is worth `1 + skew` and the rest 1,
	// so blast radius varies across candidates and the weights have something
	// to calibrate against.
	// LocksPerTxn, when above 2, lets services take different numbers of locks
	// per transaction so rollback cost varies between candidates. Default 2.
	LocksPerTxn int

	CriticalitySkew float64

	// LeaseTicks is the lock lease length. A live service renews implicitly
	// each tick; a crashed one does not, so its locks become reclaimable.
	LeaseTicks int
}

// svcState is one simulated service's transaction progress.
type svcState struct {
	id           types.ServiceID
	plan         []types.LockID // the locks this transaction needs, in order
	step         int            // how many of them it has
	held         []types.LockID
	holdLeft     int
	blocked      types.LockID // "" when not blocked
	announced    bool         // whether we have already emitted a Waiting event
	crashed      bool
	completed    int
	workLost     int
	think        int // ticks to idle before starting the next transaction
	blockedSince int // tick at which this service became blocked
	aborts       int
	partitioned  bool
	reversed     bool
	startAt      int // tick before which this service does nothing
}

// Result is everything one run measured.
type Result struct {
	Policy string
	Config Config

	Completed      int       // transactions finished
	Deadlocks      int       // genuine cycles that formed
	RecoveryTicks  []float64 // per deadlock: ticks from formation to resolution
	Disrupted      []float64 // per deadlock: services affected by the chosen victim
	WorkLost       []float64 // per deadlock: lock acquisitions the victim must redo
	StaleDecisions int       // decisions made from a view that disagreed with reality

	// Pillar 1 measurements.
	TruePairs      map[[2]types.LockID]bool            // ground-truth dangerous orderings
	FlaggedPairs   map[[2]types.LockID]int             // pair -> tick confirmed
	FirstCycleTick map[[2]types.LockID]int             // pair -> tick a real cycle first involved it
	Plans          map[types.ServiceID][2]types.LockID // what each service really does
	// FlagAttribution records, per pair, whether the two services the flag
	// named genuinely use opposite orders. A flag can name the right lock pair
	// for the wrong reason, and only this catches that.
	FlagAttribution map[[2]types.LockID]bool

	// Overhead.
	MonitorOps    int // events folded into the graphs
	LockOrderSize int

	Reclaimed int // stale locks reclaimed after the injected crash

	// PartitionedService is whoever was cut off, and WastedAborts counts how
	// often the engine chose to abort a service it could not actually reach.
	PartitionedService types.ServiceID
	WastedAborts       int
	// FutileEpisodes counts *distinct* stretches of futility rather than every
	// re-detection. WastedAborts alone is misleading: the monitor re-examines
	// the graph every CheckInterval ticks, so one deadlock that never clears
	// inflates it by hundreds. This counts how many separate times the engine
	// began aiming at the unreachable service.
	FutileEpisodes int
}

// Sim is one simulation run.
type Sim struct {
	cfg    Config
	rng    *rand.Rand
	policy recovery.Policy

	svcs   []*svcState
	byID   map[types.ServiceID]*svcState
	owner  map[types.LockID]types.ServiceID // TRUE holder
	expiry map[types.LockID]int             // tick at which the lease lapses

	wf *depmonitor.WaitForGraph // OBSERVED
	lo *depmonitor.LockOrderGraph

	inflight []pendingEvent
	seq      map[types.ServiceID]uint64

	tick        int
	res         *Result
	criticality map[types.ServiceID]float64

	openCycles map[string]int // cycle participant-set -> tick it formed
	recorder   *explain.Recorder

	// inFutileEpisode tracks whether the previous decision already aimed at
	// the unreachable service, so FutileEpisodes counts stretches not ticks.
	inFutileEpisode bool

	// obsHeld is how many locks the monitor BELIEVES each service holds, built
	// only from events that have actually been delivered. The true count lives
	// in svcState.held.
	//
	// These must be kept apart. Feeding the policy the true count handed it
	// ground truth: the rollback cost it was given was exactly the work the
	// abort then destroyed, so its "estimate" could not be wrong and
	// calibration had nothing to learn. Worse, every policy in Section 8 was
	// being scored on decisions made with information no real deployment has.
	obsHeld map[types.ServiceID]int
}

type pendingEvent struct {
	deliverAt int
	ev        types.LockEvent
}

// NewSim builds a run. criticality is uniform; the interesting variation comes
// from topology, not from hand-tuned importance values.
func NewSim(cfg Config, policy recovery.Policy) *Sim {
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = 5
	}
	s := &Sim{
		cfg:        cfg,
		rng:        rand.New(rand.NewSource(cfg.Seed)),
		policy:     policy,
		byID:       make(map[types.ServiceID]*svcState),
		owner:      make(map[types.LockID]types.ServiceID),
		expiry:     make(map[types.LockID]int),
		wf:         depmonitor.NewWaitForGraph(),
		lo:         depmonitor.NewLockOrderGraph(),
		seq:        make(map[types.ServiceID]uint64),
		openCycles: make(map[string]int),
		obsHeld:    make(map[types.ServiceID]int),
		res: &Result{
			Policy:          policy.Name(),
			Config:          cfg,
			TruePairs:       make(map[[2]types.LockID]bool),
			FlaggedPairs:    make(map[[2]types.LockID]int),
			FirstCycleTick:  make(map[[2]types.LockID]int),
			Plans:           make(map[types.ServiceID][2]types.LockID),
			FlagAttribution: make(map[[2]types.LockID]bool),
		},
	}

	for i := 0; i < cfg.Services; i++ {
		id := types.ServiceID(svcName(i))
		st := &svcState{id: id}
		s.svcs = append(s.svcs, st)
		s.byID[id] = st
	}
	// Assign lock plans in groups that deliberately conflict.
	//
	// Each group of services shares one lock pair. Most of the group acquires
	// it in the order (A, B); a ReverseFrac minority acquires it (B, A). That
	// is what creates both genuine deadlocks and the ordering violations
	// Pillar 1 is supposed to catch. Drawing pairs at random instead almost
	// never produces a collision, which leaves nothing to detect.
	nGroups := cfg.LockPool / 2
	if nGroups < 1 {
		nGroups = 1
	}
	type pair struct{ a, b types.LockID }
	groups := make([]pair, 0, nGroups)
	for g := 0; g < nGroups; g++ {
		groups = append(groups, pair{
			a: types.LockID(lockName(2 * g)),
			b: types.LockID(lockName((2*g + 1) % cfg.LockPool)),
		})
	}
	for i, st := range s.svcs {
		g := groups[i%len(groups)]
		// Within each group, every (1/ReverseFrac)-th service runs the pair
		// backwards.
		reversed := false
		if cfg.ReverseFrac > 0 {
			every := int(1.0 / cfg.ReverseFrac)
			if every < 2 {
				every = 2
			}
			reversed = (i/len(groups))%every == 1
		}
		st.reversed = reversed
		if reversed {
			st.plan = []types.LockID{g.b, g.a}
			st.startAt = cfg.ReverseStartTick
		} else {
			st.plan = []types.LockID{g.a, g.b}
		}
		// Optionally lengthen the transaction. With two-lock transactions a
		// service caught in a cycle always holds exactly one lock, so every
		// candidate's rollback cost is 1 and the term carries no information
		// at all -- which makes any test of calibration vacuous. Longer and
		// unequal transactions give rollback cost something to vary over.
		// Only H9 uses this; the pair-based Pillar 1 metrics need the plain
		// two-lock workload and are skipped when it is on.
		if cfg.LocksPerTxn > 2 {
			extra := i % (cfg.LocksPerTxn - 1)
			for k := 0; k < extra; k++ {
				tail := types.LockID(lockName((2*len(groups) + (i+k)%cfg.LockPool) % cfg.LockPool))
				if tail != st.plan[0] && tail != st.plan[1] {
					st.plan = append(st.plan, tail)
				}
			}
		}
		st.think = s.rng.Intn(cfg.ThinkTicks + 1)
	}
	s.criticality = make(map[types.ServiceID]float64, len(s.svcs))
	for i, st := range s.svcs {
		s.res.Plans[st.id] = [2]types.LockID{st.plan[0], st.plan[1]}
		c := 1.0
		if cfg.CriticalitySkew > 0 && i%3 == 0 {
			c = 1 + cfg.CriticalitySkew
		}
		s.criticality[st.id] = c
	}
	s.recordTruePairs()
	return s
}

// SetRecorder attaches a decision recorder (Objective 4).
func (s *Sim) SetRecorder(r *explain.Recorder) { s.recorder = r }

func svcName(i int) string  { return "svc" + itoa(i) }
func lockName(i int) string { return "L" + itoa(i) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// recordTruePairs computes the ground truth for Pillar 1: a lock pair is
// genuinely dangerous when two services acquire it in opposite orders.
func (s *Sim) recordTruePairs() {
	seen := make(map[[2]types.LockID][]types.ServiceID)
	for _, st := range s.svcs {
		k := [2]types.LockID{st.plan[0], st.plan[1]}
		seen[k] = append(seen[k], st.id)
	}
	for k := range seen {
		rev := [2]types.LockID{k[1], k[0]}
		if _, ok := seen[rev]; ok {
			norm := k
			if norm[0] > norm[1] {
				norm = rev
			}
			s.res.TruePairs[norm] = true
		}
	}
}

// emit pushes an event through the observation layer, where it may be delayed
// or lost before the monitor ever sees it.
func (s *Sim) emit(kind types.EventKind, svc types.ServiceID, lock types.LockID) {
	s.seq[svc]++
	ev := types.LockEvent{Kind: kind, Service: svc, Lock: lock, Seq: s.seq[svc]}
	if s.cfg.FrameworkOff {
		return
	}
	if st, ok := s.byID[svc]; ok && st.partitioned {
		return // partitioned away: still running, but we cannot hear it
	}
	if s.cfg.ObsLoss > 0 && s.rng.Float64() < s.cfg.ObsLoss {
		return // lost in the network, permanently
	}
	delay := 0
	if s.cfg.ObsDelay > 0 {
		delay = s.rng.Intn(s.cfg.ObsDelay + 1)
	}
	s.inflight = append(s.inflight, pendingEvent{deliverAt: s.tick + delay, ev: ev})
}

// deliver folds every event whose delay has elapsed into the observed graphs.
func (s *Sim) deliver() {
	var still []pendingEvent
	for _, p := range s.inflight {
		if p.deliverAt > s.tick {
			still = append(still, p)
			continue
		}
		s.wf.Apply(p.ev)
		s.lo.Observe(p.ev)
		switch p.ev.Kind {
		case types.Acquired:
			s.obsHeld[p.ev.Service]++
		case types.Released:
			if s.obsHeld[p.ev.Service] > 0 {
				s.obsHeld[p.ev.Service]--
			}
		}
		s.res.MonitorOps++
	}
	s.inflight = still
}

// trueCycles finds deadlocks in the real state, for measurement only.
func (s *Sim) trueCycles() [][]types.ServiceID {
	g := depmonitor.NewWaitForGraph()
	var seq uint64
	for lock, owner := range s.owner {
		seq++
		g.Apply(types.LockEvent{Kind: types.Acquired, Service: owner, Lock: lock, Seq: seq})
	}
	for _, st := range s.svcs {
		if st.blocked != "" && !st.crashed {
			seq++
			g.Apply(types.LockEvent{Kind: types.Waiting, Service: st.id, Lock: st.blocked, Seq: seq})
		}
	}
	return g.Cycles()
}

// release drops one lock held by a service.
func (s *Sim) release(st *svcState, lock types.LockID) {
	for i, l := range st.held {
		if l == lock {
			st.held = append(st.held[:i:i], st.held[i+1:]...)
			break
		}
	}
	delete(s.owner, lock)
	delete(s.expiry, lock)
	s.emit(types.Released, st.id, lock)
}

// abort is the recovery action: the victim gives up everything and starts over.
func (s *Sim) abort(st *svcState) int {
	lost := len(st.held)
	for _, l := range append([]types.LockID(nil), st.held...) {
		s.release(st, l)
	}
	st.held = nil
	st.step = 0
	st.holdLeft = 0
	st.blocked = ""
	st.announced = false
	st.think = s.rng.Intn(s.cfg.ThinkTicks + 1)
	st.workLost += lost
	st.aborts++
	return lost
}

// Run executes the simulation and returns what it measured.
func (s *Sim) Run() *Result {
	for s.tick = 0; s.tick < s.cfg.Ticks; s.tick++ {
		if s.cfg.CrashAt > 0 && s.tick == s.cfg.CrashAt {
			s.injectCrash()
		}
		s.updatePartition()
		s.stepServices()
		s.reclaimExpired()
		s.deliver()

		if !s.cfg.FrameworkOff && s.tick%s.cfg.CheckInterval == 0 {
			s.runMonitorInterval()
		}
		s.measureTrueDeadlocks()
	}
	s.res.LockOrderSize = s.lo.Size()
	return s.res
}

// injectCrash kills whichever service holds the most locks, without releasing
// them. This is the stale-lock scenario: the locks stay held until something
// reclaims them.
func (s *Sim) injectCrash() {
	var victim *svcState
	for _, st := range s.svcs {
		if st.crashed {
			continue
		}
		if victim == nil || len(st.held) > len(victim.held) {
			victim = st
		}
	}
	if victim != nil && len(victim.held) > 0 {
		victim.crashed = true
		victim.blocked = ""
	}
}

// reclaimExpired is the fault manager's job inside the simulation: a lock
// whose lease has lapsed is taken back. Live services renew implicitly each
// tick; a crashed one cannot, so this is what stops a crash wedging the system
// forever.
func (s *Sim) reclaimExpired() int {
	n := 0
	for lock, exp := range s.expiry {
		if s.tick < exp {
			continue
		}
		owner, held := s.owner[lock]
		if !held {
			delete(s.expiry, lock)
			continue
		}
		if st, ok := s.byID[owner]; ok {
			for i, l := range st.held {
				if l == lock {
					st.held = append(st.held[:i:i], st.held[i+1:]...)
					break
				}
			}
		}
		delete(s.owner, lock)
		delete(s.expiry, lock)
		s.emit(types.Released, owner, lock)
		n++
	}
	s.res.Reclaimed += n
	return n
}

// stepServices advances every live service by one step.
func (s *Sim) stepServices() {
	for _, st := range s.svcs {
		if st.crashed {
			continue
		}
		// A live holder renews its leases simply by still being alive.
		if s.cfg.LeaseTicks > 0 {
			for _, l := range st.held {
				s.expiry[l] = s.tick + s.cfg.LeaseTicks
			}
		}
		if s.tick < st.startAt {
			continue
		}
		if st.think > 0 {
			st.think--
			continue
		}

		switch {
		case st.holdLeft > 0:
			st.holdLeft--
			if st.holdLeft == 0 {
				for _, l := range append([]types.LockID(nil), st.held...) {
					s.release(st, l)
				}
				st.held = nil
				st.step = 0
				st.announced = false
				st.completed++
				st.think = s.rng.Intn(s.cfg.ThinkTicks + 1)
				s.res.Completed++
			}
		case st.step < len(st.plan):
			want := st.plan[st.step]
			if owner, taken := s.owner[want]; taken && owner != st.id {
				if st.blocked == "" {
					st.blockedSince = s.tick
				}
				st.blocked = want
				if !st.announced {
					s.emit(types.Waiting, st.id, want)
					st.announced = true
				}
				continue
			}
			s.owner[want] = st.id
			if s.cfg.LeaseTicks > 0 {
				s.expiry[want] = s.tick + s.cfg.LeaseTicks
			}
			st.held = append(st.held, want)
			st.step++
			st.blocked = ""
			st.announced = false
			s.emit(types.Acquired, st.id, want)
			if st.step == len(st.plan) {
				st.holdLeft = s.cfg.HoldTicks
			}
		}
	}
}

// runMonitorInterval closes a reporting interval: confirm Pillar 1 flags, then
// let the policy act on any cycle it can see.
func (s *Sim) runMonitorInterval() {
	for _, f := range s.lo.Tick() {
		key := [2]types.LockID{f.LockA, f.LockB}
		if _, already := s.res.FlaggedPairs[key]; !already {
			s.res.FlaggedPairs[key] = s.tick
			// Check the flag against what the two named services actually do.
			pa, oka := s.res.Plans[f.ServiceA]
			pb, okb := s.res.Plans[f.ServiceB]
			s.res.FlagAttribution[key] = oka && okb &&
				pa[0] == f.LockA && pa[1] == f.LockB &&
				pb[0] == f.LockB && pb[1] == f.LockA
		}
	}

	observed := s.wf.Cycles()
	if len(observed) == 0 {
		return
	}
	cycle := observed[0]

	snap := recovery.Snapshot{
		Cycle:            cycle,
		Graph:            s.wf,
		RollbackCost:     make(map[types.ServiceID]float64),
		WaitTime:         make(map[types.ServiceID]float64),
		AbortCount:       make(map[types.ServiceID]float64),
		Criticality:      s.criticality,
		PartitionSuspect: s.partitionSuspects(),
	}
	for _, id := range cycle {
		if st, ok := s.byID[id]; ok {
			// Rollback cost is how much work the monitor believes would be
			// thrown away. Deliberately the observed count, not len(st.held):
			// a real monitor only knows what has reached it.
			snap.RollbackCost[id] = float64(s.obsHeld[id])
			snap.AbortCount[id] = float64(st.aborts)
			if st.blocked != "" {
				snap.WaitTime[id] = float64(s.tick - st.blockedSince)
			}
		}
	}

	d, err := s.policy.Decide(snap)
	if err != nil {
		// ErrNotYet is the timeout baseline declining to act because nothing
		// has been stuck long enough yet. That waiting is the cost of the
		// approach and must be allowed to show up in its recovery time.
		return
	}

	// Was this decision made from a view that matched reality?
	if !s.cycleIsReal(cycle) {
		s.res.StaleDecisions++
	}

	victim, ok := s.byID[d.Chosen.Service]
	if !ok {
		return
	}
	// An abort aimed at a partitioned service is wasted: the instruction does
	// not reach it, so it keeps holding its locks and the deadlock persists.
	if victim.partitioned {
		if !s.inFutileEpisode {
			s.res.FutileEpisodes++
			s.inFutileEpisode = true
		}
		s.res.WastedAborts++
		return
	}
	s.inFutileEpisode = false
	disrupted := recovery.BlastRadius(snap, victim.id)
	lost := s.abort(victim)

	if s.recorder != nil {
		s.recorder.Record(s.tick, d, disrupted)
	}
	if ap, isAdaptive := s.policy.(*recovery.AdaptivePolicy); isAdaptive {
		// Feed the real outcome back so the weights recalibrate.
		// The feedback target is the work the abort actually destroyed, which
		// the policy did not know when it decided. Feeding back `disrupted`
		// instead was circular: the simulator computes it as blast radius,
		// which is already one of the policy's own features.
		ap.Observe(float64(lost))
	}

	s.res.Disrupted = append(s.res.Disrupted, disrupted)
	s.res.WorkLost = append(s.res.WorkLost, float64(lost))
}

// cycleIsReal checks an observed cycle against the true state.
func (s *Sim) cycleIsReal(cycle []types.ServiceID) bool {
	real := s.trueCycles()
	for _, rc := range real {
		if sameSet(rc, cycle) {
			return true
		}
	}
	return false
}

func sameSet(a, b []types.ServiceID) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]types.ServiceID(nil), a...)
	y := append([]types.ServiceID(nil), b...)
	sort.Slice(x, func(i, j int) bool { return x[i] < x[j] })
	sort.Slice(y, func(i, j int) bool { return y[i] < y[j] })
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// measureTrueDeadlocks tracks each distinct deadlock, against reality, from
// the tick it formed to the tick it was broken.
//
// Cycles are keyed by their participant set rather than tracking "is any cycle
// present". Under load several deadlocks overlap in time, and the simpler
// version merges them into one long-running event that never appears to
// resolve, which loses every recovery-time sample.
func (s *Sim) measureTrueDeadlocks() {
	cycles := s.trueCycles()

	now := make(map[string][]types.ServiceID, len(cycles))
	for _, c := range cycles {
		now[cycleKey(c)] = c
	}

	// Newly formed cycles.
	for k, c := range now {
		if _, open := s.openCycles[k]; open {
			continue
		}
		s.openCycles[k] = s.tick
		s.res.Deadlocks++
		for _, id := range c {
			if st, ok := s.byID[id]; ok && len(st.plan) == 2 {
				pk := [2]types.LockID{st.plan[0], st.plan[1]}
				if pk[0] > pk[1] {
					pk[0], pk[1] = pk[1], pk[0]
				}
				if _, seen := s.res.FirstCycleTick[pk]; !seen {
					s.res.FirstCycleTick[pk] = s.tick
				}
			}
		}
	}

	// Cycles that have gone.
	for k, since := range s.openCycles {
		if _, still := now[k]; still {
			continue
		}
		s.res.RecoveryTicks = append(s.res.RecoveryTicks, float64(s.tick-since))
		delete(s.openCycles, k)
	}
}

// cycleKey names a cycle by its sorted participant set.
func cycleKey(c []types.ServiceID) string {
	x := append([]types.ServiceID(nil), c...)
	sort.Slice(x, func(i, j int) bool { return x[i] < x[j] })
	var b []byte
	for _, s := range x {
		b = append(b, []byte(s)...)
		b = append(b, '|')
	}
	return string(b)
}

// updatePartition starts and ends the simulated network partition.
func (s *Sim) updatePartition() {
	if s.cfg.PartitionFor <= 0 || s.cfg.PartitionAt <= 0 {
		return
	}
	switch s.tick {
	case s.cfg.PartitionAt:
		// Partition the service currently holding the most locks: the one
		// whose unreachability matters most.
		var victim *svcState
		for _, st := range s.svcs {
			if st.crashed {
				continue
			}
			if victim == nil || len(st.held) > len(victim.held) {
				victim = st
			}
		}
		if victim != nil {
			victim.partitioned = true
			s.res.PartitionedService = victim.id
		}
	case s.cfg.PartitionAt + s.cfg.PartitionFor:
		for _, st := range s.svcs {
			st.partitioned = false
		}
	}
}

// partitionSuspects reports which services have gone silent.
//
// This mirrors what the fault manager does with real time: a service we have
// not heard from recently might be crashed or might be unreachable, and we
// cannot tell which. Either way it is a poor victim, because an abort sent to
// it may never arrive.
func (s *Sim) partitionSuspects() map[types.ServiceID]bool {
	out := make(map[types.ServiceID]bool)
	for _, st := range s.svcs {
		if st.partitioned {
			out[st.id] = true
		}
	}
	return out
}
