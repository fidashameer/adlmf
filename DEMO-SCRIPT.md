# Demo script — Review 2

Running on your MacBook. Total live running time is about **90 seconds**; the
rest is you talking. Every timing below was measured, not estimated.

---

## Before you walk in (5 minutes, do it once)

**1. Pre-compile everything.** Go compiles on first run, which adds several
seconds of silence at the worst possible moment.

```
cd ~/Documents/adlmf
go build ./...
go test ./... -count=1 > /dev/null
```

**2. Open two Terminal windows and arrange them so both are visible.** Not tabs
— windows, side by side. The fact that there are two is part of the point: one
is the lock server, one is your system talking to it over the network. A panel
sees that and understands nothing is faked inside a single program.

**3. Start etcd in window 1 and leave it alone.**

```
cd ~/Documents/adlmf
etcd --listen-client-urls http://127.0.0.1:2379 --advertise-client-urls http://127.0.0.1:2379
```

Use that long form, not a bare `etcd`. Plain `etcd` advertises itself as
"localhost", which resolves to IPv6 first, and its own internal client then logs
a red `connection refused` warning when that fails. Harmless — it retries over
IPv4 and everything works — but it is an alarming-looking line sitting on screen,
and you do not want to be explaining a non-problem mid-demo.

It prints a page of startup logs and then sits there looking idle. That is it
running. Do not touch this window again.

Two reasons this must happen before anything else. The demo needs it — you
already hit the `SKIP` once by forgetting. And five of the tests only run when
etcd is reachable, so with it running you show 36 tests instead of 31.

**4. In window 2, get into position:**

```
cd ~/Documents/adlmf
clear
```

**5. Have `RESULTS.txt` open** in a text editor or a third window, scrolled to
H10. You will point at it rather than scrolling live.

---

## The demo, in four beats

### Beat 1 — the live system (13 seconds)

**Optional: run it with the live dashboard.** Add `-http :8080` and open
`http://localhost:8080` in a browser. It shows the same run as a page — service
states, which locks are held by whom, who is waiting for whom, the event log,
and four counters including the mutual-exclusion count. Far easier to read from
across a room than a terminal.

```
go run ./cmd/e2e -for 20s -http :8080
```

Use a longer `-for` when showing the dashboard, so there is time to talk over
it. The page stays up after the run finishes, with a PASS banner, until you
press Ctrl+C — so you can discuss the final state rather than racing the clock.
`?theme=dark` forces dark mode for a projector, and there is a toggle in the
corner.

The thing to point at: a service showing **holds L0 · waiting on L1, held by
order** while another shows the mirror image. That is a deadlock, on screen, as
it happens — and then it turns red and one of them gets killed.

The terminal version is unchanged and still the fallback; the dashboard is off
unless you pass the flag.


Lead with this. It is the most convincing thing you have and it should not be
buried behind test output.

**Say first, before typing:** "This runs three services as three separate
operating-system processes, against a real etcd. Two of them deliberately take
the same pair of locks in opposite orders, which is the pattern that deadlocks."

```
go run ./cmd/e2e -for 12s
```

**If she asks for a specific number of services, you can do it.** The count is a
flag, and anything from 2 to 50 works:

```
go run ./cmd/e2e -for 12s -services 10
```

Expect roughly: 2 services gives about 2 deadlocks in 10 seconds, 6 gives about
25, 10 gives about 38. Exclusion holds at every size, which is the point worth
making — *"more services means more contention and more deadlocks, and zero
violations at every size."* The services beyond the first three are generated
around the same three locks on purpose: spreading them across more locks would
reduce contention and show *fewer* deadlocks, which is the opposite of what
adding services ought to demonstrate.

Then narrate the lines as they appear — do not wait and summarise at the end.

- **`PILLAR 1 flag: ... payment took L0->L1, inventory took L1->L0`** — "That is
  the early warning. It worked out from watching them that these two services
  disagree about lock ordering, and it said so *before* either of them was
  stuck."
- **`DEADLOCK ... -> abort inventory (signal to pid ...)`** — "Deadlock detected,
  and the victim is killed by sending a signal across a process boundary. That
  is a real process ID."
