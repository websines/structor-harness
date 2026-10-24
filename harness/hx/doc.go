// Package hx is the Forge harness foundation: every cross-package TYPE and
// INTERFACE the agent spine shares. Concrete spine packages (llm, session,
// tools, prompt, agent, loop) import ONLY this package for their contracts and
// resolve each other's implementations at runtime by service key
// (runtime.Context.Get) — the cordis "everything by key" discipline. That keeps
// the spine decoupled at compile time: no spine package imports another's
// concrete code, so they can be built independently.
//
// Design: forge-harness-design-v0.1.md. The three Forge-specific choices this
// foundation encodes:
//   - the session log is the FLG record (SessionEvent is meant to become a fact);
//   - a tool result is an evidence candidate (ToolResult carries what a collector needs);
//   - extension points are explicit typed middleware chains, not a generic event bus.
//
// No timestamps as authority anywhere the record reads (Law 3): operational time
// may exist for scheduling, never as a verdict or ordering-of-truth input.
package hx
