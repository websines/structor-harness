package app

import (
	"fmt"
	"strings"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/prompt"
	"github.com/websines/structor-harness/harness/tools"
	"github.com/websines/structor-harness/runtime"
)

// mkTool builds a trivial no-op tool with the given name — enough to be
// registerable and to appear in ToolRuntime.Schemas().
func mkTool(name string) hx.ToolDefinition {
	return hx.ToolDefinition{
		Schema: hx.ToolSchema{
			Name:        name,
			Description: "test tool " + name,
			Parameters:  map[string]any{"type": "object"},
		},
		Execute: func(rc hx.ToolRunContext, args []byte) (hx.ToolResult, error) {
			return hx.ToolResult{Value: []byte(fmt.Sprintf("%q ran", name))}, nil
		},
	}
}

// harness builds a registry with a minimal tools component and prompt component
// (mirroring internal/harness/spine.Build's construction of those two), returning
// the concrete ToolRuntime and SystemPrompt so a test can assert against them
// directly. The two provider fibers activate immediately (no injects); an app
// component injects both keys and therefore activates only after both are up.
func harness(t *testing.T) (*runtime.Registry, *tools.Runtime, *prompt.SystemPrompt) {
	t.Helper()
	reg := runtime.New()
	tr := tools.New(tools.Config{})
	sp := prompt.New()

	reg.Load(&runtime.Component{
		Name:    "harness/tools",
		Provide: []string{hx.KeyTools},
		Apply: func(ctx *runtime.Context) error {
			return ctx.Provide(hx.KeyTools, tr)
		},
	}, "")
	reg.Load(&runtime.Component{
		Name:    "harness/prompt",
		Provide: []string{hx.KeyPrompt},
		Apply: func(ctx *runtime.Context) error {
			return ctx.Provide(hx.KeyPrompt, sp)
		},
	}, "")
	return reg, tr, sp
}

// toolNames returns the set of registered tool names.
func toolNames(tr *tools.Runtime) map[string]bool {
	out := map[string]bool{}
	for _, s := range tr.Schemas() {
		out[s.Name] = true
	}
	return out
}

