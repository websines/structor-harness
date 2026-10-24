// Package tools is the Forge harness tool spine (forge-harness-design-v0.1.md §4):
// the tool registry plus the execution pipeline. A registered tool is a struct
// with a hand-written JSON Schema and a typed handler (hx.ToolDefinition) — the
// dsh inference DSL evaporates in Go.
//
// The one deliberate divergence from dsh (§3): there is NO generic event bus.
// Extension rides an EXPLICIT, typed middleware chain — a []hx.ToolMiddleware
// each wrapping the next around dispatch (timeout / guard / metrics). See
// pipeline.go for how Execute folds that chain over the base dispatch fn.
//
// Registrations are effects (§3): Register and Use each return a disposer that
// reverts the registration, so the whole tool surface unwinds LIFO on unload.
// The Runtime is safe for concurrent use.
package tools

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/websines/structor-harness/harness/hx"
)

// reservedRunCode is a tool name the registry refuses. The embedded code runtime
// owns this name; a plugin must not shadow it.
const reservedRunCode = "run_code"

// Defaults applied when a Config field is zero.
const (
	defaultTimeout     = 30 * time.Second
	defaultMaxParallel = 8
)

// Config tunes a Runtime. Zero values fall back to the package defaults.
type Config struct {
	// DefaultTimeout bounds one execution when a tool sets no TimeoutMs.
	DefaultTimeout time.Duration
	// MaxParallel caps concurrent goroutines within one ExecuteBatch group.
	MaxParallel int
}

// toolReg is one registered tool plus a monotonic id, so a disposer only removes
// the registration it created (a name re-registered after dispose is a new id).
type toolReg struct {
	def hx.ToolDefinition
	id  uint64
}

// mwReg is one registered middleware plus its id, kept in registration order.
type mwReg struct {
	mw hx.ToolMiddleware
	id uint64
}

// Runtime implements hx.ToolRuntime: the tool registry and the execution
// pipeline. It is the ctx.Get(KeyTools) service. Concurrency-safe.
type Runtime struct {
	mu         sync.RWMutex
	tools      map[string]*toolReg
	middleware []*mwReg
	seq        uint64 // mints tool and middleware ids

	defaultTimeout time.Duration
	maxParallel    int
}

// Compile-time proof the Runtime satisfies the shared contract.
var _ hx.ToolRuntime = (*Runtime)(nil)

// New builds a Runtime with cfg's tunables (zero fields take the defaults).
func New(cfg Config) *Runtime {
	dt := cfg.DefaultTimeout
	if dt <= 0 {
		dt = defaultTimeout
	}
	mp := cfg.MaxParallel
	if mp < 1 {
		mp = defaultMaxParallel
	}
	return &Runtime{
		tools:          make(map[string]*toolReg),
		defaultTimeout: dt,
		maxParallel:    mp,
	}
}

// Register adds a tool keyed by def.Schema.Name and returns its disposer.
// A registration is an effect: the disposer unregisters exactly this tool.
// Duplicate names and the reserved run_code name are rejected, as are a tool
// with no name or no Execute body.
func (r *Runtime) Register(def hx.ToolDefinition) (dispose func(), err error) {
	name := def.Schema.Name
	if name == "" {
		return nil, fmt.Errorf("tools: tool name is empty")
	}
	if name == reservedRunCode {
		return nil, fmt.Errorf("tools: %q is a reserved tool name", name)
	}
	if def.Execute == nil {
		return nil, fmt.Errorf("tools: tool %q has no Execute body", name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[name]; dup {
		return nil, fmt.Errorf("tools: tool %q already registered", name)
	}
	r.seq++
	reg := &toolReg{def: def, id: r.seq}
	r.tools[name] = reg

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if cur, ok := r.tools[name]; ok && cur.id == reg.id {
			delete(r.tools, name)
		}
	}, nil
}

// Schemas returns the model-facing schema of every registered tool, sorted by
// name so the tool list a request carries is deterministic.
func (r *Runtime) Schemas() []hx.ToolSchema {
	r.mu.RLock()
	out := make([]hx.ToolSchema, 0, len(r.tools))
	for _, reg := range r.tools {
		out = append(out, reg.def.Schema)
	}
	r.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Concurrent reports whether a named tool may join a parallel group. An unknown
// tool is not concurrent.
func (r *Runtime) Concurrent(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reg, ok := r.tools[name]
	return ok && reg.def.Concurrent
}

// lookup resolves a tool definition by name under the read lock.
func (r *Runtime) lookup(name string) (hx.ToolDefinition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reg, ok := r.tools[name]
	if !ok {
		return hx.ToolDefinition{}, false
	}
	return reg.def, true
}
