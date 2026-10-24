package shell

import (
	"fmt"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/runtime"
)

// Component installs the shell capability on the cordis runtime. It injects the
// subprocess and sandbox capabilities plus the tool runtime, news a Bash over
// them, registers the `bash` tool (a revertible effect), and provides KeyShell.
//
// Injecting KeySandbox means the sandbox provider must always be loaded before
// this component activates — that is intended (mechanic 9: the dependency set is
// declared and reviewable at load time). When real confinement is unavailable the
// provider is a no-op-with-record whose Available() is false, so the shell still
// comes up and simply runs commands unconfined; there is no "sandbox optional"
// branch in the wiring.
func Component() *runtime.Component {
	return ComponentAt("")
}

// ComponentAt mounts shell tools with the same workspace as the file tools.
func ComponentAt(workDir string) *runtime.Component {
	return ComponentAtWithEnvironment(workDir, nil)
}

func ComponentAtWithEnvironment(workDir string, environment []string) *runtime.Component {
	return &runtime.Component{
		Name:    "harness/shell",
		Inject:  []string{hx.KeySubprocess, hx.KeySandbox, hx.KeyTools},
		Provide: []string{hx.KeyShell},
		Apply: func(ctx *runtime.Context) error {
			sub, err := get[hx.Subprocess](ctx, hx.KeySubprocess)
			if err != nil {
				return err
			}
			sandbox, err := get[hx.SandboxProvider](ctx, hx.KeySandbox)
			if err != nil {
				return err
			}
			tr, err := get[hx.ToolRuntime](ctx, hx.KeyTools)
			if err != nil {
				return err
			}

			sh := New(sub, sandbox)

			// Registering the bash tool is an effect: its disposer (from Register)
			// is tracked and reverted when this component unloads.
			if err := ctx.Use(func() (func(), error) { return tr.Register(BashToolAtWithEnvironment(sh, workDir, environment)) }); err != nil {
				return err
			}
			return ctx.Provide(hx.KeyShell, sh)
		},
	}
}

// get resolves the service at key from the runtime context and asserts it to T —
// the by-key resolution pattern the spine uses instead of importing a concrete
// sibling package. ctx.Get enforces the component's declared confinement (a read
// of an un-injected key is an error).
func get[T any](ctx *runtime.Context, key string) (T, error) {
	var zero T
	v, err := ctx.Get(key)
	if err != nil {
		return zero, err
	}
	t, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("shell: service %q has unexpected type %T", key, v)
	}
	return t, nil
}
