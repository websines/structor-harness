package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/runtime"
)

// --- in-package fakes for the hx interfaces the loop consumes -----------------

// mapServices is a `services` backed by a plain map — the loop's dependency seam
// exercised without a live runtime registry.
// fakeCompactor satisfies the loop's optional compaction interface with a no-op.
type fakeCompactor struct{}

func (fakeCompactor) MaybeCompact(context.Context, hx.Session, int) (bool, error) { return false, nil }

type mapServices map[string]any

func (m mapServices) Get(key string) (any, error) {
	if v, ok := m[key]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("no service %q", key)
}

// fakeSession records appended events and projects surface message events into an
// LLM history, exactly the projection the loop relies on to feed the next step.
type fakeSession struct {
	mu     sync.Mutex
	id     string
	events []hx.SessionEvent
	seq    int
}

func (s *fakeSession) ID() string { return s.id }

func (s *fakeSession) Append(kind hx.EventKind, data []byte, surface bool) (hx.SessionEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	ev := hx.SessionEvent{
		Seq:     s.seq,
		Kind:    kind,
		Data:    append([]byte(nil), data...),
		Surface: surface,
		FactID:  fmt.Sprintf("fact-%d", s.seq),
	}
	s.events = append(s.events, ev)
	return ev, nil
}

func (s *fakeSession) Events() []hx.SessionEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]hx.SessionEvent(nil), s.events...)
}

func (s *fakeSession) DeriveMessages() []hx.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var msgs []hx.Message
	for _, ev := range s.events {
		if !ev.Surface {
			continue
		}
		switch ev.Kind {
		case hx.EvUserMessage, hx.EvAssistantMessage, hx.EvToolResult:
			var m hx.Message
			if err := json.Unmarshal(ev.Data, &m); err == nil {
				msgs = append(msgs, m)
			}
		}
	}
	return msgs
}

func (s *fakeSession) Header() hx.RequestHeader { return hx.RequestHeader{} }

type fakeSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*fakeSession
}

func (st *fakeSessionStore) Create(id string) (hx.Session, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sessions == nil {
		st.sessions = map[string]*fakeSession{}
	}
	s := &fakeSession{id: id}
	st.sessions[id] = s
	return s, nil
}

func (st *fakeSessionStore) Get(id string) (hx.Session, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.sessions[id]
	return s, ok
}

// fakeLLM streams a pre-scripted sequence of chunks per call and records the
// options each call was made with (so a test can assert what the model saw).
type fakeLLM struct {
	mu      sync.Mutex
	scripts [][]hx.StreamChunk
	calls   []hx.GenerateOptions
	n       int
}

func (l *fakeLLM) RegisterAdapter(hx.LlmAdapter) (func(), error) { return func() {}, nil }

func (l *fakeLLM) Stream(ctx context.Context, opts hx.GenerateOptions) (<-chan hx.StreamChunk, error) {
	l.mu.Lock()
	idx := l.n
	l.n++
	l.calls = append(l.calls, opts)
	var script []hx.StreamChunk
	if idx < len(l.scripts) {
		script = l.scripts[idx]
	}
	l.mu.Unlock()

	ch := make(chan hx.StreamChunk)
	go func() {
		defer close(ch)
		for _, c := range script {
			select {
			case ch <- c:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

func (l *fakeLLM) callAt(i int) (hx.GenerateOptions, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if i < 0 || i >= len(l.calls) {
		return hx.GenerateOptions{}, false
	}
	return l.calls[i], true
}

func (l *fakeLLM) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.calls)
}

// fakeTools has one tool that returns a fixed value and records what it ran.
type fakeTools struct {
	mu       sync.Mutex
	executed []hx.ToolExecutionInput
	result   hx.ToolResult
}

func (t *fakeTools) Register(hx.ToolDefinition) (func(), error) { return func() {}, nil }

func (t *fakeTools) Execute(in hx.ToolExecutionInput) hx.ToolResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.executed = append(t.executed, in)
	return t.result
}

func (t *fakeTools) Schemas() []hx.ToolSchema {
	return []hx.ToolSchema{{
		Name:        "get_value",
		Description: "returns a value for a key",
		Parameters:  map[string]any{"type": "object"},
	}}
}

func (t *fakeTools) Concurrent(string) bool { return false }

func (t *fakeTools) ran() []hx.ToolExecutionInput {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]hx.ToolExecutionInput(nil), t.executed...)
}

