package spine_test

import (
	"context"
	"testing"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/harness/evidence"
	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/llm"
	"github.com/websines/structor-harness/harness/session"
	"github.com/websines/structor-harness/harness/spine"
	"github.com/websines/structor-harness/record"
	"github.com/websines/structor-harness/runtime"
)

// TestH3EvidenceLoopClosed drives the full proof loop: an agent runs a tool that
// returns an EvidenceHint, the evidence bridge lifts it into typed evidence in
// the record, and the resolver derives the obligation's verdict — the agent's
// work decides the gate, and the agent never marks its own work done.
//
// It exercises both polarities: a passing test discharges the obligation; a
// reproducible counterexample falsifies it.
func TestH3EvidenceLoopClosed(t *testing.T) {
	// A tool that reports evidence for obligation O-1 with the polarity the model
	// asks for. This stands in for a real collector wrapping a real test run.
	report := hx.ToolDefinition{
		Schema: hx.ToolSchema{
			Name:        "report_test",
			Description: "report a reproducible test outcome for an obligation",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"passed": map[string]any{"type": "boolean"},
				},
			},
		},
		Execute: func(rc hx.ToolRunContext, args []byte) (hx.ToolResult, error) {
			passed := string(args) != "" && contains(args, `"passed":true`)
			return hx.ToolResult{
				Value: []byte(`"reported"`),
				EvidenceHint: &hx.EvidenceHint{
					ObligationID: "O-1",
					Supports:     passed,
					Method:       "executed_test",
					Reproducible: true,
					Summary:      "property test for O-1",
				},
			}, nil
		},
	}

	run := func(t *testing.T, passed bool) flg.OblState {
		t.Helper()
		cas, _ := record.NewLocalCAS(t.TempDir())
		evCAS, _ := record.NewLocalCAS(t.TempDir())

		sink := evidence.New(evCAS)
		if err := sink.RegisterObligation(flg.Obligation{
			ID: "O-1", Title: "refund never exceeds capture",
			AcceptancePolicy: "test-passes", DomainTags: []string{"payments"},
		}); err != nil {
			t.Fatal(err)
		}

		arg := `{"passed":false}`
		if passed {
			arg = `{"passed":true}`
		}
		fake := &llm.FakeAdapter{Script: [][]hx.StreamChunk{
			{
				{Kind: hx.ChunkToolCallDelta, BlockIndex: 0, ToolCallID: "c1", ToolName: "report_test", ArgsDelta: arg},
				{Kind: hx.ChunkFinish, Finish: hx.FinishToolCalls},
			},
			{
				{Kind: hx.ChunkTextDelta, BlockIndex: 0, Delta: "reported."},
				{Kind: hx.ChunkFinish, Finish: hx.FinishStop},
			},
		}}

		reg := runtime.New()
		if err := spine.Build(reg, spine.Options{
			CAS:      cas,
			Adapter:  fake,
			Actor:    session.Config{PrincipalID: "principal:h3", Commit: "c0ffee"},
			Tools:    []hx.ToolDefinition{report},
			Evidence: sink,
			Sections: []spine.Section{{Order: 0, Text: "You are a Forge agent."}},
		}); err != nil {
			t.Fatal(err)
		}

		factory, err := spine.Factory(reg)
		if err != nil {
			t.Fatal(err)
		}
		handle, err := factory.Create(hx.AgentConfig{Provider: fake.Route(), Model: "m", SessionID: "h3", PrincipalID: "principal:h3"})
		if err != nil {
			t.Fatal(err)
		}
		defer handle.Dispose()

		handle.Agent().Followup("run the test for O-1")
		if err := handle.Agent().WhenIdle(context.Background()); err != nil {
			t.Fatal(err)
		}

		v, err := sink.Verdict("O-1")
		if err != nil {
			t.Fatal(err)
		}
		return v.State
	}

	// A passing reproducible test discharges the obligation.
	if got := run(t, true); got != flg.StateDischarged {
		t.Fatalf("passing test: expected discharged, got %s", got)
	}
	// A reproducible counterexample falsifies it — the agent cannot call it done.
	if got := run(t, false); got != flg.StateFalsified {
		t.Fatalf("failing test: expected falsified, got %s", got)
	}
}

func contains(b []byte, s string) bool {
	return len(b) >= len(s) && indexOf(string(b), s) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
