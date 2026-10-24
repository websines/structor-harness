// Package native drives one Forge agent execution. Admission and authority stay
// with the caller; desktop runs and harness experiments share this driver.
package native

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/spine"
	"github.com/websines/structor-harness/runtime"
)

type Request struct {
	Options   spine.Options
	Task      string
	Model     string
	Principal string
	SessionID string
	MaxSteps  int
	Timeout   time.Duration
}

type Result struct {
	SessionID string
	Text      string
	Events    []hx.SessionEvent
	Elapsed   time.Duration
}

// Run owns the registry and agent until all execution has joined. It returns the
// retained events even when the model or tools fail, so callers can audit failure.
func Run(ctx context.Context, req Request) (Result, error) {
	var result Result
	if ctx == nil || req.Options.Adapter == nil || req.Options.CAS == nil || req.Model == "" || req.Principal == "" || req.Timeout <= 0 {
		return result, errors.New("native: missing execution configuration")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	reg := runtime.New()
	defer reg.Remove("harness/subprocess")
	defer reg.Remove("harness/fs")
	defer reg.Remove("harness/llm")
	if err := spine.Build(reg, req.Options); err != nil {
		return result, fmt.Errorf("native: compose harness: %w", err)
	}
	factory, err := spine.Factory(reg)
	if err != nil {
		return result, err
	}
	store, err := spine.Store(reg)
	if err != nil {
		return result, err
	}
	result.SessionID = req.SessionID
	if result.SessionID == "" {
		result.SessionID = "native:" + rand.Text()
	}
	handle, err := factory.Create(hx.AgentConfig{Task: req.Task, Provider: req.Options.Adapter.Route(), Model: req.Model,
		SessionID: result.SessionID, PrincipalID: req.Principal, MaxSteps: req.MaxSteps})
	if err != nil {
		return result, err
	}
	defer handle.Dispose()
	started := time.Now()
	turnCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	handle.Agent().Followup(req.Task)
	runErr := handle.Agent().WhenIdle(turnCtx)
	if turnCtx.Err() != nil {
		handle.Agent().Cancel("run cancelled or turn deadline exceeded")
		joinCtx, stopJoin := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		joinErr := handle.Agent().WhenIdle(joinCtx)
		waitErr := joinCtx.Err()
		stopJoin()
		if waitErr != nil {
			return result, errors.Join(runErr, fmt.Errorf("native: driver did not join: %w", waitErr))
		}
		runErr = errors.Join(runErr, joinErr)
	}
	sess, ok := store.Get(result.SessionID)
	if !ok {
		return result, errors.Join(runErr, errors.New("native: session missing after turn"))
	}
	result.Events = sess.Events()
	result.Elapsed = time.Since(started)
	messages := sess.DeriveMessages()
	for i := len(messages) - 1; i >= 0 && result.Text == ""; i-- {
		if messages[i].Role == hx.RoleAssistant {
			for _, block := range messages[i].Content {
				if block.Kind == hx.BlockText {
					result.Text += block.Text
				}
			}
		}
	}
	return result, runErr
}
