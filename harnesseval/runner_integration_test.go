//go:build integration

package harnesseval

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/improve"
	"github.com/websines/structor-harness/record"
)

// repairAdapter is a deterministic model protocol fixture, not a benchmark
// solver. It exercises the real native loop and file tools; acceptance still
// executes the resulting Go program under the independent behavioral grader.
type repairAdapter struct{ original, repair string }

func (a *repairAdapter) Route() string { return "fixture" }
func (a *repairAdapter) Stream(ctx context.Context, opts hx.GenerateOptions) (<-chan hx.StreamChunk, error) {
	lastTool := ""
	for _, m := range opts.Messages {
		if m.Role == hx.RoleTool {
			for _, b := range m.Content {
				if b.ToolName != "" {
					lastTool = b.ToolName
				}
			}
		}
	}
	// Tool result messages can omit ToolName; request progression then follows
	// the actual assistant tool calls retained in history.
	for _, m := range opts.Messages {
		if m.Role == hx.RoleAssistant {
			for _, b := range m.Content {
				if b.Kind == hx.BlockToolCall {
					lastTool = b.ToolName
				}
			}
		}
	}
	var chunks []hx.StreamChunk
	if lastTool == "write_file" {
		chunks = []hx.StreamChunk{{Kind: hx.ChunkBlockStart, BlockOf: hx.BlockText}, {Kind: hx.ChunkTextDelta, Delta: "Saved the change."}, {Kind: hx.ChunkBlockEnd}}
	} else {
		name := "read_file"
		args := map[string]string{"path": "main.go"}
		if lastTool == "read_file" {
			name = "write_file"
			args["content"] = a.original
			if strings.Contains(opts.System, "specific") || strings.Contains(opts.System, "compose") {
				args["content"] = a.repair
			}
		}
		data, _ := json.Marshal(args)
		chunks = []hx.StreamChunk{{Kind: hx.ChunkBlockStart, BlockOf: hx.BlockToolCall}, {Kind: hx.ChunkToolCallDelta, ToolCallID: "call-" + name, ToolName: name, ArgsDelta: string(data)}, {Kind: hx.ChunkBlockEnd}}
	}
	chunks = append(chunks, hx.StreamChunk{Kind: hx.ChunkUsage, Usage: &hx.Usage{InputTokens: 100, OutputTokens: 10}})
	finish := hx.FinishToolCalls
	if lastTool == "write_file" {
		finish = hx.FinishStop
	}
	chunks = append(chunks, hx.StreamChunk{Kind: hx.ChunkFinish, Finish: finish})
	out := make(chan hx.StreamChunk)
	go func() {
		defer close(out)
		for _, c := range chunks {
			select {
			case out <- c:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

type workflowEditor struct{}

func (workflowEditor) Edit(_ context.Context, r improve.EditRequest) ([]byte, error) {
	if r.Stage == "reflect" {
		return json.Marshal(map[string]any{"conditions": "duplicate handling", "observation": "specific and composed runs executed successfully", "limitations": "deterministic protocol fixture only", "evidence": []string{r.Reference.ID, r.Candidates[len(r.Candidates)-1].Run.ID}})
	}
	p := r.Base
	p.Guidance = r.Stage
	return json.Marshal(improve.Decision{Profile: p, Reason: "compare the retained native executions", Evidence: []string{r.Reference.ID}})
}

func TestNativeImprovementEndToEnd(t *testing.T) {
	fixtures, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	f := fixtures[0]
	cas, err := record.NewLocalCAS(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatal(err)
	}
	f.Task.Source, err = f.Source.Store(cas)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := improve.NewJournal(filepath.Join(t.TempDir(), "journal"), cas)
	if err != nil {
		t.Fatal(err)
	}
	budget, err := improve.NewBudget(improve.Limits{Input: 500000, Output: 40000, USD: 5, Calls: 50}, improve.Rates{Input: 1, Output: 1})
	if err != nil {
		t.Fatal(err)
	}
	guard := &improve.Guard{Adapter: &repairAdapter{original: f.Source["main.go"], repair: f.Gold["main.go"]}, Budgets: []*improve.Budget{budget}, MaxOutput: 2048}
	runner := &Runner{CAS: cas, Adapter: guard, Model: "fixture", StateDir: t.TempDir(), Principal: "fixture:operator", Timeout: time.Minute, MaxSteps: 8, BaseInstructions: "Use the native file tools to complete the requested repair."}
	loop := improve.Loop{Runner: runner, Editor: workflowEditor{}, Recorder: journal}
	base := improve.Profile{Schema: 1, Base: record.ComputeDigest([]byte(runner.BaseInstructions))}
	result, err := loop.Run(t.Context(), f.Task, base, "e2e", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Runs) != 4 || result.Fallback || result.Final.ID != "e2e-composed" {
		t.Fatalf("loop outcome: %+v", result)
	}
	baseline, err := f.Evaluate(t.Context(), cas, result.Runs[0].Artifact)
	if err != nil {
		t.Fatal(err)
	}
	final, err := f.Evaluate(t.Context(), cas, result.Final.Artifact)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Passed || !final.Passed || !final.Scored {
		t.Fatalf("baseline %+v final %+v", baseline, final)
	}
	if result.Lesson == nil || len(result.Lesson.Evidence) == 0 {
		t.Fatal("missing evidence-linked reflection")
	}
	if budget.Snapshot().Calls != 12 || budget.Snapshot().Unknown != 0 {
		t.Fatalf("calls/auxiliary accounting: %+v", budget.Snapshot())
	}
	// A later trial can retrieve the lesson, while its source remains the original.
	if result.Task.Source != f.Task.Source || result.Final.Source != f.Task.Source {
		t.Fatal("source drift")
	}
}
