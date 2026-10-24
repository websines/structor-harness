package runtime

import (
	"errors"
	"testing"
)

// Unloading runs a fiber's disposers in LIFO order and restores the state its
// effects mutated (mechanic 1 / accumulator recovery). "State" here is a counter
// and a teardown log the effects mutate and the disposers must restore.
func TestUnload_LIFORevertRestoresState(t *testing.T) {
	r := New()
	counter := 0
	var order []string

	c := &Component{
		Name: "collector",
		Apply: func(ctx *Context) error {
			// Effect A installs first, so its inverse must run LAST.
			if err := ctx.Use(func() (func(), error) {
				counter++
				return func() { counter--; order = append(order, "undoA") }, nil
			}); err != nil {
				return err
			}
			// Effect B installs second, so its inverse must run FIRST.
			return ctx.Use(func() (func(), error) {
				counter += 10
				return func() { counter -= 10; order = append(order, "undoB") }, nil
			})
		},
	}
	f := r.Load(c, "")
	if f.State() != StateActive || counter != 11 {
		t.Fatalf("expected active with counter 11, got %s counter=%d", f.State(), counter)
	}

	r.Remove("collector")
	if counter != 0 {
		t.Fatalf("state not restored on unload: counter=%d", counter)
	}
	if len(order) != 2 || order[0] != "undoB" || order[1] != "undoA" {
		t.Fatalf("disposers did not run LIFO: %v", order)
	}
}

// A disposer stack reverts exactly once — a second revert is a no-op, so a fiber
// can never double-free (the idempotence half of accumulator recovery).
func TestDisposerStack_DoubleDisposeIsNoop(t *testing.T) {
	n := 0
	s := &disposerStack{}
	_ = s.push(func() (func(), error) { n++; return func() { n-- }, nil })
	s.revert()
	s.revert() // must not run the disposer again
	if n != 0 {
		t.Fatalf("double dispose was not idempotent: n=%d", n)
	}
}

// A failed activation recovers everything it installed (partial rollback at the
// failing boundary), records its error, and is withheld — siblings untouched
// (mechanic 6). This is the runtime mirror of "contested never governs".
func TestActivate_FailureRevertsAndWithholds(t *testing.T) {
	r := New()
	counter := 0
	boom := errors.New("apply failed")

	c := &Component{
		Name: "bad",
		Apply: func(ctx *Context) error {
			_ = ctx.Use(func() (func(), error) { counter++; return func() { counter-- }, nil })
			_ = ctx.Use(func() (func(), error) { counter++; return func() { counter-- }, nil })
			return boom // both effects must be rolled back
		},
	}
	r.Load(c, "")

	f := r.Fiber("bad")
	if f.State() != StateInactive {
		t.Fatalf("failed fiber should be inactive, got %s", f.State())
	}
	if counter != 0 {
		t.Fatalf("partial install not rolled back: counter=%d", counter)
	}
	if !errors.Is(f.Err(), boom) {
		t.Fatalf("error not recorded on fiber: %v", f.Err())
	}
}

// Removing a provider deactivates its dependents (reactive coeffects), and the
// provider's binding stays READABLE while a dependent tears down (the withdrawal
// guard, mechanic 4). Re-adding the provider reactivates the dependent —
// auto-reopen's runtime twin.
func TestProviderWithdrawal_DeactivatesReadableGuardAndReopen(t *testing.T) {
	r := New()
	consumerLive := false
	var readDuringTeardown any

	provider := &Component{
		Name:    "P",
		Provide: []string{"db"},
		Apply:   func(ctx *Context) error { return ctx.Provide("db", "conn") },
	}
	consumer := &Component{
		Name:   "C",
		Inject: []string{"db"},
		Apply: func(ctx *Context) error {
			if _, err := ctx.Get("db"); err != nil {
				return err
			}
			return ctx.Use(func() (func(), error) {
				consumerLive = true
				// Teardown reads the very key being withdrawn — the guard must
				// keep the provider readable until this dependent is down.
				return func() {
					consumerLive = false
					if v, err := ctx.Get("db"); err == nil {
						readDuringTeardown = v
					}
				}, nil
			})
		},
	}

	r.Load(provider, "")
	r.Load(consumer, "")
	if !consumerLive {
		t.Fatal("consumer did not activate once its dep was provided")
	}
	if v, ok := r.Lookup("db"); !ok || v != "conn" {
		t.Fatalf("derived service table missing db: %v %v", v, ok)
	}

	r.Remove("P")
	if consumerLive {
		t.Fatal("consumer stayed active after its provider was withdrawn")
	}
	if readDuringTeardown != "conn" {
		t.Fatalf("withdrawal guard broken: teardown read %v, want conn", readDuringTeardown)
	}
	if _, ok := r.Lookup("db"); ok {
		t.Fatal("db still resolvable after provider fully unloaded")
	}

	// Re-adding the provider reopens the dependent.
	r.Load(provider, "")
	if !consumerLive {
		t.Fatal("consumer did not reactivate after provider re-added")
	}
}

