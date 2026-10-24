package selfimprove

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/skillstore"
	"github.com/websines/structor-harness/learning"
	"github.com/websines/structor-harness/record"
	"github.com/websines/structor-harness/skillopt"
)

func newLS(t *testing.T) *learning.Store {
	t.Helper()
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return learning.NewStore(flg.NewStore(cas))
}

func seed(t *testing.T, ls *learning.Store, name, body string) {
	t.Helper()
	if _, err := ls.AddCandidate(learning.Skill{ID: skillstore.SkillID(name), Title: name, Body: body}); err != nil {
		t.Fatal(err)
	}
	if _, err := ls.Promote(skillstore.SkillID(name), learning.AcceptHuman("principal:test", "seed", true)); err != nil {
		t.Fatal(err)
	}
}

func contains(sk learning.Skill, c learning.Case) bool { return strings.Contains(sk.Body, c.Want) }

// A pass promotes a validated improvement for a skill that has evals, and leaves a
// skill WITHOUT evals untouched (no fiat).
func TestRunOncePromotesOnlyEvaledSkills(t *testing.T) {
	ls := newLS(t)
	seed(t, ls, "with-evals", "v1 lacks the keyword")
	seed(t, ls, "no-evals", "this skill has no declared evals")
	// a recorded failure = the workings the pass optimizes against
	if _, err := ls.RecordFailure(learning.Failure{ID: "failure:x", Signature: "x", Summary: "bad", Prevents: "add atomic"}); err != nil {
		t.Fatal(err)
	}

	evals := MapEvals{"with-evals": {HeldOut: []learning.Case{{Name: "kw", Want: "atomic"}}, Eval: contains}}
	opt := skillopt.StaticOptimizer{Edits: []skillopt.Edit{{Op: "rewrite", Text: "now atomic and fixed"}}}
	s := New(ls, opt, evals, 1)

	reports := s.RunOnce(context.Background())
	if len(reports) != 1 {
		t.Fatalf("expected 1 eligible skill, got %d", len(reports))
	}
	if reports[0].Name != "with-evals" || !reports[0].Report.Decision.Promote {
		t.Fatalf("with-evals should have promoted: %+v", reports[0])
	}
	// with-evals improved
	if !strings.Contains(liveBody(ls, "with-evals"), "atomic") {
		t.Fatal("with-evals was not improved")
	}
	// no-evals untouched
	if liveBody(ls, "no-evals") != "this skill has no declared evals" {
		t.Fatal("no-evals must not be changed (it had no evals to gate on)")
	}
}

// A regression (an edit that fails the held-out set) is not admitted; the live
// skill is unchanged — the pass can only improve or hold, never worsen.
func TestRunOnceHoldsRegression(t *testing.T) {
	ls := newLS(t)
	seed(t, ls, "guarded", "guarded already atomic")
	evals := MapEvals{"guarded": {HeldOut: []learning.Case{{Name: "kw", Want: "atomic"}}, Eval: contains}}
	opt := skillopt.StaticOptimizer{Edits: []skillopt.Edit{{Op: "rewrite", Text: "rewrite dropping the keyword"}}}
	s := New(ls, opt, evals, 1)

	reports := s.RunOnce(context.Background())
	if len(reports) != 1 || reports[0].Report.Decision.Promote {
		t.Fatalf("regression must not promote: %+v", reports)
	}
	if !strings.Contains(liveBody(ls, "guarded"), "atomic") {
		t.Fatal("a rejected pass changed the live skill")
	}
}

// The middleware schedules a pass exactly when a wrapped tool returns an error.
func TestMiddlewareTriggersOnToolError(t *testing.T) {
	var fires int64
	s := New(newLS(t), skillopt.StaticOptimizer{}, MapEvals{}, 1)
	s.trigger = func() { atomic.AddInt64(&fires, 1) } // observe synchronously

	mw := Middleware(s)
	okFn := mw(func(hx.ToolExecutionInput) hx.ToolResult { return hx.ToolResult{} })
	errFn := mw(func(hx.ToolExecutionInput) hx.ToolResult { return hx.ToolResult{IsError: true} })

	okFn(hx.ToolExecutionInput{})
	if got := atomic.LoadInt64(&fires); got != 0 {
		t.Fatalf("a successful tool must not trigger a pass, fires=%d", got)
	}
	errFn(hx.ToolExecutionInput{})
	if got := atomic.LoadInt64(&fires); got != 1 {
		t.Fatalf("a failed tool must trigger exactly one pass, fires=%d", got)
	}
}

