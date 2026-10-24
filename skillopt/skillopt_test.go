// White-box tests (package skillopt): drive the SkillOpt loop through the REAL
// append-only record — record.NewLocalCAS + flg.NewStore + learning.NewStore —
// so every assertion is about facts that survived a round-trip through the
// canonical record (Law 1). No network: StaticOptimizer stands in for the model,
// and LLMOptimizer's parsing is exercised through parseEdits directly.
package skillopt

import (
	"context"
	"strings"
	"testing"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/skillstore"
	"github.com/websines/structor-harness/learning"
	"github.com/websines/structor-harness/record"
)

// newLearningStore wires a learning store over a fresh local CAS and its record.
func newLearningStore(t *testing.T) *learning.Store {
	t.Helper()
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return learning.NewStore(flg.NewStore(cas))
}

// seedPromoted admits a live skill the way the harness does: AddCandidate then a
// human-attested Promote — so Optimize has a real promoted baseline to improve.
func seedPromoted(t *testing.T, ls *learning.Store, name, title, body string) {
	t.Helper()
	id := skillstore.SkillID(name)
	if _, err := ls.AddCandidate(learning.Skill{ID: id, Title: title, Body: body}); err != nil {
		t.Fatal(err)
	}
	if _, err := ls.Promote(id, learning.AcceptHuman("principal:lead", "seeded", true)); err != nil {
		t.Fatal(err)
	}
}

// containsWant is the deterministic eval used across the gate tests: a candidate
// satisfies a case iff its body contains the case's Want string.
func containsWant(sk learning.Skill, c learning.Case) bool {
	return strings.Contains(sk.Body, c.Want)
}

// --- ApplyEdits ---------------------------------------------------------------

func TestApplyEditsReplace(t *testing.T) {
	body, applied := ApplyEdits("say hi to the world", []Edit{{Op: "replace", Anchor: "hi", Text: "hello"}})
	if body != "say hello to the world" {
		t.Fatalf("replace: got %q", body)
	}
	if len(applied) != 1 {
		t.Fatalf("replace: expected 1 applied, got %d", len(applied))
	}
}

func TestApplyEditsReplaceFirstOccurrenceOnly(t *testing.T) {
	body, _ := ApplyEdits("a a a", []Edit{{Op: "replace", Anchor: "a", Text: "b"}})
	if body != "b a a" {
		t.Fatalf("replace should touch only the first occurrence, got %q", body)
	}
}

func TestApplyEditsDelete(t *testing.T) {
	body, applied := ApplyEdits("foo bar baz", []Edit{{Op: "delete", Anchor: "bar "}})
	if body != "foo baz" {
		t.Fatalf("delete: got %q", body)
	}
	if len(applied) != 1 {
		t.Fatalf("delete: expected 1 applied, got %d", len(applied))
	}
}

func TestApplyEditsAddWithAnchorInsertsAfter(t *testing.T) {
	body, applied := ApplyEdits("step one. step three.", []Edit{{Op: "add", Anchor: "step one. ", Text: "step two. "}})
	if body != "step one. step two. step three." {
		t.Fatalf("add-with-anchor: got %q", body)
	}
	if len(applied) != 1 {
		t.Fatalf("add: expected 1 applied, got %d", len(applied))
	}
}

func TestApplyEditsAddWithoutAnchorAppends(t *testing.T) {
	// absent anchor → append at end
	body, applied := ApplyEdits("body", []Edit{{Op: "add", Anchor: "nowhere", Text: " tail"}})
	if body != "body tail" {
		t.Fatalf("add-absent-anchor should append, got %q", body)
	}
	// empty anchor → append at end
	body2, _ := ApplyEdits("body", []Edit{{Op: "add", Anchor: "", Text: " tail"}})
	if body2 != "body tail" {
		t.Fatalf("add-empty-anchor should append, got %q", body2)
	}
	if len(applied) != 1 {
		t.Fatalf("add: expected 1 applied, got %d", len(applied))
	}
}

func TestApplyEditsMissingAnchorSkipped(t *testing.T) {
	body, applied := ApplyEdits("unchanged", []Edit{
		{Op: "replace", Anchor: "absent", Text: "x"},
		{Op: "delete", Anchor: "gone"},
	})
	if body != "unchanged" {
		t.Fatalf("missing anchors must be skipped, got %q", body)
	}
	if len(applied) != 0 {
		t.Fatalf("no edit should have applied, got %d", len(applied))
	}
}

// --- Optimize: PROMOTE path ---------------------------------------------------

