package skillopt

import (
	"context"
	"strings"
	"testing"

	"github.com/websines/structor-harness/learning"
)

// liveBody returns the current live (non-retired) body for a skill name.
func liveBody(ls *learning.Store, name string) string {
	sk, err := currentSkill(ls, name)
	if err != nil {
		return ""
	}
	return sk.Body
}

func TestApplyRewriteReplacesWholeBody(t *testing.T) {
	got, applied := ApplyEdits("old body", []Edit{{Op: "rewrite", Text: "new body"}})
	if got != "new body" {
		t.Fatalf("rewrite: got %q", got)
	}
	if len(applied) != 1 {
		t.Fatalf("applied = %d, want 1", len(applied))
	}
	// an empty rewrite is a no-op, never a body-wipe
	got2, applied2 := ApplyEdits("keep", []Edit{{Op: "rewrite", Text: ""}})
	if got2 != "keep" || len(applied2) != 0 {
		t.Fatalf("empty rewrite changed body: %q applied=%d", got2, len(applied2))
	}
}

func TestHarvestFailuresFromRecord(t *testing.T) {
	ls := newLearningStore(t)
	if _, err := ls.RecordFailure(learning.Failure{ID: "failure:race", Summary: "rate limit race", Signature: "race", Prevents: "make count+act atomic"}); err != nil {
		t.Fatal(err)
	}
	rs := HarvestFailures(ls, "race")
	if len(rs) != 1 {
		t.Fatalf("harvested %d rollouts, want 1", len(rs))
	}
	if rs[0].Passed {
		t.Fatal("a failure rollout must be Passed=false")
	}
	if rs[0].Task != "rate limit race" || rs[0].Outcome != "make count+act atomic" {
		t.Fatalf("rollout = %+v", rs[0])
	}
	if len(HarvestFailures(ls, "")) != 1 {
		t.Fatal("empty signature should harvest all failures")
	}
}

func TestSweepPromotesOnlyValidatedImprovements(t *testing.T) {
	ls := newLearningStore(t)
	seedPromoted(t, ls, "alpha", "alpha", "alpha v1 missing the keyword")
	seedPromoted(t, ls, "beta", "beta", "beta already atomic")

	heldOut := []learning.Case{{Name: "kw", Want: "atomic"}}

	reports := Sweep(context.Background(), ls,
		StaticOptimizer{Edits: []Edit{{Op: "rewrite", Text: "now atomic and fixed"}}},
		[]SweepItem{
			{Name: "alpha", MaxEdits: 1, HeldOut: heldOut, Eval: containsWant},
			{Name: "beta", MaxEdits: 1, HeldOut: heldOut, Eval: containsWant},
		})

	if len(reports) != 2 {
		t.Fatalf("reports = %d, want 2", len(reports))
	}
	for _, r := range reports {
		if r.Err != "" {
			t.Fatalf("%s: unexpected err %q", r.Name, r.Err)
		}
		if !r.Report.Decision.Promote {
			t.Fatalf("%s: expected promote, got %q", r.Name, r.Report.Decision.Reason)
		}
	}
	if !strings.Contains(liveBody(ls, "alpha"), "atomic") {
		t.Fatal("alpha was not improved by the validated sweep")
	}
}

func TestSweepRejectsRegressionLeavesLiveUnchanged(t *testing.T) {
	ls := newLearningStore(t)
	seedPromoted(t, ls, "gamma", "gamma", "gamma has the atomic keyword")
	heldOut := []learning.Case{{Name: "kw", Want: "atomic"}}

	reports := Sweep(context.Background(), ls,
		StaticOptimizer{Edits: []Edit{{Op: "rewrite", Text: "gamma without it"}}},
		[]SweepItem{{Name: "gamma", MaxEdits: 1, HeldOut: heldOut, Eval: containsWant}})

	if reports[0].Report.Decision.Promote {
		t.Fatal("a regression must not promote")
	}
	if !strings.Contains(liveBody(ls, "gamma"), "atomic") {
		t.Fatal("a rejected edit changed the live skill")
	}
}

// Running Optimize twice with an optimizer that reproduces the (already-improved)
// body must be a NO-OP the second time — never destroy the skill by retiring the
// very version it would re-promote (regression: content-addressed candidate.ID ==
// old.ID). The live skill survives repeated passes.
func TestOptimizeIdempotentOnUnchangedBody(t *testing.T) {
	ls := newLearningStore(t)
	seedPromoted(t, ls, "idem", "idem", "v1 missing keyword")
	heldOut := []learning.Case{{Name: "kw", Want: "atomic"}}
	opt := StaticOptimizer{Edits: []Edit{{Op: "rewrite", Text: "now atomic and fixed"}}}

	r1, err := Optimize(context.Background(), ls, opt, "idem", nil, 1, heldOut, containsWant)
	if err != nil {
		t.Fatal(err)
	}
	if !r1.Decision.Promote {
		t.Fatalf("first pass should promote: %q", r1.Decision.Reason)
	}
	// Second pass: the optimizer reproduces the same body → no change → no-op.
	r2, err := Optimize(context.Background(), ls, opt, "idem", nil, 1, heldOut, containsWant)
	if err != nil {
		t.Fatal(err)
	}
	if r2.NewID != "" {
		t.Fatalf("second pass should be a no-op (no new version), got NewID=%s", r2.NewID)
	}
	if got := liveBody(ls, "idem"); got != "now atomic and fixed" {
		t.Fatalf("live skill was destroyed/changed by an idempotent second pass: %q", got)
	}
}

func TestCleanBodyStripsFences(t *testing.T) {
	if got := cleanBody("Here you go:\n```markdown\n# Skill\nbody\n```\nthanks"); got != "# Skill\nbody" {
		t.Fatalf("cleanBody fenced = %q", got)
	}
	if got := cleanBody("  # plain\nbody  "); got != "# plain\nbody" {
		t.Fatalf("cleanBody plain = %q", got)
	}
}
