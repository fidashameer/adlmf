package lockservice

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"adlmf/internal/types"
)

// etcdEndpoint lets CI or a different machine point the tests elsewhere.
func etcdEndpoint() string {
	if v := os.Getenv("ADLMF_ETCD"); v != "" {
		return v
	}
	return "http://127.0.0.1:2379"
}

// newEtcdForTest returns a backend on a unique prefix, or skips the test if no
// etcd is reachable. These are integration tests: with no server they are
// skipped, not failed.
func newEtcdForTest(t *testing.T) *EtcdBackend {
	t.Helper()
	b := NewEtcdBackend(etcdEndpoint(), fmt.Sprintf("/adlmf-test/%s/%d", t.Name(), time.Now().UnixNano()))
	if err := b.Ping(); err != nil {
		t.Skipf("no etcd at %s (%v)", etcdEndpoint(), err)
	}
	return b
}

// TestEtcdMutualExclusion is the same property as the in-memory test, but the
// exclusion is now enforced by etcd's Raft log rather than by a mutex in our
// own process. This is the test that makes the lock service's correctness claim
// mean something.
func TestEtcdMutualExclusion(t *testing.T) {
	b := newEtcdForTest(t)
	defer b.Close()

	const services = 8
	lock := types.LockID("shared")

	var inside int32
	var violations int32
	var granted int32
	var wg sync.WaitGroup

	for i := 0; i < services; i++ {
		svc := types.ServiceID(fmt.Sprintf("svc-%d", i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 10; n++ {
				h, err := b.TryAcquire(svc, lock, 10*time.Second)
				if err != nil {
					continue
				}
				atomic.AddInt32(&granted, 1)
				if atomic.AddInt32(&inside, 1) != 1 {
					atomic.AddInt32(&violations, 1)
				}
				time.Sleep(2 * time.Millisecond)
				atomic.AddInt32(&inside, -1)
				if err := b.Release(h); err != nil {
					t.Errorf("release: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if violations != 0 {
		t.Fatalf("etcd mutual exclusion violated %d times", violations)
	}
	if granted == 0 {
		t.Fatal("no service ever acquired the lock; the test proved nothing")
	}
	t.Logf("%d acquisitions across %d services, 0 mutual-exclusion violations", granted, services)
}

// TestEtcdFencingTokenIncreases checks that etcd revisions give us a usable
// fencing token, and that a stale holder cannot release the new holder's lock.
func TestEtcdFencingTokenIncreases(t *testing.T) {
	b := newEtcdForTest(t)
	defer b.Close()

	lock := types.LockID("config")

	first, err := b.TryAcquire("payment", lock, 10*time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := b.Release(first); err != nil {
		t.Fatalf("release: %v", err)
	}
	second, err := b.TryAcquire("order", lock, 10*time.Second)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}

	if !(second.Token > first.Token) {
		t.Fatalf("fencing token did not increase: first=%d second=%d", first.Token, second.Token)
	}

	// The stale holder tries to release a lock it no longer owns. The txn
	// compares mod_revision and must reject it.
	if err := b.Release(first); err != ErrNotHolder {
		t.Fatalf("stale holder must not be able to release, got %v", err)
	}
	t.Logf("fencing tokens: first=%d second=%d (etcd revisions)", first.Token, second.Token)
}

// TestEtcdLeaseExpiryReclaimsAfterCrash is the crash scenario from Section 8.
// A service takes a lock with a short lease and then "dies" — we simply stop
// renewing. etcd expires the lease, deletes the key, and the lock becomes
// available again without anyone having detected the crash explicitly.
func TestEtcdLeaseExpiryReclaimsAfterCrash(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping lease-expiry test in short mode (needs real elapsed time)")
	}
	b := newEtcdForTest(t)
	defer b.Close()

	lock := types.LockID("inventory")
	if _, err := b.TryAcquire("payment", lock, 1*time.Second); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// payment crashes here: no release, no renewal.

	if _, err := b.TryAcquire("order", lock, 5*time.Second); err != ErrHeld {
		t.Fatalf("order should be blocked while the lease is live, got %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	var acquired bool
	for time.Now().Before(deadline) {
		if _, err := b.TryAcquire("order", lock, 5*time.Second); err == nil {
			acquired = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !acquired {
		t.Fatal("lock was never reclaimed after the lease should have expired")
	}
}

// TestEtcdRenewKeepsLock checks a well-behaved holder keeps its lock past the
// original TTL by renewing.
func TestEtcdRenewKeepsLock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping renewal test in short mode")
	}
	b := newEtcdForTest(t)
	defer b.Close()

	lock := types.LockID("rate-limiter")
	h, err := b.TryAcquire("payment", lock, 2*time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	for i := 0; i < 3; i++ {
		time.Sleep(700 * time.Millisecond)
		if _, err := b.Renew(h, 2*time.Second); err != nil {
			t.Fatalf("renew %d: %v", i, err)
		}
	}

	if holder, ok := b.Holder(lock); !ok || holder != "payment" {
		t.Fatalf("payment should still hold the lock after renewals, got %q ok=%v", holder, ok)
	}
	if err := b.Release(h); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// TestEtcdEmitsWaitingEvents checks the wait-for graph can be fed from the etcd
// backend exactly as it is from the in-memory one.
func TestEtcdEmitsWaitingEvents(t *testing.T) {
	b := newEtcdForTest(t)
	defer b.Close()
	sub := b.Subscribe()

	lock := types.LockID("L1")
	if _, err := b.TryAcquire("payment", lock, 10*time.Second); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := b.TryAcquire("order", lock, 10*time.Second); err != ErrHeld {
		t.Fatalf("expected ErrHeld")
	}

	got := drain(sub, 2)
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %v", got)
	}
	if got[0].Kind != types.Acquired || got[1].Kind != types.Waiting {
		t.Fatalf("expected ACQUIRED then WAITING, got %v", got)
	}
}
