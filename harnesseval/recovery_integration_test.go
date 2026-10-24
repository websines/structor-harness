//go:build integration

package harnesseval

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/websines/structor-harness/harness/improve"
	"github.com/websines/structor-harness/record"
)

// This receipt is a controlled lifecycle-test fixture, not evidence that a
// provider or the behavioral catalog has been evaluated by this test.
func lifecycleState(t *testing.T) string {
	t.Helper()
	state := filepath.Join(t.TempDir(), "experiment")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	cas, err := record.NewLocalCAS(filepath.Join(state, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := improve.NewJournal(filepath.Join(state, "journal"), cas)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := executableProvenance()
	if err != nil {
		t.Fatal(err)
	}
	_, err = journal.Record(t.Context(), "validation", Validation{Schema: 1, Dataset: record.ComputeDigest(catalog), Fixtures: 24, Checks: 96, Checker: "blackbox-v1", Executable: executable.Digest})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestCrashResumeHelper(t *testing.T) {
	if os.Getenv("FORGE_CRASH_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	data, err := os.ReadFile(os.Getenv("FORGE_CRASH_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	var config Config
	if err = improve.Decode(data, &config); err != nil {
		t.Fatal(err)
	}
	_, err = RunPilot(t.Context(), config, os.Getenv("FORGE_CRASH_STATE"), false, nil)
	t.Fatalf("helper should have been killed while request was active: %v", err)
}

func TestCrashResumeDoesNotReplayDispatchedRequest(t *testing.T) {
	requested := make(chan struct{})
	ended := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		once.Do(func() { close(requested) })
		<-r.Context().Done()
		close(ended)
	}))
	defer server.Close()
	state := lifecycleState(t)
	config := Config{Schema: 1, Project: "crash-fixture", Model: "fixture", BaseURL: server.URL, APIKeyEnv: "FORGE_PILOT_TEST_KEY", Rates: improve.Rates{Input: 1, Output: 1}, Split: "development", Repeats: 1, Trial: improve.Limits{Input: 100000, Output: 10000, USD: 1, Calls: 20}, Total: improve.Limits{Input: 1000000, Output: 100000, USD: 10, Calls: 100}, TimeoutSeconds: 60, TrialSeconds: 30, ExecutionSeconds: 30, MaxSteps: 4, MaxOutput: 1000}
	data, _ := json.Marshal(config)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCrashResumeHelper$")
	childTemp := filepath.Join(state, "tmp")
	if err := os.Mkdir(childTemp, 0700); err != nil {
		t.Fatal(err)
	}
	command.Env = append(os.Environ(), "FORGE_CRASH_HELPER=1", "FORGE_CRASH_STATE="+state, "FORGE_CRASH_CONFIG="+configPath, "FORGE_PILOT_TEST_KEY=protocol-fixture-key", "TMPDIR="+childTemp)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill() })
	select {
	case <-requested:
	case <-time.After(120 * time.Second):
		command.Process.Kill()
		command.Wait()
		t.Fatal("no dispatched request")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("killed process unexpectedly succeeded")
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("provider connection outlived the process")
	}
	adapter := &forbiddenAdapter{}
	report, err := RunPilot(t.Context(), config, state, true, adapter)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 0 || report.Complete || len(report.Trials) != 1 || report.Trials[0].Spending.Unknown != 1 || !strings.Contains(report.StopReason, "uncertain original trial") {
		t.Fatalf("uncertain request replayed/misreported: calls %d report %+v", adapter.calls, report)
	}
	// Report retrieval after interruption is also idempotent and requires no key.
	second, err := RunPilot(t.Context(), config, state, true, adapter)
	if err != nil || second.StopReason != report.StopReason || adapter.calls != 0 {
		t.Fatalf("second resume: %+v %v", second, err)
	}
}