// The static optimizer proposes an edit that makes the body satisfy the held-out
// cases. The gate must promote, version + supersede the old skill, and Recall
// must return the new body.
func TestOptimizePromotesWhenItBeatsHeldOut(t *testing.T) {
	ls := newLearningStore(t)
	seedPromoted(t, ls, "greet", "greeting skill", "say hi")

	opt := StaticOptimizer{Edits: []Edit{{Op: "replace", Anchor: "hi", Text: "hello"}}}
	heldOut := []learning.Case{
		{Name: "c1", Want: "hello"},
		{Name: "c2", Want: "hello"},
	}

	rep, err := Optimize(context.Background(), ls, opt, "greet", nil, 3, heldOut, containsWant)
	if err != nil {
		t.Fatal(err)
	}

	if !rep.Decision.Promote {
		t.Fatalf("expected promote, got decision %+v", rep.Decision)
	}
	if rep.Decision.Mode != learning.ModeHeldOut {
		t.Fatalf("expected held_out mode, got %s", rep.Decision.Mode)
	}
	if rep.Decision.Passed != rep.Decision.Total || rep.Decision.Total != len(heldOut) {
		t.Fatalf("expected all %d cases passed, got %d/%d", len(heldOut), rep.Decision.Passed, rep.Decision.Total)
	}
	wantID := skillstore.VersionID("greet", "say hello")
	if rep.NewID != wantID {
		t.Fatalf("NewID = %q, want the versioned id %q", rep.NewID, wantID)
	}
	if rep.NewBody != "say hello" {
		t.Fatalf("NewBody = %q, want %q", rep.NewBody, "say hello")
	}

	// The live skill is now the new versioned body, promoted.
	live, err := currentSkill(ls, "greet")
	if err != nil {
		t.Fatal(err)
	}
	if live.ID != wantID || live.Body != "say hello" || live.State != learning.SkillPromoted {
		t.Fatalf("live skill = %+v, want promoted %q body %q", live, wantID, "say hello")
	}

	// The old base id is retired: no non-retired skill still carries "say hi".
	for _, sk := range ls.Recall("") {
		if sk.State != learning.SkillRetired && sk.Body == "say hi" {
			t.Fatalf("old body must be retired, still recalled as %+v", sk)
		}
	}

	// Recall by the (unchanged) title returns the improved promoted body.
	got := ls.Recall("greeting")
	found := false
	for _, sk := range got {
		if sk.ID == wantID && sk.Body == "say hello" && sk.State == learning.SkillPromoted {
			found = true
		}
	}
	if !found {
		t.Fatalf("Recall must return the new promoted body, got %+v", got)
	}
}

// --- Optimize: REJECT path (SkillOpt's core) ----------------------------------

// The static optimizer proposes an edit that does NOT satisfy the held-out set.
// The gate must reject; the live skill must be left UNCHANGED; and a rejected-
// edit fact must be recorded so the attempt is not retried.
func TestOptimizeRejectsAndLeavesLiveSkillUnchanged(t *testing.T) {
	ls := newLearningStore(t)
	seedPromoted(t, ls, "greet", "greeting skill", "say hi")

	// "hey" does not contain the required "hello".
	opt := StaticOptimizer{Edits: []Edit{{Op: "replace", Anchor: "hi", Text: "hey"}}}
	heldOut := []learning.Case{{Name: "c1", Want: "hello"}}

	rep, err := Optimize(context.Background(), ls, opt, "greet", nil, 3, heldOut, containsWant)
	if err != nil {
		t.Fatal(err)
	}

	if rep.Decision.Promote {
		t.Fatalf("expected reject, got promote: %+v", rep.Decision)
	}
	if rep.Decision.Mode != learning.ModeHeldOut {
		t.Fatalf("expected held_out mode, got %s", rep.Decision.Mode)
	}
	if rep.NewID != "" {
		t.Fatalf("a rejected epoch must have empty NewID, got %q", rep.NewID)
	}
	if len(rep.Rejected) != 1 {
		t.Fatalf("expected the applied edit recorded as rejected, got %d", len(rep.Rejected))
	}

	// The live skill is untouched: still the seeded base id, body, and promoted.
	live, err := currentSkill(ls, "greet")
	if err != nil {
		t.Fatal(err)
	}
	if live.ID != skillstore.SkillID("greet") || live.Body != "say hi" || live.State != learning.SkillPromoted {
		t.Fatalf("live skill must be unchanged, got %+v", live)
	}

	// The rejected edit is recorded as a fact (attack-is-recorded style).
	recorded := false
	for _, sk := range ls.Recall("") {
		if strings.HasPrefix(sk.ID, "rejected:") && sk.Body == "say hey" {
			recorded = true
		}
	}
	if !recorded {
		t.Fatal("the rejected edit must be recorded as a fact so it is not retried")
	}
}

