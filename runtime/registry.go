package runtime

// The registry is the orchestrator (system design §4.15, mechanic 3). It exposes
// exactly two mutating verbs — Load (insert) and Remove (retire) — and never sets
// a lifecycle state by hand. Every transition is DERIVED: after each edit the
// registry recomputes the target view (which components should be active, given
// the declarations) and reconciles the running composition toward it. Because the
// target is a function of the declared configuration alone, the quiescent system
// is independent of load order — confluence, the runtime mirror of "no recency,
// no fiat" (mechanic + §4.15 law table).
//
// Reactive coeffects (Law 2, mechanic 4) fall out of this: a component activates
// only once ALL its injected keys are provided by active fibers, and deactivates
// the instant a declared dependency is withdrawn — auto-reopen's runtime twin.
// The withdrawal guard and its termination proof are the second half of the
// candidate fifth Lean module (proofs/ForgeProofs/Runtime.lean, stubbed).

import (
	"sort"
	"sync"
)

// Registry holds a set of fibers and reconciles them toward the target view.
type Registry struct {
	mu       sync.Mutex
	fibers   map[string]*Fiber
	epochSeq uint64

	inCycle map[string]bool // fibers currently caught in a declaration cycle
	cycles  [][]string      // the cycles themselves, for reporting (mechanic 10)
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{fibers: make(map[string]*Fiber)}
}

// Load inserts (or replaces) a component and reconciles. A parent name, if given
// and known, nests the new fiber under it — fibers form a tree. Loading is the
// only way a component enters composition; whether it actually activates is
// derived, not commanded. A prior load of the same name is replaced with a fresh
// epoch (a new identity), so dependents that resolved to the old one will reload.
func (r *Registry) Load(c *Component, parent string) *Fiber {
	r.mu.Lock()
	defer r.mu.Unlock()
	c.normalize()
	r.epochSeq++
	f := &Fiber{id: c.Name, epoch: r.epochSeq, component: c, state: StateInactive}
	if old := r.fibers[c.Name]; old != nil {
		// Withdraw the provider through reconciliation so its dependents dispose first.
		old.removed = true
		r.reconcile()
		f.children = old.children
		for _, child := range f.children {
			child.parent = f
		}
		old.children = nil
		r.unlink(old)
	}
	r.fibers[c.Name] = f
	if parent != "" && parent != c.Name {
		if p := r.fibers[parent]; p != nil {
			f.parent = p
			p.children = append(p.children, f)
		}
	}
	for _, g := range r.fibers {
		g.withheld = false
	}
	r.reconcile()
	r.gc()
	return f
}

// Remove retires a fiber and reconciles. Retiring cascades down the tree: the
// fiber and all its descendants become ineligible, so unloading a parent unloads
// its children (mechanic 1). Dependents that resolved to a removed provider lose
// their coeffect and deactivate in turn (mechanic 4). The retired fibers are
// swept once quiescent, so re-Loading the same name later reactivates whatever
// still depends on it.
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	f := r.fibers[id]
	if f == nil {
		return
	}
	for _, d := range f.subtree() {
		d.removed = true
	}
	for _, g := range r.fibers {
		g.withheld = false
	}
	r.reconcile()
	r.gc()
}

// Lookup reads the DERIVED service table (mechanic 2): the live binding for key,
// taken from the active fibers' provisions. There is no stored global table — this
// computes the answer each call, so the table can never drift from the fibers that
// actually provide it.
func (r *Registry) Lookup(key string) (any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.activeProvider(key); ok {
		return p.provisions[key], true
	}
	return nil, false
}

// Fiber returns the fiber loaded under id, or nil if none is loaded (a removed
// fiber, once swept, is gone).
func (r *Registry) Fiber(id string) *Fiber {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fibers[id]
}

