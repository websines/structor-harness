package improve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/websines/structor-harness/record"
)

type testRunner struct {
	seen         []Profile
	sources      []record.Digest
	failComposed bool
}

func (r *testRunner) Execute(ctx context.Context, task Task, p Profile, id string) (Run, error) {
	r.seen = append(r.seen, p)
	r.sources = append(r.sources, task.Source)
	d, _ := Digest(p)
	result := Run{ID: id, Profile: d, Source: task.Source, Artifact: record.ComputeDigest([]byte(id)), Transcript: record.ComputeDigest([]byte("transcript:" + id))}
	if r.failComposed && len(r.seen) == 4 {
		result.Error = "executor timeout"
	}
	return result, nil
}

type testEditor struct {
	stages []string
	bad    bool
	fail   error
	cancel context.CancelFunc
}

func (e *testEditor) Edit(ctx context.Context, r EditRequest) ([]byte, error) {
	e.stages = append(e.stages, r.Stage)
	if e.cancel != nil {
		e.cancel()
		return nil, ctx.Err()
	}
	if e.fail != nil {
		return nil, e.fail
	}
	if e.bad {
		return []byte(`{"profile":{"schema":1,"base":"changed"},"reason":"trust me","evidence":["unknown"]}`), nil
	}
	if r.Stage == "reflect" {
		return json.Marshal(map[string]any{"conditions": "boundary-sensitive code", "observation": "public run contrast", "limitations": "not independently validated", "evidence": []string{r.Reference.ID, r.Candidates[len(r.Candidates)-1].Run.ID}})
	}
	p := r.Base
	p.Guidance = r.Stage
	return json.Marshal(Decision{Profile: p, Reason: "observed workflow weakness", Evidence: []string{r.Reference.ID}})
}
func testJournal(t *testing.T) *Journal {
	t.Helper()
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	j, err := NewJournal(filepath.Join(t.TempDir(), "journal"), cas)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestRunUsesOriginalSourceAndComposedProfile(t *testing.T) {
	runner := &testRunner{}
	editor := &testEditor{}
	journal := testJournal(t)
	base := Profile{Schema: Schema, Base: record.ComputeDigest([]byte("protected"))}
	task := Task{ID: "case", Family: "repair", Prompt: "repair", Source: record.ComputeDigest([]byte("original code"))}
	loop := Loop{Runner: runner, Editor: editor, Recorder: journal}
	result, err := loop.Run(t.Context(), task, base, "trial", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.seen) != 4 || len(editor.stages) != 4 || result.Final.ID != "trial-composed" || result.Adopted.Guidance != "compose" || result.Lesson == nil {
		t.Fatalf("unexpected outcome: %+v, stages %v", result, editor.stages)
	}
	for _, d := range runner.sources {
		if d != task.Source {
			t.Fatal("candidate inherited changed code")
		}
	}
	var sealed Result
	if _, err := journal.Read("trial-sealed", &sealed); err != nil {
		t.Fatal(err)
	}
	if sealed.Lesson != nil {
		t.Fatal("selection was not sealed before reflection")
	}
}

func TestRunFallsBackWithoutRepairSpending(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bad    bool
		failed bool
		runs   int
	}{{"invalid edit", true, false, 3}, {"composed timeout", false, true, 4}} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &testRunner{failComposed: tc.failed}
			editor := &testEditor{bad: tc.bad}
			loop := Loop{Runner: runner, Editor: editor, Recorder: testJournal(t)}
			result, err := loop.Run(t.Context(), Task{ID: "case", Source: record.ComputeDigest([]byte("source"))}, Profile{Schema: 1, Base: record.ComputeDigest([]byte("base"))}, "trial", nil)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Fallback || result.Final.ID != "trial-reference" || len(runner.seen) != tc.runs {
				t.Fatalf("bad fallback: %+v", result)
			}
		})
	}
}

func TestRunCancelledDoesNotDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := &testRunner{}
	l := Loop{Runner: r, Editor: &testEditor{}, Recorder: testJournal(t)}
	_, err := l.Run(ctx, Task{ID: "case", Source: record.ComputeDigest([]byte("s"))}, Profile{Schema: 1, Base: record.ComputeDigest([]byte("b"))}, "trial", nil)
	if !errors.Is(err, context.Canceled) || len(r.seen) != 0 {
		t.Fatalf("err %v, calls %d", err, len(r.seen))
	}
}

func TestResourceFailureSealsReferenceWithoutFurtherDispatch(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprint(cancel), func(t *testing.T) {
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			editor := &testEditor{fail: ErrBudget}
			if cancel {
				editor.cancel = stop
			}
			runner := &testRunner{}
			journal := testJournal(t)
			loop := Loop{Runner: runner, Editor: editor, Recorder: journal}
			result, err := loop.Run(ctx, Task{ID: "case", Source: record.ComputeDigest([]byte("source"))}, Profile{Schema: 1, Base: record.ComputeDigest([]byte("base"))}, "trial", nil)
			if err != nil || !result.Fallback || result.Final.ID != "trial-reference" || len(runner.seen) != 1 || len(editor.stages) != 1 {
				t.Fatalf("resource failure did not retain reference: %+v %v", result, err)
			}
			var sealed Result
			if _, err := journal.Read("trial-sealed", &sealed); err != nil || sealed.Final.ID != result.Final.ID || !sealed.Fallback {
				t.Fatalf("fallback not durable: %+v %v", sealed, err)
			}
		})
	}
}
