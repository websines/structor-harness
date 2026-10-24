// Black-box test (package memory_test): drive skills and failures through the
// REAL append-only record — record.NewLocalCAS + flg.NewStore + learning.NewStore
// — so every assertion is about facts that survived a round-trip through the
// canonical record, and through the real ToolRuntime the Component wires.
package memory_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/memory"
	"github.com/websines/structor-harness/harness/skill"
	"github.com/websines/structor-harness/harness/tools"
	"github.com/websines/structor-harness/learning"
	"github.com/websines/structor-harness/record"
	"github.com/websines/structor-harness/runtime"
)

// newStore wires a learning store over a fresh local CAS and its flg record.
func newStore(t *testing.T) *learning.Store {
	t.Helper()
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return learning.NewStore(flg.NewStore(cas))
}

// writeFlat writes a flat <name>.md skill file into dir.
func writeFlat(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// 1. RecordSkills folds skills as candidates, they recall by title AND body, and a
// second fold of the same skills adds nothing (idempotent-ish).
func TestRecordSkills(t *testing.T) {
	ls := newStore(t)
	skills := []skill.Skill{
		{Name: "deploy", Description: "Deploy the app.", Body: "Run the deploy steps.", Source: "/roots/deploy.md"},
		{Name: "build", Description: "Build the app.", Body: "Compile everything.", Source: "/roots/build.md"},
	}

	ids := memory.RecordSkills(ls, skills)
	if len(ids) != 2 {
		t.Fatalf("RecordSkills should record 2 ids, got %d: %v", len(ids), ids)
	}

	// Both descriptions surface as candidate summaries.
	sums := strings.Join(ls.Summaries(""), "|")
	if !strings.Contains(sums, "Deploy the app.") || !strings.Contains(sums, "Build the app.") {
		t.Fatalf("Summaries missing folded skills: %s", sums)
	}

	recalled := ls.Recall("")
	if len(recalled) != 2 {
		t.Fatalf("Recall should return 2 skills, got %d", len(recalled))
	}
	for _, sk := range recalled {
		if sk.State != learning.SkillCandidate {
			t.Fatalf("a folded skill must be a candidate, got %s", sk.State)
		}
	}

	// The body was folded too: a body-substring query recalls the right skill.
	if got := ls.Recall("Compile"); len(got) != 1 || got[0].Title != "Build the app." {
		t.Fatalf("body should be folded and recallable: %+v", got)
	}
	// Provenance travels with the fact.
	if got := ls.Recall("Deploy"); len(got) != 1 || strings.Join(got[0].Provenance, ",") != "/roots/deploy.md" {
		t.Fatalf("provenance not carried: %+v", got)
	}

	// Idempotent-ish: re-folding the same discovery records nothing new.
	if again := memory.RecordSkills(ls, skills); len(again) != 0 {
		t.Fatalf("second RecordSkills should record nothing new, got %v", again)
	}
	if got := len(ls.Recall("")); got != 2 {
		t.Fatalf("Recall should still return 2 (no duplication), got %d", got)
	}
}

// 2. RecordFailure folds a failure fact that recalls by signature with the expected
// Summary ("<action>: <reason>") and Prevents.
func TestRecordFailure(t *testing.T) {
	ls := newStore(t)
	const sig = "abc123def4567890"

	if err := memory.RecordFailure(ls, "bash", "exit 1: permission denied", sig, "check file permissions before running"); err != nil {
		t.Fatal(err)
	}

	got := ls.RecallFailures(sig)
	if len(got) != 1 {
		t.Fatalf("RecallFailures(sig) should return 1 failure, got %d", len(got))
	}
	f := got[0]
	if f.Summary != "bash: exit 1: permission denied" {
		t.Fatalf("Summary = %q, want %q", f.Summary, "bash: exit 1: permission denied")
	}
	if f.Prevents != "check file permissions before running" {
		t.Fatalf("Prevents = %q, want the guidance", f.Prevents)
	}
	if f.Signature != sig {
		t.Fatalf("Signature = %q, want %q", f.Signature, sig)
	}
}

// 3. FailureMiddleware records a failure fact on IsError and nothing on success.
func TestFailureMiddleware(t *testing.T) {
	ls := newStore(t)
	mw := memory.FailureMiddleware(ls)

	// A failing call records a failure.
	failExec := mw(func(in hx.ToolExecutionInput) hx.ToolResult {
		return hx.ToolResult{IsError: true, Value: []byte(`{"error":"boom"}`)}
	})
	failExec(hx.ToolExecutionInput{Name: "grep", Args: []byte(`{"q":"x"}`)})

	fs := ls.RecallFailures("")
	if len(fs) != 1 {
		t.Fatalf("a failing tool call should record 1 failure, got %d", len(fs))
	}
	if !strings.Contains(fs[0].Summary, "grep") || !strings.Contains(fs[0].Summary, "boom") {
		t.Fatalf("failure summary should carry tool + error excerpt: %q", fs[0].Summary)
	}

	// A success records nothing more.
	okExec := mw(func(in hx.ToolExecutionInput) hx.ToolResult {
		return hx.ToolResult{Value: []byte(`{"ok":true}`)}
	})
	okExec(hx.ToolExecutionInput{Name: "grep", Args: []byte(`{"q":"y"}`)})
	if got := len(ls.RecallFailures("")); got != 1 {
		t.Fatalf("a success must record nothing (still 1 expected), got %d", got)
	}
}

// 4. Component: a real ToolRuntime provided under KeyTools (mirroring spine's tools
// mount) plus memory.Component folds the discovered skill as a candidate and
// installs the failure middleware — a failing tool call routed through the runtime
// records a failure fact.
func TestComponent(t *testing.T) {
	ls := newStore(t)

	// A temp skill dir with one flat skill to discover.
	root := t.TempDir()
	writeFlat(t, root, "greet", "---\nname: greet\ndescription: Greet the user.\n---\nSay hello to the user.")
	roots := []skill.Root{{Dir: root, Rank: skill.RankProject}}

	reg := runtime.New()

	// Mirror spine.Build's tools mount: a tools.Runtime under KeyTools, with a
	// failing tool registered so a call can trip the failure middleware.
	reg.Load(&runtime.Component{
		Name:    "harness/tools",
		Provide: []string{hx.KeyTools},
		Apply: func(ctx *runtime.Context) error {
			tr := tools.New(tools.Config{})
			if _, err := tr.Register(hx.ToolDefinition{
				Schema: hx.ToolSchema{Name: "flaky"},
				Execute: func(rc hx.ToolRunContext, args []byte) (hx.ToolResult, error) {
					return hx.ToolResult{IsError: true, Value: []byte(`{"error":"still broken"}`)}, nil
				},
			}); err != nil {
				return err
			}
			return ctx.Provide(hx.KeyTools, tr)
		},
	}, "")

	reg.Load(memory.Component(ls, roots), "")

	// The memory component must have activated once KeyTools was provided.
	if f := reg.Fiber("harness/memory"); f == nil || f.State() != runtime.StateActive {
		t.Fatalf("memory component did not activate: %v", f)
	}

	// The discovered skill folded into the learning store as a candidate.
	recalled := ls.Recall("")
	if len(recalled) != 1 || recalled[0].State != learning.SkillCandidate {
		t.Fatalf("Component should fold the discovered skill as a candidate: %+v", recalled)
	}
	if recalled[0].Title != "Greet the user." {
		t.Fatalf("folded skill title = %q, want the skill description", recalled[0].Title)
	}

	// The failure middleware is installed: a failing tool call records a failure.
	v, ok := reg.Lookup(hx.KeyTools)
	if !ok {
		t.Fatal("KeyTools not provided")
	}
	tr := v.(hx.ToolRuntime)
	if res := tr.Execute(hx.ToolExecutionInput{Name: "flaky", Args: []byte(`{"x":1}`), Initiator: "agent"}); !res.IsError {
		t.Fatal("flaky tool should return an error result")
	}

	fs := ls.RecallFailures("")
	if len(fs) != 1 {
		t.Fatalf("the installed failure middleware should record the failing call, got %d", len(fs))
	}
	if !strings.Contains(fs[0].Summary, "flaky") || !strings.Contains(fs[0].Summary, "still broken") {
		t.Fatalf("recorded failure should carry tool + error excerpt: %q", fs[0].Summary)
	}
}