// Cycles reports the dependency cycles detected from the declarations at the last
// reconcile (mechanic 10: cycles are predicted, not discovered). The involved
// components stay inactive rather than deadlocking; each returned slice is one
// cycle's component names. An empty result means the composition is acyclic.
func (r *Registry) Cycles() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.cycles))
	for i, c := range r.cycles {
		out[i] = append([]string(nil), c...)
	}
	return out
}

// reconcile drives the running composition to the target view. It runs to a
// fixpoint: each pass deactivates fibers that fell out of target (and reloads
// those whose provider identity went stale), then activates fibers that entered
// it, until a pass changes nothing. Deadlock-freedom and termination are the
// guarantees the source calculus proves; the pass bound is a belt-and-suspenders
// stop, never expected to be reached. Called with r.mu held.
func (r *Registry) reconcile() {
	r.detectCycles()
	const maxPasses = 256
	for pass := 0; pass < maxPasses; pass++ {
		target := r.computeTarget()

		var deact []*Fiber
		for _, f := range r.fibers {
			if f.state != StateActive {
				continue
			}
			// Fell out of target, or its committed provider identity went stale
			// (a provider was replaced) — either way it must unwind.
			if !target[f.id] || r.viewStale(f) {
				deact = append(deact, f)
			}
		}
		r.deactivateInOrder(deact)

		var act []*Fiber
		for _, f := range r.fibers {
			if target[f.id] && f.state == StateInactive {
				act = append(act, f)
			}
		}
		r.activateInOrder(act)

		if len(deact) == 0 && len(act) == 0 {
			return
		}
	}
}

// computeTarget derives the set of fibers that SHOULD be active from the
// declarations alone (mechanic 3: the target view). It is a least fixpoint built
// upward — a fiber joins only once all its injected keys are covered by providers
// already in the set, and its parent is in the set. Building upward is what makes
// the runtime deadlock-free through cycles: a pure cycle A↔B can never be entered
// because neither member can join before the other, so both are simply left out
// (and separately reported by detectCycles). Called with r.mu held.
func (r *Registry) computeTarget() map[string]bool {
	active := make(map[string]bool)
	for {
		changed := false
		for id, f := range r.fibers {
			if active[id] || !r.eligible(f) {
				continue
			}
			if f.parent != nil && !active[f.parent.id] {
				continue
			}
			if r.declSatisfied(f, active) {
				active[id] = true
				changed = true
			}
		}
		if !changed {
			return active
		}
	}
}

