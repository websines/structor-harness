package llm

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
)

// drain collects every chunk from a stream channel until it closes.
func drain(ch <-chan hx.StreamChunk) []hx.StreamChunk {
	var out []hx.StreamChunk
	for c := range ch {
		out = append(out, c)
	}
	return out
}

// --- assembler ---------------------------------------------------------------

func TestAssemblerFoldsTextAndToolCall(t *testing.T) {
	var a BlockAssembler
	for _, c := range []hx.StreamChunk{
		{Kind: hx.ChunkTextDelta, Delta: "Hello "},
		{Kind: hx.ChunkTextDelta, Delta: "world"},
		{Kind: hx.ChunkToolCallDelta, ToolCallID: "call_1", ToolName: "search", ArgsDelta: `{"q":`},
		{Kind: hx.ChunkToolCallDelta, ToolCallID: "call_1", ArgsDelta: `"go"}`},
		{Kind: hx.ChunkUsage, Usage: &hx.Usage{InputTokens: 11, OutputTokens: 7}},
		{Kind: hx.ChunkFinish, Finish: hx.FinishToolCalls},
	} {
		a.Push(c)
	}

	blocks := a.Blocks()
	if len(blocks) != 2 {
		t.Fatalf("want 2 blocks, got %d: %+v", len(blocks), blocks)
	}
	if blocks[0].Kind != hx.BlockText || blocks[0].Text != "Hello world" {
		t.Errorf("block0 = %+v, want text 'Hello world'", blocks[0])
	}
	tc := blocks[1]
	if tc.Kind != hx.BlockToolCall || tc.ToolCallID != "call_1" || tc.ToolName != "search" {
		t.Errorf("block1 = %+v, want tool-call call_1/search", tc)
	}
	if string(tc.ToolArgs) != `{"q":"go"}` {
		t.Errorf("tool args = %q, want %q", tc.ToolArgs, `{"q":"go"}`)
	}
	if u := a.Usage(); u.InputTokens != 11 || u.OutputTokens != 7 {
		t.Errorf("usage = %+v, want {11 7}", u)
	}
	if a.Finish() != hx.FinishToolCalls {
		t.Errorf("finish = %q, want tool_calls", a.Finish())
	}
}

func TestAssemblerExplicitBlockBoundaries(t *testing.T) {
	var a BlockAssembler
	for _, c := range []hx.StreamChunk{
		{Kind: hx.ChunkBlockStart, BlockIndex: 0, BlockOf: hx.BlockReasoning},
		{Kind: hx.ChunkReasoningDelta, Delta: "think"},
		{Kind: hx.ChunkBlockEnd},
		{Kind: hx.ChunkBlockStart, BlockIndex: 1, BlockOf: hx.BlockText},
		{Kind: hx.ChunkTextDelta, Delta: "answer"},
		{Kind: hx.ChunkBlockEnd},
		{Kind: hx.ChunkFinish, Finish: hx.FinishStop},
	} {
		a.Push(c)
	}
	blocks := a.Blocks()
	if len(blocks) != 2 {
		t.Fatalf("want 2 blocks, got %d: %+v", len(blocks), blocks)
	}
	if blocks[0].Kind != hx.BlockReasoning || blocks[0].Text != "think" {
		t.Errorf("block0 = %+v, want reasoning 'think'", blocks[0])
	}
	if blocks[1].Kind != hx.BlockText || blocks[1].Text != "answer" {
		t.Errorf("block1 = %+v, want text 'answer'", blocks[1])
	}
}

func TestAssemblerSeparatesTwoToolCalls(t *testing.T) {
	var a BlockAssembler
	for _, c := range []hx.StreamChunk{
		{Kind: hx.ChunkToolCallDelta, ToolCallID: "call_1", ToolName: "a", ArgsDelta: `{}`},
		{Kind: hx.ChunkToolCallDelta, ToolCallID: "call_2", ToolName: "b", ArgsDelta: `{}`},
		{Kind: hx.ChunkFinish, Finish: hx.FinishToolCalls},
	} {
		a.Push(c)
	}
	blocks := a.Blocks()
	if len(blocks) != 2 {
		t.Fatalf("want 2 tool-call blocks, got %d: %+v", len(blocks), blocks)
	}
	if blocks[0].ToolCallID != "call_1" || blocks[1].ToolCallID != "call_2" {
		t.Errorf("ids = %q,%q want call_1,call_2", blocks[0].ToolCallID, blocks[1].ToolCallID)
	}
}

