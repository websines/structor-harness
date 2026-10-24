// Package resolver is THE CORE — the five rules of conflict resolution
// (system design §8). Port of forge-showcase/src/lib/resolver.ts.
//
// Pure. No DB, no I/O, NO CLOCK, and no timestamp anywhere in its inputs, so
// "recency is authority" is structurally unrepresentable rather than merely
// forbidden. Every verdict cites the evidence that produced it.
//
// The RULE ORDERING BELOW IS THE SPECIFICATION. Read resolveObligation
// top-to-bottom and you see the five rules in order, with no clock in scope.
package resolver

import (
	"sort"

	"github.com/websines/structor-harness/flg"
)

// nonGoverning is the set of states that never govern (mirrors resolver.ts).
var nonGoverning = map[flg.OblState]bool{
	flg.StateFalsified:    true,
	flg.StateContested:    true,
	flg.StateReopened:     true,
	flg.StateUnverifiable: true,
	flg.StateAuditOnly:    true,
}

// Governs reports whether a derived state is a governing truth. `open` and every
// non-governing state return false; only `discharged` governs.
func Governs(state flg.OblState) bool {
	return !nonGoverning[state] && state != flg.StateOpen
}

// InScope reports whether a falsifying witness lands inside the obligation's
// declared domain. The wildcard "*" matches everything; otherwise the tag sets
// must intersect.
//
// Exported because a UI must be able to explain why a recorded, signed falsifier
// changed nothing -- and a hand copy of this predicate in the UI layer is a
// correctness bug waiting to happen. It is a pure predicate over two already
// exported types: no state, no invariant, nothing a caller can corrupt.
func InScope(ev flg.Evidence, obl flg.Obligation) bool { return inScope(ev, obl) }

func inScope(ev flg.Evidence, obl flg.Obligation) bool {
	for _, t := range ev.Scope.Tags {
		if t == "*" {
			return true
		}
	}
	for _, t := range ev.Scope.Tags {
		for _, d := range obl.DomainTags {
			if t == d {
				return true
			}
		}
	}
	return false
}

