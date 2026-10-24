// Command exhaustivegen generates the EXHAUSTIVE resolver-level conformance corpora:
// every semantically-distinct input class of the resolver's per-obligation decision,
// enumerated in full, each with the state the Go resolver derives. It emits FOUR
// artifacts in two lock-step pairs (a JSON oracle for Go + a Lean twin for the kernel):
//
//   Phase 1 — evidence→state (crossTypeInvalidated = false):
//     conformance/exhaustive/evidence_state.json + proofs/ForgeProofs/Exhaustive.lean
//     Domain: retired ∈ {false,true} × subset of the 5 evidence-effect classes = 2×2^5 = 64.
//
//   Phase 3 — the reopened region (crossTypeInvalidated = true):
//     conformance/exhaustive/reopened_state.json + proofs/ForgeProofs/Reopened.lean
//     Same 64 shapes, but with a cross_type_invalidation supersession naming the obligation.
//     This settles the resolver-level precedence falsified > reopened (a live reproducible
//     witness dominates a reopen — Resolver.witness_dominates_reopen).
//
// Green Go tests (Go ≡ each corpus) ∧ green Lean builds (each corpus ≡ the Lean model, by
// `decide`) ⟹ the RUNNING Go resolver ≡ the PROVED Lean spec over the WHOLE finite
// resolver-level domain (128 classes), reopened region included, not 9 samples.
//
// Why 0-vs-1+ saturates: ResolveObligation branches ONLY on existential predicates
// (`len(...) > 0`, `∃ testimonial`) — never on a count or an order — so its verdict depends
// only on WHICH effect-classes are present. The GENUINELY unbounded part — the transitive
// supersession graph (flg.supersession, SCC/cycles) that computes which evidence is live and
// which obligation is reopened — is upstream of the resolver and remains the Phase-3 frontier.
//
// Regenerate:  go run ./internal/resolver/exhaustivegen   (from engine/)
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/resolver"
)

// effectClass is one distinct way an evidence item can affect the verdict.
type effectClass struct {
	bit    int
	name   string // short label used in the vector id
	goEv   flg.Evidence
	leanEv string // the matching Lean `Ev` literal
}

// The obligation domain is ["x"]; scope ["x"] is in-scope, so falsifier classes below are
// in-scope witnesses. Support classes are scope-independent (the resolver never scopes
// support); we still tag them ["x"] for a concrete, self-consistent instance.
func classes() []effectClass {
	ev := func(pol flg.Polarity, repro flg.Reproducibility, val flg.Validity) flg.Evidence {
		return flg.Evidence{
			Polarity: pol, Method: flg.Method("executed_test"),
			Reproducibility: repro, Validity: val, Scope: flg.Scope{Tags: []string{"x"}},
		}
	}
	return []effectClass{
		{0, "broken", ev(flg.PolaritySupports, flg.ReproDeterministic, flg.ValidityInvalid), "⟨.supports, .deterministic, .invalid, true⟩"},
		{1, "sup_repro", ev(flg.PolaritySupports, flg.ReproDeterministic, flg.ValidityValid), "⟨.supports, .deterministic, .valid, true⟩"},
		{2, "sup_test", ev(flg.PolaritySupports, flg.ReproTestimonial, flg.ValidityValid), "⟨.supports, .testimonial, .valid, true⟩"},
		{3, "fals_repro", ev(flg.PolarityFalsifies, flg.ReproDeterministic, flg.ValidityValid), "⟨.falsifies, .deterministic, .valid, true⟩"},
		{4, "fals_test", ev(flg.PolarityFalsifies, flg.ReproTestimonial, flg.ValidityValid), "⟨.falsifies, .testimonial, .valid, true⟩"},
	}
}

// leanObState maps a Go OblState string to its Lean ObState constructor.
var leanObState = map[string]string{
	"open": ".opn", "discharged": ".discharged", "falsified": ".falsified",
	"reopened": ".reopened", "contested": ".contested",
	"unverifiable": ".unverifiable", "audit_only": ".auditOnly",
}