- **Point out that the victim changes** between lines — inventory, then payment,
  then inventory. "It rotates. That is the anti-starvation rule working, and
  that rule is the single most important thing we added."
- **`KILL -9 pid ... while it held L0`** — "Now we destroy a process outright
  while it is holding a lock. No cleanup, no warning, like pulling a power
  cable."
- **`mutual-exclusion violations : 0`** — "Checked continuously against etcd
  throughout the run. Two services never held the same lock. And that guarantee
  comes from etcd's consensus layer, not from code we wrote."
- **`lock reclaimed in : ~2s`** — "Nothing in our code noticed that crash. The
  lease simply expired and etcd handed the lock back."

### Beat 2 — it is actually tested (16 seconds)

```
go test ./... -count=1
```

**Say:** "Thirty-six tests, five of them against the real etcd running next
door. The etcd ones matter most — they check that exclusion holds and that a
crashed holder's lock is reclaimed, and they're the evidence behind that
hypothesis rather than our own data structure agreeing with itself."

### Beat 3 — one experiment, live (5 seconds)

```
go run ./cmd/experiments -only h10 -trials 5
```

**Say:** "This is the headline comparison. Deadlocks per thousand *completed*
transactions, against three different competing strategies."

Then the two things that must come from you, not from them:

**The absurd number.** "That six-figure value is not a result. At high
contention those two baselines complete one transaction per run, so the ratio is
dividing by almost nothing. The real finding is the collapse itself — look at
the completed column: one, against our 332."

**Why this metric.** "We deliberately judge ourselves on a number our policy
does not optimise. Our own objective is disruption, and scoring ourselves on
that would be close to circular."

### Beat 4 — the full results, from the file

Do not run the full suite live. It takes eight minutes.

**Say:** "The complete run is eight minutes and reproduces exactly — same digits
on this Mac under Go 1.27 as on the Linux machine it was developed on under Go
1.24. Different architecture, different compiler. That is in the file."

Then walk them through three findings, in this order:

**What worked.** "Blast-radius selection paired with an anti-starvation term:
roughly three times the completed transactions of the prior-art baseline at 30
to 60 services."

**What worked only conditionally.** "The early warning is perfectly accurate
when it fires, but under heavy contention it arrives *after* the deadlock. It
needs to watch a service successfully take locks in the risky order before it
has evidence, and under heavy load that service deadlocks on its first attempt.
It only pays off when contention is light."

**What failed.** "The self-tuning weights do not work, and we can say exactly
why rather than just reporting a bad number. The only thing observable after
killing a service is the work destroyed, and that quantity does not depend on
blast radius — so any honest learner discards the term that actually governs
throughput. It ships switched off."

**Then the thing you volunteer rather than get caught on:** "Our own benchmark
was giving the policy information a real deployment would never have. We found
it, fixed it, and re-ran everything. One result flipped against us and we kept
it in."

---

## If something goes wrong

**`SKIP: no etcd at http://127.0.0.1:2379`** — etcd is not running. Window 1,
type `etcd`, wait five seconds, retry. Say "the lock server isn't up yet" and
move on; the program checking and declining to pretend is better behaviour than
crashing.

**`go: command not found`** — the window was opened before Go was installed, or
a shell profile issue. Fall back to the prebuilt binaries:

```
cd ~/Documents/adlmf/bin
./e2e -for 12s -stub ./stubservice
./experiments -only h10 -trials 5
```

**macOS refuses to run those binaries** ("developer cannot be verified"):

```
xattr -d com.apple.quarantine ~/Documents/adlmf/bin/*
```

**etcd says the port is in use** — an older etcd is still running from earlier.

```
pkill etcd
```

Then start it again.

**The demo shows fewer deadlocks than last time, or none.** Expected and not a
fault. It runs real processes with real timing, so the count varies run to run —
one development run saw 8, your Mac saw 3. If it is thin, say so and run it
again with `-for 20s`. Do not describe it as broken; the variability is the
honest consequence of using real processes, and it is worth saying out loud
because it contrasts with the experiments, which are seeded and identical every
time.

**Everything is on fire.** `RESULTS.txt` is already on screen in window 3 and
every number in it is real. Talk over the file. The results do not stop being
true because a terminal misbehaved.

