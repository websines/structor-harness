package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/websines/structor-harness/harness/hx"
)

// echoDef returns a tool that echoes its raw args back as the result Value.
func echoDef(name string, concurrent bool) hx.ToolDefinition {
	return hx.ToolDefinition{
		Schema:     hx.ToolSchema{Name: name, Description: "echoes args"},
		Concurrent: concurrent,
		Execute: func(rc hx.ToolRunContext, args []byte) (hx.ToolResult, error) {
			return hx.ToolResult{Value: args}, nil
		},
	}
}

func mustRegister(t *testing.T, r *Runtime, def hx.ToolDefinition) func() {
	t.Helper()
	dispose, err := r.Register(def)
	if err != nil {
		t.Fatalf("Register(%s): %v", def.Schema.Name, err)
	}
	return dispose
}

func TestRegisterAndExecuteEcho(t *testing.T) {
	r := New(Config{})
	mustRegister(t, r, echoDef("echo", false))

	args := []byte(`{"msg":"hi"}`)
	res := r.Execute(hx.ToolExecutionInput{CallID: "c1", Name: "echo", Args: args})
	if res.IsError {
		t.Fatalf("echo returned error result: %s", res.Value)
	}
	if !bytes.Equal(res.Value, args) {
		t.Fatalf("echo Value = %s, want %s", res.Value, args)
	}
}

func TestUnknownToolIsErrorResult(t *testing.T) {
	r := New(Config{})
	res := r.Execute(hx.ToolExecutionInput{CallID: "c1", Name: "nope"})
	if !res.IsError {
		t.Fatalf("unknown tool should be an error result")
	}
}

func TestDuplicateAndReservedRejected(t *testing.T) {
	r := New(Config{})
	mustRegister(t, r, echoDef("echo", false))

	if _, err := r.Register(echoDef("echo", false)); err == nil {
		t.Fatalf("duplicate name should be rejected")
	}
	if _, err := r.Register(echoDef(reservedRunCode, false)); err == nil {
		t.Fatalf("reserved name %q should be rejected", reservedRunCode)
	}
	if _, err := r.Register(hx.ToolDefinition{Schema: hx.ToolSchema{Name: "noexec"}}); err == nil {
		t.Fatalf("tool with no Execute body should be rejected")
	}
}

func TestDisposerUnregisters(t *testing.T) {
	r := New(Config{})
	dispose := mustRegister(t, r, echoDef("echo", true))

	if len(r.Schemas()) != 1 {
		t.Fatalf("expected 1 schema before dispose")
	}
	if !r.Concurrent("echo") {
		t.Fatalf("echo should report concurrent")
	}

	dispose()

	if len(r.Schemas()) != 0 {
		t.Fatalf("expected 0 schemas after dispose")
	}
	if r.Concurrent("echo") {
		t.Fatalf("disposed tool should not report concurrent")
	}
	res := r.Execute(hx.ToolExecutionInput{Name: "echo"})
	if !res.IsError {
		t.Fatalf("executing a disposed tool should be an error result")
	}

	// Disposer must be idempotent and must not remove a re-registered same name.
	dispose()
	mustRegister(t, r, echoDef("echo", false))
	dispose() // stale disposer: must not remove the new registration
	if len(r.Schemas()) != 1 {
		t.Fatalf("stale disposer removed a fresh registration")
	}
}

func TestSchemasSortedByName(t *testing.T) {
	r := New(Config{})
	mustRegister(t, r, echoDef("gamma", false))
	mustRegister(t, r, echoDef("alpha", false))
	mustRegister(t, r, echoDef("beta", false))

	got := r.Schemas()
	want := []string{"alpha", "beta", "gamma"}
	if len(got) != len(want) {
		t.Fatalf("Schemas len = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("Schemas[%d] = %q, want %q", i, got[i].Name, w)
		}
	}
}

