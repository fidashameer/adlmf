# Understanding this project

Written to be read start to finish. It covers what the project does, how each
piece works, why each technology was chosen over the alternatives, how it was
tested, and what the testing actually showed — including the parts that did not
work.

---

## 1. The problem

A lock is a way of saying "only one person may touch this at a time". If two
services both need to update a customer's order and their stock level, they each
take a lock on each thing, do the work, and release.

A deadlock happens when two services each hold something the other is waiting
for. Service A holds the order lock and wants the stock lock. Service B holds
the stock lock and wants the order lock. Neither will let go, because neither
has finished. Both wait forever.

Nothing resolves this on its own. Someone has to be forcibly stopped — their
work thrown away — so the other can finish. The two questions are therefore:

1. How do you notice a deadlock has happened?
2. Once you have noticed, **who do you kill?**

Question 1 is a solved problem and the project does not claim otherwise.
Question 2 is where the project lives.

### Why the distributed case is different

Inside a single database, all of this happens in one process. The lock manager
sees every lock operation instantly, in order, with no possibility of missing
one. Picking a victim is a local decision about local work.

Across services on separate machines, three things change, and every design
decision in this project traces back to one of them:

- **There is no shared clock.** Two machines' clocks disagree, and by an unknown
  amount. You cannot order two services' actions by timestamp.
- **Observations are unreliable.** A report of "I took lock X" may arrive late,
  arrive after a report that was actually generated later, or never arrive.
- **Services affect each other.** Killing one service can leave a queue of other
  services stranded behind it. In one database there is nobody else to strand.

That third point is the opening the project goes through.

---

## 2. What was already solved, and is not claimed

Being precise about this matters, because claiming any of it invites a short and
unpleasant conversation with a panel.

**Detecting deadlocks across machines.** Studied since the early 1980s. The
Chandy-Misra-Haas algorithm is the standard reference; Elmagarmid's survey maps
the rest. The project applies this. It does not improve it.

**Lease-based locks.** Gray and Cheriton (1989) introduced the idea of a lock
with an expiry that must be renewed. Google's Chubby (2006) is the reference
implementation of a lock service built on consensus, and both etcd and ZooKeeper
descend from it. The project uses this directly.

**Picking the cheapest victim.** Databases have done this for decades, and there
is a patent on a distributed version (US 9,104,989 B2). This is the *prior art
the project competes against* — one of the baselines in the measurements is
exactly this strategy.

**Checking lock ordering inside one program.** Linux has `lockdep`; Java has
Goodlock. Both watch the order in which locks are taken and warn when two code
paths disagree. Decades old, widely deployed.

**Recent work.** DSLR (SIGMOD 2018) builds a faster decentralised lock manager
over RDMA. HAWK (VLDB 2025) speeds up deadlock detection in large distributed
databases by predicting which nodes will contend. Both make detection or
granting faster. **Neither changes how the victim is chosen**, which is the gap
the project occupies.

---

## 3. The idea

Existing victim selection asks: *which service is cheapest to roll back?* It
looks at one service at a time, in isolation.

The project's argument is that in a connected system this is the wrong question,
because the cheapest service to kill may be the one that twelve other services
are queued behind. Killing it is locally cheap and globally expensive.

So the project adds two things.

**Pillar 1 — warn before anyone is stuck.** Watch the *order* in which services
take locks. If service A reliably takes lock X then lock Y, and service B takes
lock Y then lock X, those two will deadlock as soon as their timing overlaps.
That is knowable *before* either of them blocks. Raise a warning then.

**Pillar 2 — choose the victim by total damage.** Score each candidate by how
much disruption killing it would cause across the whole system — itself plus
everything stranded behind it — rather than by its own local cost.

Those two are the contribution. Everything else in the repository exists to
support them or to measure them.

---

## 4. Pillar 1, in detail

### The core observation

Every lock event a service reports is one of three things: `Acquired`, `Waiting`
or `Released`. From a stream of these you can build two completely different
pictures, and **keeping them separate is the most important design decision in
the project.**

**Picture one: who is stuck right now.** If service A is waiting for a lock that
service B holds, draw an arrow A → B. A cycle in those arrows is a deadlock.
This is the *wait-for graph*. It is about the present, and when a lock is
released the corresponding arrow disappears.

**Picture two: what order locks get taken in.** If a service holds lock X and
then acquires lock Y, record the edge X → Y. **This is never deleted.** When the
service releases both locks, the fact that it once took them in that order is
still true, and still dangerous, because it will do the same thing on its next
transaction.

That second picture is Pillar 1. It is the one that can warn early, precisely
because it is historical rather than live.

### How an ordering edge gets recorded

In `depmonitor/lockorder.go`, when an `Acquired` event is applied:

```
for each lock this service already holds:
    record edge (that lock -> the newly acquired lock)
then add the new lock to its held set
```

