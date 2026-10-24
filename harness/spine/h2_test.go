package spine_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/llm"
	"github.com/websines/structor-harness/harness/session"
	"github.com/websines/structor-harness/harness/spine"
	"github.com/websines/structor-harness/record"
	"github.com/websines/structor-harness/runtime"
)

// TestH2RealCommandThroughHarness drives a REAL turn where the model calls the
// bash tool and the harness actually runs the command on the host (through the
// subprocess capability, confined by the sandbox when available), then edits a
// real file through the fs tool. It proves H2: the agent's tools do real work,
// and the real output comes back as a fact in the record.
func TestH2RealCommandThroughHarness(t *testing.T) {
	// The subprocess capability is fail-closed: on a host with NO confinement
	// mechanism (a plain linux container — this repo's CI is one) it refuses to run
	// a model-chosen command at all. What this test is about is that the harness
	// really executes and really records, not whether this particular host can
	// confine, so it takes the operator opt-in explicitly. Where a mechanism DOES
	// exist (darwin/seatbelt) the flag changes nothing — the command still runs
	// confined. Without it the test would pass for the wrong reason: the refusal
	// message quotes the argv, so it CONTAINS "forge-was-here" while nothing ran.
	t.Setenv("FORGE_ALLOW_UNCONFINED_EXEC", "1")

	work := t.TempDir()
	// A file the model will edit via edit_file.
	target := filepath.Join(work, "note.txt")
	if err := os.WriteFile(target, []byte("status: TODO\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// The model's script: (1) call bash `echo forge-was-here`; (2) after seeing
	// the output, read the file; (3) after reading, edit it; (4) report done.
	fake := &llm.FakeAdapter{Script: [][]hx.StreamChunk{
		{
			{Kind: hx.ChunkToolCallDelta, BlockIndex: 0, ToolCallID: "c1", ToolName: "bash", ArgsDelta: `{"command":"echo forge-was-here"}`},
			{Kind: hx.ChunkFinish, Finish: hx.FinishToolCalls},
		},
		{
			{Kind: hx.ChunkToolCallDelta, BlockIndex: 0, ToolCallID: "c2", ToolName: "read_file", ArgsDelta: `{"path":"note.txt"}`},
			{Kind: hx.ChunkFinish, Finish: hx.FinishToolCalls},
		},
		{
			{Kind: hx.ChunkToolCallDelta, BlockIndex: 0, ToolCallID: "c3", ToolName: "edit_file", ArgsDelta: `{"path":"note.txt","old_string":"TODO","new_string":"DONE"}`},
			{Kind: hx.ChunkFinish, Finish: hx.FinishToolCalls},
		},
		{
			{Kind: hx.ChunkTextDelta, BlockIndex: 0, Delta: "Ran the command and marked the note DONE."},
			{Kind: hx.ChunkFinish, Finish: hx.FinishStop},
		},
	}}

	reg := runtime.New()
	if err := spine.Build(reg, spine.Options{
		CAS:      cas,
		Adapter:  fake,
		Actor:    session.Config{PrincipalID: "principal:h2", Commit: "c0ffee"},
		Exec:     true,
		WorkDir:  work,
		Sections: []spine.Section{{Order: 0, Text: "You are a Forge agent with shell and file tools."}},
	}); err != nil {
		t.Fatal(err)
	}

	factory, err := spine.Factory(reg)
	if err != nil {
		t.Fatalf("harness (with exec) did not come up on cordis: %v", err)
	}
	handle, err := factory.Create(hx.AgentConfig{
		Provider: fake.Route(), Model: "m", SessionID: "h2", PrincipalID: "principal:h2",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Dispose()

	handle.Agent().Followup("run echo, then mark the note done")
	if err := handle.Agent().WhenIdle(context.Background()); err != nil {
		t.Fatalf("WhenIdle: %v", err)
	}

	// The bash tool actually ran on the host: its real stdout is in the log.
	store, _ := spine.Store(reg)
	sess, _ := store.Get("h2")
	msgs := sess.DeriveMessages()
	if !anyMessageContains(msgs, "forge-was-here") {
		t.Fatal("the real command output 'forge-was-here' is not in the transcript — bash did not run for real")
	}
	// ...and it is the command's OUTPUT, not a refusal quoting the command back.
	if anyMessageContains(msgs, "refusing to spawn") {
		t.Fatal("bash was refused, not run — the assertion above matched the refusal's echo of the argv")
	}

	// The fs tool actually edited the file on disk.
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "status: DONE") {
		t.Fatalf("edit_file did not modify the real file on disk: %q", string(after))
	}

	// Three tool results, all recorded as facts.
	events := sess.Events()
	toolResults := 0
	for _, ev := range events {
		if ev.Kind == hx.EvToolResult {
			toolResults++
			if ev.FactID == "" {
				t.Fatal("tool result not recorded to the content-addressed log")
			}
		}
	}
	if toolResults != 3 {
		t.Fatalf("expected 3 tool results (bash, read_file, edit_file), got %d", toolResults)
	}
}
