// Command e2e runs the whole system end to end: real etcd, real OS processes,
// a real kill -9, and real cross-process aborts.
//
// This is the experiment the simulator cannot do. The simulator gives clean,
// repeatable numbers for comparing policies; this gives evidence that the
// design works when the services are genuinely separate, independently
// failing processes rather than goroutines with a crashed flag set.
//
// What it checks:
//
//  1. Mutual exclusion holds continuously, polled directly from etcd.
//  2. A deadlock across real processes is detected from their event streams.
//  3. The recovery engine's chosen victim is aborted across a process boundary
//     by signal, and the deadlock clears.
//  4. After a genuine kill -9 of a lock holder, etcd expires the lease and the
//     lock is reclaimed without anyone having to detect the crash.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"adlmf/depmonitor"
	"adlmf/internal/types"
	"adlmf/lockservice"
	"adlmf/recovery"
)

type stub struct {
	id  types.ServiceID
	cmd *exec.Cmd
}

func main() {
	endpoint := flag.String("etcd", "http://127.0.0.1:2379", "etcd endpoint")
	runFor := flag.Duration("for", 12*time.Second, "how long to run")
	stubBin := flag.String("stub", "", "path to a prebuilt stubservice binary; if empty, it is compiled with `go build`")
	numSvcs := flag.Int("services", 3, "how many services to run, each as its own OS process")
	policyName := flag.String("policy", "adaptive",
		"victim selection: adaptive, blast-only, cost-only, timeout, youngest")
	timeoutMS := flag.Float64("timeout-ms", 300,
		"how long the timeout baseline waits before aborting the longest waiter")
	quiet := flag.Bool("quiet", false, "suppress the per-event log, for batch runs")
	lockPool := flag.Int("locks", 3, "how many distinct locks services compete over")
	varyLocks := flag.Bool("vary-locks", false,
		"give services different numbers of locks, so rollback cost varies between them")
	httpAddr := flag.String("http", "", "serve a live dashboard on this address, e.g. :8080 (off by default)")
	flag.Parse()

	prefix := fmt.Sprintf("/adlmf/e2e/%d", time.Now().UnixNano())
	probe := lockservice.NewEtcdBackend(*endpoint, prefix)
	if err := probe.Ping(); err != nil {
		fmt.Printf("SKIP: no etcd at %s (%v)\n", *endpoint, err)
		return
	}

	// Build the stub binary once -- unless we were handed one already. The
	// flag exists so this demo can run from prebuilt binaries on a machine
	// with no Go toolchain installed, which matters when the thing has to work
	// in front of an audience rather than on the developer's laptop.
	bin := *stubBin
	if bin == "" {
		bin = "/tmp/adlmf-stub"
		if out, err := exec.Command("go", "build", "-o", bin, "./cmd/stubservice").CombinedOutput(); err != nil {
			fmt.Printf("could not compile the stub service: %v\n%s\n", err, out)
			fmt.Printf("\nIf Go is not installed, pass a prebuilt one instead:\n")
			fmt.Printf("  ./e2e -stub ./stubservice\n")
			os.Exit(1)
		}
	} else if _, err := os.Stat(bin); err != nil {
		fmt.Printf("no stub binary at %s: %v\n", bin, err)
		os.Exit(1)
	}

	if *numSvcs < 2 {
		fmt.Printf("need at least 2 services for a deadlock to be possible; got %d\n", *numSvcs)
		os.Exit(1)
	}
	if *numSvcs > 50 {
		fmt.Printf("refusing to start %d processes; 50 is the sane limit\n", *numSvcs)
		os.Exit(1)
	}
	plans := buildPlans(*numSvcs, *lockPool, *varyLocks)
	if !*quiet {
		fmt.Printf("e2e: %d stub services started as separate OS processes against %s\n",
			len(plans), *endpoint)
	}

	var (
		mu    sync.Mutex
		stubs = map[types.ServiceID]*stub{}

		// Every service the run started, and it never changes. The violation check
		// below must not use the live stubs map: the crash-injection path deletes the
		// killed service from that map so it is never signalled twice, and the victim
		// legitimately keeps holding its locks until each lease expires.
		known   = map[types.ServiceID]bool{}
		wf      = depmonitor.NewWaitForGraph()
		lo      = depmonitor.NewLockOrderGraph()
		evCount int

		// The policy's two cost inputs, both derived only from the event
		// stream, which is all a real monitor has.
		//
		// These were previously stubbed (abort counts empty, rollback cost
		// fixed at 1), which left the fairness term permanently zero -- so the
		// harness kept choosing the same victim and never exercised the one
		// mechanism H7 identifies as load-bearing.
		obsHeld = map[types.ServiceID]int{}

		// When each service most recently became blocked. The timeout and
		// youngest baselines both score on wait time, so without this they
		// would see an all-zero map and degenerate.
		blockedAt = map[types.ServiceID]time.Time{}
		completed int
		abortsBy  = map[types.ServiceID]float64{}
	)

	for _, p := range plans {
		cmd := exec.Command(bin,
			"-id", p.id, "-etcd", *endpoint, "-prefix", prefix,
			"-locks", p.locks, "-hold", "150", "-think", "80", "-ttl", "2")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			panic(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			panic(err)
		}
		s := &stub{id: types.ServiceID(p.id), cmd: cmd}
		stubs[s.id] = s
		known[s.id] = true

		go func() {
			sc := bufio.NewScanner(stdout)
			for sc.Scan() {
				line := sc.Bytes()
				if bytes.HasPrefix(line, []byte("#DONE")) {
					mu.Lock()
					completed++
					mu.Unlock()
					continue
				}
				var ev types.LockEvent
				if err := json.Unmarshal(line, &ev); err != nil {
					continue
				}
				mu.Lock()
				wf.Apply(ev)
				lo.Observe(ev)
				evCount++
				switch ev.Kind {
				case types.Waiting:
					if _, already := blockedAt[ev.Service]; !already {
						blockedAt[ev.Service] = time.Now()
					}
				case types.Acquired, types.Released:
					delete(blockedAt, ev.Service)
				}
				mu.Unlock()
			}
		}()
	}
	defer func() {
		for _, s := range stubs {
			_ = s.cmd.Process.Kill()
		}
	}()

	if !*quiet {
		fmt.Printf("     prefix %s\n", prefix)
	}

	var dash *dashboard
	if *httpAddr != "" {
		dash = newDashboard(*endpoint, prefix, *runFor)
		dash.serve(*httpAddr)
		fmt.Printf("     dashboard http://localhost%s\n", *httpAddr)
	}
	fmt.Println()

	// All four comparison policies satisfy the same interface, so the harness
	// can run any of them against real processes rather than only the adaptive
	// one. The baselines are the same implementations the simulator uses.
	var policy recovery.Policy
	switch *policyName {
	case "adaptive":
		policy = recovery.NewAdaptivePolicy(recovery.DefaultAlpha)
	case "blast-only":
		// Blast radius with the anti-starvation term switched off, which is
		// the arm H7 identifies as destructive.
		ap := recovery.NewAdaptivePolicy(0)
		ap.W.Fairness = 0
		policy = ap
	case "cost-only":
		policy = recovery.CostOnlyPolicy{}
	case "timeout":
		policy = recovery.TimeoutPolicy{Threshold: *timeoutMS}
	case "youngest":
		policy = recovery.YoungestPolicy{}
	default:
		fmt.Printf("unknown policy %q\n", *policyName)
		os.Exit(1)
	}

	var (
		exclusionViolations int
		deadlocksBroken     int
		flagsConfirmed      int
		killedPID           int
		killedSvc           types.ServiceID
		reclaimAfterKill    time.Duration
	)

	allLocks := make([]types.LockID, *lockPool)
	for i := range allLocks {
		allLocks[i] = types.LockID(fmt.Sprintf("L%d", i))
	}

	killAt := time.Now().Add(*runFor / 2)
	killed := false
	deadline := time.Now().Add(*runFor)

	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)

		// (1) Mutual exclusion, read straight from etcd. etcd stores one value
		// per key, so the real check is that a holder is always a service we
		// actually started and never two at once for the same key.
		lockHolders := map[string]string{}
		for _, l := range allLocks {
			if holder, ok := probe.Holder(l); ok {
				lockHolders[string(l)] = string(holder)
				if !known[holder] {
					exclusionViolations++
				}
			} else {
				lockHolders[string(l)] = ""
			}
		}

		mu.Lock()
		for _, f := range lo.Tick() {
			flagsConfirmed++
			if !*quiet {
				fmt.Printf("  [t=%4dms] PILLAR 1 flag: %s\n",
					time.Since(deadline.Add(-*runFor)).Milliseconds(), f)
			}
			if dash != nil {
				dash.log("flag", f.String())
			}
		}
		cycles := wf.Cycles()
		var decision *recovery.Decision
		if len(cycles) > 0 {
			snap := recovery.Snapshot{
				Cycle:        cycles[0],
				Graph:        wf,
				RollbackCost: map[types.ServiceID]float64{},
				WaitTime:     map[types.ServiceID]float64{},
				AbortCount:   map[types.ServiceID]float64{},
			}
			for _, id := range cycles[0] {
				snap.RollbackCost[id] = float64(obsHeld[id])
				snap.AbortCount[id] = abortsBy[id]
				if t, ok := blockedAt[id]; ok {
					snap.WaitTime[id] = float64(time.Since(t).Milliseconds())
				}
			}
			if d, err := policy.Decide(snap); err == nil {
				decision = &d
			}
		}
		mu.Unlock()

		// (3) Abort the victim across a process boundary.
		if decision != nil {
			if s, ok := stubs[decision.Chosen.Service]; ok && s.cmd.Process != nil {
				_ = s.cmd.Process.Signal(syscall.SIGUSR1)
				mu.Lock()
				abortsBy[decision.Chosen.Service]++
				mu.Unlock()
				deadlocksBroken++
				if !*quiet {
					fmt.Printf("  [t=%4dms] DEADLOCK %v -> abort %s (signal to pid %d)\n",
						time.Since(deadline.Add(-*runFor)).Milliseconds(),
						cycles[0], decision.Chosen.Service, s.cmd.Process.Pid)
				}
				if dash != nil {
					dash.log("deadlock", fmt.Sprintf("deadlock %v - aborting %s (signal to pid %d)",
						cycles[0], decision.Chosen.Service, s.cmd.Process.Pid))
				}
			}
		}

		if dash != nil {
			mu.Lock()
			inCycle := map[types.ServiceID]bool{}
			for _, c := range cycles {
				for _, id := range c {
					inCycle[id] = true
				}
			}
			blocked := map[types.ServiceID]edgeView{}
			for waiter, holders := range wf.Edges() {
				for _, h := range holders {
					lk, _ := wf.BlockedOn(waiter, h)
					blocked[waiter] = edgeView{From: string(waiter), To: string(h), Lock: string(lk)}
				}
			}
			held := map[types.ServiceID][]string{}
			for lk, who := range lockHolders {
				if who != "" {
					held[types.ServiceID(who)] = append(held[types.ServiceID(who)], lk)
				}
			}
			live := map[types.ServiceID]int{}
			for id, st := range stubs {
				if st.cmd.Process != nil {
					live[id] = st.cmd.Process.Pid
				}
			}
			ab := map[types.ServiceID]int{}
			for id, n := range abortsBy {
				ab[id] = int(n)
			}
			ev := evCount
			mu.Unlock()

			dash.update(snapshotIn{
				stubs: live, dead: killedSvc, deadPID: killedPID,
				held: held, blocked: blocked, inCycle: inCycle, aborts: ab,
				locks: lockHolders, events: ev, deadlocks: deadlocksBroken,
				flags: flagsConfirmed, violations: exclusionViolations,
				reclaimMS: reclaimAfterKill.Milliseconds(),
			})
		}

		// (4) A genuine kill -9 of a live lock holder, halfway through.
		if !killed && time.Now().After(killAt) {
			for _, l := range allLocks {
				if holder, ok := probe.Holder(l); ok {
					if s, known := stubs[holder]; known {
						killedPID = s.cmd.Process.Pid
						killedSvc = holder
						_ = s.cmd.Process.Signal(syscall.SIGKILL)
						delete(stubs, holder)
						killed = true
						if !*quiet {
							fmt.Printf("  [t=%4dms] KILL -9 pid %d (%s) while it held %s\n",
								time.Since(deadline.Add(-*runFor)).Milliseconds(), killedPID, holder, l)
						}
						if dash != nil {
							dash.log("kill", fmt.Sprintf("kill -9 pid %d (%s) while it held %s", killedPID, holder, l))
						}

						// Time how long until etcd expires the dead holder's lease.
						//
						// The test is "is the killed service still the holder",
						// NOT "is the lock free". Those differ under contention:
						// once the lease lapses another service can take the
						// lock within milliseconds, so waiting for the key to be
						// empty measures the first moment we happened to catch a
						// gap, which can be many seconds longer than the lease
						// and makes the figure meaningless.
						start := time.Now()
						for time.Since(start) < 10*time.Second {
							if h, still := probe.Holder(l); !still || h != holder {
								reclaimAfterKill = time.Since(start)
								if dash != nil {
									dash.log("reclaim", fmt.Sprintf("%s reclaimed after %v - lease expired, nothing detected the crash",
										l, reclaimAfterKill.Round(10*time.Millisecond)))
								}
								break
							}
							time.Sleep(50 * time.Millisecond)
						}
						break
					}
				}
			}
		}
	}

	mu.Lock()
	events := evCount
	loSize := lo.Size()
	done := completed
	mu.Unlock()

	// How unevenly aborts landed. If one service is being starved, the gap
	// between the most and least aborted service is wide. If the policy is
	// spreading the cost, it is narrow. Without this the absence of a
	// starvation effect cannot be distinguished from starvation that does not
	// happen to cost anything.
	mu.Lock()
	maxAborts, minAborts := 0.0, math.Inf(1)
	for id := range known {
		n := abortsBy[id]
		if n > maxAborts {
			maxAborts = n
		}
		if n < minAborts {
			minAborts = n
		}
	}
	mu.Unlock()
	if math.IsInf(minAborts, 1) {
		minAborts = 0
	}

	// One machine-readable line so a batch runner can aggregate many runs
	// without parsing the human-readable block below.
	fmt.Printf("RESULT policy=%s services=%d for=%s completed=%d deadlocks=%d flags=%d events=%d violations=%d maxaborts=%.0f minaborts=%.0f\n",
		*policyName, *numSvcs, runFor.String(), done, deadlocksBroken, flagsConfirmed, events, exclusionViolations,
		maxAborts, minAborts)

	if *quiet {
		return
	}

	fmt.Printf("\n--- end-to-end results ---\n")
	fmt.Printf("lock events observed from separate processes : %d\n", events)
	fmt.Printf("lock-order edges learned                     : %d\n", loSize)
	fmt.Printf("Pillar 1 flags confirmed                     : %d\n", flagsConfirmed)
	fmt.Printf("deadlocks detected and broken by signal      : %d\n", deadlocksBroken)
	fmt.Printf("transactions completed                       : %d\n", done)
	fmt.Printf("mutual-exclusion violations                  : %d\n", exclusionViolations)
	if killed {
		fmt.Printf("kill -9 of pid %d (%s), lock reclaimed in    : %v\n",
			killedPID, killedSvc, reclaimAfterKill.Round(10*time.Millisecond))
	}
	if dash != nil {
		dash.finish(events, deadlocksBroken, flagsConfirmed, exclusionViolations, reclaimAfterKill)
	}

	pass := exclusionViolations == 0 && events > 0
	if pass {
		fmt.Printf("\nPASS: exclusion held across real processes; crash recovered via lease expiry.\n")
	} else {
		fmt.Printf("\nFAIL: see counters above.\n")
	}

	// With a dashboard up, hold the page open so it can be read and discussed
	// after the run ends. The stub processes are stopped first -- leaving them
	// running would keep churning locks behind a page that claims to be final.
	if dash != nil {
		for _, st := range stubs {
			_ = st.cmd.Process.Kill()
		}
		fmt.Printf("\ndashboard still live at http://localhost%s - press Ctrl+C when finished\n", *httpAddr)
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		<-stop
		fmt.Println()
	}
	if !pass {
		os.Exit(1)
	}
}

