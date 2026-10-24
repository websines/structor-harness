package resolver_test

import (
	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/resolver"
	"testing"
)

func TestAuditUnrelatedValidSupersessionDoesNotCreateRepair(t *testing.T) {
	evs := []flg.Evidence{
		{ID: "old", ObligationID: "A", Polarity: flg.PolaritySupports, Validity: flg.ValidityValid},
		{ID: "new", ObligationID: "A", Polarity: flg.PolaritySupports, Validity: flg.ValidityValid},
		{ID: "other", ObligationID: "B", Polarity: flg.PolaritySupports, Validity: flg.ValidityValid},
	}
	sups := []flg.SupersessionEvidence{{Subject: "old", Kind: flg.KindSupersededBy, ReplacedBy: []string{"new"}}}
	v := resolver.ResolveObligation(flg.Obligation{ID: "B"}, evs, sups)
	if len(v.Violations) > 0 {
		t.Fatalf("unrelated valid supersession produced repair violations: %+v", v.Violations)
	}
}
