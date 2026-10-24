// White-box tests for compaction-as-supersession (design §2.2): they observe the
// watermark and the head directly to prove the DERIVED view shrinks while the
// append-only log is retained whole.
package session

import (
	"testing"

	"github.com/websines/structor-harness/harness/hx"
)

// summaryMsg builds a one-text-block summary message the way the compaction engine
// would hand it to Compact.
func summaryMsg(text string) hx.Message {
	return hx.Message{Role: hx.RoleSystem, Content: []hx.ContentBlock{{Kind: hx.BlockText, Text: text}}}
}

// TestCompactSupersedesSpanKeepsLog: after Compact the derived view is [summary,
// ...retained tail] — the superseded span is gone from DeriveMessages — but
// Events() still returns every event (the summary appended, nothing deleted).
func TestCompactSupersedesSpanKeepsLog(t *testing.T) {
	s := newSession(t)
	for i := 0; i < 10; i++ {
		mustAppend(t, s, hx.EvUserMessage, msgBytes(t, hx.RoleUser, string(rune('a'+i))), true)
	}
	if got := len(s.DeriveMessages()); got != 10 {
		t.Fatalf("pre-compact derived: got %d want 10", got)
	}

	// Compact events with Seq <= 6 (the oldest seven). Retained tail: Seq 7,8,9.
	if err := s.Compact(6, summaryMsg("SUMMARY")); err != nil {
		t.Fatal(err)
	}

	msgs := s.DeriveMessages()
	// [summary, g, h, i] — summary FIRST, then the retained tail in Seq order.
	if len(msgs) != 4 {
		t.Fatalf("post-compact derived: got %d want 4", len(msgs))
	}
	if msgs[0].Content[0].Text != "SUMMARY" || msgs[0].Role != hx.RoleSystem {
		t.Fatalf("summary not at the front: %+v", msgs[0])
	}
	wantTail := []string{"h", "i", "j"} // 'a'+7, 'a'+8, 'a'+9
	for i, w := range wantTail {
		if msgs[i+1].Content[0].Text != w {
			t.Fatalf("retained tail[%d]: got %q want %q", i, msgs[i+1].Content[0].Text, w)
		}
	}

	// Supersession never deletes: the log still holds all 10 originals + the summary.
	if got := len(s.Events()); got != 11 {
		t.Fatalf("Events() should retain the whole log (10 + summary = 11); got %d", got)
	}
	if s.compactedThrough != 6 {
		t.Fatalf("watermark: got %d want 6", s.compactedThrough)
	}
}

// TestCompactRederiveIsCorrect: a from-scratch rebuild of the derived view (rederive)
// equals the incrementally-served view after compaction — the cache stays correct,
// and appends after compaction remain incremental (cursor advances by one per event).
func TestCompactRederiveIsCorrect(t *testing.T) {
	s := newSession(t)
	for i := 0; i < 6; i++ {
		mustAppend(t, s, hx.EvUserMessage, msgBytes(t, hx.RoleUser, string(rune('a'+i))), true)
	}
	if err := s.Compact(3, summaryMsg("S1")); err != nil {
		t.Fatal(err)
	}
	// After Compact the cursor sits at the head end; an append then folds exactly one.
	if s.deriveNext != len(s.head) {
		t.Fatalf("cursor not reset to head end after compact: %d vs %d", s.deriveNext, len(s.head))
	}
	before := s.deriveNext
	mustAppend(t, s, hx.EvUserMessage, msgBytes(t, hx.RoleUser, "z"), true)
	got := s.DeriveMessages()
	if scanned := s.deriveNext - before; scanned != 1 {
		t.Fatalf("post-compact re-derive re-scanned %d events; expected only the 1 new one", scanned)
	}

	// Independently rebuild from the log and compare, message for message.
	s.rederive()
	want := s.msgCache
	if len(got) != len(want) {
		t.Fatalf("incremental view (%d) != rebuilt view (%d)", len(got), len(want))
	}
	for i := range want {
		if got[i].Content[0].Text != want[i].Content[0].Text {
			t.Fatalf("view[%d]: incremental %q != rebuilt %q", i, got[i].Content[0].Text, want[i].Content[0].Text)
		}
	}
	// Expected view: [S1, e, f, z] (Seq 4,5 retained + the new one).
	wantText := []string{"S1", "e", "f", "z"}
	for i, w := range wantText {
		if got[i].Content[0].Text != w {
			t.Fatalf("view[%d]: got %q want %q", i, got[i].Content[0].Text, w)
		}
	}
}