// --- runtime routing + disposer ----------------------------------------------

// routeStub is a minimal adapter that tags its single emitted chunk so a test can
// confirm which adapter a Stream reached.
type routeStub struct{ route, tag string }

func (s routeStub) Route() string { return s.route }
func (s routeStub) Stream(ctx context.Context, opts hx.GenerateOptions) (<-chan hx.StreamChunk, error) {
	ch := make(chan hx.StreamChunk, 1)
	ch <- hx.StreamChunk{Kind: hx.ChunkTextDelta, Delta: s.tag}
	close(ch)
	return ch, nil
}

func TestRuntimeRoutesByProvider(t *testing.T) {
	r := NewRuntime()
	if _, err := r.RegisterAdapter(routeStub{"prov-a", "A"}); err != nil {
		t.Fatalf("register a: %v", err)
	}
	if _, err := r.RegisterAdapter(routeStub{"prov-b", "B"}); err != nil {
		t.Fatalf("register b: %v", err)
	}

	for provider, want := range map[string]string{"prov-a": "A", "prov-b": "B"} {
		ch, err := r.Stream(context.Background(), hx.GenerateOptions{Provider: provider})
		if err != nil {
			t.Fatalf("stream %s: %v", provider, err)
		}
		got := drain(ch)
		if len(got) != 1 || got[0].Delta != want {
			t.Errorf("provider %s routed to tag %v, want %q", provider, got, want)
		}
	}

	// A double registration of the same route is rejected.
	if _, err := r.RegisterAdapter(routeStub{"prov-a", "dup"}); err == nil {
		t.Error("expected error registering duplicate route")
	}
}

func TestRuntimeUnknownProviderErrors(t *testing.T) {
	r := NewRuntime()
	if _, err := r.Stream(context.Background(), hx.GenerateOptions{Provider: "missing"}); err == nil {
		t.Fatal("expected error for unregistered provider")
	}
}

func TestRuntimeDisposerUnregisters(t *testing.T) {
	r := NewRuntime()
	dispose, err := r.RegisterAdapter(&FakeAdapter{})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := r.Stream(context.Background(), hx.GenerateOptions{Provider: "fake"}); err != nil {
		t.Fatalf("stream before dispose: %v", err)
	}
	dispose()
	if _, err := r.Stream(context.Background(), hx.GenerateOptions{Provider: "fake"}); err == nil {
		t.Error("expected error after dispose")
	}
	dispose() // idempotent — must not panic
}

// --- FakeAdapter replay ------------------------------------------------------

func TestFakeAdapterReplaysScript(t *testing.T) {
	f := &FakeAdapter{Script: [][]hx.StreamChunk{
		{
			{Kind: hx.ChunkTextDelta, Delta: "first"},
			{Kind: hx.ChunkFinish, Finish: hx.FinishStop},
		},
		{
			{Kind: hx.ChunkTextDelta, Delta: "second"},
			{Kind: hx.ChunkFinish, Finish: hx.FinishStop},
		},
	}}

	for _, want := range []string{"first", "second"} {
		ch, err := f.Stream(context.Background(), hx.GenerateOptions{})
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		got := drain(ch)
		if len(got) != 2 || got[0].Delta != want || got[1].Finish != hx.FinishStop {
			t.Errorf("replay = %+v, want text %q then finish", got, want)
		}
	}

	// Exhausted script yields a lone terminal finish.
	ch, _ := f.Stream(context.Background(), hx.GenerateOptions{})
	got := drain(ch)
	if len(got) != 1 || got[0].Kind != hx.ChunkFinish {
		t.Errorf("exhausted replay = %+v, want single finish", got)
	}
}

// --- SSE parsing over canned bytes (no HTTP) ---------------------------------

const cannedSSE = `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"content":" there"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"reasoning_content":"hmm"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":"}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"go\"}"}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}

data: [DONE]
`

