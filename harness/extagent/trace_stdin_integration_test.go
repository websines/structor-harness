//go:build integration

package extagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/shell"
	"github.com/websines/structor-harness/harness/subprocess"
	"github.com/websines/structor-harness/record"
	"github.com/websines/structor-harness/sandbox/floor"
)

// These are local scripted protocol CLIs, not installed or paid providers. The
// real production adapter and subprocess runner deliver and capture the bytes.
func TestBuilderWithTraceTaskStdinProtocol(t *testing.T) {
	prompt := "Review the documentation discussing UPDATE inventory SET quantity = 0; and its risks.\n" +
		strings.Repeat("Shared context: 日本語 / café / $() / `literal` / --option-looking text.\n", 6000)
	for _, tc := range []struct {
		name, bin, protocol, session, model string
	}{
		{name: "codex", bin: "codex", session: "stdin-codex-fixture", model: "fixture-codex-model", protocol: `printf '%s\n' '{"type":"thread.started","thread_id":"stdin-codex-fixture","model":"fixture-codex-model"}' '{"type":"item.completed","item":{"type":"agent_message","text":"complete"}}' '{"type":"turn.completed","usage":{"input_tokens":17,"output_tokens":3}}'`},
		{name: "claude-code", bin: "claude", session: "stdin-claude-fixture", model: "fixture-claude-model", protocol: `printf '%s\n' '{"type":"system","session_id":"stdin-claude-fixture","model":"fixture-claude-model"}' '{"type":"assistant","message":{"model":"fixture-claude-model","content":[{"type":"text","text":"complete"}]}}' '{"type":"result","result":"complete","usage":{"input_tokens":17,"output_tokens":3}}'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := installProtocolCLI(t, tc.bin, "cat > received.prompt\nprintf '%s\\n' \"$@\" > received.argv\n"+tc.protocol)
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			casDir := t.TempDir()
			cas, err := record.NewLocalCAS(casDir)
			if err != nil {
				t.Fatal(err)
			}
			var saved record.Digest
			build, err := BuilderWithTraceConfig(tc.name, dir, Config{TurnTimeout: 20 * time.Minute}, func(trace Trace) error {
				encoded, err := json.Marshal(trace)
				if err != nil {
					return err
				}
				saved, err = cas.Put(encoded)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			text, err := build(t.Context(), prompt, "phase:stdin")
			if err != nil || text != "complete" {
				t.Fatalf("natural task did not reach protocol CLI: result=%q error=%v", text, err)
			}
			delivered, err := root.ReadFile("received.prompt")
			if err != nil || !bytes.Equal(delivered, []byte(prompt)) {
				t.Fatalf("stdin task bytes changed: received=%d expected=%d error=%v", len(delivered), len(prompt), err)
			}
			argvData, err := root.ReadFile("received.argv")
			if err != nil {
				t.Fatal(err)
			}
			argv := strings.Split(strings.TrimSuffix(string(argvData), "\n"), "\n")
			want := []string{"exec", "--json", "--sandbox", "workspace-write", "--skip-git-repo-check", "-C", dir, "-"}
			if tc.name == "claude-code" {
				want = []string{"-p", "--permission-mode", "bypassPermissions", "--output-format", "stream-json", "--verbose"}
			}
			if !reflect.DeepEqual(argv, want) {
				t.Fatalf("CLI permissions or stdin arguments changed: got=%q want=%q", argv, want)
			}
			var deny *floor.DenyError
			if err := floor.HardDeny(append(argv, prompt), ""); !errors.As(err, &deny) || deny.Category != floor.DenyDestructiveSQL {
				t.Fatalf("fixture no longer reproduces freeform argv false positive: %v", err)
			}
			reopened, err := record.NewLocalCAS(casDir)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := reopened.Get(saved)
			if err != nil {
				t.Fatal(err)
			}
			var trace Trace
			if err := json.Unmarshal(encoded, &trace); err != nil {
				t.Fatal(err)
			}
			if trace.PromptTransport != "stdin" || trace.PromptBytes != len(prompt) || trace.PromptDigest != string(record.ComputeDigest(delivered)) || trace.TimeoutMS != (20*time.Minute).Milliseconds() {
				t.Fatalf("saved trace lost input identity or budget: %+v", trace)
			}
			if trace.Agent != tc.name || trace.SessionID != tc.session || trace.Model != tc.model || trace.PhaseID != "phase:stdin" || len(trace.Usage) != 1 || trace.ExitCode != 0 || trace.Error != "" || trace.Cancelled || trace.TimedOut || trace.Confinement != hx.ConfineNone || trace.StdoutTruncated || trace.StdoutBytes != len(trace.Stdout) {
				t.Fatalf("stdin transport changed protocol metadata: %+v", trace)
			}
			if bytes.Contains(encoded, []byte(prompt)) {
				t.Fatal("input audit metadata exposed task text")
			}
		})
	}
}

func TestBuilderWithTraceStdinCancellationJoinsWriter(t *testing.T) {
	dir := installProtocolCLI(t, "codex", `sleep 30 & child=$!
printf '%s' "$child" > child.pid
printf '%s\n' '{"type":"thread.started","thread_id":"unread-stdin-fixture"}'
wait`)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var trace Trace
	build, err := BuilderWithTraceConfig("codex", dir, Config{TurnTimeout: 20 * time.Minute}, func(got Trace) error { trace = got; return nil })
	if err != nil {
		t.Fatal(err)
	}
	prompt := strings.Repeat("large unread task input\n", 50000)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := build(ctx, prompt, "phase:stdin-cancel"); done <- err }()
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
		if !errors.Is(err, context.Canceled) || !trace.Cancelled || trace.TimedOut || trace.PromptTransport != "stdin" || trace.PromptBytes != len(prompt) || trace.PromptDigest != string(record.ComputeDigest([]byte(prompt))) {
			t.Fatalf("unread stdin broke cancellation or trace: error=%v trace=%+v", err, trace)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not join the child and blocked stdin writer")
	}
	assertBudgetChildStopped(t, dir)
}

func TestExternalTaskStdinPreservesDestructiveSQLCommandDenial(t *testing.T) {
	for _, sql := range []string{"UPDATE inventory SET quantity = 0;", "DELETE FROM inventory;", "DROP TABLE inventory;"} {
		t.Run(sql, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			command := "touch sql-command-ran; sqlite3 inventory.db '" + sql + "'"
			res := subprocess.New(nil).Spawn(hx.SpawnRequest{Ctx: t.Context(), Dir: dir, Argv: []string{"/bin/sh", "-c", command}, Confinement: hx.ConfineNone})
			if res.ExitCode != -1 || res.Confinement != hx.ConfineRefused || !strings.Contains(res.Err, "hard-deny [destructive-sql]") {
				t.Fatalf("actual SQL command was not refused: %+v", res)
			}
			tool := shell.BashToolAt(shell.New(subprocess.New(nil), nil), dir)
			args, err := json.Marshal(map[string]string{"command": command})
			if err != nil {
				t.Fatal(err)
			}
			result, err := tool.Execute(hx.ToolRunContext{Ctx: t.Context()}, args)
			if err != nil || !result.IsError || !bytes.Contains(result.Value, []byte("hard-deny [destructive-sql]")) {
				t.Fatalf("actual SQL shell tool was not refused: result=%+v error=%v", result, err)
			}
			if _, err := root.Stat("sql-command-ran"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused SQL command executed a side effect: %v", err)
			}
		})
	}
}

func TestTraceWithoutPromptMetadataRemainsReadable(t *testing.T) {
	var trace Trace
	if err := json.Unmarshal([]byte(`{"id":"old-trace","agent":"codex","stdout":"complete","timeoutMs":480000}`), &trace); err != nil {
		t.Fatal(err)
	}
	if trace.ID != "old-trace" || trace.TimeoutMS != DefaultTimeout.Milliseconds() || trace.PromptTransport != "" || trace.PromptBytes != 0 || trace.PromptDigest != "" {
		t.Fatalf("old trace gained invented input identity: %+v", trace)
	}
}
