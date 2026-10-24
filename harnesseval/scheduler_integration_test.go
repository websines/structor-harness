//go:build integration

package harnesseval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/improve"
)

type schedulerAdapter struct {
	native repairAdapter
	calls  atomic.Int64
}

func (a *schedulerAdapter) Route() string { return "fixture" }
func (a *schedulerAdapter) Stream(ctx context.Context, options hx.GenerateOptions) (<-chan hx.StreamChunk, error) {
	a.calls.Add(1)
	if !strings.HasPrefix(options.System, "You improve Forge workflow guidance.") {
		return a.native.Stream(ctx, options)
	}
	var payload struct {
		Request improve.EditRequest `json:"request"`
	}
	if err := json.Unmarshal([]byte(options.Messages[0].Content[0].Text), &payload); err != nil {
		return nil, err
	}
	// Each repeat starts a separate library branch, even with the same task ID.
	if payload.Request.Stage == "general" && len(payload.Request.Lessons) != 0 {
		return nil, errors.New("fixture: lessons leaked between repeat branches")
	}
	data, err := (workflowEditor{}).Edit(ctx, payload.Request)
	if err != nil {
		return nil, err
	}
	stream := make(chan hx.StreamChunk, 3)
	stream <- hx.StreamChunk{Kind: hx.ChunkTextDelta, Delta: string(data)}
	stream <- hx.StreamChunk{Kind: hx.ChunkUsage, Usage: &hx.Usage{InputTokens: 100, OutputTokens: 10}}
	stream <- hx.StreamChunk{Kind: hx.ChunkFinish, Finish: hx.FinishStop}
	close(stream)
	return stream, nil
}

// This is a protocol test with one controlled fixture and a synthetic admission
// receipt. It is not a live-model benchmark or evidence of an effectiveness gain.
// Native tools, source snapshots, grading, accounting, scheduling and reports are
// real; the adapter intentionally repairs only specific/composed profiles.
func TestPairedSchedulerEndToEndAndCompletedResume(t *testing.T) {
	fixtures, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	f := fixtures[0]
	f.Split = "evaluation"
	originalCatalog := catalog
	catalog, err = json.Marshal([]Fixture{f})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { catalog = originalCatalog }()
	state := lifecycleState(t)
	adapter := &schedulerAdapter{native: repairAdapter{original: f.Source["main.go"], repair: f.Gold["main.go"]}}
	config := Config{
		Schema: 1, Project: "scheduler-fixture", Model: "fixture", BaseURL: "http://127.0.0.1", APIKeyEnv: "UNUSED_FIXTURE_KEY",
		Rates: improve.Rates{Input: 1, Output: 1}, Split: "evaluation", Repeats: 2,
		Trial:          improve.Limits{Input: 2000000, Output: 200000, USD: 10, Calls: 200},
		Total:          improve.Limits{Input: 20000000, Output: 2000000, USD: 100, Calls: 1000},
		TimeoutSeconds: 1200, TrialSeconds: 300, ExecutionSeconds: 60, MaxSteps: 8, MaxOutput: 2048,
	}
	report, err := RunPilot(t.Context(), config, state, false, adapter)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || len(report.Trials) != 6 || report.Arms["a"].Passed != 0 || report.Arms["b"].Passed != 0 || report.Arms["c"].Passed != 2 || report.AgainstB.Tasks != 1 || report.AgainstB.Pairs != 2 || report.NetWinsOverB != 2 || report.ExpansionEligible || report.IntegrityViolations != 0 {
		t.Fatalf("paired scheduler outcome: %+v", report)
	}
	for _, trial := range report.Trials {
		want := map[string]int{"a": 1, "b": 5, "c": 4}[trial.Arm]
		if len(trial.Runs) != want || trial.Spending.Unknown != 0 {
			t.Fatalf("attempt allocation/accounting: %+v", trial)
		}
		for _, run := range trial.Runs {
			if run.Source != trial.Final.Source {
				t.Fatal("attempt did not receive the original source snapshot")
			}
		}
		if trial.Arm == "b" && !strings.HasSuffix(trial.Final.ID, "-at0") {
			t.Fatal("visible-check ties did not preserve the earliest retry")
		}
	}
	if adapter.calls.Load() != 68 {
		t.Fatalf("executor/editor accounting: %d calls", adapter.calls.Load())
	}
	before := adapter.calls.Load()
	restored, err := RunPilot(t.Context(), config, state, true, adapter)
	if err != nil || !restored.Complete || adapter.calls.Load() != before {
		t.Fatalf("completed resume dispatched work: %+v %v", restored, err)
	}
}
