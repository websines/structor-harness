// This file implements the PR gate (system design §7.5) under Law 5: the gate is
// INFORMATIONAL BY DEFAULT and never an unconditional block. It reads the touched
// obligations and their resolver verdicts and REPORTS — counts by state, a
// readiness list, and an advisory verdict. Proof state ALONE never denies a
// merge: the escape hatch always exists and records a named decision (§4.3).
// Only a separate policy floor (separation of duties, org security floor) may
// hard-deny, and even the escape hatch cannot bypass that floor (Rule 2).
package obligation

import (
	"errors"
	"fmt"
	"sort"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/resolver"
)

// GateVerdict is the gate's ADVISORY reading of readiness. It is a report, not a
// merge decision — see GateResult.Blocking, which is always false from proof
// state alone.
type GateVerdict string

const (
	// VerdictReady — no readiness violation among the touched obligations.
	VerdictReady GateVerdict = "ready"
	// VerdictNotReady — at least one readiness violation to surface as an
	// actionable row. Still informational: it advises review, it does not block.
	VerdictNotReady GateVerdict = "not_ready"
)

// ReadinessViolation is one actionable row (§7.5 step 3): an obligation whose
// state or anchor violates readiness. Each names the obligation and why.
type ReadinessViolation struct {
	ObligationID string
	State        flg.OblState
	Kind         string // "non_governing_state" | "stale_anchor"
	Reason       string
}

// GateResult is the full, honest gate report for a change.
type GateResult struct {
	// Counts holds every OblState (all seven keys present, zero where none) over
	// the touched obligations — deliberately not a single headline number (§16).
	Counts map[flg.OblState]int
	// Readiness lists the violating rows; empty means nothing blocks review.
	Readiness []ReadinessViolation
	// Verdict is the ADVISORY reading. Informational by default (Law 5).
	Verdict GateVerdict
	// Informational is always true here: the gate reports, it does not gate.
	Informational bool
	// Blocking is the merge-blocking decision. Proof state ALONE never sets it —
	// it stays false (Law 5). It exists so a surface can render "advisory only".
	Blocking bool

	// HardDeny is the policy floor's veto and is the ONLY thing the escape hatch
	// cannot bypass. Proof state NEVER sets it (Law 5); it stays false here. A
	// separate authorization / security-floor policy (separation of duties, org
	// security floor; §4.3, Rule 2) may set it before Ship is called.
	//
	// THIS IS THE HARD-DENY FLOOR HOOK: wire the policy floor in here, never into
	// the resolver verdict path — a falsifier or conflict must never become a
	// hard block on its own.
	HardDeny       bool
	HardDenyReason string
}

// readinessBlockingStates maps the derived states that violate readiness (§7.5
// step 3) to their actionable row reason. `open` is NOT here — an open leaf is
// the ratchet working, not a violation. `audit_only` is not here either — a
// retired scope never governs but is not a blocker. Exactly the four
// non-governing states a PR is expected to resolve: falsified, contested,
// reopened, unverifiable.
var readinessBlockingStates = map[flg.OblState]string{
	flg.StateFalsified:    "valid in-scope counterexample stands — not ready",
	flg.StateContested:    "incompatible judgments, neither dominates — needs a signed Resolution",
	flg.StateReopened:     "an input was superseded — needs a fresh run, cannot auto-close",
	flg.StateUnverifiable: "required evidence cannot currently be checked — replay envelope unavailable",
}

// Evaluate produces the gate report from the touched obligations, their resolver
// verdicts (keyed by obligation id, exactly as resolver.ResolveAll returns them),
// and the set of obligation ids whose anchor is stale (from the re-anchoring
// sweep, §4.8; nil when none are stale). It is pure — no clock, no I/O — and it
// NEVER returns a hard block from proof state: Blocking and HardDeny are left
// false for the policy floor to set.
func Evaluate(touched []flg.Obligation, verdicts map[string]flg.Verdict, staleAnchors map[string]bool) GateResult {
	res := GateResult{
		Counts:        newCounts(),
		Informational: true,
		Blocking:      false, // Law 5: proof state alone never blocks a merge.
	}
	for _, o := range touched {
		state := flg.StateOpen
		if v, ok := verdicts[o.ID]; ok {
			state = v.State
		}
		res.Counts[state]++

		if reason, blocks := readinessBlockingStates[state]; blocks {
			res.Readiness = append(res.Readiness, ReadinessViolation{
				ObligationID: o.ID,
				State:        state,
				Kind:         "non_governing_state",
				Reason:       reason,
			})
		}
		if staleAnchors[o.ID] {
			res.Readiness = append(res.Readiness, ReadinessViolation{
				ObligationID: o.ID,
				State:        state,
				Kind:         "stale_anchor",
				Reason:       "anchor target moved or was deleted — stale, kept, surfaced (§4.8)",
			})
		}
	}
	sort.Slice(res.Readiness, func(i, j int) bool {
		if res.Readiness[i].ObligationID != res.Readiness[j].ObligationID {
			return res.Readiness[i].ObligationID < res.Readiness[j].ObligationID
		}
		return res.Readiness[i].Kind < res.Readiness[j].Kind
	})
	if len(res.Readiness) > 0 {
		res.Verdict = VerdictNotReady
	} else {
		res.Verdict = VerdictReady
	}
	return res
}

