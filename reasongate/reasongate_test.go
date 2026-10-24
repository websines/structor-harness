package reasongate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/reason"
	"github.com/websines/structor-harness/resolver"
)

// vectorsDir is the shared, language-neutral oracle. From
// engine/internal/reasongate/ it lives at the workspace-level conformance/vectors/.
const vectorsDir = "../conformance/vectors"

// vector mirrors the shared JSON schema exactly (build guide §4) — the same
// struct used by internal/resolver/conformance_test.go.
type vector struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Obligation struct {
		DomainTags       []string `json:"domainTags"`
		AcceptancePolicy string   `json:"acceptancePolicy"`
		Retired          bool     `json:"retired"`
	} `json:"obligation"`
	Evidence []struct {
		Polarity        string   `json:"polarity"`
		Method          string   `json:"method"`
		Reproducibility string   `json:"reproducibility"`
		Validity        string   `json:"validity"`
		ScopeTags       []string `json:"scopeTags"`
	} `json:"evidence"`
	Supersessions []struct {
		Subject    string   `json:"subject"`
		Kind       string   `json:"kind"`
		ReplacedBy []string `json:"replacedBy"`
		Evidence   []string `json:"evidence"`
	} `json:"supersessions"`
	Expect string `json:"expect"`
}

// build turns a vector into the resolver's typed inputs, mirroring the
// construction in internal/resolver/conformance_test.go exactly.
func (v vector) build() (flg.Obligation, []flg.Evidence, []flg.SupersessionEvidence) {
	obl := flg.Obligation{
		ID:               "O-TEST",
		AcceptancePolicy: v.Obligation.AcceptancePolicy,
		DomainTags:       v.Obligation.DomainTags,
		Retired:          v.Obligation.Retired,
	}
	evidence := make([]flg.Evidence, 0, len(v.Evidence))
	for i, e := range v.Evidence {
		evidence = append(evidence, flg.Evidence{
			ID:              fmt.Sprintf("E%d", i),
			ObligationID:    "O-TEST",
			Polarity:        flg.Polarity(e.Polarity),
			Method:          flg.Method(e.Method),
			Reproducibility: flg.Reproducibility(e.Reproducibility),
			Validity:        flg.Validity(e.Validity),
			Scope:           flg.Scope{Tags: e.ScopeTags},
			PrincipalID:     "P-TEST",
			Commit:          "0000000",
			Summary:         "conformance vector evidence",
		})
	}
	sups := make([]flg.SupersessionEvidence, 0, len(v.Supersessions))
	for _, s := range v.Supersessions {
		sups = append(sups, flg.SupersessionEvidence{
			Subject:    s.Subject,
			Kind:       flg.SupersessionKind(s.Kind),
			ReplacedBy: s.ReplacedBy,
			Evidence:   s.Evidence,
		})
	}
	return obl, evidence, sups
}

func loadVector(t *testing.T, name string) vector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(vectorsDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var v vector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal %s: %v", name, err)
	}
	return v
}

// TestAgreesWithResolverOnVectors runs the proof-carrying Datalog resolver on
// every conformance vector and asserts its state equals BOTH the vector's
// expected state and the hand-coded resolver's state.
func TestAgreesWithResolverOnVectors(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(vectorsDir, "*.json"))
	if err != nil {
		t.Fatalf("glob %s: %v", vectorsDir, err)
	}
	if len(files) == 0 {
		t.Skipf("no conformance vectors at %s; skipping", vectorsDir)
	}
	for _, path := range files {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			var v vector
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatalf("unmarshal %s: %v", path, err)
			}
			obl, evs, sups := v.build()

			got, proof := ResolveViaDatalog(obl, evs, sups)
			if string(got) != v.Expect {
				t.Errorf("vector %s (%s): datalog got %q, want %q\nproof:\n%s",
					v.ID, v.Name, got, v.Expect, proofString(proof))
			}
			want := resolver.ResolveObligation(obl, evs, sups).State
			if string(got) != string(want) {
				t.Errorf("vector %s (%s): datalog %q disagrees with resolver %q",
					v.ID, v.Name, got, want)
			}
		})
	}
}

// TestProofTreesPrinted surfaces the derivations for the discharged (V2) and
// falsified (V3) vectors so the proof trees are visible in `go test -v` output.
func TestProofTreesPrinted(t *testing.T) {
	if _, err := os.Stat(filepath.Join(vectorsDir, "V2.json")); err != nil {
		t.Skipf("conformance vectors unavailable: %v", err)
	}

	v2 := loadVector(t, "V2.json")
	obl, evs, sups := v2.build()
	st, proof := ResolveViaDatalog(obl, evs, sups)
	if st != flg.StateDischarged {
		t.Fatalf("V2 state = %q, want discharged", st)
	}
	if proof == nil || proof.Fact.Key() != "discharged(O-TEST)" {
		t.Fatalf("V2 proof root = %s, want discharged(O-TEST)", rootKey(proof))
	}
	t.Logf("V2 (discharged) proof:\n%s", proof.String())

	v3 := loadVector(t, "V3.json")
	obl, evs, sups = v3.build()
	st, proof = ResolveViaDatalog(obl, evs, sups)
	if st != flg.StateFalsified {
		t.Fatalf("V3 state = %q, want falsified", st)
	}
	if proof == nil || proof.Fact.Key() != "falsified(O-TEST)" {
		t.Fatalf("V3 proof root = %s, want falsified(O-TEST)", rootKey(proof))
	}
	t.Logf("V3 (falsified) proof:\n%s", proof.String())
}

func proofString(p *reason.ProofNode) string {
	if p == nil {
		return "<nil>"
	}
	return p.String()
}

func rootKey(p *reason.ProofNode) string {
	if p == nil {
		return "<nil>"
	}
	return p.Fact.Key()
}
