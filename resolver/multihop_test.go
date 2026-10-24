package resolver

import (
	"testing"

	"github.com/websines/structor-harness/flg"
)

// The multihop supersession dimension (§2.3): a fact drops ONLY if its replacement
// chain validly resolves to an active head; a dangling / cyclic / audit-dead-end chain
// is AMBIGUOUS — the fact is KEPT LIVE and a repair violation is surfaced. These tests
// pin each topology on the real ResolveObligation.

func mhObl() flg.Obligation { return flg.Obligation{ID: "O", DomainTags: []string{"x"}} }

// mhWitness is a valid, in-scope, reproducible falsifier — a "witness". If it stays
// live the verdict is falsified; if it validly drops, it stops counting.
func mhWitness(id string) flg.Evidence {
	return flg.Evidence{
		ID: id, ObligationID: "O", Polarity: flg.PolarityFalsifies,
		Reproducibility: flg.ReproDeterministic, Validity: flg.ValidityValid,
		Scope: flg.Scope{Tags: []string{"x"}},
	}
}

func supBy(subject string, replacedBy ...string) flg.SupersessionEvidence {
	return flg.SupersessionEvidence{Subject: subject, Kind: flg.KindSupersededBy, ReplacedBy: replacedBy}
}

// LINEAR VALID: E1 (witness) superseded by E2 (an active support) → E1 validly drops,
// E2 governs → discharged, no violation.
func TestMultihopLinearValidDrop(t *testing.T) {
	e1 := mhWitness("E1")
	e2 := flg.Evidence{ID: "E2", ObligationID: "O", Polarity: flg.PolaritySupports, Validity: flg.ValidityValid}
	v := ResolveObligation(mhObl(), []flg.Evidence{e1, e2}, []flg.SupersessionEvidence{supBy("E1", "E2")})
	if v.State != flg.StateDischarged {
		t.Errorf("linear valid drop: state=%q want discharged (E1 dropped, E2 governs)", v.State)
	}
	if len(v.Violations) != 0 {
		t.Errorf("linear valid drop: unexpected violations %+v", v.Violations)
	}
}

// DANGLING: the replacement isn't present → ambiguous. E1 (witness) is KEPT LIVE →
// falsified, and a dangling_replacement violation is surfaced (no fiat drop).
func TestMultihopDanglingKeepsAndFlags(t *testing.T) {
	v := ResolveObligation(mhObl(), []flg.Evidence{mhWitness("E1")}, []flg.SupersessionEvidence{supBy("E1", "GHOST")})
	if v.State != flg.StateFalsified {
		t.Errorf("dangling: state=%q want falsified (E1 kept live)", v.State)
	}
	if len(v.Violations) != 1 || v.Violations[0].Problem != "dangling_replacement" {
		t.Errorf("dangling: violations=%+v want 1 dangling_replacement", v.Violations)
	}
}

// CYCLE: E1 ↦ E2 ↦ E1. Neither chain reaches an active head → both KEPT LIVE →
// falsified, two supersession_cycle violations.
func TestMultihopCycleKeepsAndFlags(t *testing.T) {
	v := ResolveObligation(mhObl(), []flg.Evidence{mhWitness("E1"), mhWitness("E2")},
		[]flg.SupersessionEvidence{supBy("E1", "E2"), supBy("E2", "E1")})
	if v.State != flg.StateFalsified {
		t.Errorf("cycle: state=%q want falsified (both kept live)", v.State)
	}
	if len(v.Violations) != 2 {
		t.Fatalf("cycle: violations=%+v want 2", v.Violations)
	}
	for _, vi := range v.Violations {
		if vi.Problem != "supersession_cycle" {
			t.Errorf("cycle: problem=%q want supersession_cycle", vi.Problem)
		}
	}
}