type fakePrompt struct{}

func (fakePrompt) Section(int, string) (func(), error)            { return func() {}, nil }
func (fakePrompt) Variable(string, func() string) (func(), error) { return func() {}, nil }
func (fakePrompt) Assemble(map[string]string) (string, error)     { return "you are a test agent", nil }

// fakeRegistry captures the factory set on it and delegates Create to it.
type fakeRegistry struct {
	mu      sync.Mutex
	factory hx.AgentFactory
}

func (r *fakeRegistry) SetFactory(f hx.AgentFactory) (func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factory = f
	return func() { r.mu.Lock(); r.factory = nil; r.mu.Unlock() }, nil
}

func (r *fakeRegistry) Create(cfg hx.AgentConfig) (hx.AgentHandle, error) {
	r.mu.Lock()
	f := r.factory
	r.mu.Unlock()
	if f == nil {
		return nil, fmt.Errorf("no factory")
	}
	return f.Create(cfg)
}

func (r *fakeRegistry) WithInitiator(_ hx.Agent, fn func() error) error { return fn() }
func (r *fakeRegistry) CurrentInitiator() (hx.Agent, bool)              { return nil, false }

func (r *fakeRegistry) current() hx.AgentFactory {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.factory
}

// --- the scripts a single turn plays out --------------------------------------

// toolCallScript: the model streams a line of text, then one tool call, and
// finishes asking for tool calls.
func toolCallScript() []hx.StreamChunk {
	return []hx.StreamChunk{
		{Kind: hx.ChunkBlockStart, BlockIndex: 0, BlockOf: hx.BlockText},
		{Kind: hx.ChunkTextDelta, BlockIndex: 0, Delta: "Let me look that up. "},
		{Kind: hx.ChunkBlockEnd, BlockIndex: 0},
		{Kind: hx.ChunkBlockStart, BlockIndex: 1, BlockOf: hx.BlockToolCall},
		{Kind: hx.ChunkToolCallDelta, BlockIndex: 1, ToolCallID: "call-1", ToolName: "get_value", ArgsDelta: `{"key":`},
		{Kind: hx.ChunkToolCallDelta, BlockIndex: 1, ArgsDelta: `"answer"}`},
		{Kind: hx.ChunkBlockEnd, BlockIndex: 1},
		{Kind: hx.ChunkUsage, Usage: &hx.Usage{InputTokens: 10, OutputTokens: 5}},
		{Kind: hx.ChunkFinish, Finish: hx.FinishToolCalls},
	}
}

// stopScript: the model answers and stops — no tool calls, so the turn ends.
func stopScript() []hx.StreamChunk {
	return []hx.StreamChunk{
		{Kind: hx.ChunkBlockStart, BlockIndex: 0, BlockOf: hx.BlockText},
		{Kind: hx.ChunkTextDelta, BlockIndex: 0, Delta: "The value is 42."},
		{Kind: hx.ChunkBlockEnd, BlockIndex: 0},
		{Kind: hx.ChunkFinish, Finish: hx.FinishStop},
	}
}

// loopingLLM emits a tool call on EVERY call, so the turn always owes another
// step — a runaway model. Without a step cap this loops forever; with one the turn
// is bounded. It records how many model calls it served.
type loopingLLM struct {
	mu sync.Mutex
	n  int
}

