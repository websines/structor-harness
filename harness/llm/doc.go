// Package llm is the conversation seam of the Forge harness: the adapter
// registry (Runtime), the streaming fold (BlockAssembler), and the two adapters
// the first turns need — one real OpenAI-compatible HTTP/SSE backend and one
// scripted Fake for deterministic tests. It implements the contracts in the
// shared hx package (LlmRuntime, LlmAdapter) and imports ONLY hx plus the
// standard library; sibling spine packages resolve it at runtime by service key
// (hx.KeyLLM), never by import.
//
// Memory efficiency is the throughline (design §5). An adapter returns a
// <-chan hx.StreamChunk, never a buffered response; the assembler folds those
// chunks into blocks incrementally, holding only the block currently open. The
// full model response is never materialized as one string.
package llm
