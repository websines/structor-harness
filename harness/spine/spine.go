// Package spine composes the Forge harness onto the cordis runtime: it mounts
// each capability (llm, session, tools, prompt, agents, loop) as a
// runtime.Component and loads them on one Registry, so the whole agent tree comes
// up the cordis way — services resolved by key, registrations as revertible
// effects, activation driven by reactive coeffects in dependency order. This is
// the file that makes "the harness runs on cordis" true rather than merely
// designed-for (forge-harness-design-v0.1.md §3, §9 stage H1).
//
// A consumer builds a Registry, calls Build with its options (the model adapter,
// the tools, the CAS the session log lives in), and then resolves the agent
// factory by key to drive a turn. main.go for the live path and spine_test.go for
// the deterministic one-turn integration both go through here.
package spine

import (
	"fmt"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/harness/agent"
	"github.com/websines/structor-harness/harness/app"
	"github.com/websines/structor-harness/harness/compaction"
	hctx "github.com/websines/structor-harness/harness/context"
	"github.com/websines/structor-harness/harness/correct"
	"github.com/websines/structor-harness/harness/evidence"
	"github.com/websines/structor-harness/harness/fs"
	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/llm"
	"github.com/websines/structor-harness/harness/loop"
	"github.com/websines/structor-harness/harness/mcp"
	"github.com/websines/structor-harness/harness/memory"
	"github.com/websines/structor-harness/harness/prompt"
	"github.com/websines/structor-harness/harness/sandbox"
	"github.com/websines/structor-harness/harness/selfimprove"
	"github.com/websines/structor-harness/harness/session"
	"github.com/websines/structor-harness/harness/shell"
	"github.com/websines/structor-harness/harness/skill"
	"github.com/websines/structor-harness/harness/subprocess"
	"github.com/websines/structor-harness/harness/tools"
	"github.com/websines/structor-harness/learning"
	"github.com/websines/structor-harness/query"
	"github.com/websines/structor-harness/record"
	"github.com/websines/structor-harness/runtime"
)

// Section is one ordered system-prompt section.
type Section struct {
	Order int
	Text  string
}

// Options configures a harness composition. The Adapter is the model backend
// (a real OpenAI-compatible adapter, or the scripted fake); Provider is the route
// the loop asks for and must equal Adapter.Route(). Actor stamps every session
// event with a verified principal + commit (Law: actor is never a display string).
type Options struct {
	CAS      record.CAS
	Adapter  hx.LlmAdapter
	Actor    session.Config
	Tools    []hx.ToolDefinition
	Sections []Section
	// Exec mounts the H2 execution capabilities (subprocess, sandbox, fs, shell)
	// and their real tools (read_file/write_file/edit_file, bash), rooted at
	// WorkDir. Off by default so a pure-conversation harness stays minimal.
	Exec    bool
	WorkDir string
	// ExecSandbox and ExecEnvironment are operator-owned overrides for isolated
	// experiments. nil retains the desktop's existing composition.
	ExecSandbox     hx.SandboxProvider
	ExecEnvironment []string
	// Evidence, when set, mounts the H3 evidence bridge: a tool result carrying an
	// EvidenceHint is lifted into typed evidence in this sink's record and re-run
	// through the resolver, so the agent's work feeds the gate. Register the
	// obligations on the sink before Build.
	Evidence *evidence.Sink
	// Compaction is the engine that bounds the derived context by supersession
	// (H4a). Always mounted; nil uses the default token budget. Trigger is context
	// length (estimated tokens vs MaxTokens), with MaxMessages as an optional cap.
	Compaction *compaction.Engine
	// SummaryModel/SummaryProvider, when set, give compaction a REAL LLM summarizer
	// (the named model condenses the compacted span) instead of the deterministic
	// fallback. Often the same model driving the agent.
	SummaryProvider string
	SummaryModel    string
	// CompactionScorer makes compaction QUALITY-AWARE (keep load-bearing messages,
	// compact stale ones). When nil and Evidence is set, the record-aware scorer
	// (evidence.Sink.Scorer — active obligations keep their context, settled ones
	// let it be compacted) is used automatically.
	CompactionScorer func(hx.Message) int
	// ContextStore, when set, mounts the H4b verified-context provider: each turn
	// the agent's system prompt gains a CITED, active-first, never-stale block
	// assembled from this record (obligations, evidence, learned skills, prepped
	// facts — all facts). A superseded fact drops out on the next turn.
	ContextStore *flg.Store
	// ContextQuery, when set, makes verified-context retrieval LIVE + RANKED: it is
	// called each turn for the current work (the task / latest turn) so the record
	// is retrieved by relevance (BM25 → centrality → expansion) over a fresh index,
	// instead of the unranked active-facts dump. nil keeps the static behaviour.
	ContextQuery func() string
	// ContextOrder overrides the verified-context section order; zero keeps the
	// existing default. Native runs put mutable context after the skill catalog.
	ContextOrder int
	// SkillRoots, when non-empty, mounts the skill system: skills discovered from
	// these ranked roots (project → user → shared) become an <available_skills>
	// catalog section plus a `skill` tool the model loads on demand — user skill
	// libraries.
	SkillRoots []skill.Root
	// Apps mounts modular extension bundles: each App contributes its tools + prompt
	// sections as ONE revertible cordis component.
	Apps []app.App
	// SelfCorrect mounts the self-correction layer: a repeat-tool loop-breaker and a
	// failure memory that injects "avoid repeating this" into later turns.
	SelfCorrect bool
	// MCPServers mounts one MCP client per entry, registering each server's tools as
	// mcp__<name>__<tool> on the ToolRuntime (revertible; generation-replace on
	// tools/list_changed).
	MCPServers []MCPServer
	// Learning, when set, FOLDS skills + tool failures into the learning store (over
	// the same record): each SkillRoots skill becomes an acceptance-gated candidate
	// fact and every tool error becomes a failure fact — record-backed, recallable
	// memory. Point it at a learning.Store over the ContextStore to make folded
	// skills/failures surface in the ranked verified-context.
	Learning *learning.Store
	// SelfImprove, when set, wires the SkillOpt loop into the lifecycle: a tool
	// FAILURE during a run schedules an offline, validation-gated self-improvement
	// pass over the admitted skills (skillopt.Sweep). It rides the tool-middleware
	// seam and runs on its own goroutine — never blocking the agent — and can only
	// improve the skill library or leave it unchanged.
	SelfImprove *selfimprove.Service
}