// TestRepeatedCompactionSubsumesPriorSummary: a second compaction whose watermark
// covers the first summary (its high Seq) leaves exactly one active summary at the
// front, and the whole log is still retained.
func TestRepeatedCompactionSubsumesPriorSummary(t *testing.T) {
	s := newSession(t)
	for i := 0; i < 6; i++ { // Seq 0..5
		mustAppend(t, s, hx.EvUserMessage, msgBytes(t, hx.RoleUser, string(rune('a'+i))), true)
	}
	if err := s.Compact(2, summaryMsg("S1")); err != nil { // S1 at Seq 6; retained d,e,f
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // Seq 7,8,9
		mustAppend(t, s, hx.EvUserMessage, msgBytes(t, hx.RoleUser, string(rune('p'+i))), true)
	}
	// Derived now: [S1, d, e, f, p, q, r]. Second compaction through Seq 8 subsumes
	// S1 (Seq 6) and d,e,f (Seq 3,4,5) and p,q (Seq 7,8); retains only r (Seq 9).
	if err := s.Compact(8, summaryMsg("S2")); err != nil { // S2 at Seq 10
		t.Fatal(err)
	}
	msgs := s.DeriveMessages()
	wantText := []string{"S2", "r"}
	if len(msgs) != len(wantText) {
		t.Fatalf("post-second-compact derived: got %d want %d (%v)", len(msgs), len(wantText), texts(msgs))
	}
	for i, w := range wantText {
		if msgs[i].Content[0].Text != w {
			t.Fatalf("view[%d]: got %q want %q", i, msgs[i].Content[0].Text, w)
		}
	}
	// The full log is retained: 6 originals + S1 + 3 more + S2 = 11.
	if got := len(s.Events()); got != 11 {
		t.Fatalf("Events() should retain the whole log (11); got %d", got)
	}
}

// TestDerivedBudgetReaders: the count/char accessors the compaction engine reads
// reflect the compacted view (they shrink after supersession).
func TestDerivedBudgetReaders(t *testing.T) {
	s := newSession(t)
	for i := 0; i < 5; i++ {
		mustAppend(t, s, hx.EvUserMessage, msgBytes(t, hx.RoleUser, "hello"), true) // 5 chars each
	}
	if s.DerivedLen() != 5 || s.DerivedChars() != 25 {
		t.Fatalf("pre-compact budget: len=%d chars=%d want 5/25", s.DerivedLen(), s.DerivedChars())
	}
	if err := s.Compact(3, summaryMsg("XY")); err != nil { // supersede first four; retained one "hello"
		t.Fatal(err)
	}
	// Derived: [XY, hello] -> 2 messages, 2 + 5 = 7 chars.
	if s.DerivedLen() != 2 || s.DerivedChars() != 7 {
		t.Fatalf("post-compact budget: len=%d chars=%d want 2/7", s.DerivedLen(), s.DerivedChars())
	}
}

// TestCompactRangeGuards: an out-of-range or negative throughSeq is rejected and
// leaves the log untouched.
func TestCompactRangeGuards(t *testing.T) {
	s := newSession(t)
	for i := 0; i < 3; i++ {
		mustAppend(t, s, hx.EvUserMessage, msgBytes(t, hx.RoleUser, "m"), true)
	}
	if err := s.Compact(3, summaryMsg("S")); err == nil { // Seq 3 does not exist (only 0,1,2)
		t.Fatal("expected out-of-range throughSeq to error")
	}
	if err := s.Compact(-1, summaryMsg("S")); err == nil {
		t.Fatal("expected negative throughSeq to error")
	}
	if got := len(s.Events()); got != 3 {
		t.Fatalf("failed Compact must not append: got %d want 3", got)
	}
}

func texts(msgs []hx.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		if len(m.Content) > 0 {
			out[i] = m.Content[0].Text
		}
	}
	return out
}
