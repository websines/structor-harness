package loop

import (
	"fmt"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/runtime"
)

// Factory is the hx.AgentFactory the loop registers into the AGENTS registry. It
// news up a ReactLoopAgent per Create: a fresh Session from the session store, a
// driver bound to the shared context (from which it resolves the model/tool/prompt
// runtimes by key). Consumers therefore depend on the agents registry, never on
// the loop package.
type Factory struct {
	svc services
}

// NewFactory builds a Factory that resolves services through svc. The runtime
// Component passes the live *runtime.Context here; tests pass a map.
func NewFactory(svc services) *Factory { return &Factory{svc: svc} }

// Create makes one agent: it creates the session under cfg.SessionID and returns a
// handle owning the agent's lifetime. Registration of the live agent is the
// registry's own concern (it invokes this factory); Create just builds and returns.
func (f *Factory) Create(cfg hx.AgentConfig) (hx.AgentHandle, error) {
	store, err := resolve[hx.SessionStore](f.svc, hx.KeySession)
	if err != nil {
		return nil, err
	}
	sess, err := store.Create(cfg.SessionID)
	if err != nil {
		return nil, fmt.Errorf("loop: create session %q: %w", cfg.SessionID, err)
	}
	return &agentHandle{agent: newAgent(cfg, f.svc, sess)}, nil
}

// agentHandle owns an agent's lifetime; Dispose cancels its in-flight activity.
type agentHandle struct {
	agent *ReactLoopAgent
}

func (h *agentHandle) Agent() hx.Agent { return h.agent }
func (h *agentHandle) Dispose()        { h.agent.Cancel("disposed") }

// Component is the runtime component that installs the loop: it injects the
// model/session/prompt/tool/agents services, provides KeyAgentLoop, and on apply
// registers itself as the AGENTS factory (a revertible effect, withdrawn LIFO on
// unload). Compose it into the plugin tree in main.go.
func Component() *runtime.Component {
	return &runtime.Component{
		Name:    "harness/loop",
		Inject:  []string{hx.KeyLLM, hx.KeySession, hx.KeyPrompt, hx.KeyTools, hx.KeyAgents, hx.KeyCompaction},
		Provide: []string{hx.KeyAgentLoop},
		Apply: func(ctx *runtime.Context) error {
			f := NewFactory(ctx)
			// Registering as the factory is an effect: its inverse (returned by
			// SetFactory) is tracked and reverted when this component unloads.
			if err := ctx.Use(func() (func(), error) {
				agents, err := resolve[hx.AgentRegistry](ctx, hx.KeyAgents)
				if err != nil {
					return nil, err
				}
				return agents.SetFactory(f)
			}); err != nil {
				return err
			}
			return ctx.Provide(hx.KeyAgentLoop, f)
		},
	}
}

// Compile-time proof that Factory is a full hx.AgentFactory and agentHandle a full
// hx.AgentHandle.
var (
	_ hx.AgentFactory = (*Factory)(nil)
	_ hx.AgentHandle  = (*agentHandle)(nil)
)
