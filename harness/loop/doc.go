// Package loop is the Forge harness driver — the agent's heart. It provides the
// concrete ReactLoopAgent that runs one turn at a time:
//
//	claim queued input → assemble system prompt + tool schemas → derive message
//	history from the log → stream ONE model call → dispatch the tool calls it
//	returned → append every result to the log → repeat until nothing is owed.
//
// The loop imports no concrete spine package. Every service it needs (the model
// runtime, the session store, the system prompt, the tool runtime, the agent
// registry) is resolved from the shared runtime.Context by service key
// (hx.KeyLLM, hx.KeySession, …) and consumed only through the hx interfaces — the
// cordis "everything by key" discipline that keeps the spine decoupled at compile
// time.
//
// Two things this package deliberately re-implements rather than import: (1) a
// minimal chunk→blocks fold (fold.go), because the canonical BlockAssembler is
// concrete in the llm package and importing it would couple the spine; and (2) a
// minimal Inbox (inbox.go), the per-agent delivery queue. Both are small, allocate
// little, and keep the driver buildable and testable in isolation.
//
// Ordering is by sequence, never by clock (Law 3): the session log's Seq is the
// only ordering-of-truth; nothing here reads a timestamp to decide order.
package loop
