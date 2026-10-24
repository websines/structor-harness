package hx

import "context"

// The conversation vocabulary the whole loop moves. Faithful to dsh's llm
// package, reshaped for Go: tagged structs instead of TS unions, a channel
// instead of AsyncIterable.

// Role of a message in the conversation.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// BlockKind discriminates a ContentBlock. Switch on it; a closed switch ends in
// a default that rejects the unknown kind.
type BlockKind string

const (
	BlockText       BlockKind = "text"
	BlockReasoning  BlockKind = "reasoning"
	BlockImage      BlockKind = "image"
	BlockToolCall   BlockKind = "tool-call"
	BlockToolResult BlockKind = "tool-result"
)

// ContentBlock is one piece of a message. Only the fields relevant to Kind are
// set. Large payloads (image bytes, big tool results) are referenced by CAS
// digest via BlobRef, never inlined — reference-not-copy, memory efficiency.
type ContentBlock struct {
	Kind BlockKind `json:"kind"`

	// text / reasoning
	Text string `json:"text,omitempty"`

	// image / large payloads: a CAS digest ("sha256:…") instead of inline bytes.
	BlobRef  string `json:"blob_ref,omitempty"`
	MimeType string `json:"mime_type,omitempty"`

	// tool-call
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	ToolArgs   []byte `json:"tool_args,omitempty"` // raw JSON arguments

	// tool-result (references the ToolCallID it answers)
	IsError bool `json:"is_error,omitempty"`
}

// Message is one turn of conversation. Content is ordered.
type Message struct {
	ID      string         `json:"id"`
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content"`
	Source  string         `json:"source,omitempty"` // provenance (which fact/step produced it)
}

// GenerateOptions is one model request. It is a pure function of the session log
// (Law: model-visible ⟺ logged). Signal cancellation flows through ctx.
type GenerateOptions struct {
	Provider    string       `json:"provider"`
	Model       string       `json:"model"`
	System      string       `json:"system,omitempty"`
	Messages    []Message    `json:"messages"`
	Tools       []ToolSchema `json:"tools,omitempty"`
	Effort      string       `json:"effort,omitempty"` // reasoning effort, provider-specific
	MaxTokens   int          `json:"max_tokens,omitempty"`
	PromptCache *PromptCache `json:"prompt_cache,omitempty"`
}

// PromptCache describes reusable input, never a cached answer. SystemPrefix must
// be an exact leading substring of System. The remaining text is fresh context,
// except for the separately identified immutable Task anchor.
// ReuseHistory is for agent loops; one-shot calls avoid writing disposable input.
type PromptCache struct {
	SystemPrefix string `json:"system_prefix,omitempty"`
	// Task is the immutable authorized intent already appended to System. Keeping
	// it before history makes large task packets reusable across steps/compaction.
	Task         string `json:"task,omitempty"`
	ReuseHistory bool   `json:"reuse_history,omitempty"`
}

// ChunkKind discriminates a StreamChunk.
type ChunkKind string

const (
	ChunkBlockStart     ChunkKind = "block-start"
	ChunkTextDelta      ChunkKind = "text-delta"
	ChunkReasoningDelta ChunkKind = "reasoning-delta"
	ChunkToolCallDelta  ChunkKind = "tool-call-delta"
	ChunkBlockEnd       ChunkKind = "block-end"
	ChunkUsage          ChunkKind = "usage"
	ChunkFinish         ChunkKind = "finish"
)

// FinishReason ends a stream.
type FinishReason string

const (
	FinishStop      FinishReason = "stop"
	FinishToolCalls FinishReason = "tool_calls"
	FinishLength    FinishReason = "length"
	FinishError     FinishReason = "error"
	FinishAborted   FinishReason = "aborted"
)

// Usage is token accounting from a stream.
type Usage struct {
	// Model preserves the provider's resolved identifier when it is reported.
	Model        string `json:"model,omitempty"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	// Missing provider details remain unknown, rather than measured zero.
	CachedInputTokens *int `json:"cached_input_tokens,omitempty"`
	CacheWriteTokens  *int `json:"cache_write_tokens,omitempty"`
	ReasoningTokens   *int `json:"reasoning_tokens,omitempty"`
}

// StreamChunk is one incremental piece of a model response. The loop appends
// each as an assistant/chunk fact (raw replay fidelity) AND folds it into blocks
// via a BlockAssembler. Streamed, never buffered whole.
type StreamChunk struct {
	Kind ChunkKind `json:"kind"`

	// block-start
	BlockIndex int       `json:"block_index,omitempty"`
	BlockOf    BlockKind `json:"block_of,omitempty"`

	// text-delta / reasoning-delta
	Delta string `json:"delta,omitempty"`

	// tool-call-delta
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	ArgsDelta  string `json:"args_delta,omitempty"`

	// usage
	Usage *Usage `json:"usage,omitempty"`

	// finish
	Finish FinishReason `json:"finish,omitempty"`
	Err    string       `json:"err,omitempty"` // set when Finish == FinishError
}

// LlmFailure is a typed adapter error.
type LlmFailure struct {
	Kind      string // "rate_limit" | "auth" | "network" | "provider" | "aborted"
	Message   string
	Retryable bool
}

func (e *LlmFailure) Error() string { return e.Kind + ": " + e.Message }

// LlmAdapter is one provider backend. The ONLY required method is Stream. An
// adapter is registered with LlmRuntime under a provider route. Stream returns a
// channel of chunks; a terminal ChunkFinish (possibly FinishError) closes it.
// ctx cancellation must abort the underlying request.
type LlmAdapter interface {
	// Route is the provider id this adapter answers to (e.g. "openai-compatible").
	Route() string
	// Stream issues one model call and streams chunks until a terminal finish.
	Stream(ctx context.Context, opts GenerateOptions) (<-chan StreamChunk, error)
}

// LlmRuntime is the ctx.Get(KeyLLM) service: the adapter registry + the call seam.
type LlmRuntime interface {
	RegisterAdapter(a LlmAdapter) (dispose func(), err error)
	// Stream routes opts to the adapter for opts.Provider and streams its chunks.
	Stream(ctx context.Context, opts GenerateOptions) (<-chan StreamChunk, error)
}
