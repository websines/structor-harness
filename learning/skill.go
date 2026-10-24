// Package learning is the governed team memory (system design §11): skills
// learned during runs and failures remembered from incidents, kept as SHARED,
// COMPOUNDING assets with provenance, acceptance-gated promotion and layered
// recall (§11 L0→L3, summary→raw).
//
// LAW 1 (no memory asset exists that is not reconstructable from the canonical
// record — Appendix A): this package does NOT own a store of its own. Every
// skill and failure is written as a FACT into the append-only flg record, and
// every derived view (state, recall order, usage) is projected back OUT of that
// record. Drop the projection, replay the facts, and the same memory reappears —
// exactly the guarantee flg/store.go makes for obligations.
//
// The design reuses the FLG fact ALGEBRA rather than inventing a parallel one,
// which is what "never-stale, always-increasing" memory means here:
//
//   - a skill / failure is an Obligation fact — a claim/asset the team holds;
//   - accepting a skill is SUPPORTING Evidence on that claim (it beat the
//     held-out set, or a human attested it — §11.1);
//   - retiring a skill is a SUPERSESSION fact (Kind=decision) — superseded, kept
//     for history, never recalled; append-only growth, never deletion.
//
// LAW 3 (no recency, no fiat): a skill's state is DERIVED from the presence of
// these explicit facts, never from a clock or append order. There is no
// timestamp anywhere in this package.
package learning

import (
	"fmt"
	"strings"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/record"
)

// Discriminators written into a fact so that skills, failures and their markers
// can be told apart when the record is projected back. They live in fields the
// obligation resolver would otherwise carry: LemmaID tags what kind of learning
// asset an obligation IS, and a supporting evidence's Scope.Domain tags it as a
// promotion (as opposed to a usage record, see acceptance.go).
const (
	lemmaSkill   = "learning.skill"   // an Obligation that carries a Skill
	lemmaFailure = "learning.failure" // an Obligation that carries a Failure
	domainUse    = "learning.use"     // a usage-tracking evidence, not a promotion
)

// SkillState is the derived lifecycle of a skill. It is a lattice, not a clock:
// retired dominates promoted dominates candidate, and the state is read off the
// facts that exist about the skill (§11.1), never off recency.
type SkillState string

const (
	SkillCandidate SkillState = "candidate" // recorded, not yet accepted
	SkillPromoted  SkillState = "promoted"  // passed the acceptance gate (§11.1)
	SkillRetired   SkillState = "retired"   // superseded; kept for history, not recalled
)

// Skill is a governed team asset learned during runs. Provenance are the run /
// fact ids that produced it — the lineage that makes it reconstructable (Law 1).
type Skill struct {
	ID         string
	Title      string // L0 summary — the cheap layer surfaced before the body (§11)
	Body       string // L1 raw — the full skill, paid for only after the summary
	Provenance []string
	State      SkillState
}

// Store is governed team memory backed by the append-only flg record. It holds
// no truth of its own (Law 1): facts is the record, and everything this type
// returns is projected out of it. Construct the flg.Store over a record.CAS
// (e.g. record.NewLocalCAS) and hand it here.
type Store struct {
	facts *flg.Store
}

// NewStore returns memory backed by the given append-only record.
func NewStore(facts *flg.Store) *Store { return &Store{facts: facts} }

// AddCandidate records sk as a candidate FACT in the record and returns its
// fact_id (its content address in the CAS). The skill's provenance travels with
// the fact, so the asset is reconstructable from the record alone (Law 1). No
// acceptance has happened yet — Recall will report it candidate until a
// promotion fact exists (§11.1).
func (s *Store) AddCandidate(sk Skill) (record.Digest, error) {
	ob := flg.Obligation{
		ID:               sk.ID,
		LemmaID:          lemmaSkill,
		Title:            sk.Title, // L0 summary
		AcceptancePolicy: sk.Body,  // L1 body
		DomainTags:       append([]string(nil), sk.Provenance...),
	}
	fv, err := s.facts.AppendObligation(ob)
	if err != nil {
		return "", err
	}
	return fv.FactID, nil
}

// Promote records that a candidate passed the acceptance gate by appending a
// SUPPORTING evidence fact against the skill (§11.1). The Decision (from Accept)
// carries how it was accepted — the held-out gate or a human attestation — which
// is recorded as the evidence Method, so the promotion is auditable. A Decision
// that does not promote is refused rather than silently recorded.
func (s *Store) Promote(id string, d Decision) (record.Digest, error) {
	if !d.Promote {
		return "", fmt.Errorf("learning: decision does not promote %s: %s", id, d.Reason)
	}
	ev := flg.Evidence{
		ID:              "promotion:" + id,
		ObligationID:    id,
		Polarity:        flg.PolaritySupports,
		Method:          d.method(),
		Reproducibility: flg.ReproDeterministic,
		Validity:        flg.ValidityValid,
		Scope:           flg.Scope{Domain: lemmaSkill, Tags: []string{lemmaSkill}},
		PrincipalID:     d.Principal,
		Summary:         d.Reason,
	}
	fv, err := s.facts.AppendEvidence(ev)
	if err != nil {
		return "", err
	}
	return fv.FactID, nil
}

