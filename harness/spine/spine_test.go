package spine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/llm"
	"github.com/websines/structor-harness/harness/session"
	"github.com/websines/structor-harness/harness/spine"
	"github.com/websines/structor-harness/record"
	"github.com/websines/structor-harness/runtime"
)

// TestOneTurnOnCordis drives ONE real agent turn through the REAL harness spine
// (llm, session, tools, prompt, agents, loop) mounted as cordis components on one
// Registry, with a scripted fake model adapter for determinism. It is H1: proof
// that the loop→model→tool→result→next-step cycle runs end to end on the record
// and on the cordis runtime, not just against in-package fakes.
func TestOneTurnOnCordis(t *testing.T) {
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// The model's two calls: (1) a line of text + a tool call to "answer";
	// (2) after seeing the tool result, a final line and a plain stop.
	fake := &llm.FakeAdapter{Script: [][]hx.StreamChunk{
		{
			{Kind: hx.ChunkTextDelta, BlockIndex: 0, Delta: "Let me compute that."},
			{Kind: hx.ChunkToolCallDelta, BlockIndex: 1, ToolCallID: "tc1", ToolName: "answer", ArgsDelta: `{"q":"6*7"}`},
			{Kind: hx.ChunkFinish, Finish: hx.FinishToolCalls},
		},
		{
			{Kind: hx.ChunkTextDelta, BlockIndex: 0, Delta: "The answer is 42."},
			{Kind: hx.ChunkFinish, Finish: hx.FinishStop},
		},
	}}

	// One tool the model can call. It ignores its args and returns the answer;
	// what matters is that its result becomes a fact the next model call sees.
	answer := hx.ToolDefinition{
		Schema: hx.ToolSchema{
			Name:        "answer",
			Description: "compute an arithmetic answer",
			Parameters:  map[string]any{"type": "object"},
		},
		Execute: func(rc hx.ToolRunContext, args []byte) (hx.ToolResult, error) {
			return hx.ToolResult{Value: []byte(`"42"`)}, nil
		},
	}

	reg := runtime.New()
	if err := spine.Build(reg, spine.Options{
		CAS:      cas,
		Adapter:  fake,
		Actor:    session.Config{PrincipalID: "principal:test-agent", Commit: "c0ffee"},
		Tools:    []hx.ToolDefinition{answer},
		Sections: []spine.Section{{Order: 0, Text: "You are a Forge agent."}},
	}); err != nil {
		t.Fatal(err)
	}

	// The whole tree must have come up on cordis: the loop activates only after
	// all five services provide their keys (reactive coeffects).
	factory, err := spine.Factory(reg)
	if err != nil {
		t.Fatalf("harness did not come up on cordis: %v", err)
	}

	handle, err := factory.Create(hx.AgentConfig{
		Provider:    fake.Route(), // "fake"
		Model:       "test-model",
		SessionID:   "s1",
		PrincipalID: "principal:test-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Dispose()

	// Drive one turn: the user asks, the agent runs to idle.
	handle.Agent().Followup("what is 6*7?")
	if err := handle.Agent().WhenIdle(context.Background()); err != nil {
		t.Fatalf("WhenIdle: %v", err)
	}

	// Read the session log back off the record and assert the full cycle happened.
	store, err := spine.Store(reg)
	if err != nil {
		t.Fatal(err)
	}
	sess, ok := store.Get("s1")
	if !ok {
		t.Fatal("session s1 not found in the store")
	}

	// 1. Every event is a fact in the record (non-empty content-address FactID).
	events := sess.Events()
	var kinds []hx.EventKind
	sawToolResult := false
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
		if ev.FactID == "" {
			t.Fatalf("event %s (seq %d) has no FactID — not recorded", ev.Kind, ev.Seq)
		}
		if ev.PrincipalID != "principal:test-agent" {
			t.Fatalf("event %s not actor-stamped: %q", ev.Kind, ev.PrincipalID)
		}
		if ev.Kind == hx.EvToolResult {
			sawToolResult = true
		}
	}
	if !sawToolResult {
		t.Fatal("no tool/result event — the tool was never dispatched")
	}

	// 2. The turn is well-formed: starts with turn/start, ends with turn/end,
	//    and the tool/call precedes its tool/result.
	if kinds[0] != hx.EvTurnStart {
		t.Fatalf("log does not start with turn/start: %v", kinds[0])
	}
	if kinds[len(kinds)-1] != hx.EvTurnEnd {
		t.Fatalf("log does not end with turn/end: %v", kinds[len(kinds)-1])
	}
	if idx(kinds, hx.EvToolCall) >= idx(kinds, hx.EvToolResult) {
		t.Fatal("tool/call must precede tool/result")
	}

	// 3. The SECOND model call saw the tool result: the derived message history
	//    contains the tool's answer, and the final assistant message states it.
	msgs := sess.DeriveMessages()
	if !anyMessageContains(msgs, "42") {
		t.Fatal("derived history does not contain the tool result — the model never saw it")
	}
	last := msgs[len(msgs)-1]
	if last.Role != hx.RoleAssistant || !blocksContain(last.Content, "The answer is 42.") {
		t.Fatalf("final message is not the model's answer: %+v", last)
	}

	// 4. Two model calls means two assistant messages (step 1 with the tool call,
	//    step 2 with the answer) — the loop drove a follow-on step on its own.
	if n := count(kinds, hx.EvAssistantMessage); n != 2 {
		t.Fatalf("expected 2 assistant messages (tool-call step + answer step), got %d", n)
	}
}

func idx(ks []hx.EventKind, k hx.EventKind) int {
	for i, x := range ks {
		if x == k {
			return i
		}
	}
	return -1
}

func count(ks []hx.EventKind, k hx.EventKind) (n int) {
	for _, x := range ks {
		if x == k {
			n++
		}
	}
	return n
}

func anyMessageContains(msgs []hx.Message, s string) bool {
	for _, m := range msgs {
		if blocksContain(m.Content, s) {
			return true
		}
	}
	return false
}

func blocksContain(blocks []hx.ContentBlock, s string) bool {
	for _, b := range blocks {
		if strings.Contains(b.Text, s) {
			return true
		}
	}
	return false
}
