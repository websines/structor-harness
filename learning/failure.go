package learning

import (
	"strings"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/record"
)

// Failure is failure memory (system design §11): an incident or postmortem that
// a future run should avoid repeating. Signature is the machine-matchable
// fingerprint recall keys on; Prevents states what a future run should do (or
// not do) so the failure is not repeated. Like skills, a failure is a FACT in
// the record with provenance (Law 1) — remembered forever, never re-learned.
type Failure struct {
	ID         string
	Summary    string // L0 summary of the incident
	Signature  string // the fingerprint recalled against (e.g. an error class)
	Prevents   string // what a future run should do to avoid repeating it
	Provenance []string
}

// RecordFailure writes a failure into the append-only record and returns its
// fact_id. Failure memory only grows — an incident is never forgotten, so this
// is append-only with no retirement path (Law 1: always-increasing memory).
func (s *Store) RecordFailure(f Failure) (record.Digest, error) {
	ob := flg.Obligation{
		ID:               f.ID,
		LemmaID:          lemmaFailure,
		Title:            f.Summary,                       // L0 summary
		Anchor:           flg.Anchor{Symbol: f.Signature}, // the matchable fingerprint
		AcceptancePolicy: f.Prevents,                      // the lesson to apply next time
		DomainTags:       append([]string(nil), f.Provenance...),
	}
	fv, err := s.facts.AppendObligation(ob)
	if err != nil {
		return "", err
	}
	return fv.FactID, nil
}

// RecallFailures returns the failures whose Signature matches — the check a
// future run makes before repeating a known incident (§11 failure memory). The
// match is a case-insensitive substring so a run's live signature recalls the
// remembered one; an empty signature recalls all. Order is the record's
// deterministic projection order (no recency, Law 3).
func (s *Store) RecallFailures(signature string) []Failure {
	d, err := s.derive()
	if err != nil {
		return nil
	}
	q := strings.ToLower(signature)
	var out []Failure
	for _, f := range d.failures {
		if q == "" || strings.Contains(strings.ToLower(f.Signature), q) {
			out = append(out, f)
		}
	}
	return out
}
