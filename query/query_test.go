// Black-box tests (package query_test): the query layer over a real flg.Store
// projection, exercised end to end through the exported API only.
package query_test

import (
	"bytes"
	"testing"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/query"
	"github.com/websines/structor-harness/record"
)

// seed builds a projection with a full lineage plus a supersession and an
// inferred co-anchor:
//
//	T-1 ─decomposes→ L-1 ─decomposes→ O-1 ←supports─ E-1 (superseded_by E-2)
//	                                    ↑supports─ E-2 (active replacement)
//	L-2 ─decomposes→ O-2   (O-2 shares O-1's anchor file → INFERRED co_anchored)
func seed(t *testing.T) (*flg.Store, *query.Spine) {
	t.Helper()
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := flg.NewStore(cas)
	mustObl(t, s, flg.Obligation{
		ID: "O-1", LemmaID: "L-1", Title: "refund never exceeds capture",
		Anchor: flg.Anchor{File: "pay/refund.go", Symbol: "Refund"}, DomainTags: []string{"ledger"},
	})
	mustObl(t, s, flg.Obligation{
		ID: "O-2", LemmaID: "L-2", Title: "settlement batch closes cleanly",
		Anchor: flg.Anchor{File: "pay/refund.go", Symbol: "Settle"}, DomainTags: []string{"ledger"},
	})
	mustEv(t, s, flg.Evidence{
		ID: "E-1", ObligationID: "O-1", Polarity: flg.PolaritySupports,
		Method: flg.MethodExecutedTest, Reproducibility: flg.ReproDeterministic,
		Validity: flg.ValidityValid, Commit: "c0ffee", Summary: "property test passes",
	})
	mustEv(t, s, flg.Evidence{
		ID: "E-2", ObligationID: "O-1", Polarity: flg.PolaritySupports,
		Method: flg.MethodExecutedTest, Reproducibility: flg.ReproDeterministic,
		Validity: flg.ValidityValid, Commit: "cafe02", Summary: "replacement property test passes",
	})
	if _, err := s.AppendSupersession(flg.SupersessionEvidence{
		Subject: "E-1", Kind: flg.KindSupersededBy, ReplacedBy: []string{"E-2"},
		Evidence: []string{"artifact:rerun-log"},
	}); err != nil {
		t.Fatal(err)
	}
	return s, &query.Spine{LemmaToTheorem: map[string]string{"L-1": "T-1"}}
}

func mustObl(t *testing.T, s *flg.Store, o flg.Obligation) {
	t.Helper()
	if _, err := s.AppendObligation(o); err != nil {
		t.Fatal(err)
	}
}

func mustEv(t *testing.T, s *flg.Store, e flg.Evidence) {
	t.Helper()
	if _, err := s.AppendEvidence(e); err != nil {
		t.Fatal(err)
	}
}

