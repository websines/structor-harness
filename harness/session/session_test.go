// White-box tests (package session) so they may observe the incremental derive
// cursor and the on-CAS envelope directly — the two properties the harness design
// makes load-bearing (§2.1 the log IS the record, §5 memory efficiency).
package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/record"
)

// newSession spins up a store over a fresh local CAS and creates one session,
// actor-stamped so events carry a verified principal and a commit anchor.
func newSession(t *testing.T) *session {
	t.Helper()
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := NewStore(cas).StampActor(Config{PrincipalID: "principal:marcus", Commit: "c0ffee"})
	s, err := st.Create("S-1")
	if err != nil {
		t.Fatal(err)
	}
	return s.(*session)
}

// msgBytes encodes a one-text-block message the way a surface event carries it.
func msgBytes(t *testing.T, role hx.Role, text string) []byte {
	t.Helper()
	b, err := json.Marshal(hx.Message{Role: role, Content: []hx.ContentBlock{{Kind: hx.BlockText, Text: text}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAppendAssignsContiguousSeqAndFactID: every append gets the next contiguous
// Seq and a non-empty content-addressed FactID, and the actor stamp is carried.
func TestAppendAssignsContiguousSeqAndFactID(t *testing.T) {
	s := newSession(t)
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		ev, err := s.Append(hx.EvUserMessage, msgBytes(t, hx.RoleUser, "hi"), true)
		if err != nil {
			t.Fatal(err)
		}
		if ev.Seq != i {
			t.Fatalf("seq not contiguous: got %d want %d", ev.Seq, i)
		}
		if !strings.HasPrefix(ev.FactID, "sha256:") {
			t.Fatalf("fact_id not content-addressed: %q", ev.FactID)
		}
		if ev.PrincipalID != "principal:marcus" || ev.Commit != "c0ffee" {
			t.Fatalf("event not actor-stamped: %+v", ev)
		}
		seen[ev.FactID] = true
	}
	// Distinct payloads-by-seq yield distinct fact_ids (Seq is inside the envelope).
	if len(seen) != 5 {
		t.Fatalf("expected 5 distinct fact_ids, got %d", len(seen))
	}
	if got := len(s.Events()); got != 5 {
		t.Fatalf("expected 5 events in the log, got %d", got)
	}
}

// TestDeriveMessagesOnlySurface: only surface events (user/assistant/tool
// messages) become messages, in Seq order; lifecycle and chunk events do not.
func TestDeriveMessagesOnlySurface(t *testing.T) {
	s := newSession(t)
	mustAppend(t, s, hx.EvTurnStart, []byte(`{}`), false)
	mustAppend(t, s, hx.EvUserMessage, msgBytes(t, hx.RoleUser, "add a refund cap"), true)
	mustAppend(t, s, hx.EvAssistantChunk, []byte(`{"delta":"th"}`), false)
	mustAppend(t, s, hx.EvAssistantMessage, msgBytes(t, hx.RoleAssistant, "on it"), true)
	mustAppend(t, s, hx.EvToolResult, msgBytes(t, hx.RoleTool, "tests pass"), true)
	mustAppend(t, s, hx.EvTurnEnd, []byte(`{}`), false)

	msgs := s.DeriveMessages()
	if len(msgs) != 3 {
		t.Fatalf("expected 3 surface messages, got %d", len(msgs))
	}
	wantRole := []hx.Role{hx.RoleUser, hx.RoleAssistant, hx.RoleTool}
	wantText := []string{"add a refund cap", "on it", "tests pass"}
	for i, m := range msgs {
		if m.Role != wantRole[i] {
			t.Fatalf("msg %d role: got %s want %s", i, m.Role, wantRole[i])
		}
		if m.Content[0].Text != wantText[i] {
			t.Fatalf("msg %d text: got %q want %q", i, m.Content[0].Text, wantText[i])
		}
		if !strings.HasPrefix(m.Source, "sha256:") {
			t.Fatalf("msg %d provenance not the producing fact_id: %q", i, m.Source)
		}
	}
}

// TestDeriveCacheIncremental: the fold cursor proves each event is projected once.
// After a full derive the cursor is at the log's end; appending one event and
// re-deriving advances the cursor by exactly one — O(new events), not O(log).
func TestDeriveCacheIncremental(t *testing.T) {
	s := newSession(t)
	for i := 0; i < 10; i++ {
		mustAppend(t, s, hx.EvUserMessage, msgBytes(t, hx.RoleUser, "m"), true)
	}
	if got := len(s.DeriveMessages()); got != 10 {
		t.Fatalf("expected 10 messages, got %d", got)
	}
	if s.deriveNext != 10 {
		t.Fatalf("first derive should fold the whole log once (cursor 10), got %d", s.deriveNext)
	}

	// Append one more and re-derive: the cursor must move by exactly one — the fold
	// touched only the new event, not the 10 already projected.
	before := s.deriveNext
	mustAppend(t, s, hx.EvUserMessage, msgBytes(t, hx.RoleUser, "m11"), true)
	msgs := s.DeriveMessages()
	if scanned := s.deriveNext - before; scanned != 1 {
		t.Fatalf("re-derive re-scanned %d events; expected only the 1 new one", scanned)
	}
	if len(msgs) != 11 {
		t.Fatalf("expected 11 messages after one more append, got %d", len(msgs))
	}

	// A non-surface append still advances the cursor by one but adds no message.
	before = s.deriveNext
	mustAppend(t, s, hx.EvTurnEnd, []byte(`{}`), false)
	msgs = s.DeriveMessages()
	if scanned := s.deriveNext - before; scanned != 1 {
		t.Fatalf("re-derive re-scanned %d events; expected only the 1 new one", scanned)
	}
	if len(msgs) != 11 {
		t.Fatalf("non-surface event must not add a message; got %d", len(msgs))
	}
}

// TestLargePayloadStoredByReference: a payload over the threshold is spilled to
// its own CAS blob and the event envelope holds the DIGEST, not the bytes —
// reference-not-copy (§5.3). The log never inlines the large payload.
func TestLargePayloadStoredByReference(t *testing.T) {
	s := newSession(t)
	payload := make([]byte, 16*1024) // 16 KiB — well over blobThreshold
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	ev, err := s.Append(hx.EvToolResult, payload, true)
	if err != nil {
		t.Fatal(err)
	}

	// Fetch the on-CAS envelope (fact_id addresses it) and inspect it directly.
	raw, err := s.cas.Get(record.Digest(ev.FactID))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= len(payload) {
		t.Fatalf("envelope (%d B) is not smaller than the payload (%d B) — payload was inlined", len(raw), len(payload))
	}
	var env storedEvent
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.DataRef == "" || env.Data != nil {
		t.Fatalf("large payload not referenced by digest: data_ref=%q inline=%d bytes", env.DataRef, len(env.Data))
	}
	if env.DataRef != record.ComputeDigest(payload) {
		t.Fatalf("data_ref is not the payload's content address")
	}
	if !s.cas.Has(env.DataRef) {
		t.Fatal("referenced payload blob is absent from the CAS")
	}

	// loadEvent pages the event back from the record and rehydrates the payload —
	// the reference round-trips to the original bytes.
	got, err := s.loadEvent(record.Digest(ev.FactID))
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Data) != string(payload) {
		t.Fatal("paged-back payload does not match the original")
	}
}

// TestStoreGetAndMissing: Create registers the session by id; Get returns it;
// an unknown id returns false (and a nil interface, not a typed nil).
func TestStoreGetAndMissing(t *testing.T) {
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := NewStore(cas)
	created, err := st.Create("S-42")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := st.Get("S-42")
	if !ok || got.ID() != "S-42" || got != created {
		t.Fatalf("Get did not return the created session: ok=%v", ok)
	}
	if s, ok := st.Get("nope"); ok || s != nil {
		t.Fatalf("Get of unknown id should be (nil,false), got (%v,%v)", s, ok)
	}
}

func mustAppend(t *testing.T, s *session, kind hx.EventKind, data []byte, surface bool) {
	t.Helper()
	if _, err := s.Append(kind, data, surface); err != nil {
		t.Fatal(err)
	}
}
