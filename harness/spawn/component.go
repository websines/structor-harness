package spawn

import (
	"fmt"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/runtime"
)

// Config tunes the spawn Component. It carries the knobs that are NOT resolved from
// the runtime context (the factory and session store ARE resolved, by key); the
// parent principal, caps and route come from here.
type Config struct {
	// ParentPrincipal stamps the child's PrincipalID. Empty → the calling tool's
	// Initiator (the parent agent) is used at call time.
	ParentPrincipal string
	// MaxChildren caps concurrent in-flight children. < 1 → defaultMaxChildren.
	MaxChildren int
	// ChildMaxSteps is the per-turn step cap stamped on each child. 0 → loop default.
	ChildMaxSteps int
	// Provider / Model are stamped on the child AgentConfig when set.
	Provider string
	Model    string
	// TimeoutMs bounds one whole spawn call. 0 → defaultChildTimeoutMs.
	TimeoutMs int
}

// Component installs spawn_research_agent on the cordis runtime for the SINGLE-registry
// topology: the factory (KeyAgentLoop), the session store (KeySession) and the tool
// runtime (KeyTools) all come from ONE composition, so the child agents share the
// orchestrator's tool set. Injecting KeyAgentLoop makes this component activate only
// AFTER the loop has registered itself as the factory (reactive-coeffect ordering);
// the tool registration is a revertible effect withdrawn LIFO on unload.
//
// For the ISOLATED-child topology (children get web_search/web_scrape + the Scrapling
// skill while the orchestrator gets spawn — the cmd/forge-two-agents shape of two
// registries over one shared CAS), do NOT use this Component: build the tool directly
// with Tool(Deps{Factory: <child factory>, Sessions: <child store>, ...}) and register
// it on the orchestrator's ToolRuntime. Factory and Sessions must be the child
// composition's, so the node_id session the factory creates is the one findings are
// read back from.
func Component(cfg Config) *runtime.Component {
	return &runtime.Component{
		Name:   "harness/spawn",
		Inject: []string{hx.KeyAgentLoop, hx.KeySession, hx.KeyTools},
		Apply: func(ctx *runtime.Context) error {
			factory, err := get[hx.AgentFactory](ctx, hx.KeyAgentLoop)
			if err != nil {
				return err
			}
			store, err := get[hx.SessionStore](ctx, hx.KeySession)
			if err != nil {
				return err
			}
			tr, err := get[hx.ToolRuntime](ctx, hx.KeyTools)
			if err != nil {
				return err
			}

			def := Tool(Deps{
				Factory:         factory,
				Sessions:        store,
				ParentPrincipal: cfg.ParentPrincipal,
				MaxChildren:     cfg.MaxChildren,
				ChildMaxSteps:   cfg.ChildMaxSteps,
				Provider:        cfg.Provider,
				Model:           cfg.Model,
				TimeoutMs:       cfg.TimeoutMs,
			})
			// Registering the tool is an effect: its disposer (from Register) is tracked
			// and reverted when this component unloads.
			return ctx.Use(func() (func(), error) { return tr.Register(def) })
		},
	}
}

// get resolves the service at key from the runtime context and asserts it to T — the
// by-key resolution pattern the spine uses instead of importing a concrete sibling
// package. ctx.Get enforces the component's declared confinement (a read of an
// un-injected key is an error).
func get[T any](ctx *runtime.Context, key string) (T, error) {
	var zero T
	v, err := ctx.Get(key)
	if err != nil {
		return zero, err
	}
	t, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("spawn: service %q has unexpected type %T", key, v)
	}
	return t, nil
}
