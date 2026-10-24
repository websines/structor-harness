// Package reasongate expresses Forge's five-rule conflict resolver (system
// design §8, internal/resolver) AS a stratified Datalog program over the reason
// engine. Because reason is proof-native, every obligation verdict produced here
// is PROOF-CARRYING: the returned ProofNode is a machine-checkable derivation of
// the state. The reasongate tests verify this Datalog encoding agrees, verdict
// for verdict, with the hand-coded resolver on every conformance vector.
//
// The resolver's precedence (retired > falsified > reopened > unverifiable >
// contested > discharged > open) is NOT hand-ordered here: it falls out of the
// rules' negative dependencies and the stratified fixpoint, then a final
// priority scan reads off the single state that holds.
package reasongate

import (
	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/reason"
	"github.com/websines/structor-harness/resolver"
)

// programRules is the resolver-as-Datalog program in source order. Body order is
// significant: the positive binder is always first, so every negated atom is
// safe (all its variables are already bound when it is evaluated). Stratified
// evaluation (reason.Solve) picks the actual firing order.
var programRules = []string{
	"in_scope(E,O) :- evidence_scope(E,T), obligation_domain(O,T).",
	"live(E,O) :- evidence(E,O), not superseded(E).",
	"valid_support(E,O) :- live(E,O), polarity(E,supports), valid(E).",
	"gov_falsifier(E,O) :- live(E,O), polarity(E,falsifies), valid(E), in_scope(E,O), reproducible(E).",
	"scope_falsifier(E,O) :- live(E,O), polarity(E,falsifies), valid(E), in_scope(E,O).",
	"testimonial_support(E,O) :- valid_support(E,O), testimonial(E).",
	"testimonial_falsifier(E,O) :- scope_falsifier(E,O), testimonial(E).",
	"live_broken(E,O) :- evidence(E,O), not superseded(E), broken(E).",
	"has_support(O) :- valid_support(E,O).",
	"has_gov_falsifier(O) :- gov_falsifier(E,O).",
	"has_broken(O) :- live_broken(E,O).",
	"has_testimonial_support(O) :- testimonial_support(E,O).",
	"has_testimonial_falsifier(O) :- testimonial_falsifier(E,O).",
	"audit_only(O) :- obligation(O), retired(O).",
	"falsified(O) :- obligation(O), has_gov_falsifier(O), not retired(O).",
	"reopened(O) :- obligation(O), cross_type_inval(O), not retired(O), not falsified(O).",
	"unverifiable(O) :- obligation(O), has_broken(O), not has_support(O), not retired(O), not falsified(O), not reopened(O).",
	"contested(O) :- obligation(O), has_testimonial_support(O), has_testimonial_falsifier(O), not retired(O), not falsified(O), not reopened(O), not unverifiable(O).",
	"discharged(O) :- obligation(O), has_support(O), not retired(O), not falsified(O), not reopened(O), not unverifiable(O), not contested(O).",
	"open(O) :- obligation(O), not audit_only(O), not falsified(O), not reopened(O), not unverifiable(O), not contested(O), not discharged(O).",
}

// statePriority maps each terminal state predicate to its OblState, in the
// resolver's precedence order. The first predicate whose fact is derived wins.
var statePriority = []struct {
	pred  string
	state flg.OblState
}{
	{"audit_only", flg.StateAuditOnly},
	{"falsified", flg.StateFalsified},
	{"reopened", flg.StateReopened},
	{"unverifiable", flg.StateUnverifiable},
	{"contested", flg.StateContested},
	{"discharged", flg.StateDischarged},
	{"open", flg.StateOpen},
}

// ResolveViaDatalog derives one obligation's verdict by running the resolver's
// rules AS a stratified Datalog program over facts loaded from the record, and
// returns the derived state plus the proof tree that justifies it.
func ResolveViaDatalog(obl flg.Obligation, evs []flg.Evidence, sups []flg.SupersessionEvidence) (flg.OblState, *reason.ProofNode) {
	e := reason.New()
	oid := obl.ID

	// --- EDB facts loaded from the record ---------------------------------
	e.AddFact(reason.Fact{Pred: "obligation", Args: []string{oid}})
	if obl.Retired {
		e.AddFact(reason.Fact{Pred: "retired", Args: []string{oid}})
	}
	for _, t := range obl.DomainTags {
		e.AddFact(reason.Fact{Pred: "obligation_domain", Args: []string{oid, t}})
	}
	classification := resolver.ClassifySupersession(oid, evs, sups)
	for subject := range classification.Superseded {
		e.AddFact(reason.Fact{Pred: "superseded", Args: []string{subject}})
	}
	for _, s := range sups {

		if s.Kind == flg.KindCrossTypeInvalidation && s.Subject == oid {
			e.AddFact(reason.Fact{Pred: "cross_type_inval", Args: []string{oid}})
		}
	}
	for _, ev := range evs {
		if ev.ObligationID != oid {
			continue
		}
		e.AddFact(reason.Fact{Pred: "evidence", Args: []string{ev.ID, oid}})
		e.AddFact(reason.Fact{Pred: "polarity", Args: []string{ev.ID, string(ev.Polarity)}})
		if ev.Validity == flg.ValidityValid {
			e.AddFact(reason.Fact{Pred: "valid", Args: []string{ev.ID}})
		} else {
			e.AddFact(reason.Fact{Pred: "broken", Args: []string{ev.ID}})
		}
		if ev.Reproducibility == flg.ReproTestimonial {
			e.AddFact(reason.Fact{Pred: "testimonial", Args: []string{ev.ID}})
		} else {
			e.AddFact(reason.Fact{Pred: "reproducible", Args: []string{ev.ID}})
		}
		for _, t := range ev.Scope.Tags {
			e.AddFact(reason.Fact{Pred: "evidence_scope", Args: []string{ev.ID, t}})
		}
	}

	// --- Rules ------------------------------------------------------------
	for _, src := range programRules {
		r, err := reason.ParseRule(src)
		if err != nil {
			// The program is a compile-time constant; a parse error is a bug.
			panic("reasongate: bad rule " + src + ": " + err.Error())
		}
		e.AddRule(r)
	}
	// The wildcard scope rule cannot be parsed: the '*' constant is not a token.
	// Build it programmatically. Term("*") is a constant (not uppercase).
	e.AddRule(reason.Rule{
		Name: "in_scope_wildcard",
		Head: reason.Atom{Pred: "in_scope", Args: []reason.Term{"E", "O"}},
		Body: []reason.Atom{
			{Pred: "evidence_scope", Args: []reason.Term{"E", reason.Term("*")}},
			{Pred: "obligation", Args: []reason.Term{"O"}},
		},
	})

	if err := e.Solve(); err != nil {
		// The resolver program is stratified by construction; an error is a bug.
		panic("reasongate: resolver program is not stratifiable: " + err.Error())
	}

	// --- Read off the single governing state, highest precedence first ----
	for _, sp := range statePriority {
		f := reason.Fact{Pred: sp.pred, Args: []string{oid}}
		if e.Has(f) {
			proof, _ := e.Proof(f)
			return sp.state, proof
		}
	}
	// open is the catch-all, so this is unreachable in practice.
	return flg.StateOpen, nil
}