So a service holding X that acquires Y produces the edge X → Y. The edge is
stored against the service that established it, because the next step needs to
know *who* did it.

### How a warning is raised

Whenever an edge X → Y is recorded, the code checks whether any **different**
service has previously established Y → X. If so, those two services disagree
about ordering, and that pair of locks is flagged.

The "different service" check matters. A single service that consistently takes
X then Y, every time, is perfectly safe — it can never deadlock against itself
on that pair. The danger needs two parties with opposite habits.

The warning names both locks and both services, which is why the demo prints
lines like:

```
order-violation locks=(L0,L1) payment took L0->L1, inventory took L1->L0
```

### The two hard parts, and how they are handled

This is where the distributed case stops resembling `lockdep`.

Before the mechanisms, here is the thing they all exist to prevent. Suppose the
`payment` service does this:

```
seq 1   Acquired L0
seq 2   Released L0
seq 3   Acquired L1
seq 4   Released L1
```

Read it carefully: payment takes L0, **gives it back**, and only then takes L1.
It never holds both at the same time. So there is no ordering edge here at all —
payment is not nesting these locks, and it cannot deadlock with anybody over
this pair.

Now lose event 2 in transit. The monitor sees 1, then 3. Its reasoning:

- event 1: payment now holds L0
- event 3: payment holds L0 and is acquiring L1 → **record edge L0 → L1**

That edge never happened. And now the damage compounds. If `inventory`
legitimately does nest these the other way — holds L1, takes L0 — that is a real
edge L1 → L0. The monitor now has L0 → L1 and L1 → L0 from two different
services and raises a confident warning:

```
order-violation locks=(L0,L1) payment took L0->L1, inventory took L1->L0
```

Payment is innocent. The accusation is manufactured entirely out of a missing
message. Worse, it is manufactured about a lock pair that genuinely *is*
contended, which is why it was so hard to catch — see section 10.

Three mechanisms keep this from happening.

**Problem: events arrive out of order.** If a service's "released X" report
overtakes its earlier "acquired Y" report, applying them as they land produces
an ordering that never happened.

Same four events, nothing lost, but they arrive in the order 1, 3, 2, 4 — which
a network is perfectly entitled to do. Applied as they land, the monitor records
L0 → L1 when 3 arrives, and by the time 2 ("released L0") turns up a moment
later the fabricated edge is already in the graph and never deleted. The
accusation above gets raised even though every message was delivered.

**Solution: a reorder buffer, per service.** Each service stamps its own events
with a sequence number that only ever increases. The monitor holds incoming
events in a buffer and applies them only in sequence order — if event 7 arrives
while 6 is still missing, 7 waits. Duplicates and very late stragglers (sequence
numbers already applied) are dropped.

So in the 1, 3, 2, 4 case: event 1 applies immediately. Event 3 arrives, but the
monitor is waiting for 2, so **3 goes in the buffer and nothing is recorded**.
Event 2 arrives, applies, and now 3 is next in line so it drains out of the
buffer and applies too — in the right order. Payment is correctly seen as having
released L0 before touching L1, and no edge is created.

This is why the sequence number is per-service rather than global. It gives a
total order *within* one service's own history, which is exactly what this
mechanism needs and all that anyone is entitled to assume without a shared
clock.

**Problem: events go missing entirely.** If event 6 is lost, waiting for it
forever means detection stalls permanently. That is unacceptable.

This is the harder case, because the buffer's whole strategy is to wait — and
here the thing it is waiting for is never coming. Wait indefinitely and that
service's events pile up unapplied, so the monitor stops learning anything about
it at all and genuine deadlocks involving it go unnoticed.

**Solution: skip the gap, but forget what you knew.** After one reporting
interval, the monitor gives up on the missing event and resumes from the lowest
sequence number it actually has. Critically, it also **deletes its record of
what that service was holding**:

```go
delete(g.held, svc)
g.nextSeq[svc] = lowest
```

Those two lines are the fix for the worst bug in the project, described in
section 10.

The reasoning, applied to the example: the monitor has given up on event 2. It
does not know what event 2 *was*. It might have been `Released L0` — in which
case payment holds nothing and there is no edge — or it might have been
`Acquired L5`, in which case payment holds two locks and there are two edges.
**The held set is no longer knowable**, so every inference drawn from it is a
guess dressed up as an observation.

Deleting it means that when event 3 arrives, the monitor believes payment holds
nothing, so acquiring L1 produces no edge. Payment is no longer accused.

The cost is real and worth stating plainly: any **genuine** edge that spanned
that gap is lost too. Recall drops — that is exactly why H5 measures recall
falling from 1.00 to 0.73 as loss rises. The trade is deliberate. A missed
warning means you handle a deadlock reactively, which the system can already do.
A fabricated warning means someone chases a bug in a service that does not have
one, and it corrodes trust in every other warning the system produces. Missing
some is better than inventing any.