// Retire supersedes a skill — never deletes it. It appends a supersession fact
// (Kind=decision) whose Subject is the skill; the skill and its history remain
// in the record for audit, but Recall stops returning it. This is how memory
// stays current without ever shrinking (append-only, Law 1).
func (s *Store) Retire(id, reason string) (record.Digest, error) {
	sup := flg.SupersessionEvidence{
		Subject:  id,
		Kind:     flg.KindDecision,
		Evidence: []string{reason}, // the justification, kept for audit
	}
	fv, err := s.facts.AppendSupersession(sup)
	if err != nil {
		return "", err
	}
	return fv.FactID, nil
}

// Recall returns skills relevant to query, layered per §11: PROMOTED skills
// first (the accepted layer), then candidates; retired skills are never
// recalled. Matching is a case-insensitive substring over summary and body; an
// empty query matches all. Order is deterministic — promoted-before-candidate,
// and within a state the record's own (content-address) projection order — so
// recall depends only on WHICH facts exist, never on when they were added.
func (s *Store) Recall(query string) []Skill {
	d, err := s.derive()
	if err != nil {
		return nil
	}
	q := strings.ToLower(query)
	var promoted, candidates []Skill
	for _, sk := range d.skills {
		if sk.State == SkillRetired {
			continue // superseded: in the record, never recalled
		}
		if !skillMatches(sk, q) {
			continue
		}
		if sk.State == SkillPromoted {
			promoted = append(promoted, sk)
		} else {
			candidates = append(candidates, sk)
		}
	}
	return append(promoted, candidates...)
}

// Summaries returns just the L0 summary line of each recalled skill, in recall
// order — the cheap layer a caller shows before paying to read full bodies
// (§11 layered recall, summary→raw).
func (s *Store) Summaries(query string) []string {
	sks := s.Recall(query)
	out := make([]string, len(sks))
	for i, sk := range sks {
		out[i] = sk.Title
	}
	return out
}

func skillMatches(sk Skill, lowerQuery string) bool {
	if lowerQuery == "" {
		return true
	}
	return strings.Contains(strings.ToLower(sk.Title), lowerQuery) ||
		strings.Contains(strings.ToLower(sk.Body), lowerQuery)
}

// derived is the projection of the whole learning record: skills and failures
// with their states resolved from the facts, in the record's deterministic
// order. This is the "rebuild the view from the record" half of Law 1.
type derived struct {
	skills   []Skill
	failures []Failure
}

// derive projects the append-only record into the current memory view. State is
// resolved from explicit facts only (Law 3): a skill is retired if a
// supersession names it, promoted if a promotion evidence supports it, candidate
// otherwise — retired dominating promoted dominating candidate.
func (s *Store) derive() (derived, error) {
	obls, evs, sups, err := s.facts.Project()
	if err != nil {
		return derived{}, err
	}

	promoted := make(map[string]bool)
	for _, e := range evs {
		if e.Scope.Domain == lemmaSkill && e.Polarity == flg.PolaritySupports {
			promoted[e.ObligationID] = true
		}
	}
	retired := make(map[string]bool)
	for _, sup := range sups {
		retired[sup.Subject] = true
	}

	var d derived
	seen := make(map[string]bool)
	for _, o := range obls {
		if seen[o.ID] {
			continue // a logical id is realized once; first projection wins, deterministically
		}
		switch o.LemmaID {
		case lemmaSkill:
			seen[o.ID] = true
			state := SkillCandidate
			switch {
			case retired[o.ID]:
				state = SkillRetired
			case promoted[o.ID]:
				state = SkillPromoted
			}
			d.skills = append(d.skills, Skill{
				ID:         o.ID,
				Title:      o.Title,
				Body:       o.AcceptancePolicy,
				Provenance: o.DomainTags,
				State:      state,
			})
		case lemmaFailure:
			seen[o.ID] = true
			d.failures = append(d.failures, Failure{
				ID:         o.ID,
				Summary:    o.Title,
				Signature:  o.Anchor.Symbol,
				Prevents:   o.AcceptancePolicy,
				Provenance: o.DomainTags,
			})
		}
	}
	return d, nil
}