func TestScanSSEMapsChunks(t *testing.T) {
	var got []hx.StreamChunk
	fin, err := scanSSE(bytes.NewReader([]byte(cannedSSE)), func(c hx.StreamChunk) bool {
		got = append(got, c)
		return true
	})
	if err != nil {
		t.Fatalf("scanSSE err: %v", err)
	}
	if fin != hx.FinishToolCalls {
		t.Errorf("finish = %q, want tool_calls", fin)
	}

	want := []hx.StreamChunk{
		{Kind: hx.ChunkTextDelta, Delta: "Hi"},
		{Kind: hx.ChunkTextDelta, Delta: " there"},
		{Kind: hx.ChunkReasoningDelta, Delta: "hmm"},
		{Kind: hx.ChunkToolCallDelta, ToolCallID: "call_1", ToolName: "search", ArgsDelta: `{"q":`},
		{Kind: hx.ChunkToolCallDelta, ToolCallID: "call_1", ToolName: "search", ArgsDelta: `"go"}`},
		{Kind: hx.ChunkUsage, Usage: &hx.Usage{InputTokens: 11, OutputTokens: 7}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d chunks, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Kind != w.Kind || g.Delta != w.Delta || g.ToolCallID != w.ToolCallID ||
			g.ToolName != w.ToolName || g.ArgsDelta != w.ArgsDelta {
			t.Errorf("chunk %d = %+v, want %+v", i, g, w)
		}
		if w.Usage != nil {
			if g.Usage == nil || *g.Usage != *w.Usage {
				t.Errorf("chunk %d usage = %+v, want %+v", i, g.Usage, w.Usage)
			}
		}
	}

	// The parsed chunks fold into the expected blocks end-to-end.
	var a BlockAssembler
	for _, c := range got {
		a.Push(c)
	}
	a.Push(hx.StreamChunk{Kind: hx.ChunkFinish, Finish: fin})
	blocks := a.Blocks()
	if len(blocks) != 3 {
		t.Fatalf("assembled %d blocks, want 3: %+v", len(blocks), blocks)
	}
	if blocks[0].Text != "Hi there" || blocks[1].Text != "hmm" {
		t.Errorf("text/reasoning = %q/%q", blocks[0].Text, blocks[1].Text)
	}
	if string(blocks[2].ToolArgs) != `{"q":"go"}` {
		t.Errorf("tool args = %q, want %q", blocks[2].ToolArgs, `{"q":"go"}`)
	}
}

func TestScanSSEStopsOnEmitFalse(t *testing.T) {
	count := 0
	_, err := scanSSE(bytes.NewReader([]byte(cannedSSE)), func(c hx.StreamChunk) bool {
		count++
		return false // stop after the first emitted chunk
	})
	if err != nil {
		t.Fatalf("scanSSE err: %v", err)
	}
	if count != 1 {
		t.Errorf("emitted %d chunks after stop signal, want 1", count)
	}
}

// TestScanSSEReasoningThenToolCallDistinctBlocks reproduces the LFM2 / LM Studio
// wire pattern that broke the live loop: a run of reasoning_content deltas
// followed by tool_calls deltas (id+name first, then an arguments-only fragment),
// all at the same wire position. The adapter must give the reasoning stream and
// the tool call DISTINCT BlockIndex values — otherwise a consumer that groups
// purely by BlockIndex (the loop's fold) folds the tool call onto the reasoning
// block, its Kind stays BlockReasoning, the tool-call filter misses it, and the
// tool never dispatches (obligation stays OPEN despite a real tool call).
func TestScanSSEReasoningThenToolCallDistinctBlocks(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"role":"assistant","reasoning_content":"The user "}}]}

