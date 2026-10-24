package spine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/harness/compaction"
	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/session"
	"github.com/websines/structor-harness/harness/spine"
	"github.com/websines/structor-harness/record"
	"github.com/websines/structor-harness/runtime"
)

// capturingAdapter records the system prompt of each model call and replays a
// script — so a test can prove what context actually reached the model.
type capturingAdapter struct {
	scripts    [][]hx.StreamChunk
	n          int
	lastSystem string
}

func (c *capturingAdapter) Route() string { return "cap" }

func (c *capturingAdapter) Stream(_ context.Context, opts hx.GenerateOptions) (<-chan hx.StreamChunk, error) {
	c.lastSystem = opts.System
	ch := make(chan hx.StreamChunk, 8)
	var out []hx.StreamChunk
	if c.n < len(c.scripts) {
		out = c.scripts[c.n]
	} else {
		out = []hx.StreamChunk{{Kind: hx.ChunkFinish, Finish: hx.FinishStop}}
	}
	c.n++
	go func() {
		for _, ch2 := range out {
			ch <- ch2
		}
		close(ch)
	}()
	return ch, nil
}

// TestH4ContextAndCompaction proves the two context/memory features run in the
// harness on cordis: (1) a cited, active-first, never-stale block assembled from
// the record is injected into the system prompt each turn (verified context); and
// (2) as history grows it is bounded by compaction-as-supersession — the derived
// view shrinks while the log is retained whole.
func TestH4ContextAndCompaction(t *testing.T) {
	cas, _ := record.NewLocalCAS(t.TempDir())
	ctxCAS, _ := record.NewLocalCAS(t.TempDir())

	// The record the verified-context block is assembled from: one ACTIVE fact and
	// one that has been SUPERSEDED (must not appear — never-stale).
	ctxStore := flg.NewStore(ctxCAS)
	if _, err := ctxStore.AppendObligation(flg.Obligation{
		ID: "O-CTX", Title: "refund never exceeds capture", AcceptancePolicy: "p", DomainTags: []string{"payments"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ctxStore.AppendObligation(flg.Obligation{
		ID: "O-OLD", Title: "stale legacy claim", AcceptancePolicy: "p", DomainTags: []string{"legacy"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ctxStore.AppendSupersession(flg.SupersessionEvidence{
		Subject: "O-OLD", Kind: flg.KindStatusSuperseded,
	}); err != nil {
		t.Fatal(err)
	}

	// Several tool-calling turns so the derived history grows past a tiny budget.
	toolTurn := []hx.StreamChunk{
		{Kind: hx.ChunkToolCallDelta, BlockIndex: 0, ToolCallID: "c", ToolName: "noop", ArgsDelta: `{}`},
		{Kind: hx.ChunkFinish, Finish: hx.FinishToolCalls},
	}
	stop := []hx.StreamChunk{
		{Kind: hx.ChunkTextDelta, BlockIndex: 0, Delta: "done."},
		{Kind: hx.ChunkFinish, Finish: hx.FinishStop},
	}
	adapter := &capturingAdapter{scripts: [][]hx.StreamChunk{toolTurn, toolTurn, toolTurn, toolTurn, stop}}

	noop := hx.ToolDefinition{
		Schema: hx.ToolSchema{Name: "noop", Description: "does nothing", Parameters: map[string]any{"type": "object"}},
		Execute: func(hx.ToolRunContext, []byte) (hx.ToolResult, error) {
			return hx.ToolResult{Value: []byte(`"ok"`)}, nil
		},
	}

	reg := runtime.New()
	if err := spine.Build(reg, spine.Options{
		CAS:          cas,
		Adapter:      adapter,
		Actor:        session.Config{PrincipalID: "principal:h4", Commit: "c0ffee"},
		Tools:        []hx.ToolDefinition{noop},
		Compaction:   &compaction.Engine{MaxMessages: 2}, // tiny budget forces compaction
		ContextStore: ctxStore,
		Sections:     []spine.Section{{Order: 0, Text: "You are a Forge agent."}},
	}); err != nil {
		t.Fatal(err)
	}

	factory, err := spine.Factory(reg)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := factory.Create(hx.AgentConfig{Provider: adapter.Route(), Model: "m", SessionID: "h4", PrincipalID: "principal:h4"})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Dispose()

	handle.Agent().Followup("do several steps")
	if err := handle.Agent().WhenIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	// (1) Verified context reached the model: the last system prompt carries the
	// cited active fact and NOT the superseded one.
	sys := adapter.lastSystem
	if !strings.Contains(sys, "Verified context") || !strings.Contains(sys, "O-CTX") {
		t.Fatalf("verified-context block with the active fact did not reach the model.\nsystem prompt:\n%s", sys)
	}
	if strings.Contains(sys, "O-OLD") {
		t.Fatalf("a SUPERSEDED fact leaked into the context block (should be never-stale):\n%s", sys)
	}

	// (2) Compaction fired: the log carries at least one compaction summary event,
	// and the full log is retained (Events longer than the compacted-away span).
	store, _ := spine.Store(reg)
	sess, _ := store.Get("h4")
	events := sess.Events()
	compacted := 0
	for _, ev := range events {
		if string(ev.Kind) == "compaction/summary" {
			compacted++
		}
	}
	if compacted == 0 {
		t.Fatalf("no compaction/summary event — compaction never fired over %d events", len(events))
	}
	// The derived (bounded) view is smaller than the full retained log.
	if len(sess.DeriveMessages()) >= len(events) {
		t.Fatalf("derived view (%d) not smaller than the retained log (%d) — compaction did not bound context", len(sess.DeriveMessages()), len(events))
	}
}
