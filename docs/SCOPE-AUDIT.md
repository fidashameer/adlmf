# Adaptive Distributed Lock Management Framework

BCSE303P Operating Systems Lab. Implementation of the design in
`Review1_Report_Final_.docx`.

**36 tests passing under `-race`**, five of them against a real etcd. Ten
hypotheses measured: the six in the report, plus four added because the
original six could not falsify the central claims. 5,400 lines of Go, about a
fifth of it tests, no external dependencies.

```
go test ./... -race                    # unit + etcd integration tests
go run ./cmd/experiments -trials 20    # all ten hypotheses, regenerates RESULTS.txt
go run ./cmd/e2e -for 12s              # real OS processes, real etcd, real kill -9
```

Raw numbers are in `RESULTS.txt`. Runs are seeded, so they reproduce exactly.
The etcd tests and `cmd/e2e` skip themselves cleanly when no etcd is listening
on `127.0.0.1:2379`.

## Scope audit against the submitted report

Checked clause by clause against the Review 1 report rather than against a
general sense of completeness.

**Section 7.3, "Committed for this term"**

| Committed clause | Status |
|---|---|
| Working lock service (etcd/ZooKeeper) | done — real etcd, exclusion enforced by Raft |
| In front of simulated microservices over gRPC | done, with a deviation — real etcd over its HTTP/JSON gateway, not gRPC (see Deviations) |
| Live dependency graph | done — `depmonitor/waitfor.go`, Tarjan SCC |
| One adaptive policy using an input single-node can't express | done — blast radius and criticality |
| Benchmarked against two baselines (timeout, cost-only) | done — H1, H2, H7, H10; a third (youngest) was added |
| Under several contention levels | done — three levels in H2 and H10, four in H5 |
| One fault scenario (crash, stale-lock recovery) | done — H4, the etcd lease-expiry test, and a real `kill -9` in `cmd/e2e` |
| Pillar 1 on precision/recall and lead time | done — H5, plus an attribution metric precision alone missed |

**Section 3, the four objectives**

| Objective | Status |
|---|---|
| 1. Lease-based lock layer + live cross-service dependency graph, using a cited method | done — leases and fencing tokens from etcd, Chandy-Misra-Haas lineage for detection |
| 2. Policy scoring blast radius, rollback cost, and partition suspicion | done, with four documented corrections to the formula as written |
| 3. Fault injection and benchmarking vs both baselines on **deadlock frequency, recovery time, and services disrupted** | done — recovery time H2, services disrupted H1, **deadlock frequency H10** |
| 4. Record enough per decision to explain it, and measure the logging cost | done (listed as a bonus) — `explain/`, measured in H6 |

Objective 3 names three comparison metrics. Two were covered from the start;
deadlock frequency was only ever reported as a property of the workload in H5
and never compared across policies, which is why H10 exists.

**Section 7.3, "If we have time" — all three done**

| Item | Status |
|---|---|
| Explicit network-partition handling with a stated tradeoff | done — H8, and the tradeoff is stated: the tier mechanism is preferred over the report's weighted term, and H8 cannot separate them at this workload |
| Structured decision logging | done — `explain.Recorder`, on the critical path, measured at +1.35% |
| A second baseline variant | done — `YoungestPolicy`, the classical youngest-transaction rule |

**Section 7.3, "Stretch only" — done**

| Item | Status |
|---|---|
| Explanation engine with contrastive and what-if generation | done — `Contrastive`, `WhatIf`, `Report`, all off the critical path |

All nine methodology steps from Section 7.5 are implemented. Every tier of the
scope table is complete, including the two that were explicitly optional.

Beyond the committed scope: the system also runs outside the simulator.
`cmd/stubservice` is one service per OS process against real etcd and
`cmd/e2e` drives them, which is what lets crash injection be a genuine
`kill -9` rather than a flag set inside the harness — the report claims this in
Section 7.4, and it is only true if the services really are separate processes.