// A component that reaches for a key it did not declare is refused at the point of
// use — confinement (mechanic 9), for both reads and writes.
func TestConfinement_UndeclaredAccessRefused(t *testing.T) {
	r := New()

	readBad := &Component{
		Name: "peeker", // declares no inject, yet reaches for "secret"
		Apply: func(ctx *Context) error {
			_, err := ctx.Get("secret") // never declared in inject
			return err
		},
	}
	r.Load(readBad, "")
	if got := r.Fiber("peeker").Err(); !errors.Is(got, ErrConfinement) {
		t.Fatalf("undeclared read not refused: %v", got)
	}

	writeBad := &Component{
		Name:    "leaker",
		Provide: []string{"allowed"},
		Apply: func(ctx *Context) error {
			return ctx.Provide("smuggled", 1) // never declared in provide
		},
	}
	r.Load(writeBad, "")
	if got := r.Fiber("leaker").Err(); !errors.Is(got, ErrConfinement) {
		t.Fatalf("undeclared write not refused: %v", got)
	}
}

// A dependency cycle is predicted from declarations and reported — the involved
// components stay inactive rather than deadlocking (mechanic 10). The test would
// hang if reconcile deadlocked, so its mere return is half the assertion.
func TestCycle_ReportedNotDeadlocked(t *testing.T) {
	r := New()
	a := &Component{Name: "A", Inject: []string{"y"}, Provide: []string{"x"},
		Apply: func(ctx *Context) error { return ctx.Provide("x", 1) }}
	b := &Component{Name: "B", Inject: []string{"x"}, Provide: []string{"y"},
		Apply: func(ctx *Context) error { return ctx.Provide("y", 1) }}

	r.Load(a, "")
	r.Load(b, "") // closes the cycle A→B→A

	if r.Fiber("A").State() != StateInactive || r.Fiber("B").State() != StateInactive {
		t.Fatal("cycle members activated — should stay inactive")
	}
	cycles := r.Cycles()
	if len(cycles) != 1 || len(cycles[0]) != 2 {
		t.Fatalf("cycle not reported as one 2-node cycle: %v", cycles)
	}
	if cycles[0][0] != "A" || cycles[0][1] != "B" {
		t.Fatalf("cycle membership wrong: %v", cycles[0])
	}
}

// Unloading a parent cascades to its children, and children unwind before the
// parent (mechanic 1 + the guarded ordering).
func TestParentUnload_CascadesToChildren(t *testing.T) {
	r := New()
	var order []string

	parent := &Component{
		Name: "parent",
		Apply: func(ctx *Context) error {
			return ctx.Use(func() (func(), error) {
				return func() { order = append(order, "parent") }, nil
			})
		},
	}
	child := &Component{
		Name: "child",
		Apply: func(ctx *Context) error {
			return ctx.Use(func() (func(), error) {
				return func() { order = append(order, "child") }, nil
			})
		},
	}
	r.Load(parent, "")
	r.Load(child, "parent")
	if r.Fiber("parent").State() != StateActive || r.Fiber("child").State() != StateActive {
		t.Fatal("parent/child did not both activate")
	}

	r.Remove("parent") // must cascade to child

	if r.Fiber("parent") != nil || r.Fiber("child") != nil {
		t.Fatal("removed subtree not swept")
	}
	if len(order) != 2 || order[0] != "child" || order[1] != "parent" {
		t.Fatalf("cascade did not unwind children before parent: %v", order)
	}
}

// A dependent that resolved to a provider reloads when that provider is REPLACED
// under the same key by a new identity (provider-identity epochs).
func TestProviderReplacement_ReloadsDependent(t *testing.T) {
	r := New()
	seen := []any{}

	provA := &Component{Name: "P", Provide: []string{"cfg"},
		Apply: func(ctx *Context) error { return ctx.Provide("cfg", "v1") }}
	consumer := &Component{Name: "C", Inject: []string{"cfg"},
		Apply: func(ctx *Context) error {
			v, err := ctx.Get("cfg")
			if err != nil {
				return err
			}
			seen = append(seen, v)
			return nil
		}}

	r.Load(provA, "")
	r.Load(consumer, "")

	// Replace the provider (same name, new epoch → new identity) with a new value.
	provB := &Component{Name: "P", Provide: []string{"cfg"},
		Apply: func(ctx *Context) error { return ctx.Provide("cfg", "v2") }}
	r.Load(provB, "")

	if len(seen) != 2 || seen[0] != "v1" || seen[1] != "v2" {
		t.Fatalf("dependent did not reload against replaced provider: %v", seen)
	}
}
