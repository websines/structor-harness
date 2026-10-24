package loop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/prompt"
)

func TestLoopCachePolicyRetainsIntentAndAuditsEveryRequest(t *testing.T) {
	p := prompt.New()
	p.Section(0, "shared instructions")
	p.Context(10, func() (string, error) { return "fresh verified facts", nil })
	model := &fakeLLM{scripts: [][]hx.StreamChunk{toolCallScript(), stopScript()}}
	store := &fakeSessionStore{}
	factory := NewFactory(mapServices{
		hx.KeyLLM: model, hx.KeyTools: &fakeTools{result: hx.ToolResult{Value: []byte(`"42"`)}},
		hx.KeyPrompt: p, hx.KeySession: store, hx.KeyAgents: &fakeRegistry{},
	})
	handle, err := factory.Create(hx.AgentConfig{Task: "authorized task", Provider: "test", Model: "test", SessionID: "cache", MaxSteps: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Dispose()
	handle.Agent().Followup("authorized task")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := handle.Agent().WhenIdle(ctx); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		opts, ok := model.callAt(i)
		if !ok || opts.PromptCache == nil || !opts.PromptCache.ReuseHistory ||
			opts.PromptCache.SystemPrefix != "shared instructions" || opts.PromptCache.Task != "authorized task" ||
			!strings.Contains(opts.System, "fresh verified facts") {
			t.Fatal("native cache inputs lost or current context omitted in a tool step")
		}
	}
	sess, ok := store.Get("cache")
	if !ok {
		t.Fatal("session missing")
	}
	headers := 0
	for _, event := range sess.Events() {
		if event.Kind != hx.EvRequestHeader {
			continue
		}
		headers++
		var header hx.RequestHeader
		if err := json.Unmarshal(event.Data, &header); err != nil {
			t.Fatal(err)
		}
		if header.PromptCache == nil || header.PromptCache.SystemPrefix != "shared instructions" ||
			header.PromptCache.Task != "authorized task" || !strings.Contains(header.System, "authorized task") {
			t.Fatal("wire cache rearrangement cannot be reconstructed from the session header")
		}
	}
	if headers != 2 {
		t.Fatalf("expected both requests in audit, got %d", headers)
	}
}
