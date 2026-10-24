package loop

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/llm"
	toolruntime "github.com/websines/structor-harness/harness/tools"
)

func TestCancelBeforeDriverStartsDoesNotRestartClaimedTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		model := &fakeLLM{scripts: [][]hx.StreamChunk{stopScript()}}
		a := newAgent(hx.AgentConfig{}, mapServices{hx.KeyLLM: model, hx.KeyTools: &fakeTools{}, hx.KeyPrompt: fakePrompt{}}, &fakeSession{})
		a.inbox.push(hx.DeliverFollowup, hx.Message{Role: hx.RoleUser,
			Content: []hx.ContentBlock{{Kind: hx.BlockText, Text: "cancel this turn before dispatch"}}})
		// Claim through the production wake path, but launch the driver only
		// after Cancel. Adjacent Followup/Cancel calls allow the scheduler to
		// dispatch first and do not establish the ordering this test requires.
		if !a.claimDriver() {
			t.Fatal("initial driver was not claimed")
		}
		a.Cancel("user stopped the turn")
		go a.kick()
		synctest.Wait()
		if err := a.WhenIdle(t.Context()); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled turn did not settle: %v", err)
		}
		if n := model.callCount(); n != 0 {
			t.Fatalf("canceled initial turn dispatched %d provider calls", n)
		}
		a.Followup("replacement turn")
		synctest.Wait()
		if err := a.WhenIdle(t.Context()); err != nil {
			t.Fatalf("replacement turn did not settle: %v", err)
		}
		if n := model.callCount(); n != 1 {
			t.Fatalf("replacement turn dispatched %d provider calls, want one", n)
		}
	})
}

func TestCancelJoinsRealHTTPStream(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"working\"}}]}\n\n")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("HTTP response does not support streaming")
			return
		}
		flusher.Flush()
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	defer server.Close()
	model := llm.NewRuntime()
	if _, err := model.RegisterAdapter(&llm.OpenAIAdapter{BaseURL: server.URL, HTTP: server.Client()}); err != nil {
		t.Fatal(err)
	}
	a := newAgent(hx.AgentConfig{Provider: "openai-compatible"}, mapServices{hx.KeyLLM: model, hx.KeyTools: &fakeTools{}, hx.KeyPrompt: fakePrompt{}}, &fakeSession{})
	a.Followup("stream until canceled")
	select {
	case <-started:
	case <-time.After(time.Second):
		a.Cancel("test cleanup")
		t.Fatal("HTTP stream did not start")
	}
	a.Cancel("test stop")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := a.WhenIdle(ctx); !errors.Is(err, context.Canceled) || ctx.Err() != nil {
		t.Fatalf("HTTP stream did not join with cancellation: %v (wait: %v)", err, ctx.Err())
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("upstream HTTP request remained active after cancellation")
	}
}