data: {"choices":[{"delta":{"reasoning_content":"wants run_check."}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"run_check","arguments":""}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"type":"function","function":{"arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]
`
	var got []hx.StreamChunk
	fin, err := scanSSE(strings.NewReader(sse), func(c hx.StreamChunk) bool {
		got = append(got, c)
		return true
	})
	if err != nil {
		t.Fatalf("scanSSE err: %v", err)
	}
	if fin != hx.FinishToolCalls {
		t.Errorf("finish = %q, want tool_calls", fin)
	}

	// Reasoning and tool-call deltas must land on DIFFERENT block indices, and the
	// two tool-call fragments must share ONE index so their args accumulate.
	reasoningIdx, toolIdx := -1, -1
	var args []byte
	name := ""
	for _, c := range got {
		switch c.Kind {
		case hx.ChunkReasoningDelta:
			reasoningIdx = c.BlockIndex
		case hx.ChunkToolCallDelta:
			if toolIdx >= 0 && c.BlockIndex != toolIdx {
				t.Fatalf("tool-call deltas split across block indices %d and %d", toolIdx, c.BlockIndex)
			}
			toolIdx = c.BlockIndex
			if c.ToolName != "" {
				name = c.ToolName
			}
			args = append(args, []byte(c.ArgsDelta)...)
		}
	}
	if reasoningIdx < 0 || toolIdx < 0 {
		t.Fatalf("missing reasoning or tool-call chunks: reasoningIdx=%d toolIdx=%d", reasoningIdx, toolIdx)
	}
	if reasoningIdx == toolIdx {
		t.Fatalf("reasoning and tool call share block index %d — the tool call would be folded onto the reasoning block and never dispatch", toolIdx)
	}
	if name != "run_check" || string(args) != "{}" {
		t.Errorf("tool call = %q args %q, want run_check {}", name, args)
	}

	// End-to-end through a fold that groups purely by BlockIndex (the loop's model):
	// a BlockToolCall must survive rather than be swallowed by the reasoning block.
	kindAt := map[int]hx.BlockKind{}
	for _, c := range got {
		var k hx.BlockKind
		switch c.Kind {
		case hx.ChunkReasoningDelta:
			k = hx.BlockReasoning
		case hx.ChunkTextDelta:
			k = hx.BlockText
		case hx.ChunkToolCallDelta:
			k = hx.BlockToolCall
		default:
			continue
		}
		if _, seen := kindAt[c.BlockIndex]; !seen {
			kindAt[c.BlockIndex] = k
		}
	}
	foundToolBlock := false
	for _, k := range kindAt {
		if k == hx.BlockToolCall {
			foundToolBlock = true
		}
	}
	if !foundToolBlock {
		t.Fatal("no BlockToolCall survived a group-by-BlockIndex fold — the live loop bug")
	}
}

// --- request mapping ---------------------------------------------------------

func TestMapMessages(t *testing.T) {
	msgs := []hx.Message{
		{Role: hx.RoleUser, Content: []hx.ContentBlock{{Kind: hx.BlockText, Text: "hi"}}},
		{Role: hx.RoleAssistant, Content: []hx.ContentBlock{
			{Kind: hx.BlockToolCall, ToolCallID: "c1", ToolName: "search", ToolArgs: []byte(`{"q":"go"}`)},
		}},
		{Role: hx.RoleTool, Content: []hx.ContentBlock{
			{Kind: hx.BlockToolResult, ToolCallID: "c1", Text: "42 hits"},
		}},
	}
	out := mapMessages("be brief", msgs)
	if len(out) != 4 {
		t.Fatalf("want 4 messages (system+3), got %d: %+v", len(out), out)
	}
	if out[0].Role != "system" || out[0].Content != "be brief" {
		t.Errorf("out[0] = %+v, want system", out[0])
	}
	if out[1].Role != "user" || out[1].Content != "hi" {
		t.Errorf("out[1] = %+v, want user 'hi'", out[1])
	}
	if out[2].Role != "assistant" || len(out[2].ToolCalls) != 1 ||
		out[2].ToolCalls[0].ID != "c1" || out[2].ToolCalls[0].Function.Name != "search" ||
		out[2].ToolCalls[0].Function.Arguments != `{"q":"go"}` {
		t.Errorf("out[2] = %+v, want assistant tool_call", out[2])
	}
	if out[3].Role != "tool" || out[3].ToolCallID != "c1" || out[3].Content != "42 hits" {
		t.Errorf("out[3] = %+v, want tool result", out[3])
	}
}

func TestHTTPFailureClassifies(t *testing.T) {
	cases := map[int]struct {
		kind      string
		retryable bool
	}{
		429: {"rate_limit", true},
		401: {"auth", false},
		403: {"auth", false},
		500: {"provider", true},
		400: {"provider", false},
	}
	for status, want := range cases {
		f := httpFailure(status, "oops")
		if f.Kind != want.kind || f.Retryable != want.retryable {
			t.Errorf("status %d -> %+v, want kind %q retryable %v", status, f, want.kind, want.retryable)
		}
		if !strings.Contains(f.Message, "oops") {
			t.Errorf("status %d message missing body: %q", status, f.Message)
		}
	}
}