// ResolveObligation derives one obligation's verdict from evidence +
// supersessions ONLY. The ordering is Rule 3: validity first, then polarity +
// scope, and only then reproducibility.
func ResolveObligation(obl flg.Obligation, evidence []flg.Evidence, supersessions []flg.SupersessionEvidence) (verdict flg.Verdict) {
	// 1. Facts explicitly superseded stop counting — but ONLY if the supersession
	// chain VALIDLY resolves to an active head (§2.3, multihop, reachability-based).
	// An exclude_marker keeps a fact live (audit context). A dangling / cyclic /
	// audit-dead-end chain is AMBIGUOUS: it never drops a fact by fiat — the fact
	// stays live and a repair violation is surfaced on the verdict. The 7-state
	// machine below is UNCHANGED; it runs over the validly-resolved live set.
	superseded, violations := resolveSupersession(obl.ID, evidence, supersessions)
	if len(violations) > 0 {
		defer func() { verdict.Violations = violations }()
	}
	var live []flg.Evidence
	for _, e := range evidence {
		if e.ObligationID == obl.ID && !superseded[e.ID] {
			live = append(live, e)
		}
	}

	// 2. Retired scope never governs — kept for history only.
	if obl.Retired {
		return flg.Verdict{
			ObligationID: obl.ID,
			State:        flg.StateAuditOnly,
			Citations:    []string{},
			Reason:       "scope retired — kept for history, never governs",
		}
	}

	// 3. Invalid evidence never governs. Infrastructure failure is not a factual
	// contradiction — it derives `unverifiable`, not `contested`.
	var broken, valid []flg.Evidence
	for _, e := range live {
		if e.Validity != flg.ValidityValid {
			broken = append(broken, e)
		} else {
			valid = append(valid, e)
		}
	}

	// 4. Split valid evidence by polarity. A falsifier only counts if in-scope.
	var falsifiersAll, supports []flg.Evidence
	for _, e := range valid {
		switch e.Polarity {
		case flg.PolarityFalsifies:
			if inScope(e, obl) {
				falsifiersAll = append(falsifiersAll, e)
			}
		case flg.PolaritySupports:
			supports = append(supports, e)
		}
	}

	// 5. Only a REPRODUCIBLE witness dominates automatically. A testimonial
	// "I think this is broken" is a judgment, not a witness — it cannot
	// auto-falsify, it can only contest. Letting testimony dominate would be
	// fiat by another name (forbidden by Law 3).
	var falsifiers []flg.Evidence
	for _, e := range falsifiersAll {
		if e.Reproducibility != flg.ReproTestimonial {
			falsifiers = append(falsifiers, e)
		}
	}
	// A valid, in-scope, reproducible witness defeats a universal claim
	// automatically. A passing example over a narrower domain cannot tie it.
	if len(falsifiers) > 0 {
		return flg.Verdict{
			ObligationID: obl.ID,
			State:        flg.StateFalsified,
			Citations:    ids(falsifiers),
			Reason:       "valid in-scope witness — " + falsifiers[0].Summary,
		}
	}

	// 6. The obligation was reopened because an input was superseded: its support
	// is gone, and nothing may re-close it without a fresh run.
	var reopenCites []string
	reopened := false
	for _, s := range supersessions {
		if s.Subject == obl.ID && s.Kind == flg.KindCrossTypeInvalidation {
			reopened = true
		}
		if s.Subject == obl.ID {
			// Cite the supersession fact's own id when it has one (a stored fact,
			// §5.5); fall back to the subject for anonymous conformance vectors.
			if s.ID != "" {
				reopenCites = append(reopenCites, s.ID)
			} else {
				reopenCites = append(reopenCites, s.Subject)
			}
		}
	}
	if reopened {
		return flg.Verdict{
			ObligationID: obl.ID,
			State:        flg.StateReopened,
			Citations:    reopenCites,
			Reason:       "an input was superseded — auto-reopen, never auto-close",
		}
	}

	// 7. Only invalid evidence and no support → cannot currently be checked.
	if len(supports) == 0 && len(broken) > 0 {
		return flg.Verdict{
			ObligationID: obl.ID,
			State:        flg.StateUnverifiable,
			Citations:    ids(broken),
			Reason:       "required evidence cannot currently be checked — replay envelope unavailable",
		}
	}

	// 8. Contested is reserved for genuinely incompatible TESTIMONIAL judgments
	// where neither side semantically dominates. Reproducible disagreement is
	// re-run, not voted on.
	if hasTestimonial(supports) && hasTestimonial(falsifiersAll) {
		return flg.Verdict{
			ObligationID: obl.ID,
			State:        flg.StateContested,
			Citations:    testimonialIDs(valid),
			Reason:       "incompatible testimonial judgments — neither dominates; needs a signed Resolution",
		}
	}

	// 9. Valid support, no standing falsifier → discharged.
	if len(supports) > 0 {
		return flg.Verdict{
			ObligationID: obl.ID,
			State:        flg.StateDischarged,
			Citations:    ids(supports),
			Reason:       "acceptance policy satisfied by valid in-scope support, no standing falsifier",
			Policy:       obl.AcceptancePolicy,
		}
	}

	// 10. No evidence yet.
	return flg.Verdict{
		ObligationID: obl.ID,
		State:        flg.StateOpen,
		Citations:    []string{},
		Reason:       "no evidence yet",
	}
}

// ResolveAll derives a verdict for every obligation, keyed by obligation id.
func ResolveAll(obligations []flg.Obligation, evidence []flg.Evidence, supersessions []flg.SupersessionEvidence) map[string]flg.Verdict {
	out := make(map[string]flg.Verdict, len(obligations))
	for _, o := range obligations {
		out[o.ID] = ResolveObligation(o, evidence, supersessions)
	}
	return out
}

// TheoremState derives a theorem's state from its leaf verdicts. A theorem is
// established iff every governing leaf is discharged (or audit_only) — a
// derivation, never a stored flag. Any falsified/contested/unverifiable/reopened
// leaf propagates in that precedence order.
func TheoremState(leaves []flg.Verdict) string {
	if len(leaves) == 0 {
		return "open"
	}
	if anyState(leaves, flg.StateFalsified) {
		return "falsified"
	}
	if anyState(leaves, flg.StateContested) {
		return "contested"
	}
	if anyState(leaves, flg.StateUnverifiable) {
		return "unverifiable"
	}
	if anyState(leaves, flg.StateReopened) {
		return "re-establishing"
	}
	allEstablished := true
	for _, v := range leaves {
		if v.State != flg.StateDischarged && v.State != flg.StateAuditOnly {
			allEstablished = false
			break
		}
	}
	if allEstablished {
		return "established"
	}
	return "open"
}

