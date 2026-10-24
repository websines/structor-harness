// Package session is the FLG-backed session log (forge-harness-design §2.1): the
// conversation's append-only log IS the record. Each session event is stored as a
// content-addressed blob and DeriveMessages projects the LLM message history from
// the SURFACE events — exactly as the resolver projects verdicts from facts (Law
// 1). The log is the single source of truth; the message history and the request
// header are DERIVED and cached incrementally.
//
// Backing choice (memory efficiency, §5): a session event is stored as a CAS blob
// via record.CAS and the Seq→digest log is kept here, in the session package —
// flg/store.go is left UNTOUCHED. Its fact kinds (obligation/evidence/
// supersession) are the resolver's vocabulary; a session event is a different
// shape and sits on the same substrate one layer down, the record. Storing the
// events as blobs keeps flg additive-only (nothing edited) and honors "the log
// holds the digest, never the bytes" (§2.1): the event's FactID IS the content
// address of its stored envelope.
package session

import (
	"encoding/json"
	"fmt"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/record"
)

// evCompactionSummary is the kind of the surface event a Compact appends: the ONE
// summary that stands in for a superseded span (design §2.2, compaction IS
// supersession). It is a local EventKind — hx is frozen — and a distinct value so
// the projection can float it to the FRONT of the derived view (it replaces the
// oldest span) instead of leaving it at its append position. It carries a
// JSON-encoded hx.Message, so it projects like any other surface message.
const evCompactionSummary hx.EventKind = "compaction/summary"

// blobThreshold is the payload size (bytes) above which an event's Data is
// spilled to the CAS as its own blob and referenced by digest instead of inlined
// in the event envelope (reference-not-copy, §5.3). Two events that carry the same
// large payload then cost one blob (content addressing dedups). Tunable.
const blobThreshold = 4096

// storedEvent is the on-CAS envelope for one session event. Its content address
// IS the event's FactID — mirroring flg.storedFact in store.go, an event is named
// by what it says, never by when it arrived (no timestamp; Law 3). A large Data
// payload is not carried inline: it is spilled to its own CAS blob and referenced
// here by DataRef, so the envelope stays small regardless of payload size. Exactly
// one of Data / DataRef is set.
type storedEvent struct {
	Seq         int           `json:"seq"`
	Kind        hx.EventKind  `json:"kind"`
	Surface     bool          `json:"surface"`
	PrincipalID string        `json:"principal_id,omitempty"`
	Commit      string        `json:"commit,omitempty"`
	Data        []byte        `json:"data,omitempty"`     // inline — small payloads only
	DataRef     record.Digest `json:"data_ref,omitempty"` // CAS digest of a spilled large payload
}

// session is one conversation's append-only log backed by the record, plus its
// incrementally-cached message/header projections. It implements hx.Session.
type session struct {
	id          string
	cas         record.CAS
	principalID string // the verified principal every event is stamped with (actor is a principal, never a display string)
	commit      string // the commit every event is anchored to (Law 4)

	// head is the in-memory head of the log: the events, resident for O(1) append
	// and O(new) projection. Each element's FactID is the CAS address of its stored
	// envelope, so an evicted event pages back from the CAS by that digest via
	// loadEvent — the head need not stay whole. For H1 we keep every event resident;
	// the invariant that must hold is that the DERIVE cache below stays incremental,
	// not that the head is bounded.
	head []hx.SessionEvent

	// --- derived, cached incrementally (see derive.go) ---
	msgCache   []hx.Message     // messages projected from surface events so far
	deriveNext int              // index of the next unprojected event; the fold cursor
	header     hx.RequestHeader // latest folded request/header
	headerNext int              // index of the next unfolded event for Header()

	// compactedThrough is the supersession watermark (design §2.2): every SURFACE
	// event with Seq <= this is compacted-away — dropped from the DERIVED view but
	// left in the log (Events() is unchanged; supersession never deletes). -1 means
	// nothing is compacted. Monotonic: a later Compact only raises it.
	compactedThrough int

	// compactedSet holds individually-superseded surface Seqs (quality-aware
	// compaction, CompactSet): a message anywhere in the history — not just an
	// oldest prefix — dropped from the derived view but retained in the log. Both
	// mechanisms coexist; superseded(seq) unions them.
	compactedSet map[int]bool
}

// superseded reports whether a surface event's Seq is compacted-away — by the
// prefix watermark OR the quality-aware set. The single predicate DeriveMessages,
// rederive and DerivedSeqs all consult, so both mechanisms behave identically.
func (s *session) superseded(seq int) bool {
	return seq <= s.compactedThrough || s.compactedSet[seq]
}

// compile-time proof this package satisfies the hx contract, plus the concrete
// surface the compaction engine drives through optional-interface assertions
// (compaction/ imports hx, never this package — the evidence-package pattern).
var (
	_ hx.Session                                  = (*session)(nil)
	_ interface{ Compact(int, hx.Message) error } = (*session)(nil)
	_ interface {
		CompactSet([]int, hx.Message) error
	} = (*session)(nil)
	_ interface{ DerivedSeqs() []int } = (*session)(nil)
	_ interface {
		DerivedLen() int
		DerivedChars() int
	} = (*session)(nil)
)

// ID returns the session id.
func (s *session) ID() string { return s.id }

