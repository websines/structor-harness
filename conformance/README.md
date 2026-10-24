# Conformance — the resolver oracle

This directory is the **referee** for Forge's obligation resolver. The resolver's rules
are written once as math (Lean) and proved to have the properties we want; the vectors
here are worked examples with their one correct answer. Four independent implementations
of the resolver — **Lean, Go, TypeScript, Python** — must derive the same verdict on every
vector. If an implementation disagrees with a vector, the implementation is wrong. The
list is the referee, never the other way around.

The resolver reads **no timestamps** (system-design law 3, "no recency, no fiat"): a
verdict is a pure derivation over explicit evidence and supersession edges, and every
verdict cites the evidence that produced it.

## What's here

| File | Purpose |
|---|---|
| `vectors/V*.json` | One vector per file. Each is a `(obligation, evidence, supersessions) → expect` case. |
| `schema.json` | JSON Schema (draft 2020-12) for the vector format. Every implementation validates its reader against this. |
| `README.md` | This file. |

## Vector format

Each vector is a single object (see `schema.json` for the authoritative shape and every
allowed enum value):

```json
{
  "id": "V3",
  "name": "support + reproducible in-scope witness",
  "obligation": { "domainTags": ["x"], "acceptancePolicy": "p", "retired": false },
  "evidence": [
    { "polarity": "supports",  "method": "executed_test", "reproducibility": "deterministic", "validity": "valid", "scopeTags": ["x"] },
    { "polarity": "falsifies", "method": "executed_test", "reproducibility": "deterministic", "validity": "valid", "scopeTags": ["x"] }
  ],
  "supersessions": [],
  "expect": "falsified"
}
```

Enumerations (all fixed in `schema.json`):

- **polarity** — `supports`, `falsifies`
- **method** — `formal_proof`, `deterministic_check`, `executed_test`, `counterexample`, `adversarial_campaign`, `human_attestation`
- **reproducibility** — `kernel_checked`, `deterministic`, `replayable`, `testimonial`
- **validity** — `valid`, `temporarily_unverifiable`, `invalid`
- **supersession kind** — `status_superseded`, `superseded_by`, `cross_type_invalidation`, `decision`, `exclude_marker`
- **expect** (the seven obligation states) — `open`, `discharged`, `falsified`, `reopened`, `contested`, `unverifiable`, `audit_only`

Conventions used across the vectors: in-scope = scopeTags `["x"]`, out-of-scope = `["z"]`;
`method` is `executed_test` normally but `human_attestation` whenever `reproducibility` is
`testimonial`; the obligation is `domainTags ["x"]`, `acceptancePolicy "p"`, `retired false`
unless the case says otherwise. Of the seven states only `open` and `discharged` govern;
`falsified`, `reopened`, `contested`, `unverifiable`, and `audit_only` are non-governing.

## The rule for the loop (build guide §4)

**Add a rule → add a vector → prove it in Lean → make every implementation pass. Never the
reverse.** A vector without a Lean property behind it is a test, not a spec. Concretely:

1. Change the resolver's rules.
2. Add or change the vectors here that the rule affects.
3. Prove the property in Lean; the axiom audit (`#print axioms`) must show no `sorryAx`,
   only the allowed `propext`, `Classical.choice`, `Quot.sound`.
4. Make Go, TypeScript, and Python pass the vectors.

## Who consumes these

- **Engine (Go):** `engine/internal/resolver/conformance_test.go` reads every file in
  `vectors/` and fails the build on any disagreement. This is the gate on the core; it runs
  in CI on every PR.
- **Proofs (Lean):** `proofs/ForgeProofs/Resolver.lean` `#eval`s the same cases against the
  Lean model.
- **Surfaces (TypeScript):** a TS check runs the vectors through
  `surfaces/web/src/lib/resolver.ts` (grown from `forge-showcase`).
- **Prep sidecar (Python):** the flg-core reference resolver is checked against the same set.

## The nine vectors

| Vector | Input (evidence on one obligation) | Expected verdict | Law it pins |
|---|---|---|---|
| V1 | no evidence | `open` | baseline — no evidence, no claim |
| V2 | one deterministic in-scope support | `discharged` | acceptance policy satisfied |
| V3 | support + reproducible in-scope falsifier | `falsified` | Rule 3 — reproducible in-scope witness dominates |
| V4 | deterministic support + **testimonial** falsifier only | `discharged` | Rule 3 — testimony cannot auto-falsify |
| V5 | testimonial support **vs** testimonial falsifier | `contested` | Rule 2 — non-dominating judgments contest |
| V6 | only invalid evidence | `unverifiable` | infra failure ≠ contradiction |
| V7 | support + **out-of-scope** reproducible falsifier | `discharged` | scope gates falsification |
| V8 | retired obligation + valid support | `audit_only` | law 2 — retired scope kept for history, never governs |
| V9 | valid support + `cross_type_invalidation` on the obligation | `reopened` | law 2/3 — a superseded input auto-reopens, never auto-closes |

V1–V7 are lifted verbatim from `forge-showcase/conformance.ts`. V8 and V9 are added here to
cover the two obligation states the original seven miss — `audit_only` and `reopened` — so
the set exercises all seven derived states, not just seven cases.
