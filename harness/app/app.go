// Package app is the Forge modular app/extension bundle model on the cordis
// runtime. An App is a named bundle of tools + prompt sections that mounts as ONE
// revertible component: mounting registers every tool and section as a tracked
// effect (ctx.Use), so unloading the app withdraws EXACTLY its own contributions
// in LIFO order and nothing else — the runtime's Law 1 (revertible effects) makes
// a bundle a unit of composition, the way dsh's profile bundles are patch layers
// that mount and unmount as one.
//
// A Profile is an ordered set of Apps mounted together, plus a read-only
// Inventory — a projection of what the profile declares, the Go twin of dsh's
// read-only plugin-inventory over the loader tree.
//
// v1 is deliberately PROCESS-level: a profile mounts its apps on the one shared
// Registry, so their tools and sections land on the process-wide ToolRuntime and
// SystemPrompt. Per-agent scoped presets — each live session composing its own
// tools and prompt while the others keep theirs — are a follow-up that needs
// cordis isolation realms we do not have yet (deferred, §4.15 mechanic 7).
package app

import (
	"fmt"
	"sort"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/runtime"
)

// componentPrefix namespaces an app's fiber identity within a registry, so an
// app's component never collides with a harness spine component or another app.
const componentPrefix = "harness/app/"

// Section is one ordered system-prompt contribution from an app. Order places it
// in the SystemPrompt's ordering space (ascending; ties broken by registration
// sequence, never a clock — matching internal/harness/prompt).
type Section struct {
	Order int
	Text  string
}

// App is a modular extension bundle: a named set of tools + prompt sections that
// mounts as ONE cordis component. Mounting is a revertible effect — unloading the
// app withdraws exactly its tools and sections (LIFO), nothing else.
type App struct {
	Name     string
	Tools    []hx.ToolDefinition
	Sections []Section // ordered prompt contributions
}

// ComponentName is the stable fiber identity App.Component mounts under within a
// registry. Pass it to Registry.Remove to unload JUST this app — its tools and
// sections revert while every other app's stay put.
func (a App) ComponentName() string {
	return componentPrefix + a.Name
}

// Component returns the cordis component that mounts this app. It injects
// KeyTools + KeyPrompt (so it activates only once both services are provided by
// active fibers — reactive coeffects, not call order) and registers every tool
// and section via ctx.Use, so each registration is a tracked effect that reverts
// on unload. The component provides nothing: an app is a pure consumer that
// contributes onto the shared tool runtime and system prompt.
func (a App) Component() *runtime.Component {
	return &runtime.Component{
		Name:   a.ComponentName(),
		Inject: []string{hx.KeyTools, hx.KeyPrompt},
		Apply: func(ctx *runtime.Context) error {
			v, err := ctx.Get(hx.KeyTools)
			if err != nil {
				return err
			}
			tr, ok := v.(hx.ToolRuntime)
			if !ok {
				return fmt.Errorf("app %q: %q is not an hx.ToolRuntime", a.Name, hx.KeyTools)
			}
			v, err = ctx.Get(hx.KeyPrompt)
			if err != nil {
				return err
			}
			sp, ok := v.(hx.SystemPrompt)
			if !ok {
				return fmt.Errorf("app %q: %q is not an hx.SystemPrompt", a.Name, hx.KeyPrompt)
			}

			// Each tool registration is an effect: Register hands back the inverse
			// that withdraws exactly this tool, tracked on the fiber's disposer stack.
			for _, td := range a.Tools {
				def := td
				if err := ctx.Use(func() (func(), error) { return tr.Register(def) }); err != nil {
					return fmt.Errorf("app %q: register tool %q: %w", a.Name, def.Schema.Name, err)
				}
			}
			// Each section registration is likewise an effect.
			for _, s := range a.Sections {
				sec := s
				if err := ctx.Use(func() (func(), error) { return sp.Section(sec.Order, sec.Text) }); err != nil {
					return fmt.Errorf("app %q: register section (order %d): %w", a.Name, sec.Order, err)
				}
			}
			return nil
		},
	}
}

// Profile is an ordered set of apps mounted together, with a read-only inventory.
type Profile struct {
	Apps []App
}

// Load mounts every app's component on reg, in order. Each app activates
// reactively once KeyTools + KeyPrompt are provided, so the mount order here does
// not decide the outcome (confluence) — Load only rejects a malformed profile: a
// nil registry, an app with no name, or the same app name listed twice (their
// fibers would collide on one identity). A listed app whose prerequisites are not
// yet present is NOT an error: it simply stays inactive until they are, the cordis
// way, so a profile can be loaded before or after the harness spine.
func (p Profile) Load(reg *runtime.Registry) error {
	if reg == nil {
		return fmt.Errorf("app: Profile.Load requires a registry")
	}
	seen := make(map[string]bool, len(p.Apps))
	for _, a := range p.Apps {
		if a.Name == "" {
			return fmt.Errorf("app: profile contains an app with no name")
		}
		if seen[a.Name] {
			return fmt.Errorf("app: profile lists app %q more than once", a.Name)
		}
		seen[a.Name] = true
	}
	for _, a := range p.Apps {
		reg.Load(a.Component(), "")
	}
	return nil
}

// AppInfo is one app's read-only inventory row: its name, the names of the tools
// it contributes (in declared order), and how many prompt sections it adds.
type AppInfo struct {
	Name     string
	Tools    []string
	Sections int
}

// Inventory returns a read-only projection of the profile's declared apps — name,
// tool names, and section count per app, in profile order. It reads the profile's
// own manifest, not a live registry, so it is a point-in-time description of what
// the profile CONTRIBUTES (the dsh read-only inventory idea); a projection of live
// per-fiber phase would take a registry and is a follow-up.
func (p Profile) Inventory() []AppInfo {
	out := make([]AppInfo, 0, len(p.Apps))
	for _, a := range p.Apps {
		names := make([]string, 0, len(a.Tools))
		for _, t := range a.Tools {
			names = append(names, t.Schema.Name)
		}
		out = append(out, AppInfo{Name: a.Name, Tools: names, Sections: len(a.Sections)})
	}
	return out
}

// SortedInventory is Inventory with each row's tool names sorted and the rows
// ordered by app name — a stable, order-independent view for equality assertions
// and diffing two profiles regardless of how each was declared.
func (p Profile) SortedInventory() []AppInfo {
	inv := p.Inventory()
	for i := range inv {
		sort.Strings(inv[i].Tools)
	}
	sort.Slice(inv, func(i, j int) bool { return inv[i].Name < inv[j].Name })
	return inv
}
