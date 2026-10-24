package mcp

import (
	"context"
	"fmt"
	"sync"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/runtime"
)

// Component wires one MCP server onto the cordis runtime. It injects the tool
// runtime (hx.KeyTools), news a Client over t, runs the initialize handshake,
// lists the server's tools, and registers each on the resolved ToolRuntime — all
// as a single revertible effect. The disposer stops change handling, unregisters
// every tool this server owns, and closes the client, so unloading the component
// withdraws the whole surface in LIFO order.
//
// A notifications/tools/list_changed re-list swaps the registered generation in
// place (dispose the previous set, register the next). If the initialize
// handshake or the initial listing fails, the effect returns the error and the
// component does not activate (its fiber is withheld).
func Component(server string, t Transport) *runtime.Component {
	return &runtime.Component{
		Name:   "harness/mcp/" + server,
		Inject: []string{hx.KeyTools},
		Apply: func(ctx *runtime.Context) error {
			tr, err := toolRuntime(ctx)
			if err != nil {
				return err
			}
			client := NewClient(server, t)
			reg := &registrar{tr: tr}

			// One effect owns the client lifecycle and the tool generation: its
			// install connects + registers, its disposer tears everything down.
			return ctx.Use(func() (func(), error) {
				bg := context.Background()
				if err := client.Initialize(bg); err != nil {
					_ = client.Close()
					return nil, err
				}
				defs, err := client.ListTools(bg)
				if err != nil {
					_ = client.Close()
					return nil, fmt.Errorf("mcp: list tools %q: %w", server, err)
				}
				if err := reg.replace(defs); err != nil {
					_ = client.Close()
					return nil, fmt.Errorf("mcp: register tools %q: %w", server, err)
				}
				// Later generations (list_changed) replace the registered set.
				client.OnChange(func(next []hx.ToolDefinition) {
					_ = reg.replace(next)
				})
				return func() {
					client.OnChange(nil)
					reg.clear()
					_ = client.Close()
				}, nil
			})
		},
	}
}

// toolRuntime resolves hx.KeyTools from the confined context and asserts it to
// the ToolRuntime interface (the by-key resolution the spine uses instead of
// importing a concrete sibling).
func toolRuntime(ctx *runtime.Context) (hx.ToolRuntime, error) {
	v, err := ctx.Get(hx.KeyTools)
	if err != nil {
		return nil, err
	}
	tr, ok := v.(hx.ToolRuntime)
	if !ok {
		return nil, fmt.Errorf("mcp: service %q has unexpected type %T", hx.KeyTools, v)
	}
	return tr, nil
}

// registrar owns the live tool registrations for one server across generations.
// replace atomically swaps the current set for a new one; clear withdraws it for
// good. All three methods are safe for concurrent use — replace runs on the
// re-list goroutine while clear runs on the teardown goroutine.
type registrar struct {
	tr        hx.ToolRuntime
	mu        sync.Mutex
	disposers []func()
	closed    bool
}

// replace disposes the current generation and registers defs as the next. A
// registration failure rolls back the partial set (zero tools from this
// generation) and returns the error. After clear, replace is a no-op — a late
// re-list callback cannot resurrect tools past teardown.
func (r *registrar) replace(defs []hx.ToolDefinition) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.disposeLocked()
	for _, def := range defs {
		d, err := r.tr.Register(def)
		if err != nil {
			r.disposeLocked()
			return fmt.Errorf("register %q: %w", def.Schema.Name, err)
		}
		if d != nil {
			r.disposers = append(r.disposers, d)
		}
	}
	return nil
}

// clear withdraws every registration and latches closed so no later generation
// registers.
func (r *registrar) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.disposeLocked()
}

// disposeLocked runs the current disposers in LIFO order and empties the set. The
// caller holds r.mu.
func (r *registrar) disposeLocked() {
	for i := len(r.disposers) - 1; i >= 0; i-- {
		r.disposers[i]()
	}
	r.disposers = nil
}
