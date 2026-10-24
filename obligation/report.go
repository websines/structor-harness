// This file implements exposure reporting (system design §16): "exposure, not
// coverage." It answers one honest question over a risk surface — what fraction
// has NO governing valid support — and returns the counts behind it, never a
// single flattering headline number that easy claims over harmless code could
// move. Governing support is the resolver's call (resolver.Governs): only a
// `discharged` verdict governs; falsified, contested, reopened, unverifiable,
// open and audit_only all count as nothing (§6.1, Rule 2).
package obligation

import (
	"sort"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/resolver"
)

// ExposureReport is the honest tally over a risk surface. Exposure is the number
// that must go down; the counts exist so no one can move it with easy claims over
// harmless code (§16). It reports counts, not a coverage percentage.
type ExposureReport struct {
	// RiskySurface is the number of risky obligations considered.
	RiskySurface int
	// Governed is how many have governing valid support (a `discharged` verdict).
	Governed int
	// Exposed is how many have NOTHING governing them — the numerator of Exposure.
	Exposed int
	// ExposedObligations is panel 1 (§16): "risky paths with nothing governing
	// them", sorted by id. This is the list that matters.
	ExposedObligations []string
	// ByState breaks the exposed set out by derived state, so the report shows WHY
	// each risky path is unproven (falsified vs. merely open vs. unverifiable …).
	ByState map[flg.OblState]int
	// Exposure is Exposed / RiskySurface, in [0,1]; 0 when the surface is empty.
	// Deliberately framed as exposure, not coverage — an easy positive claim over
	// harmless code cannot move it.
	Exposure float64
}

// Exposure computes the exposure report for a risk surface (a set of risky
// obligations — code touching money, auth, personal data, or an external
// boundary; §16) given their resolver verdicts (keyed by id, as ResolveAll
// returns). Pure: no clock, no I/O. A missing verdict is treated as `open` —
// unproven, and therefore exposed. Nothing here computes a coverage headline.
func Exposure(risky []flg.Obligation, verdicts map[string]flg.Verdict) ExposureReport {
	rep := ExposureReport{
		RiskySurface: len(risky),
		ByState:      map[flg.OblState]int{},
	}
	for _, o := range risky {
		state := flg.StateOpen
		if v, ok := verdicts[o.ID]; ok {
			state = v.State
		}
		if resolver.Governs(state) {
			rep.Governed++
			continue
		}
		rep.Exposed++
		rep.ByState[state]++
		rep.ExposedObligations = append(rep.ExposedObligations, o.ID)
	}
	sort.Strings(rep.ExposedObligations)
	if rep.RiskySurface > 0 {
		rep.Exposure = float64(rep.Exposed) / float64(rep.RiskySurface)
	}
	return rep
}
