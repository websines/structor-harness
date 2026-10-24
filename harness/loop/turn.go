package loop

import (
	"context"
	"errors"

	"github.com/websines/structor-harness/harness/hx"
)

// turn runs one turn to completion and reports whether another is owed. A turn is
// a turn/start marker, then a sequence of STEPS, then a turn/end marker:
//
//	turn/start
//	  ├─ preStep  (claim inbox batch, assemble prompt, run PreStep middleware)
//	  ├─ step/start + one user/message per claimed message
//	  ├─ step     (one model call; may dispatch tool calls, which owe a next step)
//	  └─ step/end
//	  … repeat while a step is owed (tool calls executed, turn not concluded) …
//	turn/end
//
// The first step claims followups too (a followup IS the turn's input); follow-on
// steps leave followups queued for the next turn. When a PreStep middleware
// rejects, or nothing is owed and no batch remains, the turn stops stepping.
func (a *ReactLoopAgent) turn() bool {
	ctx := a.activityCtx()
	a.emit(hx.EvTurnStart, nil, false)

	owesStep := false
	firstStep := true
	steps := 0
	for {
		batch, reject := a.preStep(ctx, firstStep)
		if reject {
			break
		}
		// Nothing to say and nothing owed from a prior step → the turn is done.
		if len(batch) == 0 && !owesStep {
			break
		}

		a.emit(hx.EvStepStart, nil, false)
		for _, m := range batch {
			if m.ID == "" {
				m.ID = a.nextID("user")
			}
			a.emit(hx.EvUserMessage, encode(m), true)
		}

		owesStep = a.step(ctx)
		steps++

		a.emit(hx.EvStepEnd, nil, false)
		firstStep = false
		if !owesStep {
			break
		}
		if a.maxSteps > 0 && steps >= a.maxSteps {
			a.fail(&hx.TurnError{Reason: "step_limit", Cause: hx.ErrStepLimit})
			break
		}
	}

	outcome := hx.TurnOutcome{Reason: "completed", Steps: steps}
	a.mu.Lock()
	err := a.lastErr
	a.mu.Unlock()
	if err != nil {
		outcome.Reason = "failed"
		outcome.Error = err.Error()
		if terminal, ok := errors.AsType[*hx.TurnError](err); ok {
			outcome.Reason = terminal.Reason
		}
	}
	a.emit(hx.EvTurnEnd, encode(outcome), false)
	return a.turnOwed()
}

// preStep claims this step's message batch, assembles the system prompt (stored
// for step to build its request from), and runs the PreStep middleware chain. It
// returns the (possibly rewritten) batch to enter the step with, or reject=true to
// end the turn. A service-resolution failure is a clean reject, not a panic.
func (a *ReactLoopAgent) preStep(ctx context.Context, firstStep bool) ([]hx.Message, bool) {
	// This activity owns the messages it claims even if cancellation won before
	// its first step. Leaving them owed would restart the same canceled task.
	claimed := a.inbox.claim(firstStep)
	if err := ctx.Err(); err != nil {
		for _, msg := range claimed {
			a.emit(hx.EvUserMessage, encode(msg), true)
		}
		a.fail(&hx.TurnError{Reason: "cancelled", Cause: err})
		return nil, true
	}
	// Capture initial intent before any compactor can replace the user message.
	// Followups remain in the log; starting another task requires another agent.
	if a.task == "" {
		for _, msg := range claimed {
			if msg.Role != hx.RoleUser {
				continue
			}
			for _, block := range msg.Content {
				if block.Kind == hx.BlockText {
					a.task += block.Text + "\n"
				}
			}
		}
	}

	prompt, err := resolve[hx.SystemPrompt](a.svc, hx.KeyPrompt)
	if err != nil {
		a.fail(err)
		return nil, true
	}
	var sys, prefix string
	if cached, ok := prompt.(interface {
		AssembleWithCachePrefix(map[string]string) (string, string, error)
	}); ok {
		sys, prefix, err = cached.AssembleWithCachePrefix(nil)
	} else {
		sys, err = prompt.Assemble(nil)
	}
	if err != nil {
		a.fail(err)
		return nil, true
	}
	a.system = sys
	a.systemPrefix = prefix
	if a.task != "" {
		a.system += "\n\nCurrent authorized task (retain this intent across compaction):\n" + a.task
	}

	dec := a.runPreStep(ctx, claimed)
	if dec.Reject {
		return nil, true
	}
	return dec.Enter, false
}

// runPreStep threads the claimed batch through the PreStep middleware chain. The
// base decision enters the step with exactly what was claimed; each middleware may
// rewrite the batch, inject context, or reject.
func (a *ReactLoopAgent) runPreStep(ctx context.Context, claimed []hx.Message) hx.PreStepDecision {
	fn := hx.PreStepFn(func(context.Context) hx.PreStepDecision {
		return hx.PreStepDecision{Enter: claimed}
	})
	for i := len(a.hooks.PreStep) - 1; i >= 0; i-- {
		fn = a.hooks.PreStep[i](fn)
	}
	return fn(ctx)
}
