// Package depmonitor builds and reasons about two separate graphs.
//
// This split is the single most important structural decision in the project,
// so it is worth stating plainly:
//
//  1. The WAIT-FOR GRAPH is reactive and live. An edge A -> B means "service A
//     is blocked right now on a lock that service B holds right now". A cycle
//     in this graph is a deadlock that has already happened. Building it and
//     finding cycles in it is standard, cited work (Chandy-Misra-Haas lineage)
//     and is not claimed as a contribution. Edges disappear as locks are
//     released.
//
//  2. The LOCK-ORDER GRAPH (see lockorder.go) is predictive and historical. An
//     edge A -> B means "some service has, at some point in the past, held
//     lock A and then gone on to acquire lock B". It is not cleared when locks
//     are released, because it records habits rather than current contention.
//     A second service acquiring those same two locks in the opposite order is
//     a warning sign *before* anybody is blocked. That is Pillar 1.
//
// Both are fed from the same event stream, which may be delayed, reordered
// across services, or incomplete.
package depmonitor

import (
	"sort"

	"adlmf/internal/types"
)

// WaitForGraph is the live "who is blocked on whom" graph.
type WaitForGraph struct {
	// holder[lock] = the service believed to hold that lock.
	holder map[types.LockID]types.ServiceID
	// waits[service] = set of locks that service is believed to be blocked on.
	waits map[types.ServiceID]map[types.LockID]bool
}

// NewWaitForGraph returns an empty graph.
func NewWaitForGraph() *WaitForGraph {
	return &WaitForGraph{
		holder: make(map[types.LockID]types.ServiceID),
		waits:  make(map[types.ServiceID]map[types.LockID]bool),
	}
}

// Apply folds one observation into the graph.
func (g *WaitForGraph) Apply(ev types.LockEvent) {
	switch ev.Kind {
	case types.Acquired:
		g.holder[ev.Lock] = ev.Service
		// Acquiring a lock means you are no longer waiting for it.
		if s, ok := g.waits[ev.Service]; ok {
			delete(s, ev.Lock)
			if len(s) == 0 {
				delete(g.waits, ev.Service)
			}
		}
	case types.Waiting:
		if g.waits[ev.Service] == nil {
			g.waits[ev.Service] = make(map[types.LockID]bool)
		}
		g.waits[ev.Service][ev.Lock] = true
	case types.Released:
		// Only clear the holder if this service is in fact the one we think
		// holds it. A late-arriving Released from a previous holder must not
		// erase the current one.
		if cur, ok := g.holder[ev.Lock]; ok && cur == ev.Service {
			delete(g.holder, ev.Lock)
		}
	}
}

// Holder returns who is believed to hold a lock.
func (g *WaitForGraph) Holder(lock types.LockID) (types.ServiceID, bool) {
	s, ok := g.holder[lock]
	return s, ok
}

// Edges returns the wait-for adjacency: edges[A] contains B when A is blocked
// on a lock held by B. Self-edges are dropped, since a service waiting on a
// lock it already holds is a reentrancy artefact, not a deadlock.
func (g *WaitForGraph) Edges() map[types.ServiceID][]types.ServiceID {
	out := make(map[types.ServiceID][]types.ServiceID)
	for waiter, locks := range g.waits {
		seen := make(map[types.ServiceID]bool)
		for lock := range locks {
			h, ok := g.holder[lock]
			if !ok || h == waiter || seen[h] {
				continue
			}
			seen[h] = true
			out[waiter] = append(out[waiter], h)
		}
		sort.Slice(out[waiter], func(i, j int) bool { return out[waiter][i] < out[waiter][j] })
	}
	return out
}

// BlockedOn reports which lock is making `waiter` wait for `holder`, if any.
// The recovery engine needs this to say which lock to break.
func (g *WaitForGraph) BlockedOn(waiter, holder types.ServiceID) (types.LockID, bool) {
	for lock := range g.waits[waiter] {
		if h, ok := g.holder[lock]; ok && h == holder {
			return lock, true
		}
	}
	return "", false
}

