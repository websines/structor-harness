package learning

import (
	"fmt"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/record"
)

// acceptance.go is the §11.1 acceptance gate: a learned skill is kept only if it
// beats a held-back set (the SkillOpt / skill-up prior art — a skill earns its
// place by outperforming the alternative, not by being newest). The gate is
// DETERMINISTIC: the same candidate, the same held-out cases and the same eval
// always yield the same Decision — no clock, no randomness, so a promotion is
// reproducible from the record (Law 1) and never recency-driven (Law 3).

// AcceptanceMode names which path produced a Decision.
type AcceptanceMode string

const (
	// ModeHeldOut is the §11.1 gate: promote only if the candidate beats the
	// held-out set (passes every held-out case, i.e. never regresses).
	ModeHeldOut AcceptanceMode = "held_out"
	// ModeHumanGated is the current low-volume stance: too few cases to hold a
	// set back, so a human attests the skill and usage is tracked instead.
	ModeHumanGated AcceptanceMode = "human_gated"
)

// Case is one held-back trial the candidate must satisfy. Its fields are opaque
// to the gate — the caller's eval interprets them against the skill — so the
// gate stays domain-neutral and deterministic.
type Case struct {
	Name  string
	Input string
	Want  string
}

// Decision is the gate's verdict on a candidate. It records not just whether to
// promote but the evidence for it (mode, score, principal), which Store.Promote
// writes into the record as the promotion's provenance.
type Decision struct {
	Promote   bool
	Mode      AcceptanceMode
	Passed    int    // held-out cases the candidate satisfied
	Total     int    // held-out cases evaluated
	Reason    string // human-readable justification, recorded on the fact
	Principal string // who/what accepted it — a principal id, never a display string
}

// method maps how a skill was accepted onto the evidence Method recorded for the
// promotion, so the record shows whether a gate or a human promoted it.
func (d Decision) method() flg.Method {
	if d.Mode == ModeHumanGated {
		return flg.MethodHumanAttestation
	}
	return flg.MethodDeterministicCheck
}

// Accept runs the §11.1 gate over a candidate. With a held-out set it promotes
// only if the candidate passes EVERY case (it must beat, not merely tie, the
// held-back baseline — a single regression blocks promotion). With no held-out
// set it abstains: nothing can be beaten, so it defers to the human-gated stance
// (see AcceptHuman) rather than promote by fiat. Deterministic and side-effect
// free — it decides, it does not write; Store.Promote records the outcome.
func Accept(candidate Skill, heldOut []Case, eval func(Skill, Case) bool) Decision {
	if len(heldOut) == 0 {
		return Decision{
			Promote:   false,
			Mode:      ModeHumanGated,
			Reason:    "no held-out set; human-gated promotion required",
			Principal: "principal:human-reviewer",
		}
	}
	passed := 0
	for _, c := range heldOut {
		if eval(candidate, c) {
			passed++
		}
	}
	d := Decision{
		Mode:      ModeHeldOut,
		Passed:    passed,
		Total:     len(heldOut),
		Principal: "principal:acceptance-gate",
	}
	d.Promote = passed == len(heldOut)
	if d.Promote {
		d.Reason = fmt.Sprintf("beat the held-out set: passed all %d cases", d.Total)
	} else {
		d.Reason = fmt.Sprintf("failed the held-out set: %d/%d passed", passed, d.Total)
	}
	return d
}

// AcceptHuman constructs a human-gated Decision — the degraded low-volume stance
// where a reviewer attests a skill directly (§11.1, current stance: human-gated
// promotion with usage tracking). reason should cite what justified it, e.g.
// accumulated usage from Store.Uses.
func AcceptHuman(principal, reason string, approve bool) Decision {
	return Decision{
		Promote:   approve,
		Mode:      ModeHumanGated,
		Reason:    reason,
		Principal: principal,
	}
}

// RecordUse tracks that a skill was recalled into a run, as a FACT keyed by the
// run so each use is distinct in the record (Law 1: usage is reconstructable,
// not a hidden counter). This is the signal the human-gated stance weighs when
// deciding whether a low-volume skill has earned promotion.
func (s *Store) RecordUse(id, runID string) (record.Digest, error) {
	ev := flg.Evidence{
		ID:              "use:" + id + ":" + runID,
		ObligationID:    id,
		Polarity:        flg.PolaritySupports,
		Method:          flg.MethodHumanAttestation,
		Reproducibility: flg.ReproTestimonial,
		Validity:        flg.ValidityValid,
		Scope:           flg.Scope{Domain: domainUse, Tags: []string{domainUse}},
		PrincipalID:     "principal:" + runID,
		Summary:         "skill recalled into run " + runID,
	}
	fv, err := s.facts.AppendEvidence(ev)
	if err != nil {
		return "", err
	}
	return fv.FactID, nil
}

// Uses counts how many distinct runs recalled a skill, projected from the record
// (Law 1) — never from a mutable counter. Each use-fact is identified by its
// (skill, run) id, so the same run recorded twice is one distinct use (content
// identity, not a tally). Usage evidence is scoped apart from promotion
// evidence, so counting uses never mistakes one for the other.
func (s *Store) Uses(id string) int {
	_, evs, _, err := s.facts.Project()
	if err != nil {
		return 0
	}
	seen := make(map[string]bool)
	for _, e := range evs {
		if e.ObligationID == id && e.Scope.Domain == domainUse {
			seen[e.ID] = true
		}
	}
	return len(seen)
}