**Problem: a warning raised from an incomplete picture may be wrong.**

Even with the buffer and with gaps handled, there is a window. The monitor
decides at the end of each reporting interval, and at that instant some events
are still in flight. An edge that looks like a violation right now may be
contradicted by a message arriving a moment later.

**Solution: confirm over two intervals.** A candidate warning raised during
interval N is not reported at the end of N. It is re-checked at the end of N+1,
by which time late events have had a chance to arrive, and reported only if the
evidence still stands.

Concretely: interval 5 ends, the monitor has what looks like opposite orderings
from payment and inventory, and it files this as a *candidate* — recorded, not
reported. During interval 6 more events arrive. At the end of 6 it re-checks
from scratch against everything it now knows. If two different services still
hold opposite orderings for that pair, the warning is confirmed and emitted. If
the late events dissolved the case, the candidate is silently dropped.

The code reads `if g.raisedAt[key] >= g.tick { continue }` — a candidate raised
during the interval now closing has not yet survived a full one, so it waits.
That comparison was an off-by-one bug at one point, confirming flags on the very
first interval and defeating the whole mechanism.

This is a deliberate trade of latency for accuracy, and it is what the report
means by "we only confirm a flag if it shows up more than once". It costs one
reporting interval of warning time — which matters, given that Pillar 1's lead
time is already the weakest thing about it.

### Persistence

The lock-order graph can be saved to disk and reloaded (`Save`/`Load`). It is
defined as persistent historical knowledge, so a monitor restart should not
throw away every ordering learned so far and have to rediscover it — that
rediscovery window is exactly when an unflagged deadlock can slip through.

Two things are deliberately **not** restored: the held-lock set and the
per-service sequence positions. After a restart you have no idea what any
service is currently holding or how far through its event stream you were, and
guessing would fabricate orderings — the same failure as the gap bug. Those get
rebuilt from live observation; only the historical edges and already-confirmed
flags carry over.

The live wait-for graph is **not** persisted at all, for the same reason: it
describes who is blocked *right now*, which an old file cannot tell you.

**The principle behind all three decisions:** save what stays true, discard what
was only momentarily true.

A habit stays true. "Some service once took L0 before L1" does not stop being
the case because the monitor was restarted, and it does not stop being
dangerous. Throwing it away means re-learning every habit by observation, and
during that window the system cannot warn about anything — which is precisely
when an unflagged deadlock gets through. Worse, the knowledge is slowest to
rebuild exactly where it is most needed: a rarely-exercised ordering conflict
might take hours of traffic to rediscover.

Current state does not stay true. "Payment is holding L0" was a fact about one
instant. The services kept running while the monitor was down — acquiring and
releasing locks that nobody recorded — so a saved held-set describes a world
that has moved on. Restoring it would mean drawing ordering conclusions from a
belief that is quietly out of date, which is the gap bug in a different costume:
inferring "held X, then took Y" from a held-set you are not entitled to trust.

The sequence positions fail the same test. "I had reached note 47 from payment"
is only meaningful if nothing happened since; payment has been numbering notes
the whole time the monitor was away, so resuming at 47 and then receiving 112
manufactures an enormous gap. Starting fresh and letting the existing
gap-handling logic deal with it is both simpler and more honest.

And restoring the wait-for graph would be the most dangerous of the three,
because it feeds the recovery engine rather than the warnings. A stale cycle
looks exactly like a live one. The engine would kill a service to break a
deadlock that had already resolved itself minutes earlier.

---

## 5. Pillar 2, in detail

### Blast radius

The central quantity. For a candidate victim, it is the criticality-weighted
count of services harmed by killing it: the victim itself, plus everything
queued behind it **from outside the deadlock**.

Two details in that definition were corrections to what the report says, and
both matter.

**Services inside the cycle are excluded.** The report defines blast radius as
"transitively blocked services if this participant is removed". Taken literally
that is useless: inside a deadlock every member is transitively blocked on every
other member by definition, so every candidate scores identically and the metric
discriminates between nobody. Worse, those are the services the abort
*releases* — counting them as damage has the sign backwards.

**Criticality weighting.** Not every service matters equally. A payment service
being stalled is worse than a reporting service being stalled. Each service
carries a weight, and blast radius sums weights rather than counting heads.

This is the input a single-database victim selector has no reason to compute,
because inside one database there are no other services to affect. It is what
makes Pillar 2 structurally different from the prior art.

### The scoring function

The report gives:

```
score = w1·blast_radius + w2·rollback_cost − w3·partition_penalty
```

and says to pick the lowest score. The implementation differs in four ways, each
for a stated reason.

**1. The partition term is added, not subtracted.** As written, a service that
looks unreachable gets its score *reduced*, making it **more** likely to be
chosen — the opposite of the stated intent. A sign error.

