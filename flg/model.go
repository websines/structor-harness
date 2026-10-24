// Package flg is the Factory Lineage Graph: fact versions, edges, evidence and
// supersession — the domain substrate the resolver reads (system design §5, §4.8).
//
// Ported faithfully from forge-showcase/src/lib/model.ts and flg-core's model.py.
//
// LAW 3 (no recency, no fiat): there is NO timestamp anywhere in this package.
// The type set is chosen so that "recency is authority" is structurally
// unrepresentable — a resolver cannot key on a clock it was never given.
package flg

// AllOblStates is every state the resolver can derive, in the resolver's own
// precedence order (highest first). It exists so that nothing downstream has to
// keep a hand copy: uiview's display table and the test that guards it both
// derive from this, which means adding an eighth state breaks the build and the
// test rather than silently vanishing from the UI.
func AllOblStates() []OblState {
	return []OblState{
		StateAuditOnly, StateFalsified, StateReopened,
		StateUnverifiable, StateContested, StateDischarged, StateOpen,
	}
}

// Polarity — whether a witness argues for or against an obligation.
type Polarity string

const (
	PolaritySupports  Polarity = "supports"
	PolarityFalsifies Polarity = "falsifies"
)

// Method — how a piece of evidence was produced (model.ts Method).
type Method string

const (
	MethodFormalProof         Method = "formal_proof"
	MethodDeterministicCheck  Method = "deterministic_check"
	MethodExecutedTest        Method = "executed_test"
	MethodCounterexample      Method = "counterexample"
	MethodAdversarialCampaign Method = "adversarial_campaign"
	MethodHumanAttestation    Method = "human_attestation"
)

// Reproducibility — how re-checkable the evidence is. `testimonial` is the only
// non-reproducible grade and, by Law 3, can never auto-falsify.
type Reproducibility string

const (
	ReproKernelChecked Reproducibility = "kernel_checked"
	ReproDeterministic Reproducibility = "deterministic"
	ReproReplayable    Reproducibility = "replayable"
	ReproTestimonial   Reproducibility = "testimonial"
)

// Validity — whether the evidence can currently be trusted at all. Invalid
// evidence never governs; it derives `unverifiable`, not a contradiction.
type Validity string

const (
	ValidityValid                   Validity = "valid"
	ValidityTemporarilyUnverifiable Validity = "temporarily_unverifiable"
	ValidityInvalid                 Validity = "invalid"
)

// OblState — the seven derived obligation states (system design §6.1).
// `contested`/`falsified`/`unverifiable`/`reopened`/`audit_only`/`open` never govern.
type OblState string

const (
	StateOpen         OblState = "open"
	StateDischarged   OblState = "discharged"
	StateFalsified    OblState = "falsified"
	StateReopened     OblState = "reopened"
	StateContested    OblState = "contested"
	StateUnverifiable OblState = "unverifiable"
	StateAuditOnly    OblState = "audit_only"
)

// Scope — where a piece of evidence actually lands.
type Scope struct {
	// Domain is a human-readable predicate domain this evidence covered.
	Domain string
	// Tags are machine-ish tags used to decide whether a witness lands INSIDE a
	// claim's domain. The wildcard "*" matches every domain.
	Tags []string
}

// Evidence — a single witness for or against an obligation. No timestamp: a
// witness dominates on validity/polarity/scope/reproducibility, never on recency.
type Evidence struct {
	ID              string
	ObligationID    string
	Polarity        Polarity
	Method          Method
	Reproducibility Reproducibility
	Validity        Validity
	Scope           Scope
	// PrincipalID is who produced it — a verified principal id, never a display string.
	PrincipalID string
	Commit      string
	Summary     string
}

// Anchor — a pin to a concrete code location (kept true as code moves; §4.8 M4).
//
// Repo names WHICH source root within the project the anchor lands in. A project
// is one product spanning N repos (api, web, infra), so `internal/pay/charge.go`
// is ambiguous without it. Repo is a stable LOGICAL id assigned when the repo is
// added to the project — never a remote URL or a checkout path, both of which
// move without the claim changing.
//
// Repo is deliberately part of the fact CONTENT, unlike workspace/project which
// are deliberately NOT. The line between them is whether the value changes what
// the claim SAYS or only who may SEE it: "this promise is about api/pay/charge.go"
// is part of the promise, so it is hashed into the fact address; "this promise
// belongs to workspace acme" is membership, so it lives in which store holds the
// fact (see internal/scope) and never touches the address. Putting membership in
// the content would re-address a fact on a move and break the ref-independence
// store.go depends on for overlays and P2P convergence.
//
// An empty Repo means the single-root project — the pre-multi-repo shape, and
// still the common case for a one-repo product.
type Anchor struct {
	Repo   string
	File   string
	Symbol string
}

// Obligation — a single leaf claim that must be discharged.
type Obligation struct {
	ID      string
	LemmaID string
	Title   string
	Anchor  Anchor
	// AcceptancePolicy is what it takes to discharge this leaf — cited in the verdict.
	AcceptancePolicy string
	// DomainTags is the declared domain of the claim. A witness inside it dominates.
	DomainTags []string
	// Retired marks a scope kept for history that must never govern.
	Retired bool
}

// SupersessionKind — the explicit-evidence forms that mark a fact inactive
// (never recency; system design §5.4, resolved by the supersession subresolver).
type SupersessionKind string

const (
	KindStatusSuperseded      SupersessionKind = "status_superseded"
	KindSupersededBy          SupersessionKind = "superseded_by"
	KindCrossTypeInvalidation SupersessionKind = "cross_type_invalidation"
	KindDecision              SupersessionKind = "decision"
	KindExcludeMarker         SupersessionKind = "exclude_marker"
)

// SupersessionEvidence — explicit evidence that `Subject` no longer governs.
// Subject may be an evidence id or an obligation id.
type SupersessionEvidence struct {
	// ID is this supersession fact's own fact_id (§5.5). Empty for anonymous
	// conformance vectors; populated when the fact lives in the append-only store.
	ID string
	// Subject is the fact being superseded/invalidated.
	Subject    string
	Kind       SupersessionKind
	ReplacedBy []string
	// Evidence holds citations (artifact ids / paths).
	Evidence []string
}

// Violation is a repair signal surfaced by the resolver: a supersession whose
// replacement chain does not VALIDLY resolve to an active head (a dangling
// replacement, a supersession cycle, or a chain dead-ending in an audit-only fact).
// Per §2.3 such a supersession never drops a fact by fiat — the fact stays live and
// the obligation carries this row demanding repair. Deterministic (Law 3): no clock.
type Violation struct {
	Subject string // the superseded fact whose chain is ambiguous
	Problem string // dangling_replacement | supersession_cycle | replaced_by_audit_only
	Detail  string
}

// Verdict — a derived state for one obligation. A verdict always cites the
// evidence that produced it.
type Verdict struct {
	ObligationID string
	State        OblState
	// Citations are the evidence ids that justify the verdict.
	Citations []string
	Reason    string
	// Policy is the acceptance policy satisfied, when discharged.
	Policy string
	// Violations are repair signals from ambiguous supersession chains (§2.3). Empty
	// in the common case; an ambiguous chain keeps its fact live AND records a row here.
	Violations []Violation
}