func TestMiddlewareShortCircuit(t *testing.T) {
	r := New(Config{})
	called := false
	mustRegister(t, r, hx.ToolDefinition{
		Schema: hx.ToolSchema{Name: "guarded"},
		Execute: func(rc hx.ToolRunContext, args []byte) (hx.ToolResult, error) {
			called = true
			return hx.ToolResult{Value: []byte(`"ran"`)}, nil
		},
	})

	// A deny guard that short-circuits without calling next.
	dispose := r.Use(func(next hx.ToolExecFn) hx.ToolExecFn {
		return func(in hx.ToolExecutionInput) hx.ToolResult {
			if in.Initiator == "blocked" {
				return hx.ToolResult{IsError: true, Value: []byte(`{"error":"denied"}`)}
			}
			return next(in)
		}
	})

	res := r.Execute(hx.ToolExecutionInput{Name: "guarded", Initiator: "blocked"})
	if !res.IsError {
		t.Fatalf("guard should have produced an error result")
	}
	if called {
		t.Fatalf("guard short-circuit should NOT have run the tool body")
	}

	// Same call after disposing the guard runs the body.
	dispose()
	res = r.Execute(hx.ToolExecutionInput{Name: "guarded", Initiator: "blocked"})
	if res.IsError || !called {
		t.Fatalf("after disposing guard, tool body should run; called=%v err=%v", called, res.IsError)
	}
}

func TestMiddlewareOrderOutermostFirst(t *testing.T) {
	r := New(Config{})
	mustRegister(t, r, echoDef("echo", false))

	var order []string
	mk := func(tag string) hx.ToolMiddleware {
		return func(next hx.ToolExecFn) hx.ToolExecFn {
			return func(in hx.ToolExecutionInput) hx.ToolResult {
				order = append(order, tag)
				return next(in)
			}
		}
	}
	r.Use(mk("first"))
	r.Use(mk("second"))

	r.Execute(hx.ToolExecutionInput{Name: "echo", Args: []byte(`{}`)})
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Fatalf("middleware order = %v, want [first second] (first registered = outermost)", order)
	}
}

