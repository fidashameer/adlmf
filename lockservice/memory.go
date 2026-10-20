package lockservice

import (
	"sync"
	"time"

	"adlmf/internal/types"
)

// lockState is the backend's record of one lock.
type lockState struct {
	held    bool
	holder  types.ServiceID
	token   uint64
	expiry  time.Time
	waiters []types.ServiceID // services currently blocked on this lock
}

// MemoryBackend is an in-process Backend.
//
// It is not a toy: it implements the same lease and fencing-token semantics as
// the etcd backend, so correctness tests written against it stay meaningful.
// What it does not do is survive a process crash or coordinate across
// machines, which is exactly why the etcd backend exists.
type MemoryBackend struct {
	mu        sync.Mutex
	clock     Clock
	locks     map[types.LockID]*lockState
	seq       map[types.ServiceID]uint64
	nextToken uint64
	subs      []chan types.LockEvent
	closed    bool
}

// NewMemoryBackend builds an in-memory backend driven by the given clock.
// Pass SystemClock{} for normal use or a ManualClock in tests.
func NewMemoryBackend(clock Clock) *MemoryBackend {
	return &MemoryBackend{
		clock: clock,
		locks: make(map[types.LockID]*lockState),
		seq:   make(map[types.ServiceID]uint64),
	}
}

// stateOf returns the record for a lock, creating it on first use.
// Caller must hold b.mu.
func (b *MemoryBackend) stateOf(lock types.LockID) *lockState {
	st, ok := b.locks[lock]
	if !ok {
		st = &lockState{}
		b.locks[lock] = st
	}
	return st
}

// emit publishes an event to every subscriber.
// Caller must hold b.mu.
func (b *MemoryBackend) emit(kind types.EventKind, service types.ServiceID, lock types.LockID, token uint64) {
	b.seq[service]++
	ev := types.LockEvent{
		Kind:    kind,
		Service: service,
		Lock:    lock,
		Seq:     b.seq[service],
		Token:   token,
	}
	for _, ch := range b.subs {
		// Non-blocking: a slow subscriber drops events rather than stalling
		// the lock service. That mirrors reality, where an observation can be
		// lost, and the monitor is written to tolerate it.
		select {
		case ch <- ev:
		default:
		}
	}
}

// removeWaiter drops a service from a lock's waiter list.
// Caller must hold b.mu.
func removeWaiter(st *lockState, service types.ServiceID) {
	for i, w := range st.waiters {
		if w == service {
			st.waiters = append(st.waiters[:i], st.waiters[i+1:]...)
			return
		}
	}
}

// TryAcquire implements Backend.
func (b *MemoryBackend) TryAcquire(service types.ServiceID, lock types.LockID, ttl time.Duration) (Handle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.clock.Now()
	st := b.stateOf(lock)

	// A lease that has already lapsed does not block anyone, even if nobody
	// has reaped it yet.
	if st.held && !now.Before(st.expiry) {
		prev := st.holder
		st.held = false
		st.holder = ""
		b.emit(types.Released, prev, lock, st.token)
	}

	if st.held {
		if st.holder == service {
			// Already ours: treat as a renewal so the call is idempotent.
			st.expiry = now.Add(ttl)
			return Handle{Service: service, Lock: lock, Token: st.token, Expiry: st.expiry}, nil
		}
		// Record the caller as blocked. This is what makes the wait-for graph
		// observable from outside.
		already := false
		for _, w := range st.waiters {
			if w == service {
				already = true
				break
			}
		}
		if !already {
			st.waiters = append(st.waiters, service)
			b.emit(types.Waiting, service, lock, 0)
		}
		return Handle{}, ErrHeld
	}

	// Free: grant it.
	b.nextToken++
	st.held = true
	st.holder = service
	st.token = b.nextToken
	st.expiry = now.Add(ttl)
	removeWaiter(st, service)
	b.emit(types.Acquired, service, lock, st.token)

	return Handle{Service: service, Lock: lock, Token: st.token, Expiry: st.expiry}, nil
}

// Release implements Backend.
func (b *MemoryBackend) Release(h Handle) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	st, ok := b.locks[h.Lock]
	if !ok || !st.held || st.holder != h.Service || st.token != h.Token {
		return ErrNotHolder
	}
	st.held = false
	st.holder = ""
	b.emit(types.Released, h.Service, h.Lock, h.Token)
	return nil
}

// Renew implements Backend.
func (b *MemoryBackend) Renew(h Handle, ttl time.Duration) (Handle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.clock.Now()
	st, ok := b.locks[h.Lock]
	if !ok || !st.held || st.holder != h.Service || st.token != h.Token {
		return Handle{}, ErrNotHolder
	}
	if !now.Before(st.expiry) {
		// Lapsed leases cannot be renewed. The holder has to re-acquire, and
		// will get a new fencing token if it succeeds.
		return Handle{}, ErrLeaseExpired
	}
	st.expiry = now.Add(ttl)
	return Handle{Service: h.Service, Lock: h.Lock, Token: st.token, Expiry: st.expiry}, nil
}

// ReapExpired implements Backend.
func (b *MemoryBackend) ReapExpired() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.clock.Now()
	n := 0
	for id, st := range b.locks {
		if st.held && !now.Before(st.expiry) {
			prev := st.holder
			tok := st.token
			st.held = false
			st.holder = ""
			b.emit(types.Released, prev, id, tok)
			n++
		}
	}
	return n
}

// Subscribe implements Backend.
func (b *MemoryBackend) Subscribe() <-chan types.LockEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan types.LockEvent, 1024)
	b.subs = append(b.subs, ch)
	return ch
}

// Holder implements Backend.
func (b *MemoryBackend) Holder(lock types.LockID) (types.ServiceID, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.locks[lock]
	if !ok || !st.held {
		return "", false
	}
	if !b.clock.Now().Before(st.expiry) {
		return "", false
	}
	return st.holder, true
}

// Waiters reports which services are currently blocked on a lock. Used by the
// dependency monitor and by tests.
func (b *MemoryBackend) Waiters(lock types.LockID) []types.ServiceID {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.locks[lock]
	if !ok {
		return nil
	}
	out := make([]types.ServiceID, len(st.waiters))
	copy(out, st.waiters)
	return out
}

// Close implements Backend.
func (b *MemoryBackend) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, ch := range b.subs {
		close(ch)
	}
	b.subs = nil
}

// compile-time check
var _ Backend = (*MemoryBackend)(nil)