---

## If she asks to see the locks themselves

`etcdctl` was installed alongside etcd and knows nothing about your project, so
this is independent evidence rather than your own code reporting on itself.
Start a longer run, then in a third window:

```
etcdctl get --prefix /adlmf/ --keys-only
etcdctl lease list
```

Those are the actual lock keys in the actual database, and the leases keeping
them alive. Run the first one twice a second apart and you will see locks change
hands. This is the answer to "are these real locks or are you simulating them".

## Questions you should expect

**"Why not Redis?"** Redlock's safety is disputed, Redis replication is
asynchronous so a failover can hand the same lock to two services, and there is
no fencing token. Our measurements rest on exclusion actually holding, so a lock
layer that can hand one lock to two services would make every number
meaningless.

**"Isn't this just lockdep?"** `lockdep` works because one process sees every
lock operation instantly and in order. Across a network, reports arrive late,
out of order, or not at all. Handling that is the work — and it produces a
failure mode `lockdep` cannot have: a lost report makes the system accuse a
service that is behaving perfectly correctly. Our precision metric scored 1.000
right through that bug, because the false warnings named a genuinely dangerous
lock pair for the wrong reason. We added a second metric to catch it.

**"Isn't blast radius just rollback cost?"** No. Cost is local to one service.
Blast radius counts the other services entangled behind it. Inside a single
database there is nobody else to affect, which is why the prior art has no reason
to compute it.

**"Your disruption number is worse than the baseline."** Correct, and we report
it. The policy carries an anti-starvation term that deliberately trades
disruption for throughput — without it, throughput collapses to one or two
transactions at scale while the disruption figure looks excellent. H7 and H10
use metrics the policy does not optimise, which is the fair comparison.

**"Aren't the services behind the victim actually freed by killing it?"** Fair
challenge. Blast radius measures entanglement rather than direct harm — a
service with twelve waiters holds locks in hot demand, and killing it means its
transaction restarts and immediately re-contends for those same locks. What
justifies it is not the name: it is that selecting on this quantity produces more
completed work and fewer deadlocks per unit of work than three different
baselines, and those metrics do not define it.

**"Why is the lock reclaimed in 1.75 seconds if the lease is 2 seconds?"** The
holder renews every 20 milliseconds, so when it was killed its current lease had
already been alive a fraction of a second. What gets recovered is the remainder
— anywhere between zero and two seconds.

**"Why does your code not match your report?"** Say this before they ask. There
are eight differences, and two matter: the scoring formula in the report has a
sign error that would make an unreachable service *more* likely to be killed,
and the code has a fourth term the report does not mention. Both came out of
measurement.

---

## One-page cheat card

```
PREP      window 1:  etcd --listen-client-urls http://127.0.0.1:2379 \
                          --advertise-client-urls http://127.0.0.1:2379
          window 2:  cd ~/Documents/adlmf && clear
          window 3:  RESULTS.txt open at H10
          pre-warm:  go build ./... && go test ./... >/dev/null

BEAT 1    go run ./cmd/e2e -for 12s                      13s
          (with dashboard: -for 20s -http :8080, then open localhost:8080)
          (asked for N services?  add  -services 10)
          narrate: warning -> deadlock -> rotating victim -> kill -9 -> 0 violations

BEAT 2    go test ./... -count=1                         16s
          say: 36 tests, 5 against real etcd

BEAT 3    go run ./cmd/experiments -only h10 -trials 5     5s
          volunteer: the six-figure number is a divide-by-one, not a result
          volunteer: we judge on a metric we don't optimise

BEAT 4    talk over RESULTS.txt
          worked / worked-conditionally / failed
          volunteer: our benchmark was cheating, we found it and fixed it

FALLBACK  cd bin && ./e2e -for 12s -stub ./stubservice
          pkill etcd        (port in use)
```

---

## Afterwards

Running etcd from inside the project folder creates a `default.etcd/` directory
there. Delete it before submitting anything:

```
cd ~/Documents/adlmf && rm -rf default.etcd
```

And do this whole script end to end once, timed, before the day. Not because
anything is broken — because the gap between "it works" and "I can make it work
while people watch" is where demos fail.