// Under WithCadence(3) the middleware throttles the automatic pass to every 3rd
// failure: two errors fire nothing, the third fires exactly once (count-based, no
// clock). The overridden s.trigger observes firings synchronously (no goroutines).
func TestMiddlewareCadenceThrottlesToEveryNthFailure(t *testing.T) {
	var fires int64
	s := New(newLS(t), skillopt.StaticOptimizer{}, MapEvals{}, 1).WithCadence(3)
	s.trigger = func() { atomic.AddInt64(&fires, 1) }

	errFn := Middleware(s)(func(hx.ToolExecutionInput) hx.ToolResult { return hx.ToolResult{IsError: true} })

	errFn(hx.ToolExecutionInput{})
	errFn(hx.ToolExecutionInput{})
	if got := atomic.LoadInt64(&fires); got != 0 {
		t.Fatalf("two failures under cadence 3 must not fire, fires=%d", got)
	}
	errFn(hx.ToolExecutionInput{}) // the 3rd failure fires
	if got := atomic.LoadInt64(&fires); got != 1 {
		t.Fatalf("the 3rd failure under cadence 3 must fire exactly once, fires=%d", got)
	}
	// three more failures → exactly one more fire (on the 6th).
	errFn(hx.ToolExecutionInput{})
	errFn(hx.ToolExecutionInput{})
	errFn(hx.ToolExecutionInput{})
	if got := atomic.LoadInt64(&fires); got != 2 {
		t.Fatalf("six failures under cadence 3 must fire exactly twice, fires=%d", got)
	}
}

// WithCadence(1) (and the default) preserve the current behavior: every failure
// fires a pass.
func TestMiddlewareCadenceOneFiresEveryFailure(t *testing.T) {
	var fires int64
	s := New(newLS(t), skillopt.StaticOptimizer{}, MapEvals{}, 1).WithCadence(1)
	s.trigger = func() { atomic.AddInt64(&fires, 1) }

	errFn := Middleware(s)(func(hx.ToolExecutionInput) hx.ToolResult { return hx.ToolResult{IsError: true} })
	errFn(hx.ToolExecutionInput{})
	errFn(hx.ToolExecutionInput{})
	errFn(hx.ToolExecutionInput{})
	if got := atomic.LoadInt64(&fires); got != 3 {
		t.Fatalf("cadence 1 must fire on every failure, fires=%d", got)
	}
}

// Trigger is the explicit escape hatch: it schedules a pass regardless of the
// cadence counter. Even after a single failure under cadence 3 (which fires
// nothing automatically), calling Trigger() still fires.
func TestTriggerBypassesCadence(t *testing.T) {
	var fires int64
	s := New(newLS(t), skillopt.StaticOptimizer{}, MapEvals{}, 1).WithCadence(3)
	s.trigger = func() { atomic.AddInt64(&fires, 1) }

	errFn := Middleware(s)(func(hx.ToolExecutionInput) hx.ToolResult { return hx.ToolResult{IsError: true} })
	errFn(hx.ToolExecutionInput{}) // 1 failure < cadence 3: no automatic fire
	if got := atomic.LoadInt64(&fires); got != 0 {
		t.Fatalf("one failure under cadence 3 must not auto-fire, fires=%d", got)
	}
	s.Trigger() // explicit flush: fires regardless of the counter
	if got := atomic.LoadInt64(&fires); got != 1 {
		t.Fatalf("Trigger() must fire regardless of cadence, fires=%d", got)
	}
}

// scheduleAsync is single-flight: overlapping triggers coalesce, and Wait blocks
// until the in-flight pass completes.
func TestScheduleAsyncSingleFlightAndWait(t *testing.T) {
	ls := newLS(t)
	seed(t, ls, "async-skill", "v1 lacks kw")
	evals := MapEvals{"async-skill": {HeldOut: []learning.Case{{Name: "kw", Want: "atomic"}}, Eval: contains}}
	opt := skillopt.StaticOptimizer{Edits: []skillopt.Edit{{Op: "rewrite", Text: "atomic now"}}}
	s := New(ls, opt, evals, 1)

	s.trigger() // schedule
	s.trigger() // coalesced (single-flight)
	s.Wait()

	if len(s.Last()) != 1 || !s.Last()[0].Report.Decision.Promote {
		t.Fatalf("async pass should have promoted async-skill: %+v", s.Last())
	}
}

func liveBody(ls *learning.Store, name string) string {
	for _, sk := range ls.Recall(name) {
		if skillstore.SkillName(sk.ID) == name && sk.State != learning.SkillRetired {
			return sk.Body
		}
	}
	for _, sk := range ls.Recall("") {
		if skillstore.SkillName(sk.ID) == name && sk.State != learning.SkillRetired {
			return sk.Body
		}
	}
	return ""
}