// EvaluateWithResolver is the end-to-end convenience: it CALLS the resolver (§8)
// to derive verdicts for the touched obligations from evidence and supersessions,
// then evaluates the gate. The resolver is the single source of derived state —
// the gate never re-derives it.
func EvaluateWithResolver(touched []flg.Obligation, evidence []flg.Evidence, supersessions []flg.SupersessionEvidence, staleAnchors map[string]bool) GateResult {
	verdicts := resolver.ResolveAll(touched, evidence, supersessions)
	return Evaluate(touched, verdicts, staleAnchors)
}

// Decision is the escape hatch's output: a named "unproven change" fact (§4.3).
// It is a plain struct here — real signing and any external anchoring are the
// record package's job (Rule 4: an actor label without an authenticated binding
// is invalid evidence, so Signature stays empty until record signs it). It
// carries NO timestamp (Law 3).
type Decision struct {
	// Kind is always "unproven_change" — the ship-without-proof decision.
	Kind string
	// Reason is the required human justification. An empty reason is refused.
	Reason string
	// PrincipalID is the verified principal accepting the exposure.
	PrincipalID string
	// AcceptedObligations are the readiness-violating obligation ids this decision
	// explicitly accepts exposure on — visible to exposure reporting (§16).
	AcceptedObligations []string
	// Signature is minted by the record package; empty here by construction.
	Signature string
}

// ErrEmptyReason and ErrUnverifiedPrincipal refuse an escape hatch that would not
// record a real named decision. ErrHardDeny is the policy floor's veto — the one
// thing the escape hatch cannot bypass (§4.3, Rule 2).
var (
	ErrEmptyReason         = errors.New("obligation: escape hatch requires a named reason")
	ErrUnverifiedPrincipal = errors.New("obligation: escape hatch requires a verified principal id")
	ErrHardDeny            = errors.New("obligation: policy floor hard-deny — escape hatch cannot bypass")
)

// Ship is the escape hatch: an authorized principal ships the change despite the
// gate's advisory reading, recording a named Decision (§4.3). It RECORDS, it does
// not block — the returned Decision is what lets the change proceed. There are
// exactly two refusals:
//
//   - a missing reason or principal — there would be no named decision to record; and
//   - a policy-floor HardDeny — which the escape hatch is defined to be unable to
//     bypass. Proof state alone never sets HardDeny (Law 5), so proof state alone
//     never stops Ship. A falsified or contested obligation makes the gate advise
//     "not ready", never denies the ship.
func (g GateResult) Ship(reason, principalID string) (Decision, error) {
	if reason == "" {
		return Decision{}, ErrEmptyReason
	}
	if principalID == "" {
		return Decision{}, ErrUnverifiedPrincipal
	}
	if g.HardDeny {
		// Hard-deny floor hook: the ONLY veto the escape hatch honors. This is
		// never reached from proof state — only a separately-authorized policy
		// floor sets g.HardDeny.
		return Decision{}, fmt.Errorf("%w: %s", ErrHardDeny, g.HardDenyReason)
	}
	accepted := make([]string, 0, len(g.Readiness))
	seen := map[string]bool{}
	for _, v := range g.Readiness {
		if !seen[v.ObligationID] {
			seen[v.ObligationID] = true
			accepted = append(accepted, v.ObligationID)
		}
	}
	return Decision{
		Kind:                "unproven_change",
		Reason:              reason,
		PrincipalID:         principalID,
		AcceptedObligations: accepted,
	}, nil
}

// newCounts returns a counts map with every OblState present at zero, so the gate
// report is honest about states that happen to have no obligations this change.
func newCounts() map[flg.OblState]int {
	return map[flg.OblState]int{
		flg.StateOpen:         0,
		flg.StateDischarged:   0,
		flg.StateFalsified:    0,
		flg.StateReopened:     0,
		flg.StateContested:    0,
		flg.StateUnverifiable: 0,
		flg.StateAuditOnly:    0,
	}
}