// AUDIT DEAD-END: E1 superseded by E2, but E2 carries an exclude_marker (audit-only).
// The chain dead-ends in audit → ambiguous → E1 KEPT LIVE → falsified, one violation.
func TestMultihopAuditDeadEndKeepsAndFlags(t *testing.T) {
	e1 := mhWitness("E1")
	e2 := mhWitness("E2")
	sups := []flg.SupersessionEvidence{supBy("E1", "E2"), {Subject: "E2", Kind: flg.KindExcludeMarker}}
	v := ResolveObligation(mhObl(), []flg.Evidence{e1, e2}, sups)
	if v.State != flg.StateFalsified {
		t.Errorf("audit dead-end: state=%q want falsified (E1 kept live)", v.State)
	}
	if len(v.Violations) != 1 || v.Violations[0].Problem != "replaced_by_audit_only" {
		t.Errorf("audit dead-end: violations=%+v want 1 replaced_by_audit_only", v.Violations)
	}
}

// LOCALIZED (the fix): a broken supersession DEEP in a chain flags ONLY its own node and
// must NEVER revive an upstream validly-superseded fact. F (a witness/falsifier) is
// validly superseded by R (a live support); R is itself superseded by a GHOST (broken).
// R is still a VALID HEAD for F despite R's own broken replacement — so F STAYS dropped
// and the verdict is DISCHARGED (not falsified); only R carries a violation.
// (Under the pre-localization behavior this flipped to falsified — a fiat verdict change.)
func TestMultihopDeepBreakNeverRevivesUpstream(t *testing.T) {
	f := mhWitness("F") // a falsifier; if revived, the verdict would be falsified
	r := flg.Evidence{ID: "R", ObligationID: "O", Polarity: flg.PolaritySupports, Validity: flg.ValidityValid}
	sups := []flg.SupersessionEvidence{supBy("F", "R"), supBy("R", "GHOST")}
	v := ResolveObligation(mhObl(), []flg.Evidence{f, r}, sups)
	if v.State != flg.StateDischarged {
		t.Errorf("deep break: state=%q want discharged (F stays superseded, NOT revived by fiat)", v.State)
	}
	if len(v.Violations) != 1 || v.Violations[0].Subject != "R" || v.Violations[0].Problem != "dangling_replacement" {
		t.Errorf("deep break: violations=%+v want exactly one on R (dangling_replacement)", v.Violations)
	}
}

// LOCALIZED, audit variant: F superseded by R, R superseded by an audit-only A. R stays a
// valid head for F (its own audit-dead-end is R's violation) → F stays dropped, discharged.
func TestMultihopDeepAuditNeverRevivesUpstream(t *testing.T) {
	f := mhWitness("F")
	r := flg.Evidence{ID: "R", ObligationID: "O", Polarity: flg.PolaritySupports, Validity: flg.ValidityValid}
	a := flg.Evidence{ID: "A", ObligationID: "O", Polarity: flg.PolaritySupports, Validity: flg.ValidityValid}
	sups := []flg.SupersessionEvidence{supBy("F", "R"), supBy("R", "A"), {Subject: "A", Kind: flg.KindExcludeMarker}}
	v := ResolveObligation(mhObl(), []flg.Evidence{f, r, a}, sups)
	if v.State != flg.StateDischarged {
		t.Errorf("deep audit: state=%q want discharged (F not revived)", v.State)
	}
	if len(v.Violations) != 1 || v.Violations[0].Subject != "R" || v.Violations[0].Problem != "replaced_by_audit_only" {
		t.Errorf("deep audit: violations=%+v want exactly one on R (replaced_by_audit_only)", v.Violations)
	}
}

// DETERMINISM (Law 3): identical input → identical, sorted-by-subject violations.
func TestMultihopViolationsDeterministic(t *testing.T) {
	in := func() flg.Verdict {
		return ResolveObligation(mhObl(), []flg.Evidence{mhWitness("E1"), mhWitness("E2")},
			[]flg.SupersessionEvidence{supBy("E1", "G1"), supBy("E2", "G2")})
	}
	a, b := in().Violations, in().Violations
	if len(a) != 2 || len(b) != 2 {
		t.Fatalf("want 2 violations, got %d/%d", len(a), len(b))
	}
	if a[0].Subject != "E1" || a[1].Subject != "E2" {
		t.Errorf("violations not sorted by subject: %+v", a)
	}
	if a[0] != b[0] || a[1] != b[1] {
		t.Errorf("nondeterministic violations: %+v vs %+v", a, b)
	}
}
