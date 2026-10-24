// Black-box test (package learning_test): drive skills and failures through the
// REAL append-only record — record.NewLocalCAS + flg.NewStore — so every
// assertion is about facts that survived a round-trip through the canonical
// record (Law 1), never about in-memory state.
package learning_test

import (
	"strings"
	"testing"

	"github.com/websines/structor-harness/flg"
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

// retryWithBackoff is a sample learned skill reused across the tests.
func retryWithBackoff() learning.Skill {
	return learning.Skill{
		ID:         "skill:retry-with-backoff",
		Title:      "retry idempotent calls with exponential backoff",
		Body:       "wrap idempotent outbound calls in capped exponential backoff with jitter",
		Provenance: []string{"run:R-42", "sha256:evidenceabc"},
	}
}

// TestAddCandidateIsAFactWithProvenance: adding a candidate writes a real fact
// (a content address) carrying its provenance, and it recalls as candidate.
func TestAddCandidateIsAFactWithProvenance(t *testing.T) {
	st, _ := newStore(t)
	sk := retryWithBackoff()

	id, err := st.AddCandidate(sk)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(id), "sha256:") {
		t.Fatalf("AddCandidate should return a fact_id (content address), got %q", id)
	}

	got := st.Recall("backoff")
	if len(got) != 1 {
		t.Fatalf("expected the candidate recalled, got %d skills", len(got))
	}
	if got[0].State != learning.SkillCandidate {
		t.Fatalf("a freshly added skill must be candidate, got %s", got[0].State)
	}
	if strings.Join(got[0].Provenance, ",") != strings.Join(sk.Provenance, ",") {
		t.Fatalf("provenance not carried on the fact: got %v", got[0].Provenance)
	}
}

// TestAcceptanceGatePromotesBeaterRejectsRegressor: the §11.1 gate promotes a
// skill that beats the held-out set and refuses one that does not. eval is
// deterministic (a substring check), so the Decision is reproducible.
func TestAcceptanceGatePromotesBeaterRejectsRegressor(t *testing.T) {
	st, _ := newStore(t)

	good := retryWithBackoff()
	bad := learning.Skill{
		ID:    "skill:sleep-then-retry",
		Title: "sleep a fixed second then retry",
		Body:  "retry once after a fixed sleep", // lacks "backoff" / "jitter"
	}
	if _, err := st.AddCandidate(good); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddCandidate(bad); err != nil {
		t.Fatal(err)
	}

	cases := []learning.Case{
		{Name: "must-backoff", Want: "backoff"},
		{Name: "must-jitter", Want: "jitter"},
	}
	// A skill passes a case iff its body contains the wanted technique.
	eval := func(sk learning.Skill, c learning.Case) bool {
		return strings.Contains(sk.Body, c.Want)
	}

	dGood := learning.Accept(good, cases, eval)
	if !dGood.Promote || dGood.Passed != dGood.Total {
		t.Fatalf("skill that beats the set must promote, got %+v", dGood)
	}
	dBad := learning.Accept(bad, cases, eval)
	if dBad.Promote || dBad.Passed == dBad.Total {
		t.Fatalf("skill that regresses on the set must be rejected, got %+v", dBad)
	}

	// The gate decides; the store records only a passing decision.
	if _, err := st.Promote(good.ID, dGood); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Promote(bad.ID, dBad); err == nil {
		t.Fatal("Promote must refuse a non-promoting decision")
	}

	if s := stateOf(t, st, good.ID); s != learning.SkillPromoted {
		t.Fatalf("good skill should be promoted, got %s", s)
	}
	if s := stateOf(t, st, bad.ID); s != learning.SkillCandidate {
		t.Fatalf("rejected skill should stay candidate, got %s", s)
	}
}

// TestRetireIsSupersessionNotDeletion: a retired skill is superseded — still in
// the record (obligation + supersession fact both present) but never recalled.
func TestRetireIsSupersessionNotDeletion(t *testing.T) {
	st, facts := newStore(t)
	sk := retryWithBackoff()
	if _, err := st.AddCandidate(sk); err != nil {
		t.Fatal(err)
	}

	if _, err := st.Retire(sk.ID, "replaced by a circuit-breaker skill"); err != nil {
		t.Fatal(err)
	}

	if got := st.Recall(""); len(got) != 0 {
		t.Fatalf("a retired skill must not be recalled, got %d", len(got))
	}

	// Law 1 / append-only: the skill's obligation and the supersession fact are
	// both still in the canonical record.
	obls, _, sups, err := facts.Project()
	if err != nil {
		t.Fatal(err)
	}
	if !hasObligation(obls, sk.ID) {
		t.Fatal("retired skill's fact was deleted from the record — must be kept for history")
	}
	if !supersedes(sups, sk.ID) {
		t.Fatal("retirement did not record a supersession fact")
	}
}