func TestTimeoutReturnsErrorResult(t *testing.T) {
	r := New(Config{})
	mustRegister(t, r, hx.ToolDefinition{
		Schema:    hx.ToolSchema{Name: "blocker"},
		TimeoutMs: 20,
		Execute: func(rc hx.ToolRunContext, args []byte) (hx.ToolResult, error) {
			<-rc.Ctx.Done() // cooperative: unblock when the per-call timeout fires
			return hx.ToolResult{}, rc.Ctx.Err()
		},
	})

	done := make(chan hx.ToolResult, 1)
	go func() { done <- r.Execute(hx.ToolExecutionInput{Name: "blocker"}) }()

	select {
	case res := <-done:
		if !res.IsError {
			t.Fatalf("timed-out tool should return an error result")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Execute did not return after the tool's timeout")
	}
}

func TestEvidenceHintRoundTrips(t *testing.T) {
	r := New(Config{})
	hint := &hx.EvidenceHint{
		ObligationID: "OBL-7",
		Supports:     true,
		Method:       "executed_test",
		Reproducible: true,
		Summary:      "suite passed",
	}
	mustRegister(t, r, hx.ToolDefinition{
		Schema: hx.ToolSchema{Name: "prover"},
		Execute: func(rc hx.ToolRunContext, args []byte) (hx.ToolResult, error) {
			return hx.ToolResult{Value: []byte(`{}`), EvidenceHint: hint}, nil
		},
	})

	res := r.Execute(hx.ToolExecutionInput{Name: "prover"})
	if res.EvidenceHint == nil {
		t.Fatalf("EvidenceHint was dropped")
	}
	if *res.EvidenceHint != *hint {
		t.Fatalf("EvidenceHint = %+v, want %+v", *res.EvidenceHint, *hint)
	}
}

func TestInvalidJSONArgsRejected(t *testing.T) {
	r := New(Config{})
	mustRegister(t, r, echoDef("echo", false))
	res := r.Execute(hx.ToolExecutionInput{Name: "echo", Args: []byte(`{not json`)})
	if !res.IsError {
		t.Fatalf("invalid JSON args should be an error result")
	}
}

// idTool returns a tool that echoes its own CallID as the result Value, after an
// optional delay — used to prove batch ordering survives overlapping dispatch.
func idTool(name string, concurrent bool, delay time.Duration) hx.ToolDefinition {
	return hx.ToolDefinition{
		Schema:     hx.ToolSchema{Name: name},
		Concurrent: concurrent,
		Execute: func(rc hx.ToolRunContext, args []byte) (hx.ToolResult, error) {
			if delay > 0 {
				time.Sleep(delay)
			}
			v, _ := json.Marshal(rc.CallID)
			return hx.ToolResult{Value: v}, nil
		},
	}
}

func TestExecuteBatchPreservesModelOrder(t *testing.T) {
	r := New(Config{MaxParallel: 4})
	// Concurrent tool with an INVERTED delay: earlier calls finish later, so a
	// completion-order commit would scramble the results. Model order must win.
	mustRegister(t, r, hx.ToolDefinition{
		Schema:     hx.ToolSchema{Name: "cc"},
		Concurrent: true,
		Execute: func(rc hx.ToolRunContext, args []byte) (hx.ToolResult, error) {
			var d int
			fmt.Sscanf(rc.CallID, "c%d", &d)
			time.Sleep(time.Duration(5-d) * 10 * time.Millisecond)
			v, _ := json.Marshal(rc.CallID)
			return hx.ToolResult{Value: v}, nil
		},
	})
	mustRegister(t, r, idTool("ex", false, 0)) // exclusive barrier

	calls := []hx.ToolExecutionInput{
		{CallID: "c0", Name: "cc", Args: []byte(`{}`)},
		{CallID: "c1", Name: "cc", Args: []byte(`{}`)},
		{CallID: "e2", Name: "ex", Args: []byte(`{}`)},
		{CallID: "c3", Name: "cc", Args: []byte(`{}`)},
		{CallID: "c4", Name: "cc", Args: []byte(`{}`)},
	}
	results := r.ExecuteBatch(context.Background(), calls)

	if len(results) != len(calls) {
		t.Fatalf("results len = %d, want %d", len(results), len(calls))
	}
	for i, call := range calls {
		want, _ := json.Marshal(call.CallID)
		if results[i].IsError {
			t.Fatalf("results[%d] error: %s", i, results[i].Value)
		}
		if !bytes.Equal(results[i].Value, want) {
			t.Fatalf("results[%d].Value = %s, want %s (model order violated)", i, results[i].Value, want)
		}
	}
}

func TestExecuteBatchAbortSynthesizesResults(t *testing.T) {
	r := New(Config{})
	mustRegister(t, r, idTool("ex", false, 0))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before dispatch

	calls := []hx.ToolExecutionInput{
		{CallID: "a", Name: "ex", Args: []byte(`{}`)},
		{CallID: "b", Name: "ex", Args: []byte(`{}`)},
	}
	results := r.ExecuteBatch(ctx, calls)
	if len(results) != 2 {
		t.Fatalf("expected 2 synthetic results, got %d", len(results))
	}
	for i, res := range results {
		if !res.IsError {
			t.Fatalf("results[%d] should be a synthetic aborted error", i)
		}
	}
}

func TestConcurrentRegisterExecuteRace(t *testing.T) {
	r := New(Config{MaxParallel: 8})
	mustRegister(t, r, echoDef("echo", true))

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			name := fmt.Sprintf("t%d", n)
			dispose, err := r.Register(echoDef(name, false))
			if err != nil {
				t.Errorf("Register(%s): %v", name, err)
				return
			}
			r.Execute(hx.ToolExecutionInput{Name: "echo", Args: []byte(`{}`)})
			_ = r.Schemas()
			dispose()
		}(i)
	}
	wg.Wait()
}