// resolveSupersession is the resolver-local, REACHABILITY-based (not Tarjan)
// supersession resolution (§2.3). A fact of the obligation is validly superseded —
// and so drops from the live set — ONLY if its replacement chain reaches an active
// head. A chain that dangles (replacement not present), cycles, or dead-ends in an
// audit-only fact is AMBIGUOUS: the fact is KEPT LIVE and a repair Violation is
// surfaced (never a fiat drop). Chains per obligation are tiny, so plain reachability
// with a per-path `seen` set is both correct and cheap. Deterministic (Law 3): the
// superseded set is order-free and violations are emitted in sorted-subject order.
func resolveSupersession(oblID string, evidence []flg.Evidence, sups []flg.SupersessionEvidence) (map[string]bool, []flg.Violation) {
	universe := map[string]bool{}
	for _, e := range evidence {
		if e.ObligationID == oblID {
			universe[e.ID] = true
		}
	}
	excluded := map[string]bool{}    // exclude_marker subjects: kept live (audit)
	targets := map[string][]string{} // non-exclude supersede subject -> replacement heads
	hasSup := map[string]bool{}
	for _, s := range sups {
		if s.Kind == flg.KindExcludeMarker {
			excluded[s.Subject] = true
			continue
		}
		hasSup[s.Subject] = true
		targets[s.Subject] = append(targets[s.Subject], s.ReplacedBy...)
	}

	subjects := make([]string, 0, len(hasSup))
	for s := range hasSup {
		subjects = append(subjects, s)
	}
	sort.Strings(subjects) // determinism

	superseded := map[string]bool{}
	var violations []flg.Violation
	for _, subj := range subjects {
		if !universe[subj] {
			continue
		}
		if prob := resolves(subj, targets, universe, excluded); prob == "" {
			superseded[subj] = true // every immediate replacement is a valid head → valid drop
		} else {
			violations = append(violations, flg.Violation{
				Subject: subj,
				Problem: prob,
				Detail:  "supersession of " + subj + " does not validly resolve (" + prob + "); fact kept live",
			})
		}
	}
	return superseded, violations
}

// Classification is the per-node supersession view an observer renders: which evidence
// facts of an obligation are validly superseded, and which supersessions are broken
// (violations). It reuses the EXACT reachability resolution ResolveObligation uses
// (§2.3, multihop), so a graph view can never diverge from the verdict.
type Classification struct {
	Superseded map[string]bool
	Violations []flg.Violation
}

// ClassifySupersession exposes the resolver's reachability-based supersession resolution
// to observers (internal/supgraph) — shared semantics, never a reimplementation.
func ClassifySupersession(oblID string, evidence []flg.Evidence, sups []flg.SupersessionEvidence) Classification {
	s, v := resolveSupersession(oblID, evidence, sups)
	return Classification{Superseded: s, Violations: v}
}

// resolves returns "" if node validly drops (every immediate replacement is a VALID
// HEAD), else the repair problem for node. The fix is LOCAL (§2.3): a replacement `t` is
// a valid head iff it EXISTS, is not audit, and is not itself a cycle member — REGARDLESS
// of t's own (possibly broken) later supersession. So a broken link deep in a chain flags
// ONLY its own node and can never revive an upstream validly-superseded fact by fiat.
// A superseded fact with no named replacement is a valid retire (empty targets → "").
func resolves(node string, targets map[string][]string, universe, excluded map[string]bool) string {
	for _, t := range targets[node] {
		switch {
		case !universe[t]:
			return "dangling_replacement" // replacement does not exist
		case excluded[t]:
			return "replaced_by_audit_only" // replacement is audit-only (never governs)
		case inCycle(t, targets):
			return "supersession_cycle" // replacement is a cycle member → not a valid head
		}
		// else: t exists, is not audit, and is not a cycle member → a VALID head for
		// node, even if t is itself superseded (validly or ambiguously). t's own broken
		// link is t's violation and must NOT propagate up to node.
	}
	return ""
}

// inCycle reports whether `start` reaches itself via one or more replacement steps — the
// reachability characterization of a supersession cycle (no Tarjan; chains are tiny). A
// node with no replacement chain back to itself is never a cycle member.
func inCycle(start string, targets map[string][]string) bool {
	var walk func(n string, seen map[string]bool) bool
	walk = func(n string, seen map[string]bool) bool {
		for _, t := range targets[n] {
			if t == start {
				return true
			}
			if seen[t] {
				continue
			}
			seen[t] = true
			if walk(t, seen) {
				return true
			}
		}
		return false
	}
	return walk(start, map[string]bool{})
}

// --- small pure helpers ---------------------------------------------------

func ids(evs []flg.Evidence) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.ID)
	}
	return out
}

func hasTestimonial(evs []flg.Evidence) bool {
	for _, e := range evs {
		if e.Reproducibility == flg.ReproTestimonial {
			return true
		}
	}
	return false
}

func testimonialIDs(evs []flg.Evidence) []string {
	var out []string
	for _, e := range evs {
		if e.Reproducibility == flg.ReproTestimonial {
			out = append(out, e.ID)
		}
	}
	return out
}

func anyState(leaves []flg.Verdict, s flg.OblState) bool {
	for _, v := range leaves {
		if v.State == s {
			return true
		}
	}
	return false
}