func build(t *testing.T, s *flg.Store, spine *query.Spine) *query.InMemoryIndex {
	t.Helper()
	ix, err := query.FromStore(s, spine)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

func has(ids []query.NodeID, want query.NodeID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestAuditReturnsLineageWithSupersedes: the audit traversal walks
// Theorem→Lemma→Obligation→Evidence AND includes every supersedes edge.
func TestAuditReturnsLineageWithSupersedes(t *testing.T) {
	s, spine := seed(t)
	ix := build(t, s, spine)

	a := query.Audit(ix, "T-1", 0)
	for _, want := range []query.NodeID{"T-1", "L-1", "O-1", "E-1", "E-2"} {
		if !has(a.Nodes, want) {
			t.Fatalf("audit lineage missing %s; got %v", want, a.Nodes)
		}
	}
	// the co-anchored obligation is on an INFERRED edge — audit must NOT wander to it.
	if has(a.Nodes, "O-2") {
		t.Fatalf("audit crossed an inferred co-anchor edge to O-2: %v", a.Nodes)
	}
	sup := a.SupersedesEdges()
	if len(sup) == 0 {
		t.Fatal("audit returned no supersedes edges")
	}
	foundE1 := false
	for _, e := range sup {
		if e.To == "E-1" && e.Prov == query.Canonical {
			foundE1 = true
		}
	}
	if !foundE1 {
		t.Fatalf("audit missing the supersedes edge onto E-1; got %+v", sup)
	}
}

// TestImpactFindsTouchedObligations: the reverse/drift walk from a changed spec
// fact reaches every touched obligation and its evidence, and stops at inferred
// edges.
func TestImpactFindsTouchedObligations(t *testing.T) {
	s, spine := seed(t)
	ix := build(t, s, spine)

	r := query.Impact(ix, "T-1", 0)
	for _, want := range []query.NodeID{"L-1", "O-1", "E-1", "E-2"} {
		if !has(r.Touched, want) {
			t.Fatalf("impact missing touched %s; got %v", want, r.Touched)
		}
	}
	if obls := r.TouchedOfKind(ix, query.KindObligation); !has(obls, "O-1") {
		t.Fatalf("impact touched-obligations missing O-1; got %v", obls)
	}
	// O-2 hangs off O-1 by an INFERRED co-anchor only — a drift walk must not claim it.
	if has(r.Touched, "O-2") {
		t.Fatalf("impact crossed an inferred edge to O-2: %v", r.Touched)
	}

	// seeding at the obligation itself finds its governing + superseded evidence.
	ro := query.Impact(ix, "O-1", 0)
	if !has(ro.Touched, "E-1") || !has(ro.Touched, "E-2") {
		t.Fatalf("impact from O-1 missing its evidence; got %v", ro.Touched)
	}
}

// TestReplayWalksSupersessionChain: replay follows the supersedes spine backward
// from an active replacement to what it superseded.
func TestReplayWalksSupersessionChain(t *testing.T) {
	s, spine := seed(t)
	ix := build(t, s, spine)

	r := query.Replay(ix, "E-2", 0)
	if !has(r.Chain, "E-1") {
		t.Fatalf("replay from E-2 did not reach the superseded E-1; chain=%v", r.Chain)
	}
}

// TestDeclaredQuerySetPinning: a declared query set is byte-identical across two
// runs on the same projection (and rebuild-identical), and DIFFERENT once a fact
// is added.
func TestDeclaredQuerySetPinning(t *testing.T) {
	s, spine := seed(t)
	qs := query.QuerySet{
		Name: "gate-context", Version: "v1",
		Steps: []query.Step{{Verb: query.VerbAudit, Seed: "T-1"}},
	}

	ix1 := build(t, s, spine)
	r1 := query.Run(ix1, qs)
	r2 := query.Run(ix1, qs)
	if !bytes.Equal(r1.Bytes, r2.Bytes) || r1.ResultHash != r2.ResultHash {
		t.Fatal("declared query set not byte-identical across two runs on one projection")
	}

	// rebuild the index from the same store → same projection hash → same bytes.
	ix1b := build(t, s, spine)
	rb := query.Run(ix1b, qs)
	if !bytes.Equal(r1.Bytes, rb.Bytes) {
		t.Fatal("declared query set not rebuild-identical over the same projection")
	}
	if r1.ProjectionHash != ix1.ProjectionHash() {
		t.Fatal("result not pinned to the projection hash")
	}

	// add a fact → new projection → different pinned result.
	mustObl(t, s, flg.Obligation{ID: "O-3", LemmaID: "L-1", Title: "chargeback path settles"})
	ix2 := build(t, s, spine)
	r3 := query.Run(ix2, qs)
	if r3.ProjectionHash == r1.ProjectionHash {
		t.Fatal("projection hash did not change when a fact was added")
	}
	if r3.ResultHash == r1.ResultHash || bytes.Equal(r3.Bytes, r1.Bytes) {
		t.Fatal("declared query set result did not change when a fact was added")
	}
	// the new obligation is genuinely in the audit lineage now.
	found := false
	for _, n := range r3.Steps[0].Nodes {
		if n == "O-3" {
			found = true
		}
	}
	if !found {
		t.Fatalf("added obligation O-3 not in the new audit result: %v", r3.Steps[0].Nodes)
	}
}

// TestRetrievalCitedActiveFirstRefusesInferred: retrieval returns cited,
// active-first results, labels the superseded fact as history, and refuses to
// cite an inferred-reached edge as proof.
func TestRetrievalCitedActiveFirstRefusesInferred(t *testing.T) {
	s, spine := seed(t)
	ix := build(t, s, spine)

	res := query.Retrieve(ix, sups(t, s), "refund never exceeds capture", 10)
	if len(res.Governing) == 0 {
		t.Fatal("retrieval returned no governing results")
	}
	// every governing item is cited to a real id (and path where it has one).
	for _, it := range res.Governing {
		if it.Citation() == "" {
			t.Fatalf("uncited governing item %+v", it)
		}
	}
	// O-1 is the lexical hit: active, canonical, admissible as proof.
	if !itemProvable(res.ProofCitations(), "O-1") {
		t.Fatalf("O-1 not among proof citations: %+v", res.ProofCitations())
	}

	// E-1 is superseded → it must be labelled history, never governing.
	if itemInList(res.Governing, "E-1") {
		t.Fatal("superseded E-1 leaked into the governing set")
	}
	if st, ok := itemState(res.Audit, "E-1"); !ok || st != flg.NodeSuperseded {
		t.Fatalf("E-1 not labelled superseded in audit history; got state=%v ok=%v", st, ok)
	}

	// O-2 is reachable ONLY via the inferred co-anchor edge: served, but refused
	// as proof.
	o2, ok := findItem(res.Governing, "O-2")
	if !ok {
		t.Fatal("O-2 (inferred-reached) was not surfaced in the context at all")
	}
	if o2.Prov != query.Inferred {
		t.Fatalf("O-2 reached over an inferred edge but labelled %s", o2.Prov)
	}
	if o2.CanCiteAsProof() {
		t.Fatal("gate admitted an inferred-reached edge as proof")
	}
	if itemProvable(res.ProofCitations(), "O-2") {
		t.Fatal("inferred-reached O-2 leaked into proof citations")
	}

	// and the same refusal is visible at the declared-query gate.
	pin := query.Run(ix, query.QuerySet{
		Name: "retrieve-gate", Version: "v1",
		Steps: []query.Step{{Verb: query.VerbRetrieve, Query: "refund never exceeds capture", Limit: 10}},
	})
	if !pin.RefusesInferred() {
		t.Fatal("declared-query gate did not refuse the inferred edge")
	}
}

// --- small test helpers -----------------------------------------------------

func sups(t *testing.T, s *flg.Store) []flg.SupersessionEvidence {
	t.Helper()
	_, _, sups, err := s.Project()
	if err != nil {
		t.Fatal(err)
	}
	return sups
}

func findItem(items []query.CitedItem, id query.NodeID) (query.CitedItem, bool) {
	for _, it := range items {
		if it.ID == id {
			return it, true
		}
	}
	return query.CitedItem{}, false
}

func itemInList(items []query.CitedItem, id query.NodeID) bool {
	_, ok := findItem(items, id)
	return ok
}

func itemProvable(items []query.CitedItem, id query.NodeID) bool {
	it, ok := findItem(items, id)
	return ok && it.CanCiteAsProof()
}

func itemState(items []query.CitedItem, id query.NodeID) (flg.NodeState, bool) {
	it, ok := findItem(items, id)
	return it.State, ok
}