func (l *loopingLLM) RegisterAdapter(hx.LlmAdapter) (func(), error) { return func() {}, nil }
func (l *loopingLLM) Stream(ctx context.Context, _ hx.GenerateOptions) (<-chan hx.StreamChunk, error) {
	l.mu.Lock()
	l.n++
	l.mu.Unlock()
	ch := make(chan hx.StreamChunk)
	go func() {
		defer close(ch)
		for _, c := range toolCallScript() {
			select {
			case ch <- c:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}
func (l *loopingLLM) count() int { l.mu.Lock(); defer l.mu.Unlock(); return l.n }

// --- the tests ----------------------------------------------------------------

// TestMaxStepsBoundsRunawayTurn proves §4.2 bounded loop termination: a model that
// keeps owing a step (loopingLLM) does NOT hang the driver — the turn stops at the
// AgentConfig.MaxSteps cap and WhenIdle settles. Without the cap this test would
// hang and fail on the WhenIdle timeout.
func TestMaxStepsBoundsRunawayTurn(t *testing.T) {
	llm := &loopingLLM{}
	tools := &fakeTools{result: hx.ToolResult{Value: []byte(`"x"`)}}
	store := &fakeSessionStore{}
	svc := mapServices{
		hx.KeyLLM:     llm,
		hx.KeyTools:   tools,
		hx.KeyPrompt:  fakePrompt{},
		hx.KeySession: store,
		hx.KeyAgents:  &fakeRegistry{},
	}
	f := NewFactory(svc)
	handle, err := f.Create(hx.AgentConfig{Provider: "test", Model: "test", SessionID: "s1", PrincipalID: "p1", MaxSteps: 3})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	handle.Agent().Followup("go")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := handle.Agent().WhenIdle(ctx); !errors.Is(err, hx.ErrStepLimit) {
		t.Fatalf("runaway turn must settle with a step-limit failure: %v", err)
	}
	if got := llm.count(); got != 3 {
		t.Fatalf("turn not bounded to MaxSteps: got %d model calls, want 3", got)
	}
}

// TestOneTurn drives one full turn through the Factory using only in-package
// fakes: a followup streams a model call, appends assistant/chunk +
// assistant/message, dispatches the tool call (tool/call + tool/result), the
// second step sees the tool result, and WhenIdle returns once drained.
func TestOneTurn(t *testing.T) {
	llm := &fakeLLM{scripts: [][]hx.StreamChunk{toolCallScript(), stopScript()}}
	tools := &fakeTools{result: hx.ToolResult{Value: []byte(`"42"`)}}
	store := &fakeSessionStore{}
	svc := mapServices{
		hx.KeyLLM:     llm,
		hx.KeyTools:   tools,
		hx.KeyPrompt:  fakePrompt{},
		hx.KeySession: store,
		hx.KeyAgents:  &fakeRegistry{},
	}

	f := NewFactory(svc)
	handle, err := f.Create(hx.AgentConfig{Provider: "test", Model: "test-model", SessionID: "s1", PrincipalID: "principal-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ag := handle.Agent()
	if ag.ID() == "" {
		t.Fatal("agent has empty id")
	}

	ag.Followup("what is the answer?")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := ag.WhenIdle(ctx); err != nil {
		t.Fatalf("WhenIdle did not settle: %v", err)
	}

	// Two model calls: the tool-call step and the follow-on stop step.
	if got := llm.callCount(); got != 2 {
		t.Fatalf("expected 2 model calls, got %d", got)
	}

	// One tool dispatched, with args reassembled from the two deltas.
	ran := tools.ran()
	if len(ran) != 1 {
		t.Fatalf("expected 1 tool execution, got %d", len(ran))
	}
	if ran[0].Name != "get_value" || ran[0].CallID != "call-1" {
		t.Fatalf("unexpected tool input: %+v", ran[0])
	}
	if string(ran[0].Args) != `{"key":"answer"}` {
		t.Fatalf("tool args not reassembled: %q", string(ran[0].Args))
	}
	if ran[0].Initiator != "principal-1" {
		t.Fatalf("tool not actor-stamped, initiator=%q", ran[0].Initiator)
	}

	// The SECOND model call must have seen the tool result in its message history.
	second, ok := llm.callAt(1)
	if !ok {
		t.Fatal("no second call recorded")
	}
	var sawToolResult bool
	for _, m := range second.Messages {
		if m.Role == hx.RoleTool {
			for _, b := range m.Content {
				if b.Kind == hx.BlockToolResult && b.Text == `"42"` {
					sawToolResult = true
				}
			}
		}
	}
	if !sawToolResult {
		t.Fatalf("second step did not see the tool result; messages=%+v", second.Messages)
	}

	// The event log tells the whole story, in order.
	sess, ok := store.Get("s1")
	if !ok {
		t.Fatal("session not created")
	}
	kinds := eventKinds(sess.Events())
	assertContains(t, kinds, hx.EvTurnStart)
	assertContains(t, kinds, hx.EvTurnEnd)
	assertContains(t, kinds, hx.EvAssistantChunk)
	assertContains(t, kinds, hx.EvToolCall)
	assertContains(t, kinds, hx.EvToolResult)
	if c := count(kinds, hx.EvAssistantMessage); c != 2 {
		t.Fatalf("expected 2 assistant/message events, got %d", c)
	}
	if c := count(kinds, hx.EvUserMessage); c != 1 {
		t.Fatalf("expected 1 user/message event, got %d", c)
	}

	// Ordering: turn/start first, tool/call before its tool/result, tool/result
	// before the second assistant/message, turn/end last.
	if !before(kinds, hx.EvTurnStart, hx.EvUserMessage) {
		t.Fatal("turn/start not before user/message")
	}
	if !before(kinds, hx.EvToolCall, hx.EvToolResult) {
		t.Fatal("tool/call not before tool/result")
	}
	if idx(kinds, hx.EvTurnEnd) != len(kinds)-1 {
		t.Fatal("turn/end is not the last event")
	}
}

// TestComponentWiring loads the loop Component onto a REAL runtime registry beneath
// components that provide the five injected services, and asserts the loop
// provides KeyAgentLoop and registered itself as the AGENTS factory.
func TestComponentWiring(t *testing.T) {
	reg := runtime.New()
	agents := &fakeRegistry{}

	// One component providing every service the loop injects.
	reg.Load(&runtime.Component{
		Name:    "fakes",
		Provide: []string{hx.KeyLLM, hx.KeySession, hx.KeyPrompt, hx.KeyTools, hx.KeyAgents, hx.KeyCompaction},
		Apply: func(ctx *runtime.Context) error {
			for k, v := range map[string]any{
				hx.KeyLLM:        &fakeLLM{},
				hx.KeySession:    &fakeSessionStore{},
				hx.KeyPrompt:     fakePrompt{},
				hx.KeyTools:      &fakeTools{},
				hx.KeyAgents:     agents,
				hx.KeyCompaction: &fakeCompactor{},
			} {
				if err := ctx.Provide(k, v); err != nil {
					return err
				}
			}
			return nil
		},
	}, "")

	reg.Load(Component(), "")

	v, ok := reg.Lookup(hx.KeyAgentLoop)
	if !ok {
		t.Fatal("KeyAgentLoop not provided by the loop component")
	}
	if _, ok := v.(*Factory); !ok {
		t.Fatalf("KeyAgentLoop is %T, want *Factory", v)
	}
	if agents.current() == nil {
		t.Fatal("loop did not register itself as the AGENTS factory")
	}
}

// --- small assertion helpers --------------------------------------------------

func eventKinds(events []hx.SessionEvent) []hx.EventKind {
	out := make([]hx.EventKind, len(events))
	for i, e := range events {
		out[i] = e.Kind
	}
	return out
}

func count(kinds []hx.EventKind, k hx.EventKind) int {
	n := 0
	for _, x := range kinds {
		if x == k {
			n++
		}
	}
	return n
}

func idx(kinds []hx.EventKind, k hx.EventKind) int {
	for i, x := range kinds {
		if x == k {
			return i
		}
	}
	return -1
}

func before(kinds []hx.EventKind, a, b hx.EventKind) bool {
	ia, ib := idx(kinds, a), idx(kinds, b)
	return ia >= 0 && ib >= 0 && ia < ib
}

func assertContains(t *testing.T, kinds []hx.EventKind, k hx.EventKind) {
	t.Helper()
	if idx(kinds, k) < 0 {
		t.Fatalf("event log missing %s; got %v", k, kinds)
	}
}
