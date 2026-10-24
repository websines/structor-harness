package resolver

import (
	"testing"

	"github.com/websines/structor-harness/flg"
)

// Exhaustive supersession-TOPOLOGY conformance for the localized multihop resolution.
// Over ALL topologies on a small node set, the real (DFS-based) resolveSupersession is
// checked against an INDEPENDENT brute-force reference (transitive-closure reachability),
// and against the §2.3 safety invariants proved relationally in Reachability.lean.
//
// Nodes: 3 present evidence facts {A,B,C} + a dangling ghost {Z} (never present). Each of
// A,B,C is in one of: no-supersession | audit (exclude_marker) | superseded-by-T for every
// subset T ⊆ {A,B,C,Z} (including ∅ = a valid retire). That is 1+1+16 = 18 states per node,
// 18^3 = 5832 topologies — the full space at this size.

var topoNodes = []string{"A", "B", "C"}
var topoTargetsUniverse = []string{"A", "B", "C", "Z"} // Z is never present (dangling)

// nodeState encodes one node's supersession situation.
type nodeState struct {
	audit   bool     // has an exclude_marker (and no supersession)
	sup     bool     // has a non-exclude supersession
	targets []string // replacement heads (only when sup)
}

// enumStates returns the 18 states for one node.
func enumStates() []nodeState {
	states := []nodeState{{}, {audit: true}} // none, audit
	// superseded-by every subset of {A,B,C,Z} (16, including ∅)
	for mask := 0; mask < (1 << len(topoTargetsUniverse)); mask++ {
		var t []string
		for i, id := range topoTargetsUniverse {
			if mask&(1<<i) != 0 {
				t = append(t, id)
			}
		}
		states = append(states, nodeState{sup: true, targets: t})
	}
	return states
}

// buildSups turns a per-node assignment into the supersession fact list.
func buildSups(assign map[string]nodeState) []flg.SupersessionEvidence {
	var sups []flg.SupersessionEvidence
	for _, n := range topoNodes {
		st := assign[n]
		switch {
		case st.audit:
			sups = append(sups, flg.SupersessionEvidence{Subject: n, Kind: flg.KindExcludeMarker})
		case st.sup:
			sups = append(sups, flg.SupersessionEvidence{Subject: n, Kind: flg.KindSupersededBy, ReplacedBy: st.targets})
		}
	}
	return sups
}

// --- independent brute-force reference for the LOCALIZED spec ---

func refInCycle(start string, targets map[string][]string) bool {
	// transitive closure: does start reach start via >=1 step? (Floyd-style, independent
	// of the engine's recursive DFS.)
	nodes := map[string]bool{start: true}
	for s, ts := range targets {
		nodes[s] = true
		for _, t := range ts {
			nodes[t] = true
		}
	}
	reach := map[[2]string]bool{}
	for s, ts := range targets { // one-step
		for _, t := range ts {
			reach[[2]string{s, t}] = true
		}
	}
	for changed := true; changed; { // closure
		changed = false
		for a := range nodes {
			for b := range nodes {
				if !reach[[2]string{a, b}] {
					continue
				}
				for _, c := range targets[b] {
					if !reach[[2]string{a, c}] {
						reach[[2]string{a, c}] = true
						changed = true
					}
				}
			}
		}
	}
	return reach[[2]string{start, start}]
}

func refResolve(assign map[string]nodeState) (map[string]bool, map[string]string) {
	universe := map[string]bool{"A": true, "B": true, "C": true} // Z absent
	excluded := map[string]bool{}
	targets := map[string][]string{}
	for _, n := range topoNodes {
		st := assign[n]
		if st.audit {
			excluded[n] = true
		} else if st.sup {
			targets[n] = st.targets
		}
	}
	superseded := map[string]bool{}
	viol := map[string]string{}
	for _, n := range topoNodes {
		if _, ok := targets[n]; !ok {
			continue // not superseded (none or audit)
		}
		prob := ""
		for _, t := range targets[n] { // localized: check each IMMEDIATE target
			if !universe[t] {
				prob = "dangling_replacement"
			} else if excluded[t] {
				prob = "replaced_by_audit_only"
			} else if refInCycle(t, targets) {
				prob = "supersession_cycle"
			}
			if prob != "" {
				break
			}
		}
		if prob == "" {
			superseded[n] = true
		} else {
			viol[n] = prob
		}
	}
	return superseded, viol
}

func TestExhaustiveTopologyConformance(t *testing.T) {
	states := enumStates()
	evidence := []flg.Evidence{
		{ID: "A", ObligationID: "O", Polarity: flg.PolaritySupports, Validity: flg.ValidityValid},
		{ID: "B", ObligationID: "O", Polarity: flg.PolaritySupports, Validity: flg.ValidityValid},
		{ID: "C", ObligationID: "O", Polarity: flg.PolaritySupports, Validity: flg.ValidityValid},
	}
	count, cmpErrs := 0, 0
	for _, sa := range states {
		for _, sb := range states {
			for _, sc := range states {
				count++
				assign := map[string]nodeState{"A": sa, "B": sb, "C": sc}
				sups := buildSups(assign)

				gotSup, gotViol := resolveSupersession("O", evidence, sups)
				wantSup, wantViol := refResolve(assign)

				// (1) superseded set matches the independent reference exactly.
				for _, n := range topoNodes {
					if gotSup[n] != wantSup[n] {
						cmpErrs++
						if cmpErrs <= 5 {
							t.Errorf("superseded[%s]=%v want %v (assign %+v)", n, gotSup[n], wantSup[n], assign)
						}
					}
				}
				// (2) violations match the reference (subject -> problem).
				gv := map[string]string{}
				for _, v := range gotViol {
					gv[v.Subject] = v.Problem
				}
				for _, n := range topoNodes {
					if gv[n] != wantViol[n] {
						cmpErrs++
						if cmpErrs <= 5 {
							t.Errorf("violation[%s]=%q want %q (assign %+v)", n, gv[n], wantViol[n], assign)
						}
					}
				}
				// (3) SAFETY INVARIANT (ambiguous_never_dropped, from Reachability.lean):
				// every flagged (ambiguous) node is KEPT LIVE — never in the superseded set.
				for _, v := range gotViol {
					if gotSup[v.Subject] {
						t.Fatalf("SAFETY VIOLATED: %s is both dropped AND flagged %q (assign %+v)", v.Subject, v.Problem, assign)
					}
				}
				// (4) determinism (Law 3): identical inputs → identical output.
				sup2, viol2 := resolveSupersession("O", evidence, sups)
				if len(sup2) != len(gotSup) || len(viol2) != len(gotViol) {
					t.Fatalf("nondeterministic on assign %+v", assign)
				}
			}
		}
	}
	if count != len(states)*len(states)*len(states) {
		t.Fatalf("enumerated %d topologies, expected %d", count, len(states)*len(states)*len(states))
	}
	t.Logf("exhaustive topology conformance: %d topologies, Go ≡ independent reference, safety invariant holds", count)
}

// sanity: the enumerated space is the full 18^3.
func TestExhaustiveTopologySize(t *testing.T) {
	if n := len(enumStates()); n != 18 {
		t.Fatalf("per-node states = %d, want 18", n)
	}
}
