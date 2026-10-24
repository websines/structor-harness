package skill

import (
	"fmt"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/runtime"
)

// registryKey is the local service key this component publishes the Registry under.
// It is a package-local literal on purpose: the skill system is not part of the hx
// spine, so it does NOT add a key to hx/keys.go. A consumer that wants the registry
// injects "skills" and casts to *skill.Registry.
const registryKey = "skills"

// catalogOrder places the <available_skills> block among the prompt's ordered
// sections (higher than the core instructions, before task-specific context).
const catalogOrder = 500

// Component installs the skill system. It injects the tool runtime and the system
// prompt, discovers skills across the given ranked roots, and:
//   - registers the model-invocable `skill` tool (revertible),
//   - registers the always-present catalog as a prompt section when non-empty
//     (revertible),
//   - publishes the *Registry under the local key "skills" (revertible via Provide).
//
// The Registry is built once at Apply time from discovery; all three installations
// are effects, so unloading the component withdraws the tool, the section, and the
// service in LIFO order.
func Component(roots []Root) *runtime.Component {
	return &runtime.Component{
		Name:    "harness/skill",
		Inject:  []string{hx.KeyTools, hx.KeyPrompt},
		Provide: []string{registryKey},
		Apply: func(ctx *runtime.Context) error {
			skills, err := DiscoverFilesystem(roots)
			if err != nil {
				return err
			}
			reg := NewRegistry()
			for _, s := range skills {
				reg.Add(s)
			}

			// Register the `skill` tool as a revertible effect.
			if err := ctx.Use(func() (func(), error) {
				tr, err := get[hx.ToolRuntime](ctx, hx.KeyTools)
				if err != nil {
					return nil, err
				}
				return tr.Register(SkillTool(reg))
			}); err != nil {
				return err
			}

			// Register the catalog prompt section, but only when there is something
			// to inject — an empty catalog costs no section.
			if section := CatalogSection(reg); section != "" {
				if err := ctx.Use(func() (func(), error) {
					sp, err := get[hx.SystemPrompt](ctx, hx.KeyPrompt)
					if err != nil {
						return nil, err
					}
					return sp.Section(catalogOrder, section)
				}); err != nil {
					return err
				}
			}

			// Publish the registry last, so it appears only once its tool is in.
			return ctx.Provide(registryKey, reg)
		},
	}
}

// get resolves the service at key and asserts it to T — the by-key resolution
// pattern the spine uses instead of importing a concrete sibling. ctx.Get enforces
// the component's declared confinement.
func get[T any](ctx *runtime.Context, key string) (T, error) {
	var zero T
	v, err := ctx.Get(key)
	if err != nil {
		return zero, err
	}
	t, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("skill: service %q has unexpected type %T", key, v)
	}
	return t, nil
}
