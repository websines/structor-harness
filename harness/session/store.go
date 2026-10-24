package session

import (
	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/record"
)

// Config is the actor stamp every session a Store creates carries: the verified
// principal that produces its events (never a display string) and the commit the
// events are anchored to (Law 4). It is the "session config" the harness supplies.
type Config struct {
	PrincipalID string
	Commit      string
}

// Store is the ctx.Get(KeySession) service (hx.SessionStore): it hands out
// sessions backed by a single content-addressed record. It owns no truth of its
// own — every event lives in the CAS by digest; the Store just indexes live
// sessions by id. Mirrors flg.Store's relationship to the record (store.go): the
// log is addresses, the record is canonical.
type Store struct {
	cas      record.CAS
	actor    Config
	sessions map[string]*session
}

// compile-time proof this package satisfies the hx contract.
var _ hx.SessionStore = (*Store)(nil)

// NewStore returns an empty session store backed by cas.
func NewStore(cas record.CAS) *Store {
	return &Store{cas: cas, sessions: make(map[string]*session)}
}

// StampActor sets the principal + commit every subsequently-created session is
// stamped with, and returns the store for chaining. (The SessionStore interface's
// Create takes only an id, so the actor config rides on the store.)
func (s *Store) StampActor(cfg Config) *Store {
	s.actor = cfg
	return s
}

// Create makes a fresh session backed by the record and registers it by id.
func (s *Store) Create(id string) (hx.Session, error) {
	sess := &session{
		id:               id,
		cas:              s.cas,
		principalID:      s.actor.PrincipalID,
		commit:           s.actor.Commit,
		compactedThrough: -1, // -1 = no compaction yet (Seq 0 is a real event; never treat it as superseded before a Compact)
	}
	s.sessions[id] = sess
	return sess, nil
}

// Get returns the live session for id.
func (s *Store) Get(id string) (hx.Session, bool) {
	sess, ok := s.sessions[id]
	if !ok {
		return nil, false
	}
	return sess, true
}
