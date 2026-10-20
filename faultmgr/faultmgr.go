// Package faultmgr handles what happens when services fail rather than
// deadlock.
//
// It owns three jobs the report assigns to the Fault Manager:
//
//   - Lease expiry. It drives reclamation of locks whose holder stopped
//     renewing. Against etcd the server does the expiring; against the
//     in-memory backend this component does it. Either way the rest of the
//     system sees the same thing.
//   - Crash detection. A service that stops reporting events is probably gone.
//   - Partition suspicion. A service that has gone silent might be crashed, or
//     might merely be unreachable. These look identical from outside, which is
//     exactly why the recovery engine is told about it rather than guessing:
//     aborting a service you cannot reach does not fix anything.
//
// The last point is the one that matters for correctness of the *decision*.
// Section 9 of the report lists "mistaking a network partition for a deadlock"
// as a risk; this is the component that supplies the evidence for telling them
// apart.
package faultmgr

import (
	"sync"
	"time"

	"adlmf/internal/types"
	"adlmf/lockservice"
)

// Manager watches liveness and drives stale-lock reclamation.
type Manager struct {
	backend lockservice.Backend
	clock   lockservice.Clock

	// silenceBeforeSuspect is how long a service may go quiet before it is
	// treated as possibly unreachable.
	silenceBeforeSuspect time.Duration

	mu       sync.Mutex
	lastSeen map[types.ServiceID]time.Time
	reaped   int
}

// New builds a fault manager over a backend.
func New(backend lockservice.Backend, clock lockservice.Clock, silenceBeforeSuspect time.Duration) *Manager {
	return &Manager{
		backend:              backend,
		clock:                clock,
		silenceBeforeSuspect: silenceBeforeSuspect,
		lastSeen:             make(map[types.ServiceID]time.Time),
	}
}

// Observe records that a service is alive, because we just heard from it.
func (m *Manager) Observe(ev types.LockEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSeen[ev.Service] = m.clock.Now()
}

// Heartbeat records liveness without a lock event. A service that is idle is
// not the same as a service that is gone.
func (m *Manager) Heartbeat(svc types.ServiceID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSeen[svc] = m.clock.Now()
}

// Tick runs one round of reclamation and returns how many locks were reclaimed.
func (m *Manager) Tick() int {
	n := m.backend.ReapExpired()
	m.mu.Lock()
	m.reaped += n
	m.mu.Unlock()
	return n
}

// PartitionSuspects returns the services that have gone quiet for longer than
// the threshold.
//
// Note what this deliberately does not do: it does not claim these services are
// crashed, and it does not claim they are partitioned. It reports that we
// cannot tell, which is the honest state of knowledge, and lets the recovery
// engine weigh that. A service that is merely unreachable is a bad victim
// because the abort may never reach it.
func (m *Manager) PartitionSuspects() map[types.ServiceID]bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.clock.Now()
	out := make(map[types.ServiceID]bool)
	for svc, seen := range m.lastSeen {
		if now.Sub(seen) > m.silenceBeforeSuspect {
			out[svc] = true
		}
	}
	return out
}

// Reclaimed reports the cumulative number of stale locks reclaimed.
func (m *Manager) Reclaimed() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reaped
}
