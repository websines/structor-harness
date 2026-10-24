package subprocess

import (
	"fmt"
	"os"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/runtime"
)

// allowUnconfinedEnv is the operator's opt-in to run on a host with no
// confinement mechanism. It is read ONCE, here at composition, and never per
// call: nothing a model produces can reach an environment variable of the
// already-running process, so the decision belongs to whoever started Forge. Any
// non-empty value enables it (the same convention as the other FORGE_* switches).
const allowUnconfinedEnv = "FORGE_ALLOW_UNCONFINED_EXEC"

// Component is the runtime component that installs the local subprocess
// capability: it injects the sandbox provider, provides hx.KeySubprocess, and on
// apply news up a Local over that provider and publishes it.
//
// It INJECTS KeySandbox — the same declaration shell makes, and for the stronger
// reason. shell can decline to wrap an argv; the spawner cannot decline to run
// it, so confinement has to be present before this component activates or there
// is nothing to confine with. Making it a declared dependency (mechanic 9) means
// the requirement is reviewable at load time instead of being discovered as a
// nil field on the first model-chosen command.
func Component() *runtime.Component {
	return &runtime.Component{
		Name:    "harness/subprocess",
		Inject:  []string{hx.KeySandbox},
		Provide: []string{hx.KeySubprocess},
		Apply: func(ctx *runtime.Context) error {
			v, err := ctx.Get(hx.KeySandbox)
			if err != nil {
				return err
			}
			sb, ok := v.(hx.SandboxProvider)
			if !ok {
				return fmt.Errorf("subprocess: service %q has unexpected type %T", hx.KeySandbox, v)
			}
			// Fail-closed by default (see confine): on a host with no mechanism the
			// spawner refuses, and only an operator who set the env var gets the
			// degraded profile — with the hard-deny floor still in force and every
			// result stamped hx.ConfineNone.
			if os.Getenv(allowUnconfinedEnv) != "" {
				return ctx.Provide(hx.KeySubprocess, NewDegraded(sb))
			}
			return ctx.Provide(hx.KeySubprocess, New(sb))
		},
	}
}
