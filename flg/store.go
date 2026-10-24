package flg

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/websines/structor-harness/record"
)

// store.go is the append-only spine (system design §5.5, Law 2): facts are
// immutable versions in the content-addressed record; the resolver never reads
// them directly but through a DERIVED projection (Law 1). Dropping the
// projection and rebuilding it from the record reproduces the same
// ProjectionHash byte-for-byte — the property this file exists to guarantee.

// FactKind tags what a stored fact carries.
type FactKind string

const (
	FactObligation   FactKind = "obligation"
	FactEvidence     FactKind = "evidence"
	FactSupersession FactKind = "supersession"
)

// storedFact is the on-CAS envelope. Its content address IS the fact_id (§5.5):
// a new revision of the same logical fact yields a new fact_id while keeping its
// LogicalID. No timestamp — a fact is named by what it says, never by when.
type storedFact struct {
	Kind         FactKind              `json:"kind"`
	LogicalID    string                `json:"logical_id"`
	Obligation   *Obligation           `json:"obligation,omitempty"`
	Evidence     *Evidence             `json:"evidence,omitempty"`
	Supersession *SupersessionEvidence `json:"supersession,omitempty"`
}

// FactVersion is the caller-facing handle to an appended fact.
type FactVersion struct {
	FactID    record.Digest
	LogicalID string
	Kind      FactKind
}

// Store is an append-only log of fact_ids over a content-addressed record. It
// owns no truth of its own — every fact lives in the CAS, and the log is just
// the ordered set of addresses. It is rebuildable from a manifest's object set.
//
// A Store is safe for concurrent use. That matters now that a UI reads it while
// a run appends to it: without this, an append racing a Project() is a genuine
// data race on the log slice, which the race detector catches immediately once
// any caller is concurrent. The lock guards the LOG ONLY — the CAS is
// content-addressed and immutable, so reads of it need no coordination.
type Store struct {
	mu  sync.RWMutex
	cas record.CAS
	log []record.Digest
}

// NewStore returns an empty store backed by cas.
func NewStore(cas record.CAS) *Store { return &Store{cas: cas} }

// LoadStore reconstructs a store from a preservation set (a manifest's Objects).
// This is the "drop the projection and rebuild from the record" path of Law 1.
func LoadStore(cas record.CAS, objects []record.Digest) *Store {
	log := append([]record.Digest(nil), objects...)
	return &Store{cas: cas, log: log}
}

// Pinned captures membership once while sharing immutable, addressed objects.
// Later appends affect future runs without changing an active agent's context.
func (s *Store) Pinned() *Store { return LoadStore(s.cas, s.snapshot()) }

func (s *Store) append(f storedFact) (FactVersion, error) {
	// NOTE: the CAS Put happens under the lock too. It is idempotent and
	// content-addressed, so the cost is contention, never correctness -- and
	// releasing between Put and the log append would let two writers interleave
	// into an order neither of them observed.
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(f)
	if err != nil {
		return FactVersion{}, err
	}
	d, err := s.cas.Put(b)
	if err != nil {
		return FactVersion{}, err
	}
	s.log = append(s.log, d)
	return FactVersion{FactID: d, LogicalID: f.LogicalID, Kind: f.Kind}, nil
}

// Adopt adds an address that is ALREADY in this store's CAS to the log, without
// re-marshaling the fact.
//
// It exists for one operation: widening a fact's visibility across a scope tier
// (see internal/scope.Promote). Because a stored fact's envelope excludes ref and
// clock, its address is ref-independent — the same fact carried into a wider
// record keeps its exact fact_id, so every citation of it stays valid and
// adopting it twice is idempotent rather than duplicative. Re-marshaling instead
// would risk a different address for byte-identical meaning and silently orphan
// those citations.
//
// It fails closed: an address whose bytes are not present is refused rather than
// logged, because a preservation set naming an object the CAS cannot produce is
// exactly the un-rebuildable projection Law 1 forbids.
//
// This is NOT an escape hatch for writing facts. It cannot create one — the bytes
// must already exist, which means some Append call already made them.
func (s *Store) Adopt(d record.Digest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.cas.Has(d) {
		return fmt.Errorf("flg: adopt %s: not present in this store's CAS", d)
	}
	s.log = append(s.log, d)
	return nil
}

// AppendObligation records an obligation fact. LogicalID is the obligation id.
func (s *Store) AppendObligation(o Obligation) (FactVersion, error) {
	return s.append(storedFact{Kind: FactObligation, LogicalID: o.ID, Obligation: &o})
}

// AppendEvidence records an evidence fact. LogicalID is the evidence id.
func (s *Store) AppendEvidence(e Evidence) (FactVersion, error) {
	return s.append(storedFact{Kind: FactEvidence, LogicalID: e.ID, Evidence: &e})
}

