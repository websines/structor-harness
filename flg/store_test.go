// Black-box test (package flg_test) so it may import the resolver, which imports
// flg — the append-only record → projection → resolver pipeline, end to end.
package flg_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/record"
	"github.com/websines/structor-harness/resolver"
)

// seed appends one obligation and one deterministic in-scope support for it.
func seed(t *testing.T, s *flg.Store) {
	t.Helper()
	if _, err := s.AppendObligation(flg.Obligation{
		ID: "O-1101", Title: "refund never exceeds capture",
		AcceptancePolicy: "refund<=capture", DomainTags: []string{"ledger"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvidence(flg.Evidence{
		ID: "E-1", ObligationID: "O-1101",
		Polarity: flg.PolaritySupports, Method: flg.MethodExecutedTest,
		Reproducibility: flg.ReproDeterministic, Validity: flg.ValidityValid,
		Scope:       flg.Scope{Domain: "ledger", Tags: []string{"ledger"}},
		PrincipalID: "principal:marcus", Commit: "c0ffee", Summary: "property test passes",
	}); err != nil {
		t.Fatal(err)
	}
}

func resolveStore(t *testing.T, s *flg.Store) map[string]flg.Verdict {
	t.Helper()
	obls, evs, sups, err := s.Project()
	if err != nil {
		t.Fatal(err)
	}
	return resolver.ResolveAll(obls, evs, sups)
}

// TestLaw1_ProjectionIsRebuildable is the machine-checkable form of Law 1: drop
// the projection, rebuild it from the record's object set alone, and both the
// projection hash and every derived verdict must be byte-identical.
func TestLaw1_ProjectionIsRebuildable(t *testing.T) {
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := flg.NewStore(cas)
	seed(t, store)

	h1 := store.ProjectionHash()
	v1 := resolveStore(t, store)
	if v1["O-1101"].State != flg.StateDischarged {
		t.Fatalf("expected discharged from the record, got %s", v1["O-1101"].State)
	}

	// Sign a manifest binding the projection hash to the preservation set.
	signer, _ := record.NewSelfSigner("principal:ci-bot")
	sm, err := signer.Sign(record.Manifest{
		Commit: "c0ffee", ProjectionHash: string(h1), Objects: store.ManifestObjects(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// DROP the projection; rebuild a fresh store from the manifest's objects only.
	rebuilt := flg.LoadStore(cas, sm.Manifest.Objects)
	h2 := rebuilt.ProjectionHash()
	if h2 != h1 {
		t.Fatalf("projection not rebuildable: %s != %s", h2, h1)
	}
	if string(h2) != sm.Manifest.ProjectionHash {
		t.Fatal("rebuilt hash disagrees with the signed manifest")
	}
	v2 := resolveStore(t, rebuilt)
	if !reflect.DeepEqual(v2["O-1101"], v1["O-1101"]) {
		t.Fatalf("verdict changed across rebuild: %+v vs %+v", v2["O-1101"], v1["O-1101"])
	}
}

// TestReopenThroughStore drives the drift flow (§7.3) through the real record: a
// discharged obligation, then a cross_type_invalidation supersession fact, and
// the resolver derives `reopened` citing the supersession's own fact_id.
func TestReopenThroughStore(t *testing.T) {
	cas, _ := record.NewLocalCAS(t.TempDir())
	store := flg.NewStore(cas)
	seed(t, store)

	if resolveStore(t, store)["O-1101"].State != flg.StateDischarged {
		t.Fatal("precondition: should start discharged")
	}

	// The spec changed: an input to O-1101 was superseded. Append the fact.
	if _, err := store.AppendSupersession(flg.SupersessionEvidence{
		Subject: "O-1101", Kind: flg.KindCrossTypeInvalidation,
	}); err != nil {
		t.Fatal(err)
	}

	v := resolveStore(t, store)["O-1101"]
	if v.State != flg.StateReopened {
		t.Fatalf("expected reopened after supersession, got %s", v.State)
	}
	if len(v.Citations) == 0 || !strings.HasPrefix(v.Citations[0], "sha256:") {
		t.Fatalf("reopened verdict should cite the supersession fact_id, got %v", v.Citations)
	}
}
