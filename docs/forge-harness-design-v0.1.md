# Forge Harness — Design v0.1

**Component:** the agent execution harness · **Company:** Structor · **Status:** design v0.1 (2026-08-17)
**Substrate:** the Go cordis runtime (`engine/internal/runtime/`) · **Reference:** DeepSeek `dsh` (mapped, not ported wholesale)

> This is **Forge's own harness**, not a port of `dsh`. We take its *core loop* and its *cordis-plugin
> discipline* and rebuild them in Go on the runtime we already have, with three changes that make it
> Forge's: the session log **is** the FLG record, tool results **are** evidence candidates, and
> compaction **is** supersession. Memory efficiency is a first-class constraint throughout.

---

## 1. What the harness is

The thing that actually runs an agent to produce verified software. One **turn** is:

```
claim queued input → assemble system prompt + tool schemas → derive message history from the log
→ stream ONE model call → dispatch the tool calls it returned → append every result to the log
→ repeat until nothing is owed
```

Everything is a component on the shared `runtime.Context`, reachable by key: `ctx.Get("llm")`,
`ctx.Get("tools")`, `ctx.Get("session")`, `ctx.Get("agents")`, `ctx.Get("agentLoop")`. The loop reads
services by key and never imports concrete implementations — the capability-seam discipline
(Definition / Provider / Consumer) we already model with `Component{Inject, Provide, Apply}`.

## 2. The three Forge changes (why this isn't just a port)

### 2.1 The session log IS the FLG record

`dsh`'s session log is an in-memory append-only `SessionEvent[]` that is the single source of truth;
`deriveMessages()` projects LLM history from it. In Forge, **each session event is a fact appended to
`flg.Store`** (`engine/internal/flg/store.go`) — content-addressed, append-only, supersession-capable,
signed into the record. `deriveMessages` becomes a projection over the session's facts, exactly like the
resolver projects verdicts.

Consequences, all free:
- **Auditable by construction.** Every model-visible input, every tool call, every result is a fact
  with a `fact_id`, anchored to a commit (Law 4), stamped with the agent's verified principal
  (`identity/`, Law: `actor` is never a display string).
- **Reconstructable.** "Show me the exact context the model saw at turn 7" is a projection at a
  checkpoint — the audit traversal we already built (`query/`).
- **Reference-not-copy.** Large tool outputs and file contents are stored by digest in the CAS
  (`record/`); the log holds the digest, never the bytes. Two tool calls that read the same 2 MB file
  cost one blob.

### 2.2 Compaction IS supersession

`dsh` compaction replaces a surface span of history with one summary node. In Forge that span of facts
is **superseded** by a summary fact (`SupersessionEvidence`, kind `status_superseded`). The derived
message history shrinks (bounded context, memory drops), the superseded facts stay in the record
(nothing is lost), and the summary cites what it replaced. Memory efficiency and full auditability from
the one mechanism the whole system already runs on.

### 2.3 Tool results ARE evidence candidates

A `tool/result` fact ("the test suite passed at commit c0ffee") is exactly what a collector
(`engine/internal/collectors/`) turns into typed `Evidence` for the resolver. The agent loop therefore
feeds the proof spine directly: agent acts → tool runs in the sandbox → result is a fact → collector
lifts it to evidence → resolver derives the obligation's state → the gate tells the agent what is still
owed. The adversary (`adversary/`) attacks the agent's output; a counterexample becomes falsifying
evidence + supersession. **The harness closes the loop between "an agent wrote code" and "the code is
proven."**

## 3. Substrate & the extension model

We build on `runtime.Context` (`Get`/`Provide`/`Use(Effect)`) and `runtime.Registry`
(`Load`/`Remove`/`Lookup`, reactive activation, cycle detection). Registrations are effects: every tool,
prompt section, and adapter registers through `ctx.Use(effect)` and is reverted LIFO on unload —
already proven (`proofs/ForgeProofs/Runtime.lean`).

**One deliberate divergence from `dsh`: no generic event bus.** `dsh` extensibility rides a typed
event system (`emit`/`parallel`/`serial`/`waterfall`) — 88 files extend it by TS declaration merging,
which does not port and is allocation-heavy. Forge v1 uses **explicit, typed middleware chains** for the
loop's extension points instead:

```go
// A middleware chain is an ordered []Hook the loop invokes; each Hook decides to pass, rewrite, or halt.
type PreStepHook   func(next PreStepFn) PreStepFn      // waterfall around step assembly
type RequestHook   func(next RequestFn) RequestFn      // swap provider/model/effort
type ToolHook      func(next ToolFn) ToolFn            // around tool dispatch (timeout/retry/guard)
```

Fixed extension points (mirroring `dsh`'s, minus the bus): `pre-step`, `request`, `request-error`,
`turn-stopping`, `tool-pre`/`tool-execute`/`tool-post`. This is leaner, explicit over magic (matches the
house style), and cheaper per turn. A general event bus can be added later if extension breadth demands
it; v1 does not need it.

## 4. The spine (Go packages under `engine/internal/harness/`)

Dependency-ordered; each depends only on the ones above. LOC is the rough Go budget.

| Package | Responsibility | Key Go surface | ~LOC |
|---|---|---|---|
| `harness/llm` | conversation vocabulary + adapter seam | `Message`, `ContentBlock` (tagged union), `GenerateOptions`, `StreamChunk`, `LlmRuntime` (register/`Stream`/`PrepareCall`), `LlmAdapter` iface, `BlockAssembler` | ~1.8k |
| `harness/scope` | per-agent isolation | `ScopeKey`, `NewScope`, `ScopeOf`, parent-pointer map; scoped registry layers | ~0.5k |
| `harness/session` | **FLG-backed** append-only log + projection | `Session` (`Append(event)`→fact, `DeriveMessages()`, `RequestHeader()`), `SessionStore` (`Create`, in-memory head over `flg.Store`), the ~12 core event types | ~1.5k |
| `harness/prompt` | system prompt assembly | `SystemPrompt` (`Section`/`Context`/`Variable`/`Assemble`/`Render`) | ~0.6k |
| `harness/tools` | tool registry + execution pipeline | `ToolRuntime` (`Register`/`Execute`/`ExecutionMode`/`Schemas`), `ToolDefinition struct{ Schema json.RawMessage; Handler func(ctx, args) (Result, error) }` — **no inference DSL** | ~2.0k |
| `harness/agent` | agent registry + handle | `AgentRegistry` (`ctx.agents`, `WithInitiator` via fiber-local, `SetFactory`), `Agent` iface, `Inbox`, `agent/*` vocabulary | ~1.5k |
| `harness/loop` | the driver | `ReactLoopAgent` (`kick`/`turn`/`preStep`/`step`), `executeToolCalls` (ordered commit, parallel/exclusive groups) | ~1.8k |

Plus **one adapter** (`harness/llm/deepseek` — `net/http` + SSE, ~1.2k) and **one tool** to prove the
loop. **~10–12k LOC of Go for the whole spine.** (Reference `dsh` source is ~16k TS for the same spine;
the `tools` inference DSL — ~2.9k LOC — evaporates in Go.)

## 5. Memory efficiency (a first-class constraint)

1. **Stream, never buffer.** `LlmAdapter.Stream` returns `<-chan StreamChunk` (+ a final error). Each
   chunk is appended as an `assistant/chunk` fact and folded by `BlockAssembler` incrementally; the full
   response is never held as one string.
2. **The log lives in the store, not RAM.** `Session` keeps a bounded in-memory head (recent facts +
   the derived-message cache, recomputed O(new facts)); older facts live in the CAS and are pulled by
   digest only when a projection needs them.
3. **Reference-not-copy for blobs.** Tool outputs and file contents over a threshold are `record.Put`
   and referenced by digest; the fact carries the digest. Dedup is automatic (content addressing).
4. **Compaction bounds context.** When derived history approaches the model's window, the compaction
   engine supersedes an old span with a summary fact — memory and token cost drop, the record keeps
   everything.
5. **Go hygiene.** `io.Reader` streams end-to-end, `bufio.Scanner` for SSE, bounded capture buffers with
   spill-to-CAS for large subprocess output, no whole-file slurps.

## 6. Proof-native integration (the Forge-only layer)

- **Identity.** An agent runs under a verified principal (`identity/`); `agents.WithInitiator` carries
  it; every appended fact is actor-stamped. "Which agent, attested how, wrote this" is answerable.
- **Sandbox.** Tool execution (shell/subprocess/fs) goes through the sandbox seam (`sandbox/`, argv
  confine; the C `landlock-run` binary used wholly). Default-deny egress.
- **Evidence bridge.** A small `harness/evidence` adapter subscribes to `tool/result` facts and hands
  them to the matching `collectors.Collector` → typed `Evidence` → `resolver`. The agent's `whenIdle`
  reports the gate result (`obligation/`), so the agent knows what obligations remain open.
- **Adversary.** After the agent claims an obligation discharged, `adversary/` attacks it; a
  reproducible counterexample supersedes the discharge (already built).

## 7. BYO-agent: the adapter seam

Forge is model-agnostic. The `LlmAdapter` interface has two production shapes behind it:
- **Direct provider** (like `dsh`'s DeepSeek adapter): HTTP + SSE to an OpenAI-compatible endpoint.
  Simplest; the first real turn uses this.
- **External-agent-over-ACP** (the §6.4 / Agent Client Protocol path): drive Claude Code / Codex / etc.
  as the "model," JSON-RPC over ndjson. Deferred past the first turn; the seam is designed so it drops
  in as another adapter/agent-provider without touching the loop.

## 8. Scope — what we build vs. drop

**Build (v1 spine + first real turn):** `llm`, `scope`, `session`, `prompt`, `tools`, `agent`, `loop`,
one DeepSeek adapter, one in-process tool.
**Build (second wave — real tools):** `subprocess` (local spawn + tree-kill), `fs` (literal-edit + atomic
write), `shell` (`bash` tool), `sandbox` (argv confine + landlock binary).
**Defer:** `compaction` (until context pressure is real — but design the supersession hook now),
`subagent`, `mcp`, `lsp`, `hooks`, `skill`, ACP.
**Drop (not Go, or not core):** the React `client`/`web` UI (belongs in the TS surfaces), `typert`/`api`/
`sdk` (TS-RPC), `boot`'s YAML loader (compose the plugin tree in `main.go` instead), `code-runtime`/
`code-mode` (embedded JS engine), `e2b` (remote-microVM SDK wrapper).

## 9. Build plan

- **Stage H1 — one real turn (the milestone).** Port the seven spine packages + the DeepSeek adapter +
  one trivial in-process tool onto the runtime, composed in a `cmd/forge-agent` (or wired into
  `cmd/forged`) main that news up each service on the root context. Drive: `agentLoop.Create(id,
  {provider, model})` → `agent.Followup(userMessage)` → `agent.WhenIdle()`. **Done when:** a real model
  call streams, returns a tool call, the tool runs, its result is appended as a fact in `flg.Store`, and
  the next step sees it — the full `turn→step→stream→executeToolCalls→tool/result` loop, on the real
  record, with zero native dependencies. Needs a real API key.
- **Stage H2 — real tools in the sandbox.** `subprocess` → `fs` → `shell` → `sandbox`. Now the agent
  edits files and runs commands for real, confined.
- **Stage H3 — close the proof loop.** The `evidence` bridge: tool results → collectors → resolver →
  gate reported back to the agent. This is where the harness meets the spine and Forge does its actual
  job: an agent works to discharge obligations, and the record proves whether it did.

## 10. Open decisions

1. **First adapter target.** DeepSeek/OpenAI-compatible direct call for H1 (simplest real turn), ACP
   deferred. Confirm the endpoint + key source.
2. **Where the harness lives.** `engine/internal/harness/*` (one subpackage per spine package) vs. a
   sibling module. Recommend `internal/harness/` — same module, links the record/resolver/identity
   packages in-process.
3. **Event bus vs. explicit hooks.** v1 = explicit typed middleware chains (§3). Revisit only if
   third-party extension breadth demands a generic bus.

---

*Design v0.1. Companion to `structor-build-guide-v0.3.md` (§0.5 build state) and system design §4.15
(runtime), §4.13 (surfaces), §6.4 (BYO-agent). Revise as `-v0.2` when H1 lands or the spine changes.*
