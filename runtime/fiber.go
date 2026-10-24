package runtime

// A fiber is an instantiation of a component (system design §4.15, mechanic 1):
// a live thing with a lifecycle, a place in a tree, its own effect table, and the
// two views whose disagreement drives every transition (mechanic 3). The registry
// owns the transitions; a fiber is the state they move through.

import (
	"fmt"
	"sync"
)

// State is a fiber's lifecycle position. The full cycle is
// inactive → reloading → active → unloading → inactive: a fiber reloads (runs its
// Apply, accumulating effects) into active, and unloading (running its disposers
// in reverse) back to inactive.
type State int

const (
	// StateInactive: loaded but not running — no effects installed, providing
	// nothing. The resting state, and where a withheld or cycle-blocked fiber sits.
	StateInactive State = iota
	// StateReloading: mid-activation — Apply is running and effects are
	// accumulating. Transient; a fiber is here only inside activate.
	StateReloading
	// StateActive: fully installed — its provisions count toward the derived
	// service table and its committed view is recorded.
	StateActive
	// StateUnloading: mid-teardown — disposers are reverting in LIFO order. Its
	// bindings stay readable here so a dependent can still read it (mechanic 4).
	StateUnloading
)

// String renders the lifecycle state for journalling and diagnostics.
func (s State) String() string {
	switch s {
	case StateInactive:
		return "inactive"
	case StateReloading:
		return "reloading"
	case StateActive:
		return "active"
	case StateUnloading:
		return "unloading"
	default:
		return "unknown"
	}
}

// Fiber is one instantiation of a component. Its lifecycle is never set by hand —
// the registry moves it as the target view (what it should be running against)
// disagrees with the committed view (what it activated against). Fibers form a
// tree via parent/children: unloading a parent cascades to its children.
type Fiber struct {
	id        string
	epoch     uint64 // bumped on each (re)load, so a replaced provider is a NEW identity
	component *Component

	parent   *Fiber
	children []*Fiber

	statusMu   sync.RWMutex
	state      State
	disposers  *disposerStack // this fiber's private effect table (fresh per activation)
	provisions map[string]any // keys this fiber currently provides → bindings

	// committedView records, per injected key, the provider IDENTITY this fiber
	// resolved to when it activated (mechanic 3: identities, not values). The
	// registry compares it against the current resolution to decide reloads, and
	// uses it to order teardown so a provider outlives the dependents that resolved
	// to it (mechanic 4).
	committedView map[string]string

	removed  bool  // retired by the orchestrator; ineligible, awaiting teardown + gc
	disabled bool  // present but held out of composition by configuration
	withheld bool  // its Apply failed; held out of re-entry until its target changes
	lastErr  error // the error its last activation raised, if any
}

// identity is the fiber's epoch-stamped name. Replacing a provider (a fresh load
// under the same name) yields a new epoch and thus a new identity, so a dependent
// whose committed view names the old identity knows to reload — provider-identity
// epochs (§19.7: "replacing a provider restarts consumers even when the key never
// disappears").
func (f *Fiber) identity() string { return fmt.Sprintf("%s#%d", f.id, f.epoch) }

// State reports the fiber's current lifecycle position (snapshot).
func (f *Fiber) State() State { f.statusMu.RLock(); defer f.statusMu.RUnlock(); return f.state }

// Err reports the error the fiber's last activation raised, or nil. A non-nil Err
// on an inactive fiber is a withheld component: reverted fully, error recorded,
// held out of re-entry until its target changes (mechanic 6 — the runtime mirror
// of unverifiable: visible, non-governing, never silently retried).
func (f *Fiber) Err() error { f.statusMu.RLock(); defer f.statusMu.RUnlock(); return f.lastErr }

func (f *Fiber) setState(state State) {
	f.statusMu.Lock()
	defer f.statusMu.Unlock()
	f.state = state
}

func (f *Fiber) setErr(err error) {
	f.statusMu.Lock()
	defer f.statusMu.Unlock()
	f.lastErr = err
}

// Name reports the fiber's stable identity within its registry.
func (f *Fiber) Name() string { return f.id }

// subtree returns f and all its descendants, parents before children.
func (f *Fiber) subtree() []*Fiber {
	out := []*Fiber{f}
	for _, c := range f.children {
		out = append(out, c.subtree()...)
	}
	return out
}
