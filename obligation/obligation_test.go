package obligation

import (
	"errors"
	"reflect"
	"testing"

	"github.com/websines/structor-harness/flg"
)

// obl is a tiny constructor for a leaf with a domain and an anchor file.
func obl(id string, tags []string, file string) flg.Obligation {
	return flg.Obligation{
		ID:               id,
		Title:            "leaf " + id,
		Anchor:           flg.Anchor{File: file, Symbol: id},
		AcceptancePolicy: "policy-" + id,
		DomainTags:       tags,
	}
}

// verdict is a tiny constructor for a derived verdict.
func verdict(id string, s flg.OblState) flg.Verdict {
	return flg.Verdict{ObligationID: id, State: s}
}

func TestRegistryRoundTrip(t *testing.T) {
	reg := NewRegistry()
	in := obl("O1", []string{"auth", "pii"}, "auth.go")
	if err := reg.Register(in); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, ok := reg.Lookup("O1")
	if !ok {
		t.Fatal("Lookup(O1): not found after Register")
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, in)
	}
	if _, ok := reg.Lookup("nope"); ok {
		t.Fatal("Lookup(nope): expected miss")
	}
	if reg.Len() != 1 {
		t.Fatalf("Len = %d, want 1", reg.Len())
	}

	// Empty id and duplicate id are refused, not silently accepted.
	if err := reg.Register(obl("", nil, "x.go")); !errors.Is(err, ErrEmptyID) {
		t.Fatalf("empty id: got %v, want ErrEmptyID", err)
	}
	if err := reg.Register(obl("O1", nil, "x.go")); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate id: got %v, want ErrDuplicate", err)
	}

	// Tag index associates the obligation with each declared domain.
	if by := reg.ByTag("auth"); len(by) != 1 || by[0].ID != "O1" {
		t.Fatalf("ByTag(auth) = %+v, want [O1]", by)
	}
	if by := reg.ByTag("unrelated"); len(by) != 0 {
		t.Fatalf("ByTag(unrelated) = %+v, want empty", by)
	}
}

