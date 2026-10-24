// Black-box tests (package skillsmith_test): run the make_skill / improve_skill
// tools against the REAL append-only record and assert on facts that survived a
// round-trip (Law 1). improve_skill is checked attack-is-recorded style: the old
// version is superseded, never deleted — both its obligation and the supersession
// fact remain in the record.
package skillsmith_test

import (
	"encoding/json"
	"testing"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/skillsmith"
	"github.com/websines/structor-harness/harness/skillstore"
	"github.com/websines/structor-harness/learning"
	"github.com/websines/structor-harness/record"
)

// newStore wires a learning store over a fresh local CAS and its flg record,
// returning both so a test can inspect the raw facts too.
func newStore(t *testing.T) (*learning.Store, *flg.Store) {
	t.Helper()
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	facts := flg.NewStore(cas)
	return learning.NewStore(facts), facts
}

// run executes the named skillsmith tool with args and returns its result.
func run(t *testing.T, tools []hx.ToolDefinition, name string, args map[string]any) hx.ToolResult {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range tools {
		if d.Schema.Name == name {
			res, err := d.Execute(hx.ToolRunContext{}, b)
			if err != nil {
				t.Fatalf("%s returned a Go error (must be total): %v", name, err)
			}
			return res
		}
	}
	t.Fatalf("no tool named %q in %d tools", name, len(tools))
	return hx.ToolResult{}
}

// decode unmarshals a successful tool result's JSON Value into a map.
func decode(t *testing.T, res hx.ToolResult) map[string]any {
	t.Helper()
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", res.Value)
	}
	var m map[string]any
	if err := json.Unmarshal(res.Value, &m); err != nil {
		t.Fatalf("result value is not JSON: %v (%s)", err, res.Value)
	}
	return m
}

// TestMakeSkillAppendsCandidate: make_skill records a candidate fact — recallable
// as a candidate, not active until promoted.
func TestMakeSkillAppendsCandidate(t *testing.T) {
	ls, _ := newStore(t)
	tools := skillsmith.Tools(ls)

	res := run(t, tools, "make_skill", map[string]any{
		"name":       "deploy-safely",
		"summary":    "deploy safely with smoke tests",
		"body":       "always run smoke tests before promoting a deploy",
		"provenance": "run:R-9",
	})
	out := decode(t, res)
	if out["skill_id"] != skillstore.SkillID("deploy-safely") {
		t.Fatalf("skill_id = %v, want %q", out["skill_id"], skillstore.SkillID("deploy-safely"))
	}
	if out["state"] != "candidate" {
		t.Fatalf("a made skill must be a candidate, got state=%v", out["state"])
	}

	got := ls.Recall("smoke tests")
	if len(got) != 1 || got[0].State != learning.SkillCandidate {
		t.Fatalf("make_skill must append a recallable candidate, got %+v", got)
	}
	if len(got[0].Provenance) != 1 || got[0].Provenance[0] != "run:R-9" {
		t.Fatalf("provenance not carried onto the fact, got %v", got[0].Provenance)
	}
}

// TestMakeSkillBadArgsIsErrorResult: a missing required field is a
// ToolResult{IsError:true}, never a Go error or panic.
func TestMakeSkillBadArgsIsErrorResult(t *testing.T) {
	ls, _ := newStore(t)
	tools := skillsmith.Tools(ls)

	res := run(t, tools, "make_skill", map[string]any{"summary": "no name or body"})
	if !res.IsError {
		t.Fatal("missing name must yield an error result")
	}
	if len(ls.Recall("")) != 0 {
		t.Fatal("a rejected make_skill must not append any fact")
	}
}

// TestImproveSkillVersionsAndSupersedes: improve_skill records a new versioned
// candidate AND retires the prior id. The new body is recalled; the old id is
// superseded but its obligation and the supersession fact both remain in the
// record (append-only, never a deletion).
func TestImproveSkillVersionsAndSupersedes(t *testing.T) {
	ls, facts := newStore(t)
	tools := skillsmith.Tools(ls)

	// Author the original, then improve it.
	run(t, tools, "make_skill", map[string]any{
		"name":    "deploy-safely",
		"summary": "deploy safely with smoke tests",
		"body":    "run smoke tests before a deploy",
	})
	oldID := skillstore.SkillID("deploy-safely")

	const newBody = "run smoke tests, then a canary, before a full deploy"
	res := run(t, tools, "improve_skill", map[string]any{
		"name":     "deploy-safely",
		"new_body": newBody,
		"reason":   "add a canary stage after an incident",
	})
	out := decode(t, res)

	wantNewID := skillstore.VersionID("deploy-safely", newBody)
	if out["new_skill_id"] != wantNewID {
		t.Fatalf("new_skill_id = %v, want content-addressed %q", out["new_skill_id"], wantNewID)
	}
	if out["retired"] != oldID {
		t.Fatalf("retired = %v, want the prior id %q", out["retired"], oldID)
	}
	if out["state"] != "candidate" {
		t.Fatalf("the new version must be a candidate, got state=%v", out["state"])
	}

	// The new body is recalled as a live candidate under the SAME name ...
	live := ls.Recall("canary")
	if len(live) != 1 || live[0].ID != wantNewID || live[0].Body != newBody {
		t.Fatalf("improved body must be recalled as the live version, got %+v", live)
	}
	if skillstore.SkillName(live[0].ID) != "deploy-safely" {
		t.Fatalf("the new version must keep the same name, got %q", skillstore.SkillName(live[0].ID))
	}
	// ... and the old id is no longer recalled (superseded).
	for _, sk := range ls.Recall("") {
		if sk.ID == oldID {
			t.Fatalf("the retired version must not be recalled, got %+v", sk)
		}
	}

	// Attack-is-recorded: the old obligation AND a supersession naming it both
	// remain in the canonical record — Retire never deletes.
	obls, _, sups, err := facts.Project()
	if err != nil {
		t.Fatal(err)
	}
	if !hasObligation(obls, oldID) {
		t.Fatal("the superseded version's obligation was deleted — must be kept for history")
	}
	if !supersedes(sups, oldID) {
		t.Fatal("improve_skill did not record a supersession of the old id")
	}
	// The new version's obligation is present too.
	if !hasObligation(obls, wantNewID) {
		t.Fatal("the new version's obligation was not recorded")
	}
}

// TestImproveUnknownSkillIsErrorResult: improving a name with no live skill is an
// error result and appends nothing.
func TestImproveUnknownSkillIsErrorResult(t *testing.T) {
	ls, _ := newStore(t)
	tools := skillsmith.Tools(ls)

	res := run(t, tools, "improve_skill", map[string]any{
		"name":     "does-not-exist",
		"new_body": "body",
		"reason":   "n/a",
	})
	if !res.IsError {
		t.Fatal("improving a nonexistent skill must yield an error result")
	}
	if len(ls.Recall("")) != 0 {
		t.Fatal("a rejected improve_skill must not append any fact")
	}
}

func hasObligation(obls []flg.Obligation, id string) bool {
	for _, o := range obls {
		if o.ID == id {
			return true
		}
	}
	return false
}

func supersedes(sups []flg.SupersessionEvidence, subject string) bool {
	for _, s := range sups {
		if s.Subject == subject {
			return true
		}
	}
	return false
}
