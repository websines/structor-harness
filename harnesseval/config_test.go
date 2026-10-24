package harnesseval

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/improve"
)

type forbiddenAdapter struct{ calls int }

func (f *forbiddenAdapter) Route() string { return "fixture" }
func (f *forbiddenAdapter) Stream(context.Context, hx.GenerateOptions) (<-chan hx.StreamChunk, error) {
	f.calls++
	return nil, nil
}

func TestPilotRequiresGraderValidationBeforeCalls(t *testing.T) {
	config := Config{Schema: 1, Project: "fixture", Model: "fixture", BaseURL: "http://127.0.0.1:1234", APIKeyEnv: "PILOT_KEY", Rates: improve.Rates{Input: 1, Output: 1}, Split: "development", Repeats: 1, Trial: improve.Limits{Input: 10000, Output: 10000, USD: 1, Calls: 10}, Total: improve.Limits{Input: 100000, Output: 100000, USD: 10, Calls: 100}, TimeoutSeconds: 60, TrialSeconds: 30, ExecutionSeconds: 10, MaxSteps: 4, MaxOutput: 1000}
	adapter := &forbiddenAdapter{}
	if _, err := RunPilot(t.Context(), config, filepath.Join(t.TempDir(), "state"), false, adapter); err == nil {
		t.Fatal("unvalidated experiment admitted")
	}
	if adapter.calls != 0 {
		t.Fatal("model call before preflight")
	}
}
