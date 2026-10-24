package resolver

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/websines/structor-harness/flg"
)

// exhaustiveCorpus is the shared oracle for the FULL evidence→state domain — every
// semantically-distinct input class, not 9 samples. It is generated together with its
// Lean twin (proofs/ForgeProofs/Exhaustive.lean) by `go run ./internal/resolver/
// exhaustivegen`; the Lean side kernel-checks (`decide`) that the proved model derives
// the identical state for every class. So: Go ≡ corpus (this test) ∧ corpus ≡ Lean
// (the Lean build) ⟹ the RUNNING Go resolver ≡ the PROVED Lean spec over the whole
// finite domain. This is Phase 1 of the spec↔impl bridge (evidence→state only;
// the supersession graph is Phase 3).
const exhaustiveCorpus = "../conformance/exhaustive/evidence_state.json"

// wantClasses is the exact size of the enumerated evidence→state domain:
//
//	retired ∈ {false, true}  ×  subset of the 5 evidence-effect classes  =  2 × 2^5 = 64.
const wantClasses = 64

// reopenedCorpus is the REOPENED region (Phase 3): the same 64 evidence shapes, each with a
// cross_type_invalidation supersession naming the obligation (crossTypeInvalidated = true).
// Its Lean twin (proofs/ForgeProofs/Reopened.lean) kernel-checks that the Lean model derives
// the identical state, so Go ≡ Lean over the reopened region too — settling the resolver-level
// precedence falsified > reopened. The transitive supersession GRAPH that produces the reopen
// (flg.supersession SCC/cycles) is upstream of the resolver and is the remaining frontier.
const reopenedCorpus = "../conformance/exhaustive/reopened_state.json"

// TestExhaustiveEvidenceStateConformance checks the running Go resolver against the
// Lean-certified corpus on EVERY one of the 64 classes. A single mismatch fails the
// build — the resolver can no longer drift from the proved spec on any evidence→state
// case without CI (and this test) catching it.
func TestExhaustiveEvidenceStateConformance(t *testing.T) {
	raw, err := os.ReadFile(exhaustiveCorpus)
	if err != nil {
		t.Fatalf("read exhaustive corpus: %v (regenerate: go run ./internal/resolver/exhaustivegen)", err)
	}
	var vs []vector
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatalf("unmarshal exhaustive corpus: %v", err)
	}
	if len(vs) != wantClasses {
		t.Fatalf("corpus has %d classes, want %d — stale corpus; regenerate with go run ./internal/resolver/exhaustivegen", len(vs), wantClasses)
	}
	for _, v := range vs {
		obl, evidence, sups := v.build()
		got := string(ResolveObligation(obl, evidence, sups).State)
		if got != v.Expect {
			t.Errorf("class %s (%s): Go resolver got %q, Lean-certified corpus expects %q", v.ID, v.Name, got, v.Expect)
		}
	}
}

// TestReopenedRegionConformance checks the running Go resolver against the Lean-certified
// reopened corpus on every crossTypeInvalidated=true class — the region where the Lean model
// used to disagree (reopened>falsified) before the Phase-3 precedence fix. A single mismatch
// fails the build. With TestExhaustiveEvidenceStateConformance this covers the full 128-class
// resolver-level domain (evidence→state + reopened).
func TestReopenedRegionConformance(t *testing.T) {
	raw, err := os.ReadFile(reopenedCorpus)
	if err != nil {
		t.Fatalf("read reopened corpus: %v (regenerate: go run ./internal/resolver/exhaustivegen)", err)
	}
	var vs []vector
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatalf("unmarshal reopened corpus: %v", err)
	}
	if len(vs) != wantClasses {
		t.Fatalf("reopened corpus has %d classes, want %d — stale; regenerate with go run ./internal/resolver/exhaustivegen", len(vs), wantClasses)
	}
	for _, v := range vs {
		obl, evidence, sups := v.build()
		got := string(ResolveObligation(obl, evidence, sups).State)
		if got != v.Expect {
			t.Errorf("reopened class %s (%s): Go resolver got %q, Lean-certified corpus expects %q", v.ID, v.Name, got, v.Expect)
		}
	}
}

// TestSaturationRobustness is the EMPIRICAL validation of the saturation bound (0-vs-1+):
// for every class, duplicating evidence, reordering it, and adding provably-inert evidence
// (a valid out-of-scope falsifier) leaves the derived state UNCHANGED. This is why one
// representative per class suffices to cover the infinite space of evidence multisets —
// the verdict depends only on WHICH effect-classes are present, never on counts or order.
// (The claim is proved, not just tested, in proofs/ForgeProofs/Saturation.lean.)
func TestSaturationRobustness(t *testing.T) {
	raw, err := os.ReadFile(exhaustiveCorpus)
	if err != nil {
		t.Fatalf("read exhaustive corpus: %v", err)
	}
	var vs []vector
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatalf("unmarshal exhaustive corpus: %v", err)
	}

	// inert: a valid, in-domain-absent (out-of-scope) reproducible falsifier. The resolver
	// drops out-of-scope falsifiers, so its presence must never change any verdict.
	inert := flg.Evidence{
		ID: "INERT", ObligationID: "O-TEST", Polarity: flg.PolarityFalsifies,
		Method: "executed_test", Reproducibility: flg.ReproDeterministic, Validity: flg.ValidityValid,
		Scope: flg.Scope{Tags: []string{"out-of-scope"}}, PrincipalID: "P-TEST", Commit: "0000000",
	}

	for _, v := range vs {
		obl, base, _ := v.build()
		want := ResolveObligation(obl, base, nil).State

		variants := map[string][]flg.Evidence{
			"triplicated": triplicate(base),
			"plus-inert":  append(clone(base), inert),
			"reordered":   reverse(append(triplicate(base), inert)),
		}
		for name, ev := range variants {
			if got := ResolveObligation(obl, ev, nil).State; got != want {
				t.Errorf("class %s (%s): %s variant changed the verdict %q→%q — saturation violated", v.ID, v.Name, name, want, got)
			}
		}
	}
}

// --- deterministic transforms (Law 3: no randomness) ---

func clone(evs []flg.Evidence) []flg.Evidence {
	out := make([]flg.Evidence, len(evs))
	copy(out, evs)
	return out
}

// triplicate repeats each item three times with distinct ids — same effect-classes,
// higher counts, so the verdict must not move.
func triplicate(evs []flg.Evidence) []flg.Evidence {
	out := make([]flg.Evidence, 0, len(evs)*3)
	for k := 0; k < 3; k++ {
		for _, e := range evs {
			c := e
			c.ID = e.ID + "-" + string(rune('a'+k))
			out = append(out, c)
		}
	}
	return out
}

func reverse(evs []flg.Evidence) []flg.Evidence {
	out := clone(evs)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
