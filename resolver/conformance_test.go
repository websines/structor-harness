package resolver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/websines/structor-harness/flg"
)

// vectorsDir is the shared, language-neutral oracle (build guide §4). It is
// owned by a sibling agent; from engine/internal/resolver/ it lives at the
// workspace-level conformance/vectors/.
const vectorsDir = "../conformance/vectors"

// vector mirrors the shared JSON schema exactly (build guide §4).
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

// build turns a vector into the resolver's typed inputs, with placeholder
// provenance the schema does not carry.
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

func TestConformanceVectors(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(vectorsDir, "*.json"))
	if err != nil {
		t.Fatalf("glob %s: %v", vectorsDir, err)
	}
	if len(files) == 0 {
		t.Skipf("no conformance vectors at %s — sibling agent owns conformance/vectors/; skipping", vectorsDir)
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
			obl, evidence, sups := v.build()
			got := ResolveObligation(obl, evidence, sups).State
			if string(got) != v.Expect {
				t.Errorf("vector %s (%s): got %q, want %q", v.ID, v.Name, got, v.Expect)
			}
		})
	}
}