// vector mirrors conformance/vectors/*.json exactly.
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
	Supersessions []any  `json:"supersessions"`
	Expect        string `json:"expect"`
}

func main() {
	cls := classes()
	var evVectors, reVectors []vector
	var evLean, reLean []string

	for retiredBit := 0; retiredBit < 2; retiredBit++ {
		retired := retiredBit == 1
		for mask := 0; mask < 32; mask++ {
			obl := flg.Obligation{
				ID: "O-TEST", AcceptancePolicy: "p", DomainTags: []string{"x"}, Retired: retired,
			}
			var goEv []flg.Evidence
			var present, leanEv []string
			for _, c := range cls {
				if mask&(1<<c.bit) != 0 {
					e := c.goEv
					e.ID = fmt.Sprintf("E%d", c.bit)
					e.ObligationID = "O-TEST"
					e.PrincipalID, e.Commit, e.Summary = "P-TEST", "0000000", "exhaustive"
					goEv = append(goEv, e)
					present = append(present, c.name)
					leanEv = append(leanEv, c.leanEv)
				}
			}
			evList := "[]"
			if len(leanEv) > 0 {
				evList = "[" + strings.Join(leanEv, ", ") + "]"
			}

			// Phase 1 — evidence→state (no supersession).
			evState := string(resolver.ResolveObligation(obl, goEv, nil).State)
			evVectors = append(evVectors, buildVector(
				fmt.Sprintf("EX-r%d-m%02d", retiredBit, mask), label(retired, false, present),
				retired, cls, mask, evState, nil))
			evLean = append(evLean, fmt.Sprintf("  (⟨%t, false, %s⟩, %s)", retired, evList, leanObState[evState]))

			// Phase 3 — the reopened region (a cross_type_invalidation names the obligation).
			cti := []flg.SupersessionEvidence{{Subject: "O-TEST", Kind: flg.KindCrossTypeInvalidation}}
			reState := string(resolver.ResolveObligation(obl, goEv, cti).State)
			reVectors = append(reVectors, buildVector(
				fmt.Sprintf("RE-r%d-m%02d", retiredBit, mask), label(retired, true, present),
				retired, cls, mask, reState, []any{map[string]string{
					"subject": "O-TEST", "kind": string(flg.KindCrossTypeInvalidation),
				}}))
			reLean = append(reLean, fmt.Sprintf("  (⟨%t, true, %s⟩, %s)", retired, evList, leanObState[reState]))
		}
	}

	writeJSON("evidence_state.json", evVectors)
	writeJSON("reopened_state.json", reVectors)
	writeLean("Exhaustive.lean", "ForgeProofs.Exhaustive", "exhaustive_go_lean_agree", evExhaustiveHeader, evLean)
	writeLean("Reopened.lean", "ForgeProofs.Reopened", "reopened_go_lean_agree", reReopenedHeader, reLean)
	fmt.Printf("generated %d evidence→state + %d reopened classes\n", len(evVectors), len(reVectors))
}

// buildVector assembles one JSON vector (shared oracle schema).
func buildVector(id, name string, retired bool, cls []effectClass, mask int, expect string, sups []any) vector {
	var v vector
	v.ID = id
	v.Name = name
	v.Obligation.DomainTags = []string{"x"}
	v.Obligation.AcceptancePolicy = "p"
	v.Obligation.Retired = retired
	if sups == nil {
		sups = []any{}
	}
	v.Supersessions = sups
	for _, c := range cls {
		if mask&(1<<c.bit) != 0 {
			v.Evidence = append(v.Evidence, struct {
				Polarity        string   `json:"polarity"`
				Method          string   `json:"method"`
				Reproducibility string   `json:"reproducibility"`
				Validity        string   `json:"validity"`
				ScopeTags       []string `json:"scopeTags"`
			}{
				Polarity: string(c.goEv.Polarity), Method: "executed_test",
				Reproducibility: string(c.goEv.Reproducibility), Validity: string(c.goEv.Validity),
				ScopeTags: []string{"x"},
			})
		}
	}
	v.Expect = expect
	return v
}

