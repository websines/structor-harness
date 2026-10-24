//go:build integration

package extagent

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These fixtures execute real owned subprocesses with scripted CLI protocol
// output. They do not invoke paid providers or replace the production spawner.
func TestBuilderWithTraceConfigReportsSelectedBudget(t *testing.T) {
	for _, tc := range []struct {
		name             string
		configured, want time.Duration
	}{
		{name: "zero preserves default", want: DefaultTimeout},
		{name: "longer turn", configured: 20 * time.Minute, want: 20 * time.Minute},
		{name: "maximum turn", configured: MaxTimeout, want: MaxTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := installProtocolCLI(t, "codex", `printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"complete"}}' '{"type":"turn.completed"}'`)
			var trace Trace
			build, err := BuilderWithTraceConfig("codex", dir, Config{TurnTimeout: tc.configured}, func(got Trace) error { trace = got; return nil })
			if err != nil {
				t.Fatal(err)
			}
			text, err := build(t.Context(), "scripted protocol fixture", "phase:budget")
			if err != nil || text != "complete" || trace.TimeoutMS != tc.want.Milliseconds() || trace.Cancelled || trace.TimedOut || trace.StdoutTruncated || trace.Confinement != "none" || trace.ExitCode != 0 {
				t.Fatalf("selected budget changed execution or trace: result=%q error=%v trace=%+v", text, err, trace)
			}
		})
	}
}

func TestBuilderWithTraceConfigTimeoutKillsOwnedProcessTree(t *testing.T) {
	dir := installProtocolCLI(t, "codex", `sleep 30 & child=$!
printf '%s' "$child" > child.pid
printf '%s\n' '{"type":"thread.started","thread_id":"timeout-fixture"}'
wait`)
	var trace Trace
	const budget = 2 * time.Second
	build, err := BuilderWithTraceConfig("codex", dir, Config{TurnTimeout: budget}, func(got Trace) error { trace = got; return nil })
	if err != nil {
		t.Fatal(err)
	}
	_, err = build(t.Context(), "owned timeout fixture", "phase:timeout")
	if !errors.Is(err, context.DeadlineExceeded) || !trace.TimedOut || trace.Cancelled || trace.TimeoutMS != budget.Milliseconds() || trace.ExitCode == 0 || trace.Error == "" || trace.Stdout == "" {
		t.Fatalf("configured deadline lost terminal state or partial transcript: error=%v trace=%+v", err, trace)
	}
	assertBudgetChildStopped(t, dir)
}

func TestBuilderWithTraceConfigLongBudgetPreservesParentCancellation(t *testing.T) {
	dir := installProtocolCLI(t, "codex", `sleep 30 & child=$!
printf '%s' "$child" > child.pid
printf '%s\n' '{"type":"thread.started","thread_id":"cancel-fixture"}'
wait`)
	var trace Trace
	const budget = 20 * time.Minute
	build, err := BuilderWithTraceConfig("codex", dir, Config{TurnTimeout: budget}, func(got Trace) error { trace = got; return nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := build(ctx, "owned cancellation fixture", "phase:cancel-long"); done <- err }()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := root.Stat("child.pid"); err == nil {
			break
		}
		select {
		case <-tick.C:
		case err := <-done:
			t.Fatalf("CLI finished before owned descendant started: %v", err)
		case <-timer.C:
			t.Fatal("owned descendant did not start")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !trace.Cancelled || trace.TimedOut || trace.TimeoutMS != budget.Milliseconds() || trace.ExitCode == 0 || trace.Error == "" {
			t.Fatalf("long budget broke parent cancellation: error=%v trace=%+v", err, trace)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not promptly join the owned CLI")
	}
	assertBudgetChildStopped(t, dir)
}

func assertBudgetChildStopped(t *testing.T, dir string) {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	data, err := root.ReadFile("child.pid")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid owned child pid %q: %v", data, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("owned descendant %d survived timeout/cancellation", pid)
	}
}