// Append records one event as a fact in the record and returns it with Seq and
// FactID assigned. Seq is the contiguous position (len of the log) — ordering is
// by Seq, never by clock (Law 3). A large Data payload is spilled to the CAS and
// referenced by digest; the event envelope is then content-addressed and its
// address IS the FactID. The event is actor-stamped (principal + commit) and
// appended to the in-memory head.
func (s *session) Append(kind hx.EventKind, data []byte, surface bool) (hx.SessionEvent, error) {
	seq := len(s.head)

	env := storedEvent{
		Seq:         seq,
		Kind:        kind,
		Surface:     surface,
		PrincipalID: s.principalID,
		Commit:      s.commit,
	}
	// Reference-not-copy: spill a large payload to its own blob; keep small ones inline.
	if len(data) > blobThreshold {
		ref, err := s.cas.Put(data)
		if err != nil {
			return hx.SessionEvent{}, err
		}
		env.DataRef = ref
	} else {
		env.Data = data
	}

	b, err := json.Marshal(env)
	if err != nil {
		return hx.SessionEvent{}, err
	}
	factID, err := s.cas.Put(b)
	if err != nil {
		return hx.SessionEvent{}, err
	}

	ev := hx.SessionEvent{
		Seq:         seq,
		Kind:        kind,
		Data:        data, // resident in the head (H1); the record holds it by digest
		Surface:     surface,
		FactID:      string(factID),
		PrincipalID: s.principalID,
		Commit:      s.commit,
	}
	s.head = append(s.head, ev)
	return ev, nil
}

// Events returns the log in Seq order. For H1 the whole head is resident; a
// bounded-head build would page evicted events back from the CAS by FactID via
// loadEvent, so callers must not assume the slice is backed by RAM alone.
func (s *session) Events() []hx.SessionEvent { return s.head }

// loadEvent reconstructs an event from the record by its FactID (its envelope's
// content address), rehydrating a spilled payload from its own blob. This is the
// paging path an evicted event takes back into the head; keeping it here lets the
// head be bounded without changing the projection.
func (s *session) loadEvent(factID record.Digest) (hx.SessionEvent, error) {
	b, err := s.cas.Get(factID)
	if err != nil {
		return hx.SessionEvent{}, err
	}
	var env storedEvent
	if err := json.Unmarshal(b, &env); err != nil {
		return hx.SessionEvent{}, err
	}
	data := env.Data
	if env.DataRef != "" {
		if data, err = s.cas.Get(env.DataRef); err != nil {
			return hx.SessionEvent{}, err
		}
	}
	return hx.SessionEvent{
		Seq:         env.Seq,
		Kind:        env.Kind,
		Data:        data,
		Surface:     env.Surface,
		FactID:      string(factID),
		PrincipalID: env.PrincipalID,
		Commit:      env.Commit,
	}, nil
}

// Compact realizes "compaction IS supersession" (design §2.2): it replaces the
// surface span with Seq <= throughSeq by the single summary message. Concretely it
// (1) appends summary as a NEW surface event — a fact in the append-only log, the
// supersession recorded, never a deletion — and (2) raises the compaction
// watermark so every surface event with Seq <= throughSeq drops out of the DERIVED
// view. Events() still returns the whole log (nothing is lost — memory drops, the
// record does not); only DeriveMessages shrinks to [summary, ...retained tail].
//
// It is a CONCRETE method (hx is frozen); the compaction engine reaches it through
// an optional-interface assertion, like the evidence bridge reaches ToolRuntime.
func (s *session) Compact(throughSeq int, summary hx.Message) error {
	if throughSeq < 0 {
		return fmt.Errorf("session: Compact throughSeq %d is negative", throughSeq)
	}
	if throughSeq >= len(s.head) {
		return fmt.Errorf("session: Compact throughSeq %d out of range (log holds %d events)", throughSeq, len(s.head))
	}

	// Record the supersession as a fact: the summary is appended as its own surface
	// event (Seq = current head length, so it is strictly newer than throughSeq and
	// is NOT superseded by this call). Its provenance is its own fact_id.
	b, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	if _, err := s.Append(evCompactionSummary, b, true); err != nil {
		return err
	}

	// Raise the watermark (monotonic): the span with Seq <= throughSeq is now
	// superseded. A Compact that would only lower it still records its summary but
	// changes no span — the caller (the engine) guards against that.
	if throughSeq > s.compactedThrough {
		s.compactedThrough = throughSeq
	}

	// The derived cache changed shape at the compaction point: the superseded span
	// disappears and the summary floats to the front. Recompute it from the log
	// (see rederive) and reset the fold cursor to the head end, so later Append/
	// DeriveMessages stay incremental (O(new events)).
	s.rederive()
	return nil
}

// CompactSet supersedes an ARBITRARY set of surface events (quality-aware
// compaction, design §2 follow-up): the engine chooses which messages to drop by
// value, not just by age, so a load-bearing old message survives while a stale
// newer one is compacted. Each seq in supersededSeqs is dropped from the derived
// view (retained in the log — supersession never deletes); the summary is appended
// as one surface event that floats to the front, standing in for the whole set.
func (s *session) CompactSet(supersededSeqs []int, summary hx.Message) error {
	if len(supersededSeqs) == 0 {
		return nil
	}
	if s.compactedSet == nil {
		s.compactedSet = make(map[int]bool)
	}
	for _, seq := range supersededSeqs {
		if seq < 0 || seq >= len(s.head) {
			return fmt.Errorf("session: CompactSet seq %d out of range (log holds %d events)", seq, len(s.head))
		}
		s.compactedSet[seq] = true
	}
	b, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	if _, err := s.Append(evCompactionSummary, b, true); err != nil {
		return err
	}
	s.rederive()
	return nil
}