func TestScopeSelectsOnlyTouched(t *testing.T) {
	reg := NewRegistry()
	must := func(err error) {
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	must(reg.Register(obl("O-auth", []string{"auth"}, "auth.go")))
	must(reg.Register(obl("O-pay", []string{"money"}, "pay.go")))
	must(reg.Register(obl("O-doc", []string{"docs"}, "readme.md")))

	// A change touching the "auth" domain and the file pay.go pulls in exactly
	// O-auth (tag) and O-pay (anchor file) — and nothing else.
	got := ScopeIDs(reg, Change{Tags: []string{"auth"}, Files: []string{"pay.go"}})
	want := []string{"O-auth", "O-pay"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Scope = %v, want %v", got, want)
	}

	// An unrelated change touches nothing.
	if got := ScopeIDs(reg, Change{Tags: []string{"telemetry"}}); len(got) != 0 {
		t.Fatalf("unrelated Scope = %v, want empty", got)
	}

	// The wildcard change touches everything (id-sorted).
	all := ScopeIDs(reg, Change{Tags: []string{"*"}})
	if !reflect.DeepEqual(all, []string{"O-auth", "O-doc", "O-pay"}) {
		t.Fatalf("wildcard Scope = %v, want all three sorted", all)
	}
}

func TestGateCountsAndReadiness(t *testing.T) {
	// One obligation in each of the seven states.
	touched := []flg.Obligation{
		obl("O-dis", nil, "a.go"),
		obl("O-fal", nil, "b.go"),
		obl("O-opn", nil, "c.go"),
		obl("O-unv", nil, "d.go"),
		obl("O-con", nil, "e.go"),
		obl("O-reo", nil, "f.go"),
		obl("O-aud", nil, "g.go"),
	}
	verdicts := map[string]flg.Verdict{
		"O-dis": verdict("O-dis", flg.StateDischarged),
		"O-fal": verdict("O-fal", flg.StateFalsified),
		"O-opn": verdict("O-opn", flg.StateOpen),
		"O-unv": verdict("O-unv", flg.StateUnverifiable),
		"O-con": verdict("O-con", flg.StateContested),
		"O-reo": verdict("O-reo", flg.StateReopened),
		"O-aud": verdict("O-aud", flg.StateAuditOnly),
	}
	// The discharged leaf additionally has a stale anchor (from the §4.8 sweep).
	stale := map[string]bool{"O-dis": true}

	g := Evaluate(touched, verdicts, stale)

	// Counts: exactly one per state.
	for _, s := range []flg.OblState{
		flg.StateDischarged, flg.StateFalsified, flg.StateOpen,
		flg.StateUnverifiable, flg.StateContested, flg.StateReopened, flg.StateAuditOnly,
	} {
		if g.Counts[s] != 1 {
			t.Fatalf("Counts[%s] = %d, want 1", s, g.Counts[s])
		}
	}

	// Readiness: falsified, contested, reopened, unverifiable (4 non-governing)
	// plus the one stale-anchor row = 5. open, audit_only and discharged do NOT
	// violate readiness.
	if len(g.Readiness) != 5 {
		t.Fatalf("Readiness rows = %d, want 5: %+v", len(g.Readiness), g.Readiness)
	}
	var staleRows, stateRows int
	for _, v := range g.Readiness {
		switch v.Kind {
		case "stale_anchor":
			staleRows++
			if v.ObligationID != "O-dis" {
				t.Fatalf("stale row on %s, want O-dis", v.ObligationID)
			}
		case "non_governing_state":
			stateRows++
			if _, blocking := readinessBlockingStates[v.State]; !blocking {
				t.Fatalf("non-governing row for non-blocking state %s", v.State)
			}
		default:
			t.Fatalf("unknown readiness kind %q", v.Kind)
		}
	}
	if staleRows != 1 || stateRows != 4 {
		t.Fatalf("rows: stale=%d state=%d, want 1 and 4", staleRows, stateRows)
	}

	// The verdict is advisory-not-ready, and the gate is informational, never a
	// hard block from proof state (Law 5).
	if g.Verdict != VerdictNotReady {
		t.Fatalf("Verdict = %s, want %s", g.Verdict, VerdictNotReady)
	}
	if !g.Informational {
		t.Fatal("gate must be informational")
	}
	if g.Blocking || g.HardDeny {
		t.Fatal("proof state alone must never set Blocking or HardDeny")
	}
}

func TestGateCleanChangeIsReady(t *testing.T) {
	// A supporting, valid, reproducible witness discharges the leaf via the real
	// resolver — exercising the EvaluateWithResolver call path.
	o := obl("O1", []string{"auth"}, "auth.go")
	ev := []flg.Evidence{{
		ID:              "E1",
		ObligationID:    "O1",
		Polarity:        flg.PolaritySupports,
		Method:          flg.MethodDeterministicCheck,
		Reproducibility: flg.ReproDeterministic,
		Validity:        flg.ValidityValid,
		Scope:           flg.Scope{Tags: []string{"auth"}},
		Summary:         "static invariant holds",
	}}
	g := EvaluateWithResolver([]flg.Obligation{o}, ev, nil, nil)

	if g.Counts[flg.StateDischarged] != 1 {
		t.Fatalf("expected one discharged, got counts %+v", g.Counts)
	}
	if len(g.Readiness) != 0 {
		t.Fatalf("clean change should have no readiness rows, got %+v", g.Readiness)
	}
	if g.Verdict != VerdictReady {
		t.Fatalf("Verdict = %s, want %s", g.Verdict, VerdictReady)
	}
}

func TestEscapeHatchRecordsAndDoesNotBlock(t *testing.T) {
	// A change with a standing falsifier: the gate advises not-ready.
	touched := []flg.Obligation{obl("O-fal", nil, "b.go"), obl("O-opn", nil, "c.go")}
	verdicts := map[string]flg.Verdict{
		"O-fal": verdict("O-fal", flg.StateFalsified),
		"O-opn": verdict("O-opn", flg.StateOpen),
	}
	g := Evaluate(touched, verdicts, nil)
	if g.Verdict != VerdictNotReady {
		t.Fatalf("precondition: want not_ready, got %s", g.Verdict)
	}

	// Ship records a named decision and lets the change proceed (no block).
	dec, err := g.Ship("hotfix for outage, exposure accepted", "P-alice")
	if err != nil {
		t.Fatalf("Ship: unexpected error %v", err)
	}
	if dec.Kind != "unproven_change" {
		t.Fatalf("Decision.Kind = %q, want unproven_change", dec.Kind)
	}
	if dec.Reason == "" || dec.PrincipalID != "P-alice" {
		t.Fatalf("Decision did not record the named reason/principal: %+v", dec)
	}
	// It accepts exposure on exactly the readiness-violating obligation (O-fal),
	// not the merely-open one.
	if !reflect.DeepEqual(dec.AcceptedObligations, []string{"O-fal"}) {
		t.Fatalf("AcceptedObligations = %v, want [O-fal]", dec.AcceptedObligations)
	}
	// Real signing is the record package's job — Signature is empty here.
	if dec.Signature != "" {
		t.Fatalf("Signature should be minted by record, not here: %q", dec.Signature)
	}

	// Missing reason / principal are refused — there would be no named decision.
	if _, err := g.Ship("", "P-alice"); !errors.Is(err, ErrEmptyReason) {
		t.Fatalf("empty reason: got %v, want ErrEmptyReason", err)
	}
	if _, err := g.Ship("reason", ""); !errors.Is(err, ErrUnverifiedPrincipal) {
		t.Fatalf("empty principal: got %v, want ErrUnverifiedPrincipal", err)
	}
}

func TestEscapeHatchCannotBypassHardDeny(t *testing.T) {
	// The policy floor (NOT proof state) hard-denies. The escape hatch honors it.
	g := Evaluate(nil, nil, nil)
	g.HardDeny = true
	g.HardDenyReason = "separation-of-duty: author cannot self-approve auth change"

	if _, err := g.Ship("please just merge it", "P-author"); !errors.Is(err, ErrHardDeny) {
		t.Fatalf("hard-deny: got %v, want ErrHardDeny", err)
	}
}

func TestExposureMath(t *testing.T) {
	// A four-obligation risk surface: one discharged (governs), three unproven.
	risky := []flg.Obligation{
		obl("O1", nil, "a.go"),
		obl("O2", nil, "b.go"),
		obl("O3", nil, "c.go"),
		obl("O4", nil, "d.go"),
	}
	verdicts := map[string]flg.Verdict{
		"O1": verdict("O1", flg.StateDischarged),
		"O2": verdict("O2", flg.StateFalsified),
		"O4": verdict("O4", flg.StateUnverifiable),
		// O3 has no verdict → treated as open → exposed.
	}
	rep := Exposure(risky, verdicts)

	if rep.RiskySurface != 4 {
		t.Fatalf("RiskySurface = %d, want 4", rep.RiskySurface)
	}
	if rep.Governed != 1 {
		t.Fatalf("Governed = %d, want 1", rep.Governed)
	}
	if rep.Exposed != 3 {
		t.Fatalf("Exposed = %d, want 3", rep.Exposed)
	}
	if rep.Exposure != 0.75 {
		t.Fatalf("Exposure = %v, want 0.75", rep.Exposure)
	}
	if !reflect.DeepEqual(rep.ExposedObligations, []string{"O2", "O3", "O4"}) {
		t.Fatalf("ExposedObligations = %v, want [O2 O3 O4]", rep.ExposedObligations)
	}
	if rep.ByState[flg.StateFalsified] != 1 || rep.ByState[flg.StateOpen] != 1 || rep.ByState[flg.StateUnverifiable] != 1 {
		t.Fatalf("ByState = %+v, want one each falsified/open/unverifiable", rep.ByState)
	}

	// An empty surface is 0 exposure, not a divide-by-zero.
	if empty := Exposure(nil, nil); empty.Exposure != 0 || empty.Exposed != 0 {
		t.Fatalf("empty surface = %+v, want zero exposure", empty)
	}
}
