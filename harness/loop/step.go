package loop

import (
	"context"
	"fmt"

	"github.com/websines/structor-harness/harness/hx"
)

// step is one model call — the heart of a turn. It:
//
//  1. builds the request from the derived message history + assembled system
//     prompt + the tool schemas, then threads it through the Request middleware;
//  2. streams the response, appending EACH chunk as an assistant/chunk event (raw
//     replay fidelity) AND folding it into blocks with the in-package fold (never
//     buffering the whole response as one string);
//  3. on a terminal finish, creates the assistant message and appends it as
//     assistant/message;
//  4. filters the tool-call blocks and, if any, dispatches them.
//
// It returns whether another step is owed: true when tool calls ran and none
// concluded the turn (their results are now in the log for the next model call),
// false when the model stopped plainly, errored, or a tool concluded the turn.
func (a *ReactLoopAgent) step(ctx context.Context) bool {
	tools, err := resolve[hx.ToolRuntime](a.svc, hx.KeyTools)
	if err != nil {
		a.fail(err)
		return false
	}
	llm, err := resolve[hx.LlmRuntime](a.svc, hx.KeyLLM)
	if err != nil {
		a.fail(err)
		return false
	}

	// Bound the context before building the request: a compaction engine, when
	// present, supersedes the oldest span of history with a summary so the derived
	// view stays within budget (design §2.2 — the log stays whole, only the derived
	// view shrinks). Best-effort: if the service is absent or unusable, skip it.
	if c, cerr := a.svc.Get(hx.KeyCompaction); cerr == nil {
		if eng, ok := c.(interface {
			MaybeCompact(context.Context, hx.Session, int) (bool, error)
		}); ok {
			if _, err := eng.MaybeCompact(ctx, a.session, a.lastPromptTokens); err != nil {
				if cancelled := ctx.Err(); cancelled != nil {
					a.fail(&hx.TurnError{Reason: "cancelled", Cause: cancelled})
				} else {
					a.fail(&hx.TurnError{Reason: "compaction_error", Cause: err})
				}
				return false
			}
		}
	}

	base := hx.GenerateOptions{
		Provider:    a.cfg.Provider,
		Model:       a.cfg.Model,
		System:      a.system,
		Messages:    a.session.DeriveMessages(),
		Tools:       tools.Schemas(),
		PromptCache: &hx.PromptCache{SystemPrefix: a.systemPrefix, Task: a.task, ReuseHistory: true},
	}
	opts := a.runRequest(ctx, base)

	// Record the folded request header the call is reconstructed from (audit).
	a.emit(hx.EvRequestHeader, encode(hx.RequestHeader{
		Provider:    opts.Provider,
		Model:       opts.Model,
		System:      opts.System,
		PromptCache: opts.PromptCache,
	}), false)

	ch, err := llm.Stream(ctx, opts)
	if err != nil {
		if cancelled := ctx.Err(); cancelled != nil {
			a.fail(&hx.TurnError{Reason: "cancelled", Cause: cancelled})
			return false
		}
		a.fail(&hx.TurnError{Reason: "provider_error", Cause: err})
		return false
	}

	f := newFold()
	finish := hx.FinishError
	finishError := "provider stream ended without a successful finish"
	for chunk := range ch {
		a.emit(hx.EvAssistantChunk, encode(chunk), false)
		f.consume(chunk)
		if chunk.Kind == hx.ChunkUsage && chunk.Usage != nil {
			// The exact prompt size the model counted — feeds the next step's
			// compaction budget (an exact count beats the character estimate).
			a.lastPromptTokens = chunk.Usage.InputTokens
		}
		if chunk.Kind == hx.ChunkFinish {
			finish = chunk.Finish
			if chunk.Err != "" {
				finishError = chunk.Err
			}
		}
	}

	// Raw chunks remain in the audit log. Failed tool-call fragments must never
	// become replayable assistant messages with no matching result.
	if ctx.Err() != nil || finish == hx.FinishError || finish == hx.FinishAborted {
		if err := ctx.Err(); err != nil {
			a.fail(&hx.TurnError{Reason: "cancelled", Cause: err})
		} else {
			a.fail(&hx.TurnError{Reason: "provider_error", Cause: fmt.Errorf("%s: %s", finish, finishError)})
		}
		return false
	}
	blocks := f.result()
	if len(blocks) > 0 {
		msg := hx.Message{ID: a.nextID("assistant"), Role: hx.RoleAssistant, Content: blocks}
		a.emit(hx.EvAssistantMessage, encode(msg), true)
	}

	var calls []hx.ContentBlock
	for _, b := range blocks {
		if b.Kind == hx.BlockToolCall {
			calls = append(calls, b)
		}
	}
	if len(calls) == 0 {
		return false
	}

	concluded := a.executeToolCalls(ctx, calls)
	return !concluded
}

// runRequest threads the base GenerateOptions through the Request middleware chain
// (swap provider/model/effort, inject headers). An empty chain returns base.
func (a *ReactLoopAgent) runRequest(ctx context.Context, base hx.GenerateOptions) hx.GenerateOptions {
	fn := hx.RequestFn(func(_ context.Context, b hx.GenerateOptions) hx.GenerateOptions {
		return b
	})
	for i := len(a.hooks.Request) - 1; i >= 0; i-- {
		fn = a.hooks.Request[i](fn)
	}
	return fn(ctx, base)
}
