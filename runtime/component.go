package runtime

// A component is the unit of composition (system design §4.15, mechanic 1):
// (inject, provide, apply) — the service keys it reads, the keys it provides,
// and an effect function whose every registration yields a tracked inverse. The
// full dependency set is declared BEFORE the component runs, so capabilities are
// reviewable at load time rather than discovered as accesses happen (mechanic 9:
// inject = reviewable capability set — the BYO-agent trust review as a mechanism).

import (
	"errors"
	"fmt"
)

// ErrConfinement is returned when a component reaches for a key it did not
// declare — a read of an un-injected key or a write of an un-provided key. It is
// the runtime face of "evidence declares its scope": a component touches only the
// keys it declared, and an undeclared access fails at the point of use, not
// silently (mechanic 9). This is a language-level check, not a sandbox — untrusted
// code still runs behind the §4.6 boundary.
var ErrConfinement = errors.New("runtime: confinement violation")

// Component is a pluggable part: what it needs, what it offers, and how it
// installs itself. Apply may be nil (a component that only scopes children — a
// pure fiber-tree node). Inject and provide are declarations; the runtime derives
// everything else from them.
type Component struct {
	Name    string               // stable identity within a registry
	Inject  []string             // service keys this component reads
	Provide []string             // service keys this component offers
	Apply   func(*Context) error // installs the component's effects; nil = pure scope

	injects  map[string]bool
	provides map[string]bool
}

// normalize builds the declaration lookup sets once, so hot-path membership tests
// are O(1). Called by the registry when a component is loaded.
func (c *Component) normalize() {
	c.injects = make(map[string]bool, len(c.Inject))
	for _, k := range c.Inject {
		c.injects[k] = true
	}
	c.provides = make(map[string]bool, len(c.Provide))
	for _, k := range c.Provide {
		c.provides[k] = true
	}
}

// Context is a component's confined capability handle during Apply — the only way
// it reaches the outside. Every method enforces the component's declaration: it
// reads only injected keys, writes only provided keys, and registers effects onto
// its own fiber's disposer stack. The handle is captured freely by closures, so
// teardown code (a disposer) can still read through it — the withdrawal guard
// keeps a provider readable while its dependents unwind (mechanic 4).
type Context struct {
	reg   *Registry
	fiber *Fiber
}

// Get reads an injected service. It refuses any key the component did not declare
// in inject (ErrConfinement) — confinement checked at the point of use. A declared
// key is resolved from the DERIVED service table (the union over active fibers'
// provisions, never a stored global); during teardown it still resolves the
// provider being withdrawn, because that provider's bindings stay readable until
// this fiber has finished deactivating (mechanic 4, the withdrawal guard).
func (c *Context) Get(key string) (any, error) {
	if !c.fiber.component.injects[key] {
		return nil, fmt.Errorf("%w: %q read a key not declared in inject", ErrConfinement, key)
	}
	if v, ok := c.reg.readable(key); ok {
		return v, nil
	}
	return nil, fmt.Errorf("runtime: injected service %q is not currently provided", key)
}

// Provide publishes a binding for a key the component declared in provide. It is
// refused (ErrConfinement) for any undeclared key — "writes only the keys it
// declares to provide." The publication is itself a revertible effect: it records
// the binding in the fiber's provisions and yields the disposer that removes it,
// so unloading the fiber withdraws the service through the same LIFO teardown as
// any other effect (mechanic 1 + mechanic 2).
func (c *Context) Provide(key string, value any) error {
	if !c.fiber.component.provides[key] {
		return fmt.Errorf("%w: %q wrote a key not declared in provide", ErrConfinement, key)
	}
	return c.Use(func() (func(), error) {
		c.fiber.provisions[key] = value
		return func() { delete(c.fiber.provisions, key) }, nil
	})
}

// Use registers a revertible effect onto this fiber's effect table. The returned
// error is the effect's own installation failure, surfaced so Apply can decide
// whether to proceed; the runtime reverts whatever succeeded if Apply ultimately
// fails.
func (c *Context) Use(e Effect) error {
	return c.fiber.disposers.push(e)
}
