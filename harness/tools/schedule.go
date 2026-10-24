package tools

import (
	"context"
	"sync"

	"github.com/websines/structor-harness/harness/hx"
)

// ExecuteBatch runs a turn's tool calls with parallelism where it is safe, but
// COMMITS RESULTS IN INPUT (MODEL) ORDER regardless of how dispatch overlaps.
//
// It walks calls left to right, gathering a maximal run of Concurrent tools into
// one parallel group (bounded by maxParallel goroutines) and running each
// exclusive (non-Concurrent) tool alone. An exclusive call is a barrier: every
// earlier call has completed before it starts, and no later call starts until it
// finishes. Because each result is written to results[i] at the call's own input
// index, the returned slice is always in model order — the overlap is invisible
// to the log, so replay stays valid.
//
// If ctx is cancelled, calls that have not been dispatched get a synthetic error
// ToolResult (never a zero-value gap), so the log still has one result per call.
func (r *Runtime) ExecuteBatch(ctx context.Context, calls []hx.ToolExecutionInput) []hx.ToolResult {
	results := make([]hx.ToolResult, len(calls))

	for i := 0; i < len(calls); {
		if err := ctxErr(ctx); err != nil {
			for ; i < len(calls); i++ {
				results[i] = abortedResult(calls[i], err)
			}
			break
		}

		if r.Concurrent(calls[i].Name) {
			// Extend the parallel group over consecutive concurrent calls.
			j := i
			for j < len(calls) && r.Concurrent(calls[j].Name) {
				j++
			}
			r.runParallel(ctx, calls, results, i, j)
			i = j
			continue
		}

		// Exclusive call: runs alone as a barrier.
		results[i] = r.runOne(ctx, calls[i])
		i++
	}
	return results
}

// runParallel executes calls[start:end] concurrently, bounded by maxParallel,
// each writing to its own results index (disjoint indices — no shared writes).
func (r *Runtime) runParallel(ctx context.Context, calls []hx.ToolExecutionInput, results []hx.ToolResult, start, end int) {
	n := r.maxParallel
	if n < 1 {
		n = 1
	}
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for idx := start; idx < end; idx++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = r.runOne(ctx, calls[i])
		}(idx)
	}
	wg.Wait()
}

// runOne dispatches a single call, honoring the batch-level ctx as an additional
// abort signal on top of the call's own Ctx. A batch already cancelled yields a
// synthetic aborted result rather than executing the tool.
func (r *Runtime) runOne(ctx context.Context, call hx.ToolExecutionInput) hx.ToolResult {
	if err := ctxErr(ctx); err != nil {
		return abortedResult(call, err)
	}
	mctx, cancel := mergeCtx(ctx, call.Ctx)
	defer cancel()
	call.Ctx = mctx
	return r.Execute(call)
}

// mergeCtx returns a context done when either a or b is done. When b is nil it
// returns a unchanged. The cancel func must be called to release resources.
func mergeCtx(a, b context.Context) (context.Context, context.CancelFunc) {
	if b == nil {
		return a, func() {}
	}
	ctx, cancel := context.WithCancel(a)
	stop := make(chan struct{})
	go func() {
		select {
		case <-b.Done():
			cancel()
		case <-stop:
		}
	}()
	return ctx, func() { cancel(); close(stop) }
}

// ctxErr reports ctx's cancellation error, tolerating a nil ctx.
func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// abortedResult is the synthetic error result for a call not run because the
// batch was cancelled — so the log keeps one result per call and replay is valid.
func abortedResult(call hx.ToolExecutionInput, err error) hx.ToolResult {
	return errResult("tools: call %q (%s) aborted: %v", call.CallID, call.Name, err)
}