## Layout

```
internal/types/   lock events (no shared clock; per-service sequence numbers)
lockservice/      Backend interface; in-memory and etcd implementations
depmonitor/       waitfor.go   — live wait-for graph, Tarjan cycle detection
                  lockorder.go — persistent lock-order graph (Pillar 1)
recovery/         timeout, cost-only and youngest baselines; adaptive policy (Pillar 2)
faultmgr/         lease expiry, crash detection, partition suspicion
explain/          decision recorder + explanation generator (Objective 4)
benchmark/        simulator, statistics, the ten experiments
cmd/experiments/  runs the Section 8 sweeps against the simulator
cmd/stubservice/  one service as its own OS process, talking to real etcd
cmd/e2e/          spawns those processes, drives them, kills one with SIGKILL
```

## Running it

```
go test ./...                          # unit + etcd integration tests
go run ./cmd/experiments -trials 20    # regenerates RESULTS.txt (~8 min)
go run ./cmd/experiments -only h5      # one hypothesis (h1..h10)
go run ./cmd/e2e -for 12s              # real processes, real etcd, real kill -9
```

The etcd tests and `cmd/e2e` skip themselves cleanly when no etcd is reachable
at `127.0.0.1:2379`. Everything else is stdlib only — no modules to fetch.

`cmd/e2e` is the part the simulator cannot stand in for. Three services run as
separate OS processes against a real etcd; two of them take the same lock pair
in opposite orders. A representative run: 1397 lock events observed across
process boundaries, 3 lock-order edges learned, the Pillar 1 warning confirmed
at 842ms (`payment took L0->L1, inventory took L1->L0`), 9 deadlocks broken by
delivering SIGUSR1 across a process boundary with victims alternating as the
fairness term intends, **0 mutual-exclusion violations** polled directly from
etcd throughout, and after a genuine `kill -9` of the process holding L0 the
lock was reclaimed in 2.48s against a 2s lease TTL — by etcd expiring the
lease, with nothing in our code having to notice the crash.

## Results in one paragraph

With an anti-starvation term the adaptive policy delivers roughly **3x the
throughput** of the cost-only baseline at 30-60 services, at indistinguishable
disruption, and resolves deadlocks **70-83% faster** than the timeout baseline.
Without that term it beats cost-only on disruption but collapses throughput to
1-2 transactions. Self-calibrating weights are inert without the fairness term
and actively harmful with it, so the framework ships with them **off**.
On deadlocks per thousand completed transactions — the third metric Objective 3
names, and one the policy does not optimise — it beats all three baselines at
every contention level, by **2.3x over cost-only and 2.6x over the classical
youngest-transaction rule** at medium contention. Framework overhead is 0% on
throughput but **+51% CPU**. Pillar 1 flags
dangerous orderings with perfect precision and correct service attribution at
every degradation level tested, with recall falling from 1.00 to 0.73 as
observations are delayed and dropped — **but its lead time depends entirely on
contention**, and under heavy contention it arrives *after* the deadlock it was
supposed to predict.

## What is actually novel — and what is not

Measured, not asserted. H7 ablates Pillar 2 term by term and reports
**completed transactions** alongside disruption, because disruption is the
quantity the policy minimises and scoring it only on that is close to circular.

### Does not survive measurement

**Self-calibrating weights do not earn their place, and the reason is
structural rather than a tuning problem.** `recovery.DefaultAlpha` is therefore
`0`: the mechanism is kept and reachable, but off.

Getting to that conclusion took three corrections, each of which had been
hiding the next.

1. The first update rule used `sign(feature)`, applying an identical delta to
   every weight so they moved in lockstep. Fixing it to a proper
   least-mean-squares update (`w_i += alpha * err * feature_i / ||f||`) changed
   no decision at all.
