package hx

import "context"

// The tool contract. A Go tool is a struct with a hand-written JSON Schema and a
// typed handler — the dsh defineTool type-inference DSL evaporates here.

// ToolSchema is the model-facing description of a tool (what goes in the request).
type ToolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"` // a JSON Schema object
}

// ToolRunContext is handed to a tool body. Signal (ctx) cancels a running tool;
// Initiator is the verified agent that initiated the call (for actor-stamping
// and sandbox policy).
type ToolRunContext struct {
	Ctx       context.Context
	CallID    string
	Initiator string // agent id / principal
}

// ToolResult is the outcome of one tool call. Value is the model-visible JSON
// result; Content are richer blocks (large parts referenced by CAS digest).
// ConcludesTurn ends the turn; AdditionalContext is staged into the next step.
// EvidenceHint, when set, tells the H3 evidence bridge which obligation this
// result bears on and with what polarity — a tool result is an evidence candidate.
type ToolResult struct {
	IsError           bool
	Value             []byte // JSON
	Content           []ContentBlock
	ConcludesTurn     bool
	AdditionalContext []Message
	EvidenceHint      *EvidenceHint
}

// EvidenceHint links a tool result to the proof spine (H3). Kept minimal here;
// the evidence bridge maps it onto flg.Evidence via a collector.
type EvidenceHint struct {
	ObligationID string
	Supports     bool   // true = supports, false = falsifies
	Method       string // e.g. "executed_test", "deterministic_check"
	Reproducible bool
	Summary      string
}

// ToolDefinition is a registered tool: its schema plus its body.
type ToolDefinition struct {
	Schema ToolSchema
	// Concurrent reports whether this tool is safe to run in parallel with others.
	Concurrent bool
	// TimeoutMs bounds one execution (0 = runtime default).
	TimeoutMs int
	// Execute runs the tool. args is the raw JSON arguments the model produced.
	Execute func(rc ToolRunContext, args []byte) (ToolResult, error)
}

// ToolExecutionInput is one call to run.
type ToolExecutionInput struct {
	Ctx       context.Context
	CallID    string
	Name      string
	Args      []byte
	Initiator string
}

// ToolRuntime is the ctx.Get(KeyTools) service.
type ToolRuntime interface {
	// Register adds a tool and returns its disposer (registrations are effects).
	Register(def ToolDefinition) (dispose func(), err error)
	// Execute runs one call through the pipeline (pre/guard/execute/post).
	Execute(in ToolExecutionInput) ToolResult
	// Schemas returns the model-facing schemas for all registered tools.
	Schemas() []ToolSchema
	// Concurrent reports whether a named tool may run in a parallel group.
	Concurrent(name string) bool
}

// --- explicit middleware chains (no generic event bus) -----------------------

// ToolMiddleware wraps tool dispatch (timeout / retry / guard / metrics). Each
// wraps the next in the chain; return without calling next to short-circuit.
type ToolMiddleware func(next ToolExecFn) ToolExecFn

// ToolExecFn is the dispatch function a ToolMiddleware wraps.
type ToolExecFn func(in ToolExecutionInput) ToolResult
