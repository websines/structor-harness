package llm

import (
	"context"
	"sync"

	"github.com/websines/structor-harness/harness/hx"
)

// Runtime is the KeyLLM service: an adapter registry plus the call seam. Adapters
// register under their Route() (a provider id); Stream routes an opts to the
// adapter whose Route == opts.Provider. It is concurrency-safe — every registry
// read and mutation is guarded by a single mutex, so registration and Stream may
// race freely from many fibers.
type Runtime struct {
	mu       sync.Mutex
	adapters map[string]hx.LlmAdapter
}

var _ hx.LlmRuntime = (*Runtime)(nil)

// NewRuntime returns an empty Runtime ready for RegisterAdapter.
func NewRuntime() *Runtime {
	return &Runtime{adapters: make(map[string]hx.LlmAdapter)}
}

// RegisterAdapter binds a under its Route() and returns a disposer that removes
// exactly that registration. A route names ONE backend: registering a route that
// is already taken is an error, never a silent clobber. The disposer is
// idempotent and removes only the adapter it registered — a re-registration
// under the same route is not torn out by a stale disposer.
func (r *Runtime) RegisterAdapter(a hx.LlmAdapter) (func(), error) {
	if a == nil {
		return nil, &hx.LlmFailure{Kind: "provider", Message: "llm: nil adapter"}
	}
	route := a.Route()
	if route == "" {
		return nil, &hx.LlmFailure{Kind: "provider", Message: "llm: adapter has empty route"}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, taken := r.adapters[route]; taken {
		return nil, &hx.LlmFailure{Kind: "provider", Message: "llm: route already registered: " + route}
	}
	r.adapters[route] = a

	var once sync.Once
	dispose := func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.adapters[route] == a {
				delete(r.adapters, route)
			}
		})
	}
	return dispose, nil
}

// Stream routes opts to the adapter registered for opts.Provider and returns its
// chunk channel. No adapter for the provider is a typed error — never a silent
// empty stream the loop would mistake for a finished turn.
func (r *Runtime) Stream(ctx context.Context, opts hx.GenerateOptions) (<-chan hx.StreamChunk, error) {
	r.mu.Lock()
	a, ok := r.adapters[opts.Provider]
	r.mu.Unlock()
	if !ok {
		return nil, &hx.LlmFailure{Kind: "provider", Message: "llm: no adapter for provider " + opts.Provider}
	}
	return a.Stream(ctx, opts)
}
