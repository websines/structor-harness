package skillsmith_test

import (
	"testing"

	"github.com/websines/structor-harness/harness/skillsmith"
	"github.com/websines/structor-harness/harness/skillstore"
	"github.com/websines/structor-harness/learning"
)

func TestReviewRepeatedImprovementKeepsLiveVersion(t *testing.T) {
	ls, _ := newStore(t)
	tools := skillsmith.Tools(ls)
	res := run(t, tools, "make_skill", map[string]any{"name": "deploy", "summary": "deploy safely", "body": "Run tests."})
	if res.IsError {
		t.Fatalf("create: %s", res.Value)
	}
	args := map[string]any{"name": "deploy", "new_body": "Run tests, then canary.", "reason": "Add canary."}
	res = run(t, tools, "improve_skill", args)
	if res.IsError {
		t.Fatalf("first improvement: %s", res.Value)
	}
	if _, err := ls.Promote(skillstore.VersionID("deploy", "Run tests, then canary."), learning.AcceptHuman("reviewer", "approved", true)); err != nil {
		t.Fatal(err)
	}
	res = run(t, tools, "improve_skill", args)
	if res.IsError {
		t.Fatalf("retry: %s", res.Value)
	}
	if live := ls.Recall(""); len(live) != 1 {
		t.Fatalf("repeated identical improvement destroyed the live skill: %+v", live)
	}
}