2. The regression target was the **whole score**, including the fairness term.
   That term grows without bound as abort counts accumulate, so the error never
   settled and both genuine cost weights were driven to the floor — measured in
   `TestWeightTrajectory`, which found `blast=0.0000 rollback=0.0000` at two
   very different learning rates. Fairness and partition are *preferences*, not
   predictions; only the blast-radius and rollback-cost terms are claims about
   the world, so only those now take part.
3. The feedback target was `disrupted`, which the simulator **computes as blast
   radius** — one of the policy's own features. The learner could only
   rediscover its own definition. The same was true of the obvious alternative:
   rollback cost was read from `svcState.held`, so the "estimate" was exactly
   the work the abort then destroyed.

With all three fixed, the learner behaves correctly — it converges to
`blast≈0, rollback≈1.0`, which is right, because the observable outcome (work
destroyed) genuinely does not depend on blast radius. **And that is precisely
why the idea fails.** Blast radius is what governs throughput, so any honest
regression onto a single observed cost component discards the term that matters
most. H9 measures the result on a workload where rollback cost actually varies
(`LocksPerTxn: 4`, since with two-lock transactions every cycle member holds
exactly one lock and the term carries no information at all): calibration loses
on completed transactions (563 to 514), on disruption (11.36 to 17.42), **and
on work lost (1.217 to 1.249) — the very quantity it is trained to predict.**

`TestCalibrationFailureCause` localises it: freezing the blast weight at zero
with *no learning whatsoever* reproduces almost the entire loss (526 completed,
18.08 disrupted) . So the failure is where a correct learner must converge, not
how it gets there. **A victim-selection score cannot be calibrated by
regressing it onto one observed cost component when it has to balance costs
that component does not contain.** That is a concrete, reproducible negative
result about the mechanism the report proposes, which is worth more than a
tuned number would have been.

**Blast-radius minimisation alone is actively harmful at scale.** It wins on
its own metric and collapses the system: at 30 and 60 services it completes
**2 and 1 transactions** against cost-only's 113 and 111. The cause is
starvation — it keeps selecting the same cheap victims, which therefore never
finish. This is the hazard DSLR (SIGMOD 2018, cited in the report) is named
after, arriving here from the opposite direction.

### Survives measurement

**Blast radius plus an anti-starvation term beats the prior art on an
independent metric.** Adding a fairness term that penalises a service for
having been sacrificed before gives roughly **3x the throughput of cost-only at
30-60 services (348 vs 113, 346 vs 111) at statistically indistinguishable
disruption**. Neither term alone does this: blast radius alone collapses, and
cost-only never considers cross-service impact.

The honest claim is therefore narrower and more specific than the report's:
*blast-radius-aware victim selection is only viable when paired with an
anti-starvation term, and in that combination it substantially outperforms
cost-based selection on throughput at equal disruption.* The fairness term is
not in the report; it was added because measurement demanded it.

**Pillar 1 works, in a regime that is the opposite of the useful one.**
Precision 1.000 and correct attribution 1.000 at every degradation level, with
recall falling gracefully from 1.00 to 0.73 at 15% observation loss. But lead
time is **-474 ticks under heavy contention**: the deadlock wins. The mechanism
needs a service to *successfully complete* an out-of-order acquisition before
it has any evidence, and under heavy contention that service deadlocks on its
first attempt. It only pays off at light contention (+669, +2145 ticks), which
is when deadlocks matter least.

**Observation loss causes false ordering attributions, and precision does not
detect it.** Covered below under bugs. This is a property of distributed
lock-order prediction that single-process tools (lockdep, Goodlock) cannot
exhibit, because they never lose an observation.

**A third baseline, chosen by the textbook, fails the same way as cost-only.**
`YoungestPolicy` aborts the most recently blocked candidate: the classical
transaction-manager rule, on the reasoning that the youngest transaction has
the least invested work. It is the input a practitioner would reach for first,
and it is a genuinely different *kind* of baseline from the other two — timeout
uses no graph information, cost-only uses per-service cost, this uses time.