**2. Partition suspicion is a tier, not just a term.** The penalty is a 0-or-1
indicator while blast radius and rollback cost are unbounded. In a plain
weighted sum, *any* fixed weight can be outvoted by a large enough cost
difference, so the intent ("do not sacrifice a service merely because its
messages are slow") cannot be reliably expressed as a weight. It also is not
really a cost trade-off: if a service is unreachable, the kill order may not
arrive at all, so the action *fails* rather than costing more. The code
therefore prefers reachable candidates outright, and only scores unreachable
ones when every candidate is unreachable. The weighted term is kept too, so
ranking within a tier still reflects it.

**3. A fourth term the report does not contain: fairness.** Each service carries
a count of how often it has already been sacrificed, and that count is added to
its score. Without this, the policy keeps selecting the same cheap victims, and
those services never complete anything. This was added because the measurements
demanded it, not for elegance — see section 9.

**4. The score has two halves that must not be mixed.** This turned out to be
the subtle one:

```go
predicted[svc] = w.BlastRadius*BlastRadius(...) + w.RollbackCost*rollbackCost
scores[svc]    = predicted[svc] + w.Partition*partitionPenalty + w.Fairness*abortCount
```

`predicted` is a claim about the world — how much damage this kill would do. The
partition and fairness terms are *preferences* — which victim we would rather
pick given equal damage. Only the first half is a prediction, so only the first
half is something a learning mechanism is entitled to adjust. Conflating them
broke the self-tuning feature completely; section 9 explains how.

### Why the tie-break is deterministic

Candidates are sorted before scoring, so two services with identical scores
always resolve the same way. This is not cosmetic: it is what makes the whole
experiment suite reproducible run to run and machine to machine.

---

## 6. How the pieces fit together

Five components, matching the report's architecture section.

```
Services                  report lock events (acquired / waiting / released)
   |
   v
Lock Service              etcd. Grants and revokes locks. Enforces exclusion.
   |
   v
Dependency Monitor        TWO graphs:
                            wait-for graph   -> who is stuck now (detection)
                            lock-order graph -> ordering history (Pillar 1)
   |
   v
Recovery Decision Engine  scores candidates, picks a victim      (Pillar 2)
   |
   v
Fault Manager             lease expiry, crash detection, partition suspicion
   |
   v
Decision Recorder         records why, so it can be explained later
```

The engine sits behind a single Go interface with one method, `Decide`. That is
what makes the comparison in the measurements fair rather than rhetorical:
swapping our policy for a baseline changes exactly one value in the harness, and
nothing else about the workload, the timing or the measurement differs.

---

## 7. Technology choices, and what was rejected

This is the section to read closely, because "why not X?" is the most common
question asked of any project.

### Why etcd, and not Redis

**Redis is the obvious choice and it is the wrong one here.** Redis locks are
usually done with `SETNX` or the Redlock algorithm, and Redlock's safety is
genuinely disputed — Martin Kleppmann's critique argues it provides no real
guarantee when a client pauses or a clock drifts, because nothing stops a client
that *thinks* it still holds the lock from acting on that belief. Redis
replication is asynchronous by default, so a failover can lose a lock grant
entirely: two services then both believe they hold it.

For this project that is fatal. The whole measurement rests on mutual exclusion
actually holding, and if the lock layer can hand the same lock to two services,
every number is meaningless.

**etcd gives three things Redis does not:**

1. **Consensus.** Every write goes through Raft, so exclusion is enforced by
   agreement across the cluster rather than by one server's memory. This is why
   the demo can claim `mutual-exclusion violations: 0` as evidence rather than
   as hope — the guarantee is not coming from code we wrote.
2. **A lease API that matches the need exactly.** Grant a lease with a TTL,
   attach the lock key to it, renew while working. Stop renewing and etcd
   deletes the key itself. A crashed service's lock is reclaimed with nobody
   having to detect the crash.
3. **A free fencing token.** More on this below.

**ZooKeeper and Consul** were both viable. ZooKeeper is the older sibling in the
same Chubby lineage and its sequential znodes could serve as fencing tokens;
Consul has a comparable session/TTL model. Both were rejected on weight rather
than capability — ZooKeeper brings a JVM and heavier operational setup, and etcd
is what Kubernetes is built on, which makes it the most defensible choice for a
microservices-shaped project. The lock layer sits behind an interface, so
swapping in either means writing one new implementation and changing nothing
else.

### Why fencing tokens, and why etcd's revision number

This is worth understanding properly because it is the one place the project
engages with a real, named criticism in the literature.

The problem Kleppmann describes: service A acquires a lock, then pauses — a
garbage-collection pause, a network delay, the process being suspended. While it
is paused the lease expires and service B acquires the lock legitimately. Then A
wakes up, still believing it holds the lock, and writes. Two writers. Corruption.
A timeout alone cannot prevent this, because A has no way to know time passed.

The fix is a **fencing token**: a number handed out with each acquisition, always
larger than the last. The resource being protected refuses any write carrying a
token lower than the highest it has seen. A's stale token is rejected on arrival.

The implementation uses **etcd's revision number** as the token. etcd assigns
every write a revision, and revisions increase monotonically across the entire
keyspace, so a later acquisition is *guaranteed* a strictly higher number than
an earlier one.

Why that matters: the alternative is maintaining our own counter, which is one
more piece of state to keep correct, replicate and recover. The revision comes
free from a store that has already solved the problem with consensus. The
cheapest correct mechanism is the one you do not implement.

### How a lock is actually taken

```
1. Ask etcd for a lease with a TTL.
2. Run a transaction: "if this key's create_revision == 0, put my service id"
   — in other words, write only if nobody holds it.
3. etcd serialises competing transactions through Raft. Exactly one sees
   succeeded = true.
4. The revision of that successful write becomes the fencing token.
5. While working, renew the lease. Stop renewing and etcd deletes the key.
```

Step 3 is the mutual exclusion guarantee, and it is worth being clear that it
comes from Raft, not from anything in this repository.

### Why Go, and not Java or Python

The report's reasoning, with one correction.

**Separate OS processes matter.** The project claims it injects crashes with a
real `kill -9` rather than a flag flipped inside a simulator. That claim is only
true if the services genuinely are separate processes, and Go produces
self-contained binaries that are trivial to spawn, signal and kill. `cmd/e2e`
does exactly that.

**Goroutines make simulation cheap.** Running many independently-timed services
inside one benchmark process is straightforward, which is what makes the
deterministic experiments possible.

**Measurement noise.** Recovery times are being measured in milliseconds. Python
adds interpreter overhead; the JVM adds longer garbage-collection pauses. Go's
pauses are typically sub-millisecond.

**The correction:** the submitted report says Go has "no GC pauses". That is
wrong — Go has a garbage collector. The defensible statement is that its pause
times are much shorter than the JVM's, which is what actually matters for
millisecond measurements. Worth knowing before someone points it out.

Rust or C++ would have removed the garbage collector entirely, at the cost of
substantially more development time for a benefit the measurements do not need.

### Why the HTTP gateway instead of clientv3 — and why it does not matter

The report names etcd's official Go client, `clientv3`. The implementation talks
to etcd over its v3 HTTP/JSON gateway instead.

**This was forced, not chosen.** The development sandbox could not reach
`proxy.golang.org` or `go.etcd.io`, so the module could not be downloaded. The
options were to abandon real etcd or to speak to it over HTTP.

**Why it is not a meaningful compromise:** the gateway is a thin translation
layer in front of the same gRPC service. The same lease grants, the same
transactions, the same revisions, the same Raft. It also left the project with
**zero external dependencies**, which turned into a genuine advantage — nothing
to download, and nothing that can break because a package moved. That is why it
built and ran on the MacBook first try.

It also survived a real test: development used etcd 3.5.17, the Mac has 3.7.2,
and it worked unchanged.

### Why no wall-clock timestamps on events

A `LockEvent` carries a per-service sequence number and deliberately no
timestamp:

> there is deliberately no wall-clock timestamp on a lock event. Services in
> this system are assumed to be independently deployed with no shared clock.

**Rejected: wall-clock time.** Machine clocks disagree by unknown amounts.
Ordering two services' actions by timestamp would be quietly wrong, and wrong in
a way that only shows up under load.

**Rejected: Lamport or vector clocks.** These would give genuine
cross-service causal ordering. They were not used because nothing in the design
needs it — Pillar 1 only ever reasons about one service's own ordering of its
own locks — and they add message overhead and complexity for a guarantee the
project does not consume.

**Chosen: per-service sequence numbers.** The minimum that is actually
justifiable. A total order *within* one service's history, which is all the
ordering anyone is entitled to assume, and no claim at all about ordering
between services.

### Why Tarjan's algorithm for cycle detection

A deadlock is a cycle in the wait-for graph. Tarjan's strongly-connected-
components algorithm finds every cycle in one pass, in time proportional to the
size of the graph.

**Why not Chandy-Misra-Haas probes?** CMH is designed for the situation where no
single party has a view of the whole graph, so it sends probe messages around to
discover cycles. Here, the monitor already receives every service's event
stream, so it *can* build the graph locally — and given a graph, a standard graph
algorithm is simpler and faster than a message protocol. The report cites CMH as
the lineage of the approach, which is accurate: the project uses the established
wait-for-graph method and does not claim to improve detection.

### Why both a simulator and real processes

Two harnesses, for two different and incompatible purposes.

**The simulator** (`benchmark/`) is a deterministic discrete-event simulation.
Every run is seeded, so the same configuration produces byte-identical results —
verified: the Mac under Go 1.27.1 and Linux under Go 1.24.7 produce the same
digits. That determinism is what makes policy comparison meaningful. If timing
varied between runs, a difference between two policies could never be separated
from luck.

**The real-process harness** (`cmd/e2e` + `cmd/stubservice`) runs one service
per OS process against real etcd, and is deliberately **not** deterministic.
Real scheduling, real network latency, real process death.

Why both: the simulator shows that the *policy* behaves a certain way; the
process harness shows that the *system* does. Neither substitutes for the other.
The report claims crash injection by `kill -9`, and only the second harness
makes that claim true.

**Rejected: Docker containers over gRPC**, which the report specifies. No Docker
daemon was available in the sandbox. Separate OS processes communicating via
pipes and signals preserve what mattered — genuine process isolation, genuine
`kill -9`, genuine independent failure — while dropping only container-level
network manipulation, which the simulator covers by injecting delay and loss
directly.

### Why observed and true state are kept apart

This was not in the original design and is arguably the most important thing
learned during implementation.

The simulator maintains two separate views: what is *actually* true, and what
the monitor has *observed* via delivered events. The policy is only ever shown
the observed view. Measurement uses the true view.

The first version violated this — rollback cost was read from true service
state. The consequence was severe and silent: the policy's "estimate" of the
work a kill would destroy was *exactly* the work it then destroyed. It could not
be wrong. Every policy in the comparison was being scored on decisions made with
information no real deployment has.

It is now built from an observed held-count maintained purely from delivered
events, so it can be wrong — which is the point. Section 9 covers what this
revealed.

### Why plain arithmetic for weight adjustment, not a neural network

The report is explicit: "plain arithmetic, not a neural network, and every
update can be traced back to one specific past decision."

That is the right instinct and the reason is Objective 4. The project promises
to explain *why* each decision was made. A learned model with opaque weights
would make every explanation "because the model said so", which defeats the
objective. A least-mean-squares update — nudge each weight in proportion to how
wrong the prediction was — is inspectable: you can point at the one past outcome
that moved a weight and by how much.

(The mechanism still failed, for reasons that have nothing to do with this
choice. See section 9.)

### Why recording and explaining are separate

`explain/` has two halves that never run at the same time.

The **Recorder** runs on the critical path, while a deadlock is being broken. It
appends a small struct to a pre-allocated buffer. No string formatting, no file
writing, no allocation in steady state.

The **Generator** runs later, off the critical path, only when someone wants to
read something. It turns records into sentences.

Why split: if they were one component, every recovery decision would pay the
cost of producing prose nobody has asked for. Measured cost of recording: 1.35%,
not separable from trial noise.

The split also enables something otherwise impossible. The recorder stores the
score of **every** candidate, not just the winner. That is what allows
counterfactual questions — "what if you had killed the other one?" — to be
answered afterwards without re-running the decision. Had it stored only the
chosen action, the report's stretch goal would have been unreachable.

---

## 8. How it was tested, and why the tests look like that

### The principle that shaped every experiment

The original six experiments had a flaw that took a while to see. The main one
compared our policy against the baseline on **services disrupted** — but that is
the quantity our policy exists to minimise. Scoring a policy on its own
objective is close to circular: of course the thing optimising a number scores
well on that number.

So the rule became: **every claimed mechanism must be tested against a metric it
does not itself optimise.** Four of the ten experiments exist only because of
this, and several earlier results did not survive it.

The metric that does this work is **completed transactions** — how much useful
work the system actually finished. No policy optimises it directly, and it
cannot be gamed by a policy that achieves beautiful disruption figures by
grinding everything to a halt.

### The ten experiments

| | What it tests | Why it is designed that way |
|---|---|---|
| H1 | Services disrupted vs cost-only | The report's original claim. Kept, and reported even though it now comes out *against* us, because deleting an unflattering result is worse than explaining it |
| H2 | Recovery time vs timeout baseline | One of three metrics Objective 3 names |
| H3 | Framework overhead | Report set the bound in advance (<10% throughput) rather than after seeing the number |
| H4 | Crash and stale-lock recovery | The load-bearing evidence is the etcd integration test, where exclusion is enforced by Raft, not by our own data structure |
| H5 | Pillar 1 precision, recall, lead time | Four contention levels, and four degradation levels of delay and loss, because Pillar 1's behaviour turns out to depend entirely on contention |
| H6 | Explanation recording cost | Tests the critical-path split claim directly |
| H7 | Pillar 2 ablation, term by term | Added. Reports completed transactions, which the policy does not optimise |
| H8 | Network partition handling | Added. An ablation *within one policy* rather than across policies, so a difference can be attributed to the mechanism being toggled |
| H9 | Self-calibration under varying cost | Added. Tests the learning mechanism against its own training target, which is the only fair test of it |
| H10 | Deadlock frequency vs all three baselines | Added. The third metric Objective 3 names, which had never been compared across policies |

### Three baselines, not one

**Timeout** — kill whoever has waited longest. Uses no graph information at all.
The naive approach.

**Cost-only** — kill whoever is cheapest to roll back. The prior art, including
the patent. Deliberately a strong opponent, not a strawman: on a single machine
it is the right answer.

**Youngest** — kill whoever blocked most recently. The classical transaction-
manager rule, on the reasoning that the youngest transaction has least invested
work. Added late, and it earned its place: it is the input a practitioner would
reach for first, and it is a genuinely different *kind* of baseline from the
other two — timeout uses no graph, cost-only uses cost, this uses time.

Having three matters for the defence. It closes off the objection that
cost-only was a weak opponent chosen to flatter the result.

### Statistics

Twenty trials per configuration, with 95% confidence intervals. A difference is
only described as real when the intervals do not overlap. Several results that
looked meaningful at a glance did not survive this.

---

## 9. What held up, and what did not

### Held up: blast radius plus fairness

Blast-radius selection **on its own is actively harmful**. At 30 and 60 services
it completes 1–2 transactions against cost-only's 111–113 — while posting the
*best* disruption figure in the table. The cause is starvation: it keeps
selecting the same low-cost victims, so those services never finish anything.

This is the cleanest illustration in the project of why a policy must not be
judged on the metric it optimises. On its own scorecard it looks excellent. The
system is dead.

Adding the fairness term fixes it. In combination:

- roughly **three times** cost-only's completed transactions at 30–60 services,
  at comparable disruption
- best **deadlocks per thousand completed transactions** at every contention
  level, 2.3× better than cost-only and 2.6× better than the youngest rule at
  medium contention

The honest claim is therefore narrower and more specific than the report's:
*blast-radius-aware victim selection is only viable when paired with an
anti-starvation term, and in that combination it outperforms both cost-based and
time-based selection on throughput and on deadlocks per unit of completed work.*

The fairness term is not in the report. It was added because measurement
demanded it.

### Held up conditionally: Pillar 1

Perfect precision (1.000) and perfect attribution (1.000) at every degradation
level tested. Recall falls gracefully from 1.00 to 0.73 as observations are
delayed and dropped.

**But the lead time depends entirely on contention, and inverts where it
matters:**

| Contention | Lead time |
|---|---|
| heavy | **−474 ticks** — the deadlock happens first |
| moderate | +15 |
| light | +669 |
| very light | +2145 |

The reason is structural and worth understanding, because it is not fixable by
tuning. Pillar 1 needs a service to **successfully complete** an out-of-order
acquisition before it has any evidence. Under heavy contention, that service
deadlocks on its very first attempt — so the deadlock arrives before the warning
it was supposed to produce. It only pays off when contention is light enough for
conflicting orderings to be exercised separately before they overlap, which is
when deadlocks matter least.

### Did not hold up: self-calibrating weights

The report presents adaptive, self-tuning weights as a contribution. They do not
work, and establishing that properly took three corrections — each of which was
hiding the next.

1. **The first update rule used `sign(feature)`**, applying an identical nudge to
   every weight so they all moved in lockstep. Fixing it to a proper
   least-mean-squares update changed no decision anywhere.
2. **The regression target was the whole score**, including the fairness term.
   That term grows without bound as kill counts accumulate, so the error never
   settled and both genuine cost weights were driven to zero. Measured directly:
   `blast=0.0000 rollback=0.0000` at two very different learning rates.
3. **The feedback signal was circular.** It was `disrupted`, which the harness
   *computes as* blast radius — one of the policy's own inputs. The learner could
   only rediscover its own definition. The obvious alternative was no better:
   rollback cost was being read from true state, so that "estimate" was exactly
   the work the kill then destroyed.

With all three fixed, the learner behaves **correctly** — it converges to
`blast≈0, rollback≈1.0`. And that is precisely why the idea fails. The only
thing observable after a kill is the work destroyed, and that genuinely does not
depend on blast radius. So any honest regression discards the blast-radius
weight. But blast radius is what governs throughput.

Measured on a workload where rollback cost actually varies, calibration loses on
completed transactions (563 → 514), on disruption (11.36 → 17.42), **and on work
lost (1.217 → 1.249) — the very quantity it is trained to predict.**

One more test pins the cause down: freezing the blast weight at zero with **no
learning at all** reproduces almost the entire loss. So the failure is in *where
a correct learner must converge*, not in how it gets there.

The conclusion, stated generally: **a victim-selection score cannot be
calibrated by regressing it onto one observed cost component when it has to
balance costs that component does not contain.** The mechanism is mis-specified,
not mistuned. It ships disabled, with the code retained so the result stays
reproducible.

### The methodological finding

Our own benchmark was handing the policy ground truth. It had been silently
flattering every result, and it was invisible until self-calibration was pushed
hard enough to expose it.

The general lesson: **a harness can flatter the thing it measures, and the
failure mode is silent.** Worth saying out loud, because catching it is a
stronger signal of rigour than any individual number in the results.

---

## 10. Two bugs worth being able to explain

Both were found by tests rather than by reading code, and both are the kind a
panel enjoys asking about.

### A departing lock holder could delete the next holder's lock

The etcd backend originally tracked leases keyed by *lock*. The race: between a
departing holder's delete transaction succeeding and it looking up which lease
to revoke, the next holder can already have acquired and overwritten the entry.
The departing holder then revokes the **new** holder's lease, etcd deletes their
key, and that holder's own release fails.

Fixed by keying leases on the fencing token, which is unique per acquisition, so
the race cannot occur. Found by running the etcd mutual-exclusion test
repeatedly rather than once.

### Lost observations caused confident warnings about innocent services

Described in section 4. When an observation was lost, Pillar 1 kept its record
of what the service was holding across the hole, and inferred "held X, then took
Y" edges that never happened. Because a fabricated edge can be the reverse of a
real one, it produced confident warnings about correctly-behaving services. Under
15% observation loss it fired warnings at tick 40 for a conflict that could not
physically exist before tick 400.

**The instructive part: precision did not catch it.** The fabricated warnings
still named a genuinely dangerous lock pair — just for the wrong reason, naming
the wrong services. Precision was 1.000 throughout.

So a second metric was added, **attribution**: do the two services a warning
names really disagree about ordering? That is the metric that would have caught
it, and it is now reported separately in H5.

This failure cannot occur in `lockdep` or Goodlock, because single-process tools
never lose an observation. It is specific to the distributed setting — which is
exactly where the project claims to be working, and is arguably the most novel
thing in it.

---

## 11. Known limits

Stated here so a panel hears them from you first.

- **The experiment sweeps run against the simulator**, not real etcd. `cmd/e2e`
  exercises the full system, but the comparisons do not. Given that the
  simulator was caught handing the policy ground truth, closing this gap matters
  more than it otherwise would.
- **H8 cannot separate the report's weighted partition term from the tier
  preference.** Both drive futile kills to zero. A separate measurement shows
  why: the cost gap between a reachable and an unreachable candidate is near
  zero in this workload, so a fixed weight of 2.0 dominates by luck of
  magnitudes. A workload with larger cost spreads would overrule the weighted
  term; the tier depends on no margin at all.
- **Pillar 1's lead time stays negative under heavy contention.** Structural, not
  fixable by tuning.
- **CPU cost is +51%** even though the throughput bound passes. The stated bound
  was throughput, and it passes, but the CPU number should not be buried.
- **Self-calibration should probably be deleted** rather than shipped disabled.
- **The report does not match the code in eight places**, including the sign
  error and the fourth scoring term. This is outstanding work, not a
  measurement limit.

---

## 12. Questions you should have an answer ready for

**"Why not Redis?"** Redlock's safety is disputed, replication is asynchronous
so a failover can hand the same lock to two services, and it offers no fencing
token. The measurements rest on exclusion actually holding.

**"Isn't this just lockdep?"** `lockdep` works because one process sees every
lock operation instantly and in order. Across a network, observations arrive
late, out of order, or never. Handling that is the work — and it produces a
failure mode (false accusations from lost observations) that `lockdep` cannot
exhibit.

**"Isn't blast radius just cost?"** No. Cost is local to one service. Blast
radius counts the other services stranded behind it. Inside one database there
is nobody else to strand, which is why the prior art has no reason to compute it.

**"Why is your disruption figure worse than the baseline in H1?"** Because the
policy carries an anti-starvation term that deliberately trades disruption for
throughput. On disruption alone it looks worse. H7 and H10 use metrics the
policy does not optimise, which is the fair comparison.

**"That 655,410 figure is absurd."** Correct, and it should not be read as a
result. At high contention both the cost-only and youngest baselines complete
*one* transaction per run, so a per-thousand-transactions ratio divides by
essentially nothing. The real finding is the collapse itself: two different
baselines, one cost-based and one time-based, both grind to a halt where ours
completes 332. Point at the completed column.

**"Why does the demo give different numbers each run?"** Because it is real
processes with real scheduling and real network latency. The experiments are
seeded and reproduce digit-for-digit across machines and compiler versions; the
demo cannot and should not. One shows how the policy behaves, the other that the
system does.

**"Why did the lock take 1.75s to come back if the lease is 2s?"** The holder
renews every 20ms, so when it was killed its current lease had already been
alive a fraction of a second. What is recovered is the remainder, anywhere
between 0 and 2s.

**"What is actually new here?"** In order of how well it holds up: lock-order
prediction in a setting where observations can be lost, and the false-accusation
failure mode that comes with it; the measured finding that blast-radius selection
starves the system without an anti-starvation partner; and a specific account of
why self-tuning weights cannot work for this problem. The strongest items are
negative or conditional results — which is what honest engineering usually
produces.