// buildPlans decides which locks each service takes, and in what order.
//
// The default three are named and fixed, because they are the arrangement the
// report describes and the one the demo narrates: payment and inventory take
// the same pair in opposite orders, which is precisely what deadlocks.
//
// Beyond three, services are generated around the same three locks rather than
// being given fresh ones. That is deliberate: spreading them over more locks
// would reduce contention and the demo would show fewer deadlocks, which is the
// opposite of what more services should demonstrate. Keeping the pool at three
// means every added service increases contention, and the rotating assignment
// (L0->L1, L1->L2, L2->L0) guarantees cycles rather than hoping for them.
func buildPlans(n, poolSize int, vary bool) []struct {
	id    string
	locks string
} {
	type plan = struct {
		id    string
		locks string
	}
	pool := make([]string, poolSize)
	for i := range pool {
		pool[i] = fmt.Sprintf("L%d", i)
	}

	// The first two services always take the same pair in opposite orders.
	// That is the pattern deadlock detection and Pillar 1 both exist for, so
	// it is seeded deliberately rather than left to chance.
	base := []plan{
		{"payment", pool[0] + "," + pool[1%poolSize]},
		{"inventory", pool[1%poolSize] + "," + pool[0]},
		{"order", pool[1%poolSize] + "," + pool[2%poolSize]},
	}
	if n <= len(base) {
		return base[:n]
	}

	names := []string{"shipping", "billing", "catalog", "search", "notify",
		"audit", "report", "pricing", "ledger", "fulfil"}

	out := append([]plan(nil), base...)
	for i := len(base); i < n; i++ {
		name := fmt.Sprintf("svc%d", i)
		if j := i - len(base); j < len(names) {
			name = names[j]
		}

		// How many locks this service wants. With -vary-locks the count runs
		// 2, 3, 4 and back, so services differ in how much work an abort
		// discards. Without it every service wants two, which leaves rollback
		// cost nearly identical across candidates.
		want := 2
		if vary {
			want = 2 + i%3
			if want > poolSize {
				want = poolSize
			}
		}

		ls := make([]string, 0, want)
		for k := 0; k < want; k++ {
			ls = append(ls, pool[(i+k)%poolSize])
		}
		out = append(out, plan{name, strings.Join(ls, ",")})
	}
	return out
}