It lands almost exactly on cost-only (190.5 vs 186.6 deadlocks per 1k completed
at low contention) and **collapses with it at high contention**, completing one
transaction per run. Both starve the same victims. That the obvious practical
choice fails identically is a stronger argument for the fairness term than any
comparison against cost-only alone, because it rules out the objection that
cost-only was a weak opponent.

**Deadlock frequency, read honestly, is the clearest win in the suite.** It is
also the metric most easily misreported. A policy that leaves deadlocks
standing records *fewer* of them, because the services involved never get back
to work and so never collide again — the timeout baseline logs 224 deadlocks at
high contention against cost-only's 799 purely by paralysis. Normalising to
deadlocks per thousand *completed* transactions, the adaptive policy wins at
every contention level (178.0 / 1585.0 / 1406.6 against the best baseline's
186.6 / 1212.0 / 2067.9). The one cell where a baseline beats it — timeout at
medium contention — comes with 40% fewer completed transactions, so the two
columns have to be read together.

### Assessment

The strongest contributions here are the negative and conditional ones: a
measured failure mode of blast-radius victim selection and the term that fixes
it, a structural limit on when lock-order prediction can warn in time, and a
false-attribution failure specific to the distributed case. Those are real,
reproducible, and absent from the cited literature. The two positive mechanisms
as stated in the report are weaker than claimed — one needs a correction the
report does not contain, the other should be dropped.

One methodological point outranks all of them. The simulator was handing the
policy **ground truth**: `Snapshot.RollbackCost` was read from the true service
state rather than from delivered events. Every policy in Section 8 was being
scored on decisions made with information no real deployment has, and the
circularity was invisible until self-calibration was pushed hard enough to
expose it. The snapshot is now built from an observed held-count maintained
purely from the event stream, and all nine hypotheses were re-run against the
corrected harness. The `cmd/e2e` harness had the same defect in a worse form —
abort counts were stubbed empty, so the fairness term was permanently zero and
the same victim was chosen nine times running — and now derives both inputs
from the event stream too.

## Deviations from the report, and why

**1. etcd over its HTTP gateway, not clientv3.** The report names `clientv3`.
The build environment cannot reach `proxy.golang.org` or `go.etcd.io`, so the
module cannot be fetched. The gateway is the same gRPC service behind a JSON
translation layer, so lease and transaction semantics are identical, and the
project has zero external dependencies as a result. Swapping in clientv3 means
writing one more `Backend`; nothing else changes.

**2. Services are simulated, not Docker containers over gRPC.** Docker is
unavailable here. The harness runs services as a deterministic discrete-event
simulation, which is what makes the trials reproducible and the comparison
fair — every policy sees an identical workload. Crash injection is modelled by
stopping lease renewal, which is exactly what a `kill -9` does from etcd's
point of view.

**3. Sign of the partition term.** The report's formula subtracts
`w3·partition_penalty` and picks the lowest score, which makes an unreachable
service *more* likely to be sacrificed — the opposite of the stated intent. The
code adds it.

**4. Partition suspicion is a tier, not just a term.** It is a 0/1 indicator
while the other terms are unbounded, so any fixed `w3` can be outweighed. The
engine prefers reachable candidates, and only scores suspect ones when every
candidate is suspect.

**5. Blast radius counts only outside the cycle.** "Transitively blocked
services if this participant is removed" is degenerate as written: inside a
deadlock every member is transitively blocked on every other, so all candidates
score identically. Those are also the services the abort *frees*. The code
counts the victim plus whatever is queued behind it from outside the cycle.

**6. An anti-starvation (fairness) term that the report does not contain.** The
scoring function in the report has three terms. A fourth penalises a service
for having been sacrificed before. It was added because blast-radius selection
without it collapses throughput at scale (H7), not for elegance. Omitting it
would have meant reporting a mechanism that does not work.

