//go:build integration

package extagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func installProtocolCLI(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestBuilderWithTraceCapturesStructuredCLI(t *testing.T) {
	dir := installProtocolCLI(t, "codex", `printf '%s\n' '{"type":"thread.started","thread_id":"owned-thread"}' '{"type":"item.completed","item":{"type":"agent_message","text":"result text"}}' '{"type":"turn.completed","usage":{"input_tokens":17,"output_tokens":3}}'
printf 'diagnostic\n' >&2`)
	var trace Trace
	build, err := BuilderWithTrace("codex", dir, func(got Trace) error { trace = got; return nil })
	if err != nil {
		t.Fatal(err)
	}
	text, err := build(t.Context(), "local protocol fixture", "phase:test")
	if err != nil {
		t.Fatal(err)
	}
	if text != "result text" || trace.SessionID != "owned-thread" || len(trace.Usage) != 1 || trace.ExitCode != 0 || trace.Error != "" {
		t.Fatalf("structured CLI result was not captured: text=%q trace=%+v", text, trace)
	}
	if trace.PhaseID != "phase:test" || trace.Agent != "codex" || trace.Confinement != "none" || trace.Stderr != "diagnostic\n" || trace.StdoutBytes != len(trace.Stdout) {
		t.Fatalf("process trace lost execution fields: %+v", trace)
	}
}

func TestBuilderWithTraceRetainsTranscriptAboveSpawnerDefault(t *testing.T) {
	dir := installProtocolCLI(t, "codex", `printf '{"type":"item.started","padding":"'
head -c 400000 /dev/zero | tr '\000' x
printf '%s\n' '"}' '{"type":"item.completed","item":{"type":"agent_message","text":"complete result"}}' '{"type":"turn.completed"}'`)
	var trace Trace
	build, err := BuilderWithTrace("codex", dir, func(got Trace) error { trace = got; return nil })
	if err != nil {
		t.Fatal(err)
	}
	text, err := build(t.Context(), "large transcript fixture", "phase:large")
	if err != nil || text != "complete result" || trace.StdoutTruncated || trace.StdoutBytes <= 256<<10 || trace.StdoutBytes != len(trace.Stdout) {
		t.Fatalf("large transcript lost its terminal result: text=%q error=%v stdout=%d/%d truncated=%v", text, err, len(trace.Stdout), trace.StdoutBytes, trace.StdoutTruncated)
	}
}

func TestBuilderWithTraceRejectsStructuredFailureAtZeroExit(t *testing.T) {
	tests := []struct{ name, agent, bin, event string }{
		{"codex failed turn", "codex", "codex", `{"type":"turn.failed","error":{"message":"provider unavailable"}}`},
		{"codex error event", "codex", "codex", `{"type":"error","message":"provider unavailable"}`},
		{"claude error result", "claude-code", "claude", `{"type":"result","is_error":true,"result":"provider unavailable"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := installProtocolCLI(t, tt.bin, "printf '%s\\n' '"+tt.event+"'")
			var trace Trace
			build, err := BuilderWithTrace(tt.agent, dir, func(got Trace) error { trace = got; return nil })
			if err != nil {
				t.Fatal(err)
			}
			_, err = build(t.Context(), "local failure fixture", "phase:failure")
			if err == nil || !strings.Contains(err.Error(), "provider unavailable") || !strings.Contains(trace.Error, "provider unavailable") || trace.ExitCode != 0 {
				t.Fatalf("structured failed turn was reported as success: error=%v trace=%+v", err, trace)
			}
		})
	}
}

func TestBuilderWithTraceBoundsCaptureAndRejectsIncompleteStdout(t *testing.T) {
	dir := installProtocolCLI(t, "codex", `head -c 4500000 /dev/zero | tr '\000' x
head -c 4500000 /dev/zero | tr '\000' y >&2`)
	var trace Trace
	build, err := BuilderWithTrace("codex", dir, func(got Trace) error { trace = got; return nil })
	if err != nil {
		t.Fatal(err)
	}
	_, err = build(t.Context(), "bounded capture fixture", "phase:bounds")
	if err == nil || !strings.Contains(err.Error(), "truncated") || trace.Error == "" {
		t.Fatalf("truncated transcript was reported as success: %v", err)
	}
	if !trace.StdoutTruncated || !trace.StderrTruncated || trace.StdoutBytes != 4500000 || trace.StderrBytes != 4500000 || len(trace.Stdout) >= trace.StdoutBytes || len(trace.Stderr) >= trace.StderrBytes {
		t.Fatalf("capture limits lost bytes or truncation flags: stdout=%d/%d stderr=%d/%d", len(trace.Stdout), trace.StdoutBytes, len(trace.Stderr), trace.StderrBytes)
	}
}

func TestBuilderWithTraceReturnsPersistenceFailure(t *testing.T) {
	dir := installProtocolCLI(t, "codex", "printf '%s\\n' '{\"type\":\"turn.completed\"}'")
	want := errors.New("trace store unavailable")
	build, err := BuilderWithTrace("codex", dir, func(Trace) error { return want })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := build(t.Context(), "persistence fixture", "phase:persist"); !errors.Is(err, want) {
		t.Fatalf("persistence failure was lost: %v", err)
	}
}

func TestBuilderWithTraceCancellationKillsOwnedCLIProcessTree(t *testing.T) {
	dir := installProtocolCLI(t, "codex", `sleep 30 & child=$!
printf '%s' "$child" > child.pid
wait`)
	var trace Trace
	build, err := BuilderWithTrace("codex", dir, func(got Trace) error { trace = got; return nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := build(ctx, "owned tree fixture", "phase:cancel"); done <- err }()
	var pid int
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for pid == 0 {
		if data, err := os.ReadFile(filepath.Join(dir, "child.pid")); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		if pid != 0 {
			break
		}
		select {
		case <-tick.C:
		case err := <-done:
			t.Fatalf("CLI ended before process ownership was observed: %v", err)
		case <-deadline.C:
			t.Fatal("CLI descendant did not start")
		}
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !trace.Cancelled || trace.TimedOut || trace.Error == "" || trace.ExitCode == 0 {
			t.Fatalf("CLI cancellation lost terminal state: error=%v trace=%+v", err, trace)
		}
	case <-time.After(time.Second):
		t.Fatal("CLI cancellation did not return after tree kill")
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("owned CLI descendant %d survived cancellation", pid)
	}
}
