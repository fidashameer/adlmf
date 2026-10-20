// Package types holds the data types shared between the lock service, the
// dependency monitor and the recovery engine.
//
// Design note: there is deliberately no wall-clock timestamp on a lock event.
// Services in this system are assumed to be independently deployed with no
// shared clock, so each service stamps its events with its own monotonically
// increasing sequence number. The monitor may observe events late, out of
// order, or not at all. Anything that reasons about these events has to cope
// with that rather than assume a global ordering.
package types

import "fmt"

// ServiceID identifies one microservice instance (e.g. "payment").
type ServiceID string

// LockID identifies one lockable shared resource (e.g. "inventory/sku-1").
type LockID string

// EventKind describes what happened to a lock.
type EventKind int

const (
	// Acquired: the service now holds the lock.
	Acquired EventKind = iota
	// Waiting: the service has started blocking on a lock someone else holds.
	Waiting
	// Released: the service gave the lock up (voluntarily or via lease expiry).
	Released
)

func (k EventKind) String() string {
	switch k {
	case Acquired:
		return "ACQUIRED"
	case Waiting:
		return "WAITING"
	case Released:
		return "RELEASED"
	}
	return "UNKNOWN"
}

// LockEvent is one observation reported by a service about its own lock usage.
//
// Seq is per-service and strictly increasing. It gives a total order *within*
// one service's own history, which is all the ordering we are entitled to
// assume. It gives no ordering between two different services.
type LockEvent struct {
	Kind    EventKind
	Service ServiceID
	Lock    LockID
	Seq     uint64
	// Token is the fencing token handed out with the lock. It only has a
	// meaningful value on Acquired events. See lockservice for why it exists.
	Token uint64
}

func (e LockEvent) String() string {
	return fmt.Sprintf("%s service=%s lock=%s seq=%d", e.Kind, e.Service, e.Lock, e.Seq)
}
