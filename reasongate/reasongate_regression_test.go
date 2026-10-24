package reasongate_test

import (
	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/reasongate"
	"github.com/websines/structor-harness/resolver"
	"testing"
)

func TestAuditDanglingSupersessionProofMatchesVerdict(t *testing.T) {
	o := flg.Obligation{ID: "O", DomainTags: []string{"auth"}}
	evs := []flg.Evidence{
		{ID: "pass", ObligationID: "O", Polarity: flg.PolaritySupports, Validity: flg.ValidityValid, Reproducibility: flg.ReproDeterministic},
		{ID: "fail", ObligationID: "O", Polarity: flg.PolarityFalsifies, Validity: flg.ValidityValid, Reproducibility: flg.ReproDeterministic, Scope: flg.Scope{Tags: []string{"auth"}}},
	}
	scenarios := [][]flg.SupersessionEvidence{
		{{Subject: "fail", Kind: flg.KindSupersededBy, ReplacedBy: []string{"missing"}}},
		{{Subject: "fail", Kind: flg.KindSupersededBy, ReplacedBy: []string{"pass"}}, {Subject: "pass", Kind: flg.KindSupersededBy, ReplacedBy: []string{"fail"}}},
		{{Subject: "fail", Kind: flg.KindExcludeMarker}},
		{{Subject: "fail", Kind: flg.KindSupersededBy, ReplacedBy: []string{"pass"}}},
		{{Subject: "fail", Kind: flg.KindSupersededBy, ReplacedBy: []string{"pass"}}, {Subject: "pass", Kind: flg.KindSupersededBy, ReplacedBy: []string{"missing"}}},
	}
	for _, sups := range scenarios {
		hand := resolver.ResolveObligation(o, evs, sups)
		datalog, _ := reasongate.ResolveViaDatalog(o, evs, sups)
		if hand.State != datalog {
			t.Fatalf("resolver=%s proof=%s violations=%+v supersessions=%+v", hand.State, datalog, hand.Violations, sups)
		}
	}
}
