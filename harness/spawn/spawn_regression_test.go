package spawn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/llm"
	"github.com/websines/structor-harness/harness/loop"
	"github.com/websines/structor-harness/harness/prompt"
	"github.com/websines/structor-harness/harness/shell"
	"github.com/websines/structor-harness/harness/subprocess"
	"github.com/websines/structor-harness/harness/tools"
)

type reviewServices map[string]any

func (s reviewServices) Get(key string) (any, error) {
	v, ok := s[key]
	if !ok {
		return nil, fmt.Errorf("missing %s", key)
	}
	return v, nil
}

func TestCanceledSpawnJoinsOwnedProcessTree(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	command := "sleep 30 & child=$!; printf '%s' \"$child\" > child.pid; wait"
	args, err := json.Marshal(map[string]any{"command": command, "timeout_ms": 3000})
	if err != nil {
		t.Fatal(err)
	}
	event, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "owned-process", "function": map[string]any{"name": "bash", "arguments": string(args)}}}}, "finish_reason": "tool_calls"}}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+string(event)+"\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	model := llm.NewRuntime()
	if _, err := model.RegisterAdapter(&llm.OpenAIAdapter{BaseURL: server.URL, HTTP: server.Client()}); err != nil {
		t.Fatal(err)
	}
	runtime := tools.New(tools.Config{})
	// Use the production shell and subprocess ownership path. Explicit degraded
	// mode keeps this lifecycle check independent of the host's sandbox facility.
	if _, err := runtime.Register(shell.BashToolAt(shell.New(subprocess.NewDegraded(nil), nil), dir)); err != nil {
		t.Fatal(err)
	}
	store := newStore(t)
	factory := &reviewCaptureFactory{base: loop.NewFactory(reviewServices{hx.KeySession: store, hx.KeyLLM: model, hx.KeyTools: runtime, hx.KeyPrompt: prompt.New()})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	def := Tool(Deps{Factory: factory, Sessions: store, Provider: "openai-compatible"})
	done := make(chan hx.ToolResult, 1)
	finished := false
	go func() {
		res, _ := def.Execute(hx.ToolRunContext{Ctx: ctx}, []byte(`{"subquery":"start the owned process","node_id":"owned-process-node"}`))
		done <- res
	}()
	t.Cleanup(func() {
		cancel()
		if !finished {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("test cleanup could not join the owned child")
			}
		}
	})
	var pid int
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for pid == 0 {
		if data, err := root.ReadFile("child.pid"); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		if pid != 0 {
			break
		}
		select {
		case <-tick.C:
		case res := <-done:
			finished = true
			t.Fatalf("spawn finished before owning a process: %s", res.Value)
		case <-deadline.C:
			t.Fatal("owned process did not start")
		}
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	cancel()
	select {
	case res := <-done:
		finished = true
		if !res.IsError {
			t.Fatal("canceled spawn reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("spawn did not join its owned process after cancellation")
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("owned descendant %d still exists after spawn returned", pid)
	}
	joined, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if err := factory.last.Agent().WhenIdle(joined); !errors.Is(err, context.Canceled) || joined.Err() != nil {
		t.Fatalf("child driver remained active after process cancellation: %v", err)
	}
}

type reviewStreamingModel struct {
	started   chan struct{}
	finishing chan struct{}
	release   chan struct{}
}

func (m *reviewStreamingModel) RegisterAdapter(hx.LlmAdapter) (func(), error) { return func() {}, nil }
func (m *reviewStreamingModel) Stream(ctx context.Context, _ hx.GenerateOptions) (<-chan hx.StreamChunk, error) {
	out := make(chan hx.StreamChunk)
	go func() {
		defer close(out)
		close(m.started)
		for {
			select {
			case out <- hx.StreamChunk{Kind: hx.ChunkTextDelta, Delta: "working "}:
			case <-ctx.Done():
				if m.finishing != nil {
					close(m.finishing)
					<-m.release
				}
				return
			}
		}
	}()
	return out, nil
}

type reviewCaptureFactory struct {
	base *loop.Factory
	last hx.AgentHandle
}

func (f *reviewCaptureFactory) Create(cfg hx.AgentConfig) (hx.AgentHandle, error) {
	h, err := f.base.Create(cfg)
	f.last = h
	return h, err
}

func TestReviewTimeoutCollectsOnlyAfterChildStopped(t *testing.T) {
	store := newStore(t)
	model := &reviewStreamingModel{started: make(chan struct{}), finishing: make(chan struct{}), release: make(chan struct{})}
	svc := reviewServices{hx.KeySession: store, hx.KeyLLM: model, hx.KeyTools: tools.New(tools.Config{}), hx.KeyPrompt: prompt.New()}
	factory := &reviewCaptureFactory{base: loop.NewFactory(svc)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	def := Tool(Deps{Factory: factory, Sessions: store})
	done := make(chan hx.ToolResult, 1)
	finished := false
	release := sync.OnceFunc(func() { close(model.release) })
	go func() {
		res, err := def.Execute(hx.ToolRunContext{Ctx: ctx}, []byte(`{"subquery":"check","node_id":"timeout-node"}`))
		if err != nil {
			res.IsError = true
		}
		done <- res
	}()
	t.Cleanup(func() {
		cancel()
		release()
		if !finished {
			select {
			case <-done:
				finished = true
			case <-time.After(5 * time.Second):
				t.Error("test cleanup could not join the spawn call")
			}
		}
		if finished && factory.last != nil {
			joined, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = factory.last.Agent().WhenIdle(joined)
			if joined.Err() != nil {
				t.Errorf("test cleanup could not join the child driver: %v", joined.Err())
			}
		}
	})
	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not start")
	}
	cancel()
	select {
	case <-model.finishing:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not observe cancellation")
	}
	select {
	case <-done:
		finished = true
		release()
		t.Fatal("spawn returned while the child's stream still owned work")
	case <-time.After(30 * time.Millisecond):
	}
	release()
	select {
	case res := <-done:
		finished = true
		if !res.IsError {
			t.Fatal("expected canceled wait")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("spawn did not return after child cleanup")
	}
	end, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := factory.last.Agent().WhenIdle(end); !errors.Is(err, context.Canceled) || end.Err() != nil {
		t.Fatalf("expected joined cancelled turn, got %v (wait: %v)", err, end.Err())
	}
}