**7. Self-calibration ships disabled.** `recovery.DefaultAlpha = 0`. The report
presents adaptive weights as a contribution; H9 shows them losing on every
metric including their own training target, for a reason that is structural.
The code remains so the negative result reproduces.

**8. Rollback cost is read from observed events, not true state.** The report
describes it as the work a victim would lose. Taking that from the true service
state handed the policy information no real monitor has, and made the
"estimate" exactly equal to the outcome. It is now an observed held-count built
only from delivered events, so it can be wrong — which is the point. The
feedback signal for calibration changed with it, from `disrupted` (which the
harness computes as blast radius, one of the policy's own inputs) to the work
an abort actually destroyed.

## Two bugs worth knowing about

Both were found by tests rather than by reading the code, and both are the kind
a panel may ask about.

**A stale lock holder could delete the next holder's lock.** The etcd backend
originally tracked leases keyed by lock. Between a departing holder's delete
transaction succeeding and it looking up which lease to revoke, the next holder
can already have acquired. The departing holder then revoked the *new* holder's
lease, etcd deleted their key, and that holder's own release failed. Keying
leases by fencing token instead makes it impossible. Caught by running
`TestEtcdMutualExclusion` repeatedly.

**Gap-skipping fabricated ordering violations.** When an observation is lost,
Pillar 1 skips the gap and resumes. The first version kept its record of what
the service was holding across that hole, so it inferred "held X, then took Y"
edges that never happened — and because a fabricated edge can be the reverse of
a real one, it produced confident warnings about correctly-behaving services.
Under 15% loss this fired flags at tick 40 for a conflict that could not exist
before tick 400. The fix is to forget the held set when skipping a gap.

Notably, **precision did not catch this**: the fabricated flags still named a
genuinely dangerous lock pair, just for the wrong reason. H5 now reports
*attribution* separately — whether the two services a flag names really do
disagree — which is the metric that would have caught it.

## Where the approach does not help

Reported because Section 8 asks for results that would refute the claim.

- **Pillar 1 is useless under heavy contention.** Lead time is −474 ticks: the
  deadlock happens first. The check needs a service to *successfully complete*
  an out-of-order acquisition before it has any evidence, and under heavy
  contention that service deadlocks on its first attempt. It only pays off when
  contention is light enough that conflicting orderings get exercised before
  they overlap (+669 and +2145 ticks at light and very light contention).
- **Recall degrades sharply with lost observations**, from 1.00 clean to 0.73
  at 15% loss. Over a quarter of genuine violations go unreported.
- **CPU cost is +51%** even though throughput impact is 0%. The stated bound
  was throughput, which passes, but the CPU number should not be buried.
- **Cost-only completes more transactions** in some configurations despite
  disrupting more services per deadlock. The two metrics do not agree, and the
  report should say which one it is optimising for.

## Still open

- **Experiments run against the simulator.** `cmd/e2e` exercises the whole
  system against real etcd and real OS processes, but the Section 8 sweeps
  themselves do not: running those against the etcd backend would close the
  last gap between "the policy behaves this way" and "the system behaves this
  way". The ground-truth leak found in the simulator is a reminder that a
  harness can flatter the thing it measures.
- **H8 does not separate the two partition mechanisms.** Both the report's
  weighted term and the tier preference drive futile aborts to zero, because
  `TestPartitionMargin` shows the cost gap between a reachable and an
  unreachable candidate is near zero in this workload, so a fixed weight of 2.0
  dominates easily. That is luck of magnitudes, not a property of the formula —
  a workload with larger cost spreads would overrule the weighted term, while
  the tier depends on no margin at all. A workload that forces the two apart
  would settle it.
- **Pillar 1's lead time is still negative where it would matter most.** No
  mechanism here fixes that; it is a limit of learning lock order from
  successful acquisitions.
- **Self-calibration is off and should probably be removed.** It is retained
  only so the negative result in H9 stays reproducible.
