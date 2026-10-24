package hx

import "context"

// The agent registry, the agent handle, the system prompt, and the loop's
// explicit extension points. The loop consumes all of these by key.

// SystemPrompt is the ctx.Get(KeyPrompt) service: ordered sections, dynamic
// context providers, and variables, assembled into one prompt string.
type SystemPrompt interface {
	// Section registers a static ordered section; returns its disposer.
	Section(order int, text string) (dispose func(), err error)
	// Variable registers a named {{variable}} provider.
	Variable(name string, provide func() string) (dispose func(), err error)
	// Assemble builds the full system prompt for a request.
	Assemble(vars map[string]string) (string, error)
}

// DeliverKind controls how a message enters an agent (dsh's followup/steer/inject).
type DeliverKind string

const (
	DeliverFollowup DeliverKind = "followup" // next turn, wake
	DeliverSteer    DeliverKind = "steer"    // next step, wake
	DeliverInject   DeliverKind = "inject"   // next step, no wake
)

// Agent is a live driver. The loop package provides the concrete ReactLoopAgent.
type Agent interface {
	ID() string
	// Send delivers a user/steering message; kind sets when it lands and whether
	// it wakes the driver.
	Send(kind DeliverKind, msg Message)
	// Followup is Send(DeliverFollowup, …): the common "user says something" path.
	Followup(text string)
	// WhenIdle blocks until the agent has no owed work (turn/step queues empty).
	WhenIdle(ctx context.Context) error
	// Cancel aborts in-flight activity with a cause.
	Cancel(cause string)
}

// AgentHandle owns an agent's lifetime.
type AgentHandle interface {
	Agent() Agent
	Dispose()
}

// AgentConfig starts an agent.
type AgentConfig struct {
	// Task is retained in the system prompt independently of compacted history.
	Task        string
	Provider    string
	Model       string
	SessionID   string
	PrincipalID string // the verified principal the agent runs under
	// MaxSteps bounds a single turn's model↔tool steps (§4.2 bounded loop
	// termination): a model stuck re-calling tools is stopped after this many steps
	// so the turn concludes and the driver never hangs. 0 uses the loop's default.
	MaxSteps int
}

// AgentFactory (ctx.Get(KeyAgentLoop)) creates agents. The loop package sets
// itself as the factory so consumers depend on the AGENTS registry, not the loop.
type AgentFactory interface {
	Create(cfg AgentConfig) (AgentHandle, error)
}

// AgentRegistry is the ctx.Get(KeyAgents) service: live agents + the initiator +
// the factory slot.
type AgentRegistry interface {
	SetFactory(f AgentFactory) (dispose func(), err error)
	Create(cfg AgentConfig) (AgentHandle, error)
	// WithInitiator runs fn with a as the process-local initiator (fiber-local);
	// tool bodies read it via RequireInitiator to actor-stamp their facts.
	WithInitiator(a Agent, fn func() error) error
	// CurrentInitiator returns the initiator of the current async chain, if any.
	CurrentInitiator() (Agent, bool)
}

// --- the loop's explicit extension points (typed middleware, no event bus) ---

// PreStepDecision is what a PreStep middleware returns: reject the step, or enter
// it with a (possibly rewritten) batch of user messages.
type PreStepDecision struct {
	Reject bool
	Enter  []Message
}

// PreStepFn assembles/decides one step.
type PreStepFn func(ctx context.Context) PreStepDecision

// PreStepMiddleware wraps step assembly (inject context, gate the step).
type PreStepMiddleware func(next PreStepFn) PreStepFn

// RequestFn produces the GenerateOptions for one model call.
type RequestFn func(ctx context.Context, base GenerateOptions) GenerateOptions

// RequestMiddleware wraps request building (swap provider/model/effort).
type RequestMiddleware func(next RequestFn) RequestFn

// Hooks is the loop's registered extension chains. Empty chains = default behavior.
type Hooks struct {
	PreStep []PreStepMiddleware
	Request []RequestMiddleware
	Tool    []ToolMiddleware
}
