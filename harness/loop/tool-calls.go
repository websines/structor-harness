package loop

import (
	"context"

	"github.com/websines/structor-harness/harness/hx"
)

// batchRuntime is the OPTIONAL interface a ToolRuntime may also satisfy to run a
// group of calls at once. The loop type-asserts to it; when absent, it falls back
// to sequential execution. Either way results are committed to the log in model
// order.
type batchRuntime interface {
	ExecuteBatch(context.Context, []hx.ToolExecutionInput) []hx.ToolResult
}

// executeToolCalls dispatches the tool-call blocks the model returned. For each
// call it appends a tool/call event BEFORE dispatch and a tool/result event AFTER,
// committing results in model order. A result's AdditionalContext is staged into
// the inbox for the next step (as inject — extra context, no wake). It returns
// whether any result concluded the turn.
//
// When every call is concurrency-safe and the runtime offers ExecuteBatch, the
// group runs as one batch (all tool/call events first, then the batch, then all
// tool/result events in order); otherwise calls run one at a time through the Tool
// middleware chain.
func (a *ReactLoopAgent) executeToolCalls(ctx context.Context, calls []hx.ContentBlock) bool {
	tools, err := resolve[hx.ToolRuntime](a.svc, hx.KeyTools)
	if err != nil {
		a.fail(err)
		return true // cannot dispatch → conclude the turn rather than loop forever
	}

	if bt, ok := tools.(batchRuntime); ok && len(calls) > 1 && a.allConcurrent(tools, calls) {
		ins := make([]hx.ToolExecutionInput, len(calls))
		for i, c := range calls {
			a.emit(hx.EvToolCall, encode(c), false)
			ins[i] = a.toolInput(ctx, c)
		}
		results := bt.ExecuteBatch(ctx, ins)
		concluded := false
		for i, c := range calls {
			var res hx.ToolResult
			if i < len(results) {
				res = results[i]
			}
			a.emit(hx.EvToolResult, encode(a.toolResultMessage(c, res)), true)
			a.stage(res)
			if res.ConcludesTurn {
				concluded = true
			}
		}
		return concluded
	}

	concluded := false
	for _, c := range calls {
		a.emit(hx.EvToolCall, encode(c), false)
		res := a.runTool(a.toolInput(ctx, c), tools)
		a.emit(hx.EvToolResult, encode(a.toolResultMessage(c, res)), true)
		a.stage(res)
		if res.ConcludesTurn {
			concluded = true
		}
	}
	return concluded
}

// runTool dispatches one call through the Tool middleware chain (timeout / retry /
// guard). An empty chain calls the runtime's Execute directly.
func (a *ReactLoopAgent) runTool(in hx.ToolExecutionInput, tools hx.ToolRuntime) hx.ToolResult {
	fn := hx.ToolExecFn(func(in hx.ToolExecutionInput) hx.ToolResult {
		return tools.Execute(in)
	})
	for i := len(a.hooks.Tool) - 1; i >= 0; i-- {
		fn = a.hooks.Tool[i](fn)
	}
	return fn(in)
}

// toolInput builds the execution input for one tool-call block, stamped with the
// initiating actor.
func (a *ReactLoopAgent) toolInput(ctx context.Context, c hx.ContentBlock) hx.ToolExecutionInput {
	return hx.ToolExecutionInput{
		Ctx:       ctx,
		CallID:    c.ToolCallID,
		Name:      c.ToolName,
		Args:      c.ToolArgs,
		Initiator: a.initiator(),
	}
}

// toolResultMessage projects a tool result into the tool-role message the next
// model call will see. The model-visible JSON value rides in the result block's
// text; richer blocks (large parts referenced by CAS digest) follow it.
func (a *ReactLoopAgent) toolResultMessage(call hx.ContentBlock, res hx.ToolResult) hx.Message {
	block := hx.ContentBlock{
		Kind:       hx.BlockToolResult,
		ToolCallID: call.ToolCallID,
		ToolName:   call.ToolName,
		IsError:    res.IsError,
		Text:       string(res.Value),
	}
	content := append([]hx.ContentBlock{block}, res.Content...)
	return hx.Message{ID: a.nextID("tool"), Role: hx.RoleTool, Content: content}
}

// stage pushes a result's AdditionalContext into the inbox for the next step.
func (a *ReactLoopAgent) stage(res hx.ToolResult) {
	for _, m := range res.AdditionalContext {
		a.inbox.push(hx.DeliverInject, m)
	}
}

// allConcurrent reports whether every call in the group is safe to run in parallel.
func (a *ReactLoopAgent) allConcurrent(tools hx.ToolRuntime, calls []hx.ContentBlock) bool {
	for _, c := range calls {
		if !tools.Concurrent(c.ToolName) {
			return false
		}
	}
	return true
}
