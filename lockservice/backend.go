// Package lockservice provides lease-based distributed locking.
//
// The Backend interface is the seam that keeps the rest of the project
// independent of where locks actually live. Two implementations are planned:
//
//   - MemoryBackend  — in-process, used for deterministic tests and for the
//     benchmark harness, where we need repeatable contention and the ability
//     to step time forward by hand.
//   - EtcdBackend    — real distributed locking on etcd, using its lease API
//     for TTLs and its watch API to stream events.
//
// Everything downstream (dependency monitor, recovery engine, harness) talks
// to this interface only, so the same experiment can be run against either.
package lockservice

import (
	"errors"
	"time"

	"adlmf/internal/types"
)

var (
	// ErrHeld is returned by TryAcquire when another service holds the lock.
	ErrHeld = errors.New("lockservice: lock is held by another service")
	// ErrNotHolder is returned when a service tries to release or renew a
	// lock it does not currently hold.
	ErrNotHolder = errors.New("lockservice: caller is not the current holder")
	// ErrLeaseExpired is returned when a handle's lease has already lapsed.
	ErrLeaseExpired = errors.New("lockservice: lease has expired")
)

// Handle is proof that a service holds a lock.
//
// Token is a fencing token: a per-lock counter that strictly increases every
// time the lock is granted. It exists because of the Kleppmann/antirez
// argument discussed in the report — a holder that pauses long enough for its
// lease to lapse can come back and try to act as though it still holds the
// lock. A resource that records the highest token it has seen can reject the
// stale holder's write. Without this, lease-based locking is not safe under
// arbitrary process pauses.
type Handle struct {
	Service types.ServiceID
	Lock    types.LockID
	Token   uint64
	Expiry  time.Time
}

// Backend is the pluggable lock implementation.
type Backend interface {
	// TryAcquire attempts to take the lock without blocking. On success it
	// returns a Handle whose lease runs for ttl. On failure it returns ErrHeld
	// and records the caller as a waiter, which is what makes the wait-for
	// graph observable.
	TryAcquire(service types.ServiceID, lock types.LockID, ttl time.Duration) (Handle, error)

	// Release gives the lock up. Releasing a lock you no longer hold (because
	// your lease lapsed and someone else took it) returns ErrNotHolder.
	Release(h Handle) error

	// Renew extends an unexpired lease by ttl from now. A lapsed lease cannot
	// be renewed; that is the whole point of a lease.
	Renew(h Handle, ttl time.Duration) (Handle, error)

	// ReapExpired reclaims every lock whose lease has lapsed and returns how
	// many it reclaimed. It is driven by the fault manager rather than running
	// on a timer inside the backend, so that tests can control exactly when
	// reclamation happens.
	ReapExpired() int

	// Subscribe returns a stream of lock events. Each subscriber gets its own
	// channel. Used by the dependency monitor.
	Subscribe() <-chan types.LockEvent

	// Holder reports who currently holds a lock, if anyone.
	Holder(lock types.LockID) (types.ServiceID, bool)

	// Close shuts the backend down and closes subscriber channels.
	Close()
}