func TestOptimizeErrorsWithoutLiveSkill(t *testing.T) {
	ls := newLearningStore(t)
	_, err := Optimize(context.Background(), ls, StaticOptimizer{}, "missing", nil, 1, nil, containsWant)
	if err == nil {
		t.Fatal("expected an error when no live skill exists")
	}
}

// --- Harvest ------------------------------------------------------------------

func TestHarvestDerivesRolloutsFromMessages(t *testing.T) {
	msgs := []hx.Message{
		{Role: hx.RoleAssistant, Content: []hx.ContentBlock{
			{Kind: hx.BlockText, Text: "read the file"},
			{Kind: hx.BlockToolCall, ToolName: "Read"},
		}},
		{Role: hx.RoleTool, Content: []hx.ContentBlock{
			{Kind: hx.BlockToolResult, Text: "file contents", IsError: false},
		}},
		{Role: hx.RoleAssistant, Content: []hx.ContentBlock{
			{Kind: hx.BlockToolCall, ToolName: "Build"},
		}},
		{Role: hx.RoleTool, Content: []hx.ContentBlock{
			{Kind: hx.BlockToolResult, Text: "compile error", IsError: true},
		}},
	}

	got := Harvest(msgs)
	if len(got) != 2 {
		t.Fatalf("expected 2 rollouts, got %d (%+v)", len(got), got)
	}
	if got[0].Task != "call Read" || got[0].Outcome != "file contents" || !got[0].Passed {
		t.Fatalf("rollout[0] = %+v", got[0])
	}
	if got[1].Task != "call Build" || got[1].Outcome != "compile error" || got[1].Passed {
		t.Fatalf("rollout[1] = %+v", got[1])
	}
}

// --- LLMOptimizer parsing (canned JSON, no network) ---------------------------

func TestParseEditsDropsMalformedAndCaps(t *testing.T) {
	// Fenced prose around a JSON array; one bogus op must be dropped.
	canned := "sure, here are the edits:\n```json\n" +
		`[{"op":"replace","anchor":"hi","text":"hello"},` +
		`{"op":"bogus","anchor":"x","text":"y"},` +
		`{"op":"delete","anchor":"foo","text":""}]` +
		"\n```"
	edits, ok := parseEdits(canned, 5)
	if !ok {
		t.Fatal("expected the array to parse")
	}
	if len(edits) != 2 {
		t.Fatalf("expected 2 valid edits (bogus dropped), got %d (%+v)", len(edits), edits)
	}
	if edits[0].Op != "replace" || edits[1].Op != "delete" {
		t.Fatalf("unexpected edits: %+v", edits)
	}

	// Cap at maxEdits.
	capped, ok := parseEdits(`[{"op":"add","anchor":"","text":"a"},{"op":"add","anchor":"","text":"b"},{"op":"add","anchor":"","text":"c"}]`, 2)
	if !ok || len(capped) != 2 {
		t.Fatalf("expected 2 capped edits, got ok=%v len=%d", ok, len(capped))
	}
}

func TestParseEditsRejectsNonJSON(t *testing.T) {
	if _, ok := parseEdits("no json array here at all", 5); ok {
		t.Fatal("expected ok=false when no JSON array is present")
	}
}

func TestParseEditsDropsUnlocatableAndEmptyAdds(t *testing.T) {
	// replace/delete without anchor and add without text are all no-ops.
	edits, ok := parseEdits(`[{"op":"replace","anchor":"","text":"x"},{"op":"delete","anchor":"","text":""},{"op":"add","anchor":"a","text":""}]`, 5)
	if !ok {
		t.Fatal("expected the array to parse")
	}
	if len(edits) != 0 {
		t.Fatalf("expected all no-op edits dropped, got %+v", edits)
	}
}

// LLMOptimizer with a nil runtime is a no-op epoch (no model available).
func TestLLMOptimizerNilRuntimeIsNoOp(t *testing.T) {
	opt := LLMOptimizer(nil, "prov", "model")
	edits, err := opt.Propose(context.Background(), "body", nil, 3)
	if err != nil {
		t.Fatalf("nil runtime must not error, got %v", err)
	}
	if len(edits) != 0 {
		t.Fatalf("nil runtime must yield no edits, got %+v", edits)
	}
}