// TestRecallIsPromotedFirstSummariesFirst: recall layers the accepted skill
// ahead of candidates, and Summaries surfaces the L0 line in that same order.
func TestRecallIsPromotedFirstSummariesFirst(t *testing.T) {
	st, _ := newStore(t)

	promoted := retryWithBackoff()
	candidate := learning.Skill{
		ID:    "skill:cache-idempotency-keys",
		Title: "cache idempotency keys to dedupe retried backoff calls",
		Body:  "store idempotency keys so a retried call with backoff is deduped",
	}
	if _, err := st.AddCandidate(promoted); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	// Promote one via a human-gated decision.
	if _, err := st.Promote(promoted.ID, learning.AcceptHuman("principal:lead", "reviewed", true)); err != nil {
		t.Fatal(err)
	}

	got := st.Recall("backoff") // matches both skills
	if len(got) != 2 {
		t.Fatalf("both skills should match, got %d", len(got))
	}
	if got[0].ID != promoted.ID || got[0].State != learning.SkillPromoted {
		t.Fatalf("promoted skill must come first, got %+v", got[0])
	}
	if got[1].State != learning.SkillCandidate {
		t.Fatalf("candidate should follow the promoted skill, got %s", got[1].State)
	}

	sums := st.Summaries("backoff")
	if len(sums) != 2 || sums[0] != promoted.Title {
		t.Fatalf("summaries must be promoted-first and be the L0 titles, got %v", sums)
	}
}

// TestFailureRecordedAndRecalledBySignature: a failure is a fact recalled by a
// signature match, so a future run can check before repeating the incident.
func TestFailureRecordedAndRecalledBySignature(t *testing.T) {
	st, _ := newStore(t)

	f := learning.Failure{
		ID:         "failure:double-charge-on-retry",
		Summary:    "retry double-charged the customer",
		Signature:  "IdempotencyViolation:charge",
		Prevents:   "attach an idempotency key before retrying a charge",
		Provenance: []string{"incident:INC-7"},
	}
	id, err := st.RecordFailure(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(id), "sha256:") {
		t.Fatalf("RecordFailure should return a fact_id, got %q", id)
	}

	got := st.RecallFailures("IdempotencyViolation")
	if len(got) != 1 {
		t.Fatalf("failure should be recalled by signature, got %d", len(got))
	}
	if got[0].Prevents != f.Prevents || got[0].Signature != f.Signature {
		t.Fatalf("recalled failure lost its lesson: %+v", got[0])
	}
	if len(st.RecallFailures("UnrelatedSignature")) != 0 {
		t.Fatal("a non-matching signature must recall nothing")
	}
}

// TestUsageTrackingIsReconstructed: distinct runs recalling a skill are counted
// from the record, supporting the human-gated low-volume stance.
func TestUsageTrackingIsReconstructed(t *testing.T) {
	st, _ := newStore(t)
	sk := retryWithBackoff()
	if _, err := st.AddCandidate(sk); err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{"R-1", "R-2", "R-1"} { // R-1 repeated: same fact, deduped
		if _, err := st.RecordUse(sk.ID, run); err != nil {
			t.Fatal(err)
		}
	}
	if n := st.Uses(sk.ID); n != 2 {
		t.Fatalf("expected 2 distinct runs counted from the record, got %d", n)
	}
	// Usage must not be mistaken for a promotion.
	if s := stateOf(t, st, sk.ID); s != learning.SkillCandidate {
		t.Fatalf("usage alone must not promote, got %s", s)
	}
}

// stateOf finds a skill's derived state via Recall (empty query matches all).
func stateOf(t *testing.T, st *learning.Store, id string) learning.SkillState {
	t.Helper()
	for _, sk := range st.Recall("") {
		if sk.ID == id {
			return sk.State
		}
	}
	return "" // not recalled (e.g. retired)
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