// Cycles returns every set of services that are mutually deadlocked.
//
// It uses Tarjan's strongly-connected-components algorithm. Any SCC with more
// than one member is a set of services that can each reach the others, which
// is exactly a deadlock. This runs in O(V+E) and returns the whole participant
// set, which is what the recovery engine needs in order to choose a victim
// from among them.
func (g *WaitForGraph) Cycles() [][]types.ServiceID {
	edges := g.Edges()

	// Collect every node that appears as a waiter or as a holder of a
	// contended lock, so the traversal is over a stable node set.
	nodeSet := make(map[types.ServiceID]bool)
	for from, tos := range edges {
		nodeSet[from] = true
		for _, to := range tos {
			nodeSet[to] = true
		}
	}
	nodes := make([]types.ServiceID, 0, len(nodeSet))
	for n := range nodeSet {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i] < nodes[j] })

	var (
		index    = make(map[types.ServiceID]int)
		lowlink  = make(map[types.ServiceID]int)
		onStack  = make(map[types.ServiceID]bool)
		stack    []types.ServiceID
		counter  int
		sccs     [][]types.ServiceID
		strongOf func(v types.ServiceID)
	)

	strongOf = func(v types.ServiceID) {
		index[v] = counter
		lowlink[v] = counter
		counter++
		stack = append(stack, v)
		onStack[v] = true

		for _, w := range edges[v] {
			if _, seen := index[w]; !seen {
				strongOf(w)
				if lowlink[w] < lowlink[v] {
					lowlink[v] = lowlink[w]
				}
			} else if onStack[w] {
				if index[w] < lowlink[v] {
					lowlink[v] = index[w]
				}
			}
		}

		if lowlink[v] == index[v] {
			var comp []types.ServiceID
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			if len(comp) > 1 {
				sort.Slice(comp, func(i, j int) bool { return comp[i] < comp[j] })
				sccs = append(sccs, comp)
			}
		}
	}

	for _, n := range nodes {
		if _, seen := index[n]; !seen {
			strongOf(n)
		}
	}
	return sccs
}

// Dependents returns every service that is transitively blocked on `target`,
// not counting target itself.
//
// This is the raw input to blast radius. If we abort `target`, its work is
// rolled back, and everything queued up behind it is affected. Reactive
// victim-selection schemes that only look at the victim's own rollback cost
// are exactly the ones that cannot see this set.
func (g *WaitForGraph) Dependents(target types.ServiceID) []types.ServiceID {
	edges := g.Edges()

	// Reverse the adjacency: who is waiting on whom.
	reverse := make(map[types.ServiceID][]types.ServiceID)
	for waiter, holders := range edges {
		for _, h := range holders {
			reverse[h] = append(reverse[h], waiter)
		}
	}

	seen := map[types.ServiceID]bool{target: true}
	queue := []types.ServiceID{target}
	var out []types.ServiceID
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, dep := range reverse[cur] {
			if seen[dep] {
				continue
			}
			seen[dep] = true
			out = append(out, dep)
			queue = append(queue, dep)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// DependentsExcluding returns every service that can transitively reach
// `target` in the wait-for graph without passing through a service in
// `exclude`.
//
// Why the exclusion set matters: inside a deadlock cycle every member is
// transitively blocked on every other member, so plain reverse reachability
// scores all candidates identically and tells the recovery engine nothing.
// Excluding the other cycle members answers the question that actually
// discriminates between them: how much work is queued up behind this service
// from *outside* the deadlock. That is the quantity that differs between
// victims and the one worth weighing.
func (g *WaitForGraph) DependentsExcluding(target types.ServiceID, exclude map[types.ServiceID]bool) []types.ServiceID {
	edges := g.Edges()

	reverse := make(map[types.ServiceID][]types.ServiceID)
	for waiter, holders := range edges {
		for _, h := range holders {
			reverse[h] = append(reverse[h], waiter)
		}
	}

	seen := map[types.ServiceID]bool{target: true}
	queue := []types.ServiceID{target}
	var out []types.ServiceID
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, dep := range reverse[cur] {
			if seen[dep] || exclude[dep] {
				continue
			}
			seen[dep] = true
			out = append(out, dep)
			queue = append(queue, dep)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
