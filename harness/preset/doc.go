// Package preset is PER-AGENT scoped composition on the Forge harness: one process
// runs several differently-composed agents, and each sees the shared base tools and
// system prompt PLUS its own preset — without seeing any other agent's preset.
//
// Where package app mounts an App onto the ONE shared Registry (process-level: every
// agent in the process sees every mounted app), preset gives each agent a private
// VIEW: a shared base plus a per-agent OVERLAY. The overlay never touches the base,
// so agent alpha's extra tools and prompt sections are invisible to agent beta and
// vice versa, while both still see the common base underneath.
//
//	               shared base (tools.Runtime, prompt.SystemPrompt)
//	               /                                              \
//	alpha's overlay: {toolA, SEC-A}                 beta's overlay: {toolB, SEC-B}
//	alpha sees {common, toolA} + {BASE, SEC-A}      beta sees {common, toolB} + {BASE, SEC-B}
//
// The two view types satisfy the SAME interfaces the base does, so an agent uses a
// scoped view anywhere the base runtime/prompt is used:
//
//   - ScopedTools implements hx.ToolRuntime. Schemas() returns base ∪ overlay with
//     the overlay winning on a name clash; Execute prefers an overlay tool and falls
//     back to the base; Register adds to the overlay only.
//   - ScopedPrompt implements hx.SystemPrompt. Assemble() renders the base's
//     assembly followed by this scope's own sections; Section adds to the overlay.
//
// Shadowing is by NAME: an overlay tool named the same as a base tool wins for that
// agent (its Schemas() and Execute both resolve the overlay's version), while the
// base's tool of that name is completely unchanged for anyone using the base
// directly or through another agent's scope. Nothing the overlay does mutates the
// shared base — that is the property that keeps agents' presets from leaking into
// one another.
//
// # What this is, and what it is NOT
//
// This package delivers per-agent tool/skill/prompt SETS: each agent gets its own
// VIEW of which tools exist and which prompt sections are present. That is the
// tractable layer achievable on today's runtime.
//
// It does NOT deliver capability CONFINEMENT. A scoped tool still executes with the
// full ambient access of the process — routing a call to an overlay vs. the base
// only decides WHICH body runs, not with what authority it runs. Making a scoped
// tool genuinely unable to reach what its scope forbids requires cordis isolation
// realms (system design §4.15 mechanic 7), which the Forge component runtime
// deliberately OMITS for now (see internal/runtime/doc.go). Full capability
// isolation is therefore out of scope and deferred until mechanic 7 lands. Read the
// per-agent boundary here as "what an agent SEES and can NAME", not "what an agent
// is PREVENTED from reaching".
//
// # Variables
//
// Because Assemble composes through the opaque hx.SystemPrompt interface (which
// exposes only Assemble, not the individual base sections and their orders), a
// scope's {{variable}} references resolve within their own layer — base variables
// for base sections, the scope's variables for the scope's sections — plus any
// request-time vars, which are passed to BOTH layers. Cross-layer interleaving of
// sections strictly by order number is not possible through the interface, so the
// base renders as one block and the scope's sections follow as the next.
package preset
