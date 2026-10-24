package resolver

import (
	"encoding/json"
	"os"
	"testing"
)

// TestCanonicalTableEqualsResolver proves the code-generated canonical rule table
// (ruletable_gen.go) + the generic evaluator (EvalCanonical) derive the identical
// evidence→state verdict as the hand-coded ResolveObligation on ALL 64 classes. So the
// resolver's decision rules exist as ONE canonical data table
// (conformance/canonical/evidence_state_rules.json) whose Go projection is GENERATED and
// checked-equivalent over the whole finite domain — not a hand-written twin (Phase 2 of
// the spec↔impl bridge). The Lean model is proven equal to the same table in
// proofs/ForgeProofs/Canonical.lean.
func TestCanonicalTableEqualsResolver(t *testing.T) {
	raw, err := os.ReadFile(exhaustiveCorpus)
	if err != nil {
		t.Fatalf("read exhaustive corpus: %v (regenerate: go run ./internal/resolver/exhaustivegen)", err)
	}
	var vs []vector
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatalf("unmarshal exhaustive corpus: %v", err)
	}
	if len(vs) != wantClasses {
		t.Fatalf("corpus has %d classes, want %d", len(vs), wantClasses)
	}
	for _, v := range vs {
		obl, evidence, sups := v.build()
		hand := ResolveObligation(obl, evidence, sups).State
		canon := EvalCanonical(obl, evidence, sups)
		if canon != hand {
			t.Errorf("class %s (%s): canonical table got %q, hand-coded resolver %q", v.ID, v.Name, canon, hand)
		}
	}
}