// MCPServer names one Model Context Protocol server and the transport to reach it.
type MCPServer struct {
	Name      string
	Transport mcp.Transport
}

// Build mounts the six harness components on reg. The five service components
// declare no inject and activate immediately; loop.Component() injects all five
// and therefore activates only after they are provided — the reactive-coeffect
// ordering does the wiring, not the call order here. Returns an error if the CAS
// or adapter is missing (a harness with no record or no model is misconfigured).
func Build(reg *runtime.Registry, o Options) error {
	if o.CAS == nil {
		return fmt.Errorf("spine: Build requires a CAS (the session log's record)")
	}
	if o.Adapter == nil {
		return fmt.Errorf("spine: Build requires an LLM adapter")
	}

	// llm: the model runtime + the configured adapter (registered as an effect).
	reg.Load(&runtime.Component{
		Name:    "harness/llm",
		Provide: []string{hx.KeyLLM},
		Apply: func(ctx *runtime.Context) error {
			rt := llm.NewRuntime()
			if err := ctx.Use(func() (func(), error) { return rt.RegisterAdapter(o.Adapter) }); err != nil {
				return err
			}
			return ctx.Provide(hx.KeyLLM, rt)
		},
	}, "")

	// session: the FLG-backed log store, stamped with the actor.
	reg.Load(&runtime.Component{
		Name:    "harness/session",
		Provide: []string{hx.KeySession},
		Apply: func(ctx *runtime.Context) error {
			return ctx.Provide(hx.KeySession, session.NewStore(o.CAS).StampActor(o.Actor))
		},
	}, "")

	// tools: the registry + the configured tools (each registered as an effect).
	reg.Load(&runtime.Component{
		Name:    "harness/tools",
		Provide: []string{hx.KeyTools},
		Apply: func(ctx *runtime.Context) error {
			tr := tools.New(tools.Config{})
			for _, td := range o.Tools {
				def := td
				if err := ctx.Use(func() (func(), error) { return tr.Register(def) }); err != nil {
					return err
				}
			}
			return ctx.Provide(hx.KeyTools, tr)
		},
	}, "")

	// prompt: the system-prompt assembler + the configured sections.
	reg.Load(&runtime.Component{
		Name:    "harness/prompt",
		Provide: []string{hx.KeyPrompt},
		Apply: func(ctx *runtime.Context) error {
			sp := prompt.New()
			for _, s := range o.Sections {
				sec := s
				if err := ctx.Use(func() (func(), error) { return sp.Section(sec.Order, sec.Text) }); err != nil {
					return err
				}
			}
			return ctx.Provide(hx.KeyPrompt, sp)
		},
	}, "")

	// agents: the live-agent registry + factory slot.
	reg.Load(&runtime.Component{
		Name:    "harness/agents",
		Provide: []string{hx.KeyAgents},
		Apply: func(ctx *runtime.Context) error {
			return ctx.Provide(hx.KeyAgents, agent.New())
		},
	}, "")

	// H2 execution capabilities (optional). sandbox is a provider with no inject
	// (activates immediately); subprocess injects sandbox — it will not spawn a
	// model-chosen command it cannot confine, so confinement must exist before it
	// comes up; fs injects tools; shell injects subprocess + sandbox + tools — so
	// reactive coeffects bring each up only after its dependencies are active, in
	// load order or not. Their tools register on the same ToolRuntime.
	if o.Exec {
		reg.Load(subprocess.Component(), "")
		if o.ExecSandbox == nil {
			reg.Load(sandbox.Component(), "")
		} else {
			reg.Load(&runtime.Component{Name: "harness/sandbox", Provide: []string{hx.KeySandbox}, Apply: func(ctx *runtime.Context) error { return ctx.Provide(hx.KeySandbox, o.ExecSandbox) }}, "")
		}
		reg.Load(fs.Component(o.WorkDir), "")
		reg.Load(shell.ComponentAtWithEnvironment(o.WorkDir, o.ExecEnvironment), "")
	}

	// H4a compaction: always provide a compaction engine under KeyCompaction (the
	// loop injects it and calls MaybeCompact each step). nil → default budget.
	eng := o.Compaction
	if eng == nil {
		eng = &compaction.Engine{}
	}
	reg.Load(&runtime.Component{
		Name:    "harness/compaction",
		Inject:  []string{hx.KeyLLM},
		Provide: []string{hx.KeyCompaction},
		Apply: func(ctx *runtime.Context) error {
			// Give the engine a real LLM summarizer when a summary model is named
			// and the engine did not already carry one.
			if o.SummaryModel != "" && eng.Summarize == nil {
				v, err := ctx.Get(hx.KeyLLM)
				if err != nil {
					return err
				}
				if rt, ok := v.(hx.LlmRuntime); ok {
					eng.Summarize = compaction.LLMSummarizer(rt, o.SummaryProvider, o.SummaryModel)
				}
			}
			// Quality-aware selection: explicit scorer, else the record-aware one
			// from the evidence sink (active obligations keep their context).
			if eng.Score == nil {
				if o.CompactionScorer != nil {
					eng.Score = o.CompactionScorer
				} else if o.Evidence != nil {
					eng.Score = o.Evidence.Scorer()
				}
			}
			return ctx.Provide(hx.KeyCompaction, eng)
		},
	}, "")

	// H3 evidence bridge (optional): installs the evidence middleware on the tool
	// runtime and provides the sink. Injects KeyTools, so it activates after tools.
	if o.Evidence != nil {
		reg.Load(evidence.Component(o.Evidence), "")
	}

	// H4b verified-context injection (optional): a context provider on the prompt
	// that assembles a cited, active-first block from ContextStore each turn.
	if o.ContextStore != nil {
		index, err := query.FromStore(o.ContextStore, nil)
		if err != nil {
			return fmt.Errorf("spine: build context index: %w", err)
		}
		order := o.ContextOrder
		if order == 0 {
			order = hctx.DefaultOrder
		}
		reg.Load(hctx.ComponentAt(o.ContextStore, index, o.ContextQuery, order), "")
	}

	// Extension layer (all optional, all mount as cordis components that inject the
	// core services and activate reactively once those are present):
	//   - skills: a user skill library (ranked roots) → catalog section + `skill` tool.
	//   - apps: modular extension bundles, each contributing tools + prompt sections.
	//   - self-correction: repeat-tool loop-breaker + failure-memory advisory.
	//   - MCP: external tool servers, each exposed as mcp__<name>__<tool>.
	if len(o.SkillRoots) > 0 {
		reg.Load(skill.Component(o.SkillRoots), "")
	}
	for _, a := range o.Apps {
		reg.Load(a.Component(), "")
	}
	if o.SelfCorrect {
		reg.Load(correct.Component(), "")
	}
	for _, m := range o.MCPServers {
		reg.Load(mcp.Component(m.Name, m.Transport), "")
	}
	// Fold skills + tool failures into the learning store (record-backed memory).
	if o.Learning != nil {
		reg.Load(memory.Component(o.Learning, o.SkillRoots), "")
	}
	// Wire the SkillOpt loop into the lifecycle: a tool failure schedules an
	// offline validation-gated self-improvement pass (injects KeyTools).
	if o.SelfImprove != nil {
		reg.Load(selfimprove.Component(o.SelfImprove), "")
	}

	// loop: injects the core services + compaction, provides KeyAgentLoop, factory.
	reg.Load(loop.Component(), "")

	return nil
}

// Factory resolves the agent factory from a built registry, or errors if the
// loop component did not activate (which means a dependency did not come up).
func Factory(reg *runtime.Registry) (hx.AgentFactory, error) {
	v, ok := reg.Lookup(hx.KeyAgentLoop)
	if !ok {
		return nil, fmt.Errorf("spine: agent loop not active — a dependency failed to provide its service")
	}
	f, ok := v.(hx.AgentFactory)
	if !ok {
		return nil, fmt.Errorf("spine: %s is not an AgentFactory", hx.KeyAgentLoop)
	}
	return f, nil
}

// Store resolves the session store from a built registry (to read a session log
// back after a turn).
func Store(reg *runtime.Registry) (hx.SessionStore, error) {
	v, ok := reg.Lookup(hx.KeySession)
	if !ok {
		return nil, fmt.Errorf("spine: session store not active")
	}
	s, ok := v.(hx.SessionStore)
	if !ok {
		return nil, fmt.Errorf("spine: %s is not a SessionStore", hx.KeySession)
	}
	return s, nil
}
