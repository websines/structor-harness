package sandbox

import (
	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/runtime"
)

// Component is the runtime component that installs the sandbox capability: it
// injects nothing and provides KeySandbox, and on apply news a Local confinement
// provider and publishes it under the key. Compose it into the plugin tree in
// main.go ahead of the shell/subprocess components, which resolve it by key to
// confine the argv they run.
func Component() *runtime.Component {
	return &runtime.Component{
		Name:    "harness/sandbox",
		Provide: []string{hx.KeySandbox},
		Apply: func(ctx *runtime.Context) error {
			it := New()
			return ctx.Provide(hx.KeySandbox, it)
		},
	}
}