// declSatisfied reports whether every key f injects is offered (by declaration)
// by some fiber already in the candidate active set.
func (r *Registry) declSatisfied(f *Fiber, active map[string]bool) bool {
	for key := range f.component.injects {
		found := false
		for id := range active {
			if r.fibers[id].component.provides[key] {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// eligible reports whether a fiber may be part of composition at all: not
// withheld (its Apply failed), not caught in a cycle, and no ancestor removed or
// disabled — the cascade condition that unloads a subtree with its parent.
func (r *Registry) eligible(f *Fiber) bool {
	if f.withheld || r.inCycle[f.id] {
		return false
	}
	for a := f; a != nil; a = a.parent {
		if a.removed || a.disabled {
			return false
		}
	}
	return true
}

// activateInOrder activates the given fibers respecting order: a fiber activates
// only after its parent is active and every injected key is provided by an active
// fiber (mechanic 4: a dependent activates only after its providers). Whatever
// cannot yet activate is left for a later pass or left inactive.
func (r *Registry) activateInOrder(set []*Fiber) {
	pending := make(map[string]*Fiber, len(set))
	for _, f := range set {
		pending[f.id] = f
	}
	for len(pending) > 0 {
		progressed := false
		for id, f := range pending {
			if f.parent != nil && f.parent.state != StateActive {
				continue
			}
			if !r.injectsActive(f) {
				continue
			}
			r.activate(f)
			delete(pending, id)
			progressed = true
		}
		if !progressed {
			return
		}
	}
}

// injectsActive reports whether every key f injects is currently provided by an
// active fiber (the runtime truth, read from live provisions).
func (r *Registry) injectsActive(f *Fiber) bool {
	for key := range f.component.injects {
		if _, ok := r.activeProvider(key); !ok {
			return false
		}
	}
	return true
}

// deactivateInOrder unwinds the given fibers respecting THE WITHDRAWAL GUARD
// (mechanic 4): a provider deactivates only after every dependent that resolved to
// it — and every child nested under it — has deactivated. So a dependent's
// teardown code can still read the very binding being withdrawn, and a parent's
// teardown runs after its children's. This is the guarded-withdrawal ordering the
// source calculus proves terminating; the loop drains a strictly shrinking set,
// with an arbitrary-order fallback only for a mutually-blocked residue (which the
// target computation already prevents).
func (r *Registry) deactivateInOrder(set []*Fiber) {
	pending := make(map[string]*Fiber, len(set))
	for _, f := range set {
		pending[f.id] = f
	}
	for len(pending) > 0 {
		progressed := false
		for id, f := range pending {
			if r.blockedByDependent(f, pending) {
				continue
			}
			r.deactivate(f)
			delete(pending, id)
			progressed = true
		}
		if !progressed {
			for id, f := range pending {
				r.deactivate(f)
				delete(pending, id)
			}
		}
	}
}

// blockedByDependent reports whether some fiber still pending teardown resolved to
// f (committed view names f's identity) or is nested under f. While true, f must
// stay active — its bindings readable — so that dependent's teardown can use them.
func (r *Registry) blockedByDependent(f *Fiber, pending map[string]*Fiber) bool {
	for _, g := range pending {
		if g == f || g.state != StateActive {
			continue
		}
		if g.parent == f {
			return true
		}
		for _, pid := range g.committedView {
			if pid == f.identity() {
				return true
			}
		}
	}
	return false
}

// activate runs a fiber's Apply, accumulating its effects onto a fresh disposer
// stack, and records the committed view. If Apply fails, the fiber recovers
// everything it installed (accumulator recovery via LIFO revert), records the
// error, and is withheld from re-entry until its target changes — sibling fibers
// are untouched (mechanic 6). Called with r.mu held.
func (r *Registry) activate(f *Fiber) {
	f.setState(StateReloading)
	f.disposers = &disposerStack{}
	f.provisions = make(map[string]any)
	f.committedView = make(map[string]string)

	if f.component.Apply != nil {
		if err := f.component.Apply(&Context{reg: r, fiber: f}); err != nil {
			f.disposers.revert()
			f.provisions = make(map[string]any)
			f.committedView = nil
			f.setState(StateInactive)
			f.withheld = true
			f.setErr(err)
			return
		}
	}
	// Record which provider identity each injected key resolved to (mechanic 3).
	for key := range f.component.injects {
		if p, ok := r.activeProvider(key); ok {
			f.committedView[key] = p.identity()
		}
	}
	f.setErr(nil)
	f.setState(StateActive)
}

// deactivate unwinds a fiber: its disposers revert in LIFO order (its provisions
// among them, so its services withdraw), and it returns to inactive. Its bindings
// remain readable up to this call because the registry orders providers last (the
// withdrawal guard). Called with r.mu held.
func (r *Registry) deactivate(f *Fiber) {
	if f.state != StateActive {
		return
	}
	f.setState(StateUnloading)
	if f.disposers != nil {
		f.disposers.revert()
	}
	f.provisions = nil
	f.committedView = nil
	f.setState(StateInactive)
}

// viewStale reports whether an active fiber's committed provider identity for any
// injected key no longer matches the current active provider — a provider was
// replaced under the same key (provider-identity epochs). Such a fiber must
// reload against the new identity, even though the key never disappeared.
func (r *Registry) viewStale(f *Fiber) bool {
	for key, committed := range f.committedView {
		if p, ok := r.activeProvider(key); ok && p.identity() != committed {
			return true
		}
	}
	return false
}

// activeProvider returns the first ACTIVE fiber providing key — the resolution
// used for activation decisions and committed views. Iteration order over the map
// is unspecified; with a single provider per key (the common case) the result is
// deterministic.
func (r *Registry) activeProvider(key string) (*Fiber, bool) {
	for _, f := range r.fibers {
		if f.state == StateActive {
			if _, ok := f.provisions[key]; ok {
				return f, true
			}
		}
	}
	return nil, false
}

// readable returns a binding for key from any fiber that is active OR unloading —
// the read path a Context.Get uses. Including unloading fibers is the withdrawal
// guard in the read direction: a provider's binding stays legible to a dependent
// tearing down against it.
func (r *Registry) readable(key string) (any, bool) {
	for _, f := range r.fibers {
		if f.state == StateActive || f.state == StateUnloading {
			if v, ok := f.provisions[key]; ok {
				return v, true
			}
		}
	}
	return nil, false
}

// detectCycles finds dependency cycles from the declarations alone and records
// them (mechanic 10). A cycle's members are marked so eligible() keeps them
// inactive — reported at load, never deadlocked. Called with r.mu held.
func (r *Registry) detectCycles() {
	nodes := make([]string, 0, len(r.fibers))
	for id, f := range r.fibers {
		if !f.removed {
			nodes = append(nodes, id)
		}
	}
	sort.Strings(nodes)

	// Edge f → g when f injects a key g provides (f depends on g).
	adj := make(map[string][]string, len(nodes))
	for _, id := range nodes {
		f := r.fibers[id]
		seen := make(map[string]bool)
		for key := range f.component.injects {
			for _, gid := range nodes {
				if !seen[gid] && r.fibers[gid].component.provides[key] {
					adj[id] = append(adj[id], gid)
					seen[gid] = true
				}
			}
		}
	}

	r.inCycle = make(map[string]bool)
	r.cycles = nil
	for _, scc := range tarjanSCC(nodes, adj) {
		cyclic := len(scc) > 1
		if !cyclic { // singleton is a cycle only via a self-edge
			for _, w := range adj[scc[0]] {
				if w == scc[0] {
					cyclic = true
					break
				}
			}
		}
		if cyclic {
			for _, n := range scc {
				r.inCycle[n] = true
			}
			r.cycles = append(r.cycles, scc)
		}
	}
}

// tarjanSCC returns the strongly connected components of the graph, each sorted;
// components of size > 1 (or a singleton with a self-edge) are dependency cycles.
func tarjanSCC(nodes []string, adj map[string][]string) [][]string {
	var (
		index    int
		idx      = make(map[string]int)
		low      = make(map[string]int)
		onStack  = make(map[string]bool)
		stack    []string
		out      [][]string
		strongcc func(v string)
	)
	strongcc = func(v string) {
		idx[v] = index
		low[v] = index
		index++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range adj[v] {
			if _, seen := idx[w]; !seen {
				strongcc(w)
				if low[w] < low[v] {
					low[v] = low[w]
				}
			} else if onStack[w] && idx[w] < low[v] {
				low[v] = idx[w]
			}
		}
		if low[v] == idx[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			sort.Strings(comp)
			out = append(out, comp)
		}
	}
	for _, v := range nodes {
		if _, seen := idx[v]; !seen {
			strongcc(v)
		}
	}
	return out
}

// unlink detaches a fiber from its parent's child list.
func (r *Registry) unlink(f *Fiber) {
	if f.parent == nil {
		return
	}
	siblings := f.parent.children
	for i, c := range siblings {
		if c == f {
			f.parent.children = append(siblings[:i], siblings[i+1:]...)
			break
		}
	}
}

// gc sweeps removed fibers once they are quiescent (inactive), unlinking them from
// the tree and dropping them from the registry — so their names are free to be
// re-Loaded fresh. Called with r.mu held, after reconcile.
func (r *Registry) gc() {
	for id, f := range r.fibers {
		if f.removed && f.state == StateInactive {
			r.unlink(f)
			delete(r.fibers, id)
		}
	}
}