func label(retired, crossType bool, present []string) string {
	base := strings.Join(present, "+")
	if base == "" {
		base = "no-evidence"
	}
	var pre []string
	if retired {
		pre = append(pre, "retired")
	}
	if crossType {
		pre = append(pre, "reopen")
	}
	if len(pre) > 0 {
		return strings.Join(pre, "·") + " · " + base
	}
	return base
}

const evExhaustiveHeader = `/-
  Exhaustive.lean — GENERATED by engine/internal/resolver/exhaustivegen. DO NOT EDIT.

  The FULL evidence→state domain of the resolver, enumerated: retired ∈ {false,true} ×
  every subset of the 5 evidence-effect classes = 64 classes (crossTypeInvalidated = false —
  the reopened region is Reopened.lean). Each is paired with the ObState the Go resolver
  derived. The kernel-checked decide proves the Lean model (deriveObligationState) derives
  the IDENTICAL ObState for every class — Go ≡ Lean over the whole evidence→state domain.

  Regenerate with:  go run ./internal/resolver/exhaustivegen   (from engine/)
-/`

const reReopenedHeader = `/-
  Reopened.lean — GENERATED by engine/internal/resolver/exhaustivegen. DO NOT EDIT.

  The REOPENED region of the resolver domain (Phase 3): the SAME 64 evidence shapes, each
  with crossTypeInvalidated = true (a cross_type_invalidation supersession names the
  obligation), paired with the ObState the Go resolver derived.

  This settles the resolver-level precedence: a live reproducible WITNESS dominates a reopen
  (falsified > reopened) — see Resolver.witness_dominates_reopen. Go and reasongate already
  derived this; the Lean model now agrees, and the decide below proves Go ≡ Lean over the
  entire reopened region. With Exhaustive.lean this closes the FULL 128-class resolver-level
  domain. The remaining UNBOUNDED frontier is the transitive supersession GRAPH upstream
  (flg.supersession, SCC/cycles) that produces the live-evidence/reopen inputs consumed here.

  Regenerate with:  go run ./internal/resolver/exhaustivegen   (from engine/)
-/`

func writeJSON(name string, vectors []vector) {
	dir := filepath.Join(repoRoot(), "conformance", "exhaustive")
	must(os.MkdirAll(dir, 0o755))
	b, err := json.MarshalIndent(vectors, "", "  ")
	must(err)
	must(os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o644))
}

func writeLean(file, ns, thm, header string, cases []string) {
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\nimport ForgeProofs.Resolver\n\nnamespace " + ns + "\n\n")
	b.WriteString("/-- The enumerated classes, each paired with the Go-derived ObState.\n")
	b.WriteString("    Fields: ⟨retired, crossTypeInvalidated, evidence⟩. -/\n")
	b.WriteString("def cases : List (Obligation × ObState) := [\n")
	b.WriteString(strings.Join(cases, ",\n"))
	b.WriteString("\n]\n\n")
	b.WriteString("/-- EXHAUSTIVE CONFORMANCE (Go ≡ Lean): the Lean model derives exactly the\n")
	b.WriteString("    Go-derived ObState for every enumerated class. Kernel-checked by decide (not\n")
	b.WriteString("    native_decide), so the axiom audit stays clean. -/\n")
	b.WriteString("theorem " + thm + " :\n")
	b.WriteString("    cases.all (fun c => decide (deriveObligationState c.1 = c.2)) = true := by decide\n\n")
	b.WriteString("#print axioms " + thm + "\n\n")
	b.WriteString("end " + ns + "\n")
	dir := filepath.Join(repoRoot(), "proofs", "ForgeProofs")
	must(os.WriteFile(filepath.Join(dir, file), []byte(b.String()), 0o644))
}

func repoRoot() string {
	// engine/ is the CWD when run via `go run ./internal/resolver/exhaustivegen`; the
	// workspace root (holding conformance/ and proofs/) is its parent.
	wd, _ := os.Getwd()
	return filepath.Dir(wd)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
