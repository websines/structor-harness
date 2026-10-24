// Package agent is the harness's agent registry: the factory slot the loop
// installs itself into, the live-agent set, and the process-local "current
// initiator" that tool bodies read (via RequireInitiator) to actor-stamp their
// facts. It also carries the Inbox that the loop's ReactLoopAgent embeds.
//
// Registry implements hx.AgentRegistry. Consumers depend on this registry (by
// key hx.KeyAgents), never on the loop's concrete driver: the loop registers a
// factory here and everyone else resolves agents through it, so the spine stays
// decoupled at compile time.
package agent

import (
	"errors"
	"sync"

	"github.com/websines/structor-harness/harness/hx"
)

// Registry implements hx.AgentRegistry.
type Registry struct {
	mu sync.Mutex

	factory   hx.AgentFactory
	factoryID int64 // identity of the current factory registration (for its disposer)
	nextFacID int64

	// initiator is the process-local current initiator. WithInitiator saves the
	// prior value, sets the new one, and restores it when fn returns, so nesting
	// on one goroutine is exact: the Go call stack IS the initiator stack, and the
	// restore is deferred so it holds even if fn panics.
	//
	// It is deliberately a single mutex-guarded field, not a goroutine-id keyed
	// map (which is fragile — ids are reused and are unreachable across a spawned
	// goroutine). H1 drives a single agent, so two distinct initiators never run
	// concurrently; the save/restore keeps nested calls correct. If concurrent
	// independent initiators are ever required, this one field is the single place
	// to swap in a per-chain carrier.
	initiator    hx.Agent
	hasInitiator bool

	live map[*trackedHandle]struct{} // live agents, keyed by their tracking handle
}

var _ hx.AgentRegistry = (*Registry)(nil)

// New returns an empty Registry with no factory set.
func New() *Registry {
	return &Registry{live: make(map[*trackedHandle]struct{})}
}

// SetFactory installs f as the agent factory and returns a disposer that
// restores the previously installed factory (LIFO effect discipline). The
// disposer is a no-op if a later SetFactory has since replaced f, and is safe to
// call more than once.
func (r *Registry) SetFactory(f hx.AgentFactory) (func(), error) {
	if f == nil {
		return nil, errors.New("agent: SetFactory: nil factory")
	}
	r.mu.Lock()
	prevFac := r.factory
	prevID := r.factoryID
	id := r.nextFacID
	r.nextFacID++
	r.factory = f
	r.factoryID = id
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			if r.factoryID == id { // revert only if still ours (identity, not value equality)
				r.factory = prevFac
				r.factoryID = prevID
			}
			r.mu.Unlock()
		})
	}, nil
}

// Create delegates to the installed factory and tracks the resulting agent as
// live until its handle is disposed. It errors if no factory is set.
func (r *Registry) Create(cfg hx.AgentConfig) (hx.AgentHandle, error) {
	r.mu.Lock()
	f := r.factory
	r.mu.Unlock()
	if f == nil {
		return nil, errors.New("agent: Create: no factory set (the loop must SetFactory first)")
	}
	h, err := f.Create(cfg)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, errors.New("agent: Create: factory returned a nil handle")
	}
	th := &trackedHandle{inner: h, reg: r}
	r.mu.Lock()
	r.live[th] = struct{}{}
	r.mu.Unlock()
	return th, nil
}

// WithInitiator runs fn with a as the process-local current initiator, restoring
// the prior initiator when fn returns (or panics). Nested calls compose: an inner
// WithInitiator sees its own agent, and the outer's is restored on the inner's
// return.
func (r *Registry) WithInitiator(a hx.Agent, fn func() error) error {
	if fn == nil {
		return errors.New("agent: WithInitiator: nil fn")
	}
	r.mu.Lock()
	prev := r.initiator
	hadPrev := r.hasInitiator
	r.initiator = a
	r.hasInitiator = a != nil
	r.mu.Unlock()

	// Restore on the way out even if fn panics, so a broken chain can never leak
	// its initiator into the next one.
	defer func() {
		r.mu.Lock()
		r.initiator = prev
		r.hasInitiator = hadPrev
		r.mu.Unlock()
	}()
	return fn()
}

// CurrentInitiator returns the initiator of the current chain, if any.
func (r *Registry) CurrentInitiator() (hx.Agent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.initiator, r.hasInitiator
}

// Live returns a snapshot of the agents currently tracked as live (order
// unspecified). It is a registry-tracking convenience, not part of hx.
func (r *Registry) Live() []hx.Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]hx.Agent, 0, len(r.live))
	for th := range r.live {
		if a := th.inner.Agent(); a != nil {
			out = append(out, a)
		}
	}
	return out
}

func (r *Registry) untrack(th *trackedHandle) {
	r.mu.Lock()
	delete(r.live, th)
	r.mu.Unlock()
}

// trackedHandle wraps a factory handle so Dispose untracks the agent from the
// registry's live set exactly once before delegating to the real disposer.
type trackedHandle struct {
	inner hx.AgentHandle
	reg   *Registry
	once  sync.Once
}

var _ hx.AgentHandle = (*trackedHandle)(nil)

// Agent returns the wrapped agent.
func (h *trackedHandle) Agent() hx.Agent { return h.inner.Agent() }

// Dispose untracks the agent and disposes the underlying handle, at most once.
func (h *trackedHandle) Dispose() {
	h.once.Do(func() {
		h.reg.untrack(h)
		h.inner.Dispose()
	})
}