// assemble renders the system prompt or fails the test.
func assemble(t *testing.T, sp *prompt.SystemPrompt) string {
	t.Helper()
	s, err := sp.Assemble(nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	return s
}

// twoAppProfile is the fixture used across the tests: two apps, each one tool +
// one section.
func twoAppProfile() Profile {
	alpha := App{
		Name:     "alpha",
		Tools:    []hx.ToolDefinition{mkTool("alpha_tool")},
		Sections: []Section{{Order: 10, Text: "ALPHA SECTION"}},
	}
	beta := App{
		Name:     "beta",
		Tools:    []hx.ToolDefinition{mkTool("beta_tool")},
		Sections: []Section{{Order: 20, Text: "BETA SECTION"}},
	}
	return Profile{Apps: []App{alpha, beta}}
}

// A profile mounts every app's component; after load both apps' tools are on the
// ToolRuntime and both apps' sections are in the assembled prompt.
func TestProfileLoadMountsAllApps(t *testing.T) {
	reg, tr, sp := harness(t)
	p := twoAppProfile()

	if err := p.Load(reg); err != nil {
		t.Fatalf("Profile.Load: %v", err)
	}

	// Both app fibers must be active (their injects were satisfied).
	for _, a := range p.Apps {
		f := reg.Fiber(a.ComponentName())
		if f == nil {
			t.Fatalf("app %q: no fiber loaded under %q", a.Name, a.ComponentName())
		}
		if f.State() != runtime.StateActive {
			t.Fatalf("app %q: fiber state = %v, want Active (err=%v)", a.Name, f.State(), f.Err())
		}
	}

	names := toolNames(tr)
	if !names["alpha_tool"] || !names["beta_tool"] {
		t.Fatalf("registered tools = %v, want both alpha_tool and beta_tool", names)
	}

	got := assemble(t, sp)
	if !strings.Contains(got, "ALPHA SECTION") || !strings.Contains(got, "BETA SECTION") {
		t.Fatalf("assembled prompt = %q, want both sections present", got)
	}
	// Section order (alpha=10 before beta=20) is honored.
	if strings.Index(got, "ALPHA SECTION") > strings.Index(got, "BETA SECTION") {
		t.Fatalf("assembled prompt out of order: %q", got)
	}
}

// Inventory lists both apps with their tool names and section counts.
func TestInventory(t *testing.T) {
	p := twoAppProfile()

	inv := p.Inventory()
	if len(inv) != 2 {
		t.Fatalf("Inventory len = %d, want 2", len(inv))
	}
	// Profile order is preserved.
	if inv[0].Name != "alpha" || inv[1].Name != "beta" {
		t.Fatalf("Inventory names = [%q %q], want [alpha beta]", inv[0].Name, inv[1].Name)
	}
	for _, row := range inv {
		want := row.Name + "_tool"
		if len(row.Tools) != 1 || row.Tools[0] != want {
			t.Fatalf("app %q tools = %v, want [%q]", row.Name, row.Tools, want)
		}
		if row.Sections != 1 {
			t.Fatalf("app %q sections = %d, want 1", row.Name, row.Sections)
		}
	}

	// The inventory is a read-only projection: mutating a returned row's slice
	// must not disturb a fresh inventory.
	inv[0].Tools[0] = "MUTATED"
	if again := p.Inventory(); again[0].Tools[0] != "alpha_tool" {
		t.Fatalf("Inventory not independent across calls: got %q", again[0].Tools[0])
	}
}

// Revertibility: unloading ONE app's component (via the registry's Remove teardown
// path) withdraws exactly its tool and section; the other app's remain. This is
// the whole point of an app being one revertible component.
func TestUnloadOneAppRevertsOnlyIts(t *testing.T) {
	reg, tr, sp := harness(t)
	p := twoAppProfile()
	if err := p.Load(reg); err != nil {
		t.Fatalf("Profile.Load: %v", err)
	}

	// Precondition: both present.
	if names := toolNames(tr); !names["alpha_tool"] || !names["beta_tool"] {
		t.Fatalf("precondition: both tools should be registered, got %v", names)
	}

	// Unload alpha only, through the runtime's per-component teardown path. Remove
	// retires the fiber and reverts its disposers (LIFO): alpha's tool and section
	// withdraw; beta's are untouched.
	reg.Remove(App{Name: "alpha"}.ComponentName())

	names := toolNames(tr)
	if names["alpha_tool"] {
		t.Fatalf("after unloading alpha, alpha_tool is still registered: %v", names)
	}
	if !names["beta_tool"] {
		t.Fatalf("after unloading alpha, beta_tool was wrongly removed: %v", names)
	}

	got := assemble(t, sp)
	if strings.Contains(got, "ALPHA SECTION") {
		t.Fatalf("after unloading alpha, ALPHA SECTION still in prompt: %q", got)
	}
	if !strings.Contains(got, "BETA SECTION") {
		t.Fatalf("after unloading alpha, BETA SECTION wrongly gone: %q", got)
	}

	// Beta's fiber is still active; alpha's is swept.
	if f := reg.Fiber(App{Name: "beta"}.ComponentName()); f == nil || f.State() != runtime.StateActive {
		t.Fatalf("beta fiber should still be active after unloading alpha")
	}
}

// The App.Component disposer removes exactly the single tool it registered — the
// register-returns-its-inverse contract, exercised at the effect level (no
// registry). A minimal hand-built ToolRuntime + SystemPrompt stand in for the
// harness services.
func TestComponentEffectDisposerRemovesSingleTool(t *testing.T) {
	reg := runtime.New()
	tr := tools.New(tools.Config{})
	sp := prompt.New()
	reg.Load(&runtime.Component{
		Name:    "harness/tools",
		Provide: []string{hx.KeyTools},
		Apply:   func(ctx *runtime.Context) error { return ctx.Provide(hx.KeyTools, tr) },
	}, "")
	reg.Load(&runtime.Component{
		Name:    "harness/prompt",
		Provide: []string{hx.KeyPrompt},
		Apply:   func(ctx *runtime.Context) error { return ctx.Provide(hx.KeyPrompt, sp) },
	}, "")

	solo := App{
		Name:     "solo",
		Tools:    []hx.ToolDefinition{mkTool("solo_tool")},
		Sections: []Section{{Order: 5, Text: "SOLO SECTION"}},
	}
	reg.Load(solo.Component(), "")

	if !toolNames(tr)["solo_tool"] {
		t.Fatalf("solo_tool should be registered after mount")
	}
	if s := assemble(t, sp); !strings.Contains(s, "SOLO SECTION") {
		t.Fatalf("SOLO SECTION should be present after mount: %q", s)
	}

	// Full teardown of the app component removes its tool and section.
	reg.Remove(solo.ComponentName())
	if toolNames(tr)["solo_tool"] {
		t.Fatalf("solo_tool should be gone after component teardown")
	}
	if s := assemble(t, sp); strings.Contains(s, "SOLO SECTION") {
		t.Fatalf("SOLO SECTION should be gone after component teardown: %q", s)
	}

	// Re-mounting the same app registers it again cleanly (a fresh effect stack).
	reg.Load(solo.Component(), "")
	if !toolNames(tr)["solo_tool"] {
		t.Fatalf("solo_tool should be re-registered after re-mount")
	}
}

// Profile.Load rejects a malformed profile without touching the registry outcome.
func TestProfileLoadValidation(t *testing.T) {
	reg, _, _ := harness(t)

	if err := (Profile{}).Load(nil); err == nil {
		t.Fatalf("Load(nil registry) should error")
	}
	if err := (Profile{Apps: []App{{Name: ""}}}).Load(reg); err == nil {
		t.Fatalf("Load with an unnamed app should error")
	}
	dup := Profile{Apps: []App{{Name: "x"}, {Name: "x"}}}
	if err := dup.Load(reg); err == nil {
		t.Fatalf("Load with a duplicate app name should error")
	}
}

// An app whose prerequisites are absent is not an error at Load; it simply stays
// inactive until they arrive — order-independent activation (confluence).
func TestAppInactiveUntilPrereqsPresent(t *testing.T) {
	reg := runtime.New()
	p := Profile{Apps: []App{{
		Name:     "late",
		Tools:    []hx.ToolDefinition{mkTool("late_tool")},
		Sections: []Section{{Order: 1, Text: "LATE"}},
	}}}

	// Load the profile FIRST, before any provider exists.
	if err := p.Load(reg); err != nil {
		t.Fatalf("Profile.Load (no prereqs yet): %v", err)
	}
	if f := reg.Fiber(App{Name: "late"}.ComponentName()); f == nil || f.State() == runtime.StateActive {
		t.Fatalf("app should be loaded-but-inactive before its prereqs are present")
	}

	// Now bring up the providers; the app activates reactively.
	tr := tools.New(tools.Config{})
	sp := prompt.New()
	reg.Load(&runtime.Component{
		Name: "harness/tools", Provide: []string{hx.KeyTools},
		Apply: func(ctx *runtime.Context) error { return ctx.Provide(hx.KeyTools, tr) },
	}, "")
	reg.Load(&runtime.Component{
		Name: "harness/prompt", Provide: []string{hx.KeyPrompt},
		Apply: func(ctx *runtime.Context) error { return ctx.Provide(hx.KeyPrompt, sp) },
	}, "")

	if f := reg.Fiber(App{Name: "late"}.ComponentName()); f == nil || f.State() != runtime.StateActive {
		t.Fatalf("app should activate once its prereqs are provided")
	}
	if !toolNames(tr)["late_tool"] {
		t.Fatalf("late_tool should be registered once the app activated")
	}
}