func TestReviewProductionConcurrentToolsOverlap(t *testing.T) {
	runtime := toolruntime.New(toolruntime.Config{})
	var started atomic.Int32
	var failed atomic.Bool
	both := make(chan struct{})
	_, err := runtime.Register(hx.ToolDefinition{Schema: hx.ToolSchema{Name: "read"}, Concurrent: true, Execute: func(rc hx.ToolRunContext, _ []byte) (hx.ToolResult, error) {
		if started.Add(1) == 2 {
			close(both)
		}
		select {
		case <-both:
		case <-time.After(100 * time.Millisecond):
			failed.Store(true)
		}
		return hx.ToolResult{Value: []byte(`"done"`)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	a := newAgent(hx.AgentConfig{}, mapServices{hx.KeyTools: runtime}, &fakeSession{})
	a.executeToolCalls(context.Background(), []hx.ContentBlock{
		{Kind: hx.BlockToolCall, ToolCallID: "first", ToolName: "read", ToolArgs: []byte(`{}`)},
		{Kind: hx.BlockToolCall, ToolCallID: "second", ToolName: "read", ToolArgs: []byte(`{}`)},
	})
	if failed.Load() {
		t.Fatal("concurrent tools did not overlap: first tool completed before second started")
	}
}

func TestReviewStepLimitWithInjectedContextBecomesIdle(t *testing.T) {
	model := &fakeLLM{scripts: [][]hx.StreamChunk{toolCallScript()}}
	tools := &fakeTools{result: hx.ToolResult{Value: []byte(`"done"`), AdditionalContext: []hx.Message{{Role: hx.RoleSystem, Content: []hx.ContentBlock{{Kind: hx.BlockText, Text: "Avoid repeating this tool call."}}}}}}
	agent := newAgent(hx.AgentConfig{MaxSteps: 1}, mapServices{hx.KeyLLM: model, hx.KeyTools: tools, hx.KeyPrompt: fakePrompt{}}, &fakeSession{})
	agent.Followup("inspect the file")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := agent.WhenIdle(ctx); !errors.Is(err, hx.ErrStepLimit) {
		t.Fatalf("bounded turn must quiesce and report its limit: %v", err)
	}
}

func TestReviewStreamFailureDoesNotLeaveUnansweredToolCall(t *testing.T) {
	model := &fakeLLM{scripts: [][]hx.StreamChunk{{
		{Kind: hx.ChunkToolCallDelta, ToolCallID: "read-1", ToolName: "read_file", ArgsDelta: `{}`},
		{Kind: hx.ChunkFinish, Finish: hx.FinishError, Err: "connection reset"},
	}, stopScript()}}
	store := &fakeSessionStore{}
	svc := mapServices{hx.KeyLLM: model, hx.KeyTools: &fakeTools{}, hx.KeyPrompt: fakePrompt{}, hx.KeySession: store, hx.KeyAgents: &fakeRegistry{}}
	h, err := NewFactory(svc).Create(hx.AgentConfig{SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Dispose()
	a := h.Agent()
	a.Followup("Inspect the file")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var terminal *hx.TurnError
	if err := a.WhenIdle(ctx); !errors.As(err, &terminal) || terminal.Reason != "provider_error" {
		t.Fatalf("failed stream must surface its terminal reason: %v", err)
	}
	a.Followup("Retry please")
	if err := a.WhenIdle(ctx); err != nil {
		t.Fatal(err)
	}
	request, ok := model.callAt(1)
	if !ok {
		t.Fatal("missing followup request")
	}
	pending := map[string]bool{}
	for _, msg := range request.Messages {
		for _, b := range msg.Content {
			if b.Kind == hx.BlockToolCall {
				pending[b.ToolCallID] = true
			}
			if b.Kind == hx.BlockToolResult {
				delete(pending, b.ToolCallID)
			}
		}
	}
	if len(pending) > 0 {
		t.Fatalf("followup replays unanswered calls from failed stream: %v", pending)
	}
}

func TestReviewTruncatedHTTPStreamDoesNotDispatchTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"read-1","function":{"name":"read_file","arguments":"{}"}}]}}]}`+"\n\n")
		// The body ends without finish_reason or [DONE], as with an interrupted upstream.
	}))
	defer server.Close()
	model := llm.NewRuntime()
	if _, err := model.RegisterAdapter(&llm.OpenAIAdapter{BaseURL: server.URL, HTTP: server.Client()}); err != nil {
		t.Fatal(err)
	}
	tools := &fakeTools{}
	a := newAgent(hx.AgentConfig{Provider: "openai-compatible", MaxSteps: 1}, mapServices{hx.KeyLLM: model, hx.KeyTools: tools, hx.KeyPrompt: fakePrompt{}}, &fakeSession{})
	a.Followup("read the file")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var terminal *hx.TurnError
	if err := a.WhenIdle(ctx); !errors.As(err, &terminal) || terminal.Reason != "provider_error" {
		t.Fatalf("truncated HTTP stream must surface a provider failure: %v", err)
	}
	if n := len(tools.ran()); n != 0 {
		t.Fatalf("dispatched %d tools from an HTTP stream with no terminal event", n)
	}
}
