// Command stubservice is one simulated microservice, running as its own OS
// process and talking to a real etcd.
//
// It exists so that crash injection is a genuine kill -9 rather than a flag
// flipped inside the simulator. Section 7.4 of the report claims exactly this,
// and it is only true if the services really are separate processes.
//
// The service acquires its locks in a fixed order, holds them, releases them,
// then thinks for a while. Every lock event is written to stdout as a JSON
// line so the orchestrator can feed the dependency monitor without needing a
// watch implementation.
//
// SIGUSR1 means "abort": drop everything and restart the transaction. That is
// how the recovery engine preempts a victim across a process boundary.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"adlmf/internal/types"
	"adlmf/lockservice"
)

func main() {
	id := flag.String("id", "svc", "service id")
	endpoint := flag.String("etcd", "http://127.0.0.1:2379", "etcd endpoint")
	prefix := flag.String("prefix", "/adlmf/e2e", "etcd key prefix")
	lockList := flag.String("locks", "L0,L1", "locks to acquire, in order")
	holdMS := flag.Int("hold", 120, "ms to hold once all locks are held")
	thinkMS := flag.Int("think", 60, "max ms to idle between transactions")
	ttl := flag.Int("ttl", 3, "lock lease TTL in seconds")
	flag.Parse()

	svc := types.ServiceID(*id)
	var locks []types.LockID
	for _, l := range strings.Split(*lockList, ",") {
		if l = strings.TrimSpace(l); l != "" {
			locks = append(locks, types.LockID(l))
		}
	}

	backend := lockservice.NewEtcdBackend(*endpoint, *prefix)
	if err := backend.Ping(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: cannot reach etcd: %v\n", svc, err)
		os.Exit(1)
	}

	// Relay every event the backend emits to stdout as JSON.
	events := backend.Subscribe()
	enc := json.NewEncoder(os.Stdout)
	go func() {
		for ev := range events {
			_ = enc.Encode(ev)
		}
	}()

	// SIGUSR1 is the abort signal from the recovery engine.
	var abort atomic.Bool
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGUSR1)
	go func() {
		for range sig {
			abort.Store(true)
		}
	}()

	rng := rand.New(rand.NewSource(time.Now().UnixNano() ^ int64(len(*id))))
	lease := time.Duration(*ttl) * time.Second

	for {
		var held []lockservice.Handle
		abort.Store(false)

		// Acquire every lock in order, retrying while blocked.
		ok := true
		for _, want := range locks {
			for {
				if abort.Load() {
					ok = false
					break
				}
				h, err := backend.TryAcquire(svc, want, lease)
				if err == nil {
					held = append(held, h)
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !ok {
				break
			}
		}

		if ok {
			// Hold, renewing so a live service never loses its lease. A
			// crashed one stops here and etcd expires the locks for us.
			deadline := time.Now().Add(time.Duration(*holdMS) * time.Millisecond)
			for time.Now().Before(deadline) && !abort.Load() {
				time.Sleep(20 * time.Millisecond)
				for i := range held {
					if nh, err := backend.Renew(held[i], lease); err == nil {
						held[i] = nh
					}
				}
			}

			// A completed transaction is one that took every lock it wanted
			// and held them for the full duration without being aborted. The
			// marker is deliberately not JSON, so the harness can tell it
			// apart from a lock event without changing the event type.
			if !abort.Load() {
				fmt.Printf("#DONE %s\n", svc)
			}
		}

		// Release in reverse order, whether we finished or were aborted.
		for i := len(held) - 1; i >= 0; i-- {
			_ = backend.Release(held[i])
		}

		time.Sleep(time.Duration(rng.Intn(*thinkMS+1)) * time.Millisecond)
	}
}
