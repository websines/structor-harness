package resolver

//go:generate go run ./canongen

import "github.com/websines/structor-harness/flg"

// This file is the small, hand-written GENERIC EVALUATOR for the resolver's canonical
// evidence→state rule table. The RULES themselves are DATA: the single source of truth is
// conformance/canonical/evidence_state_rules.json, code-generated into ruletable_gen.go
// (var canonicalRuleTable) by `go run ./internal/resolver/canongen`.
//
// Phase 2 of the spec↔impl bridge. Before this, the resolver's decision rules were
// hand-written in three places (this package's ResolveObligation, reasongate's Datalog,
// the Lean model) and only CHECKED equal. Now they exist as ONE canonical data table with
// a generated Go projection: this evaluator + the generated table is proven equal to the
// hand-coded ResolveObligation over the ENTIRE 64-class evidence→state domain
// (canonical_test.go), and the Lean model is proven equal to the SAME table in
// proofs/ForgeProofs/Canonical.lean. The trusted hand-written surface shrinks to the
// predicate extractors below plus the table walk — the rule ORDERING is data, not code.
//
// Scope: evidence→state only. The supersession GRAPH (reopened / crossTypeInvalidated) is
// Phase 3 and absent from the table. The hand-coded ResolveObligation stays the live
// authority; EvalCanonical is a verified-equivalent shadow (retiring the hand-coded path
// in favour of the generated table is a safe follow-up once trusted).

// canonRule is one priority-ordered rule of the canonical table: the first rule whose
// guard predicates ALL hold determines the state.
type canonRule struct {
	Guard []string
	State flg.OblState
}

// canonPredicates computes the canonical evidence→state predicate vocabulary from an
// obligation and its evidence, over LIVE evidence (superseded facts dropped) — mirroring
// ResolveObligation's own definitions exactly, so the two cannot diverge silently (the
// exhaustive test is the guard). crossTypeInvalidated/reopened is Phase 3, not here.
func canonPredicates(obl flg.Obligation, evidence []flg.Evidence, supersessions []flg.SupersessionEvidence) map[string]bool {
	superseded := map[string]bool{}
	for _, s := range supersessions {
		if s.Kind != flg.KindExcludeMarker {
			superseded[s.Subject] = true
		}
	}
	var live []flg.Evidence
	for _, e := range evidence {
		if e.ObligationID == obl.ID && !superseded[e.ID] {
			live = append(live, e)
		}
	}

	hasWitness, hasValidSupport, broken := false, false, false
	testSupport, testFalsifier := false, false
	for _, e := range live {
		if e.Validity != flg.ValidityValid {
			broken = true
			continue
		}
		switch e.Polarity {
		case flg.PolaritySupports:
			hasValidSupport = true
			if e.Reproducibility == flg.ReproTestimonial {
				testSupport = true
			}
		case flg.PolarityFalsifies:
			if inScope(e, obl) {
				if e.Reproducibility == flg.ReproTestimonial {
					testFalsifier = true
				} else {
					hasWitness = true
				}
			}
		}
	}
	return map[string]bool{
		"retired":              obl.Retired,
		"has_witness":          hasWitness,
		"testimonial_conflict": testSupport && testFalsifier,
		"has_valid_support":    hasValidSupport,
		"broken":               broken,
	}
}

// EvalCanonical derives the evidence→state verdict by walking the code-generated canonical
// rule table (ruletable_gen.go) over the predicate vocabulary — first matching rule wins.
// Proven equal to ResolveObligation over the whole 64-class domain (canonical_test.go).
func EvalCanonical(obl flg.Obligation, evidence []flg.Evidence, supersessions []flg.SupersessionEvidence) flg.OblState {
	p := canonPredicates(obl, evidence, supersessions)
	for _, r := range canonicalRuleTable {
		ok := true
		for _, g := range r.Guard {
			if !p[g] {
				ok = false
				break
			}
		}
		if ok {
			return r.State
		}
	}
	return flg.StateOpen
}