// AppendSupersession records a supersession fact. Its own ID is left empty in
// the stored bytes (the fact_id is the content address, assigned on read), so
// the address never depends on itself. LogicalID defaults to the subject.
func (s *Store) AppendSupersession(sup SupersessionEvidence) (FactVersion, error) {
	sup.ID = "" // the fact_id is the content address; do not store it inside the content
	logical := sup.Subject
	return s.append(storedFact{Kind: FactSupersession, LogicalID: logical, Supersession: &sup})
}

// WalkedFact is one appended fact in append order, carrying its content-address
// fact_id plus the decoded payload — what the operational projections (provenance
// lineage, bitemporal transaction time) read.
type WalkedFact struct {
	FactID       record.Digest
	Kind         FactKind
	LogicalID    string
	Obligation   *Obligation
	Evidence     *Evidence
	Supersession *SupersessionEvidence
}

// Walk returns every appended fact in APPEND order, each decoded and tagged with
// its fact_id. Unlike Project (which sorts by address for a reproducible,
// order-independent projection — Law 1), Walk preserves the order facts were
// recorded: the operational transaction order (P2), which a store rebuilt via
// LoadStore does NOT recover, and correctly so — "when the system recorded it" is
// not part of the canonical record. It adds no clock (Law 3 holds). A
// supersession's ID is set to its address here, as in Project.
func (s *Store) Walk() ([]WalkedFact, error) {
	log := s.snapshot()
	out := make([]WalkedFact, 0, len(log))
	for _, d := range log {
		b, err := s.cas.Get(d)
		if err != nil {
			return nil, err
		}
		var f storedFact
		if err := json.Unmarshal(b, &f); err != nil {
			return nil, err
		}
		wf := WalkedFact{FactID: d, Kind: f.Kind, LogicalID: f.LogicalID, Obligation: f.Obligation, Evidence: f.Evidence, Supersession: f.Supersession}
		if f.Kind == FactSupersession && wf.Supersession != nil {
			wf.Supersession.ID = string(d)
		}
		out = append(out, wf)
	}
	return out, nil
}

// ManifestObjects returns the preservation set — every fact address, sorted so
// the set is deterministic and the manifest bytes are reproducible.
// snapshot returns a copy of the log under a read lock. Every reader goes
// through this: handing out s.log directly would let a caller observe an append
// mid-iteration, which is the race this lock exists to close.
func (s *Store) snapshot() []record.Digest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]record.Digest, len(s.log))
	copy(out, s.log)
	return out
}

func (s *Store) ManifestObjects() []record.Digest {
	// DEDUPED. The log may hold the same address twice — a retried write appends
	// the same content, and content addressing means that is ONE fact, not two.
	// Without this, Project() yields the obligation twice and a UI shows a
	// duplicate row for an action the caller believed was idempotent.
	seen := make(map[record.Digest]bool)
	out := make([]record.Digest, 0, len(s.snapshot()))
	for _, d := range s.snapshot() {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ProjectionHash is the single digest a manifest binds evidence to (§10). It is
// computed over the SORTED fact addresses, so it depends only on WHICH facts are
// in the record, never on the order they were appended — drop and rebuild in any
// order and the hash is identical. This is the machine-checkable form of Law 1.
func (s *Store) ProjectionHash() record.Digest {
	objs := s.ManifestObjects()
	var buf []byte
	for _, d := range objs {
		buf = append(buf, []byte(d)...)
		buf = append(buf, '\n')
	}
	return record.ComputeDigest(buf)
}

// Project derives the resolver's inputs from the append-only record: every
// obligation, evidence and supersession fact, decoded from the CAS. Supersession
// facts get their fact_id assigned here (their content address), so a reopened
// verdict can cite a real fact id (§5.5). Facts are returned in a deterministic
// order (by fact address) so the projection is fully reproducible.
func (s *Store) Project() (obls []Obligation, evs []Evidence, sups []SupersessionEvidence, err error) {
	for _, d := range s.ManifestObjects() {
		b, gErr := s.cas.Get(d)
		if gErr != nil {
			return nil, nil, nil, gErr
		}
		var f storedFact
		if uErr := json.Unmarshal(b, &f); uErr != nil {
			return nil, nil, nil, uErr
		}
		switch f.Kind {
		case FactObligation:
			if f.Obligation != nil {
				obls = append(obls, *f.Obligation)
			}
		case FactEvidence:
			if f.Evidence != nil {
				evs = append(evs, *f.Evidence)
			}
		case FactSupersession:
			if f.Supersession != nil {
				sup := *f.Supersession
				sup.ID = string(d) // the fact_id is the content address
				sups = append(sups, sup)
			}
		}
	}
	return obls, evs, sups, nil
}
