// End-to-end tests: the compaction Engine driving the REAL FLG-backed session
// (session is this agent's own package, wired here as an integration). They prove
// the design §2.2 contract — over budget, the oldest span is superseded; the
// derived view becomes [summary, ...recent K]; Events() still returns every event;
// under budget, MaybeCompact is a no-op.
package compaction_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/websines/structor-harness/harness/compaction"
	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/session"
	"github.com/websines/structor-harness/record"
)

func TestCanceledCompactionLeavesRecordUnchanged(t *testing.T) {
	tests := []struct {
		name   string
		scored bool
		during bool
	}{
		{name: "prefix already canceled"},
		{name: "prefix canceled while summarizing", during: true},
		{name: "scored already canceled", scored: true},
		{name: "scored canceled while summarizing", scored: true, during: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := newSession(t)
			for i := range 8 {
				appendMsg(t, sess, "message "+strconv.Itoa(i))
			}
			beforeEvents, beforeMessages := sess.Events(), sess.DeriveMessages()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			started := make(chan struct{})
			calls := 0
			engine := &compaction.Engine{
				MaxTokens:   1_000_000,
				MaxMessages: 3,
				Summarize: func(ctx context.Context, _ []hx.Message) (string, error) {
					calls++
					close(started)
					<-ctx.Done()
					return "", ctx.Err()
				},
			}
			if tt.scored {
				engine.Score = func(hx.Message) int { return 0 }
			}
			type outcome struct {
				compacted bool
				err       error
			}
			var result outcome
			if tt.during {
				done := make(chan outcome, 1)
				go func() {
					compacted, err := engine.MaybeCompact(ctx, sess, 0)
					done <- outcome{compacted, err}
				}()
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("summarizer did not start")
				}
				cancel()
				select {
				case result = <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("canceled compactor did not join its summarizer")
				}
			} else {
				cancel()
				result.compacted, result.err = engine.MaybeCompact(ctx, sess, 0)
			}
			if result.compacted || !errors.Is(result.err, context.Canceled) {
				t.Fatalf("canceled compaction committed: compacted=%v error=%v", result.compacted, result.err)
			}
			if !reflect.DeepEqual(sess.Events(), beforeEvents) || !reflect.DeepEqual(sess.DeriveMessages(), beforeMessages) {
				t.Fatal("canceled compaction changed the durable record or its derived history")
			}
			wantCalls := 0
			if tt.during {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("summarizer called %d times, want %d", calls, wantCalls)
			}
		})
	}
}

// newSession spins up a real session over a fresh local CAS.
func newSession(t *testing.T) hx.Session {
	t.Helper()
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := session.NewStore(cas).
		StampActor(session.Config{PrincipalID: "principal:marcus", Commit: "c0ffee"}).
		Create("S-1")
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// appendMsg appends one user message carrying text.
func appendMsg(t *testing.T, sess hx.Session, text string) {
	t.Helper()
	b, err := json.Marshal(hx.Message{Role: hx.RoleUser, Content: []hx.ContentBlock{{Kind: hx.BlockText, Text: text}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Append(hx.EvUserMessage, b, true); err != nil {
		t.Fatal(err)
	}
}

func text(m hx.Message) string {
	if len(m.Content) == 0 {
		return ""
	}
	return m.Content[0].Text
}

// TestMaybeCompactOverBudgetByCount: past the message-count budget, the engine
// supersedes the oldest span; the derived view becomes [summary, ...recent K] and
// fits the budget, while Events() retains every original plus the summary.
func TestMaybeCompactOverBudgetByCount(t *testing.T) {
	sess := newSession(t)
	e := &compaction.Engine{MaxMessages: 5, MaxTokens: 1_000_000}
	for i := 0; i < 12; i++ { // m0..m11 — well over the 5-message budget
		appendMsg(t, sess, "m"+strconv.Itoa(i))
	}

	compacted, err := e.MaybeCompact(context.Background(), sess, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !compacted {
		t.Fatal("expected compaction when over the message-count budget")
	}

	msgs := sess.DeriveMessages()
	if len(msgs) > e.MaxMessages {
		t.Fatalf("derived view still over budget: %d > %d", len(msgs), e.MaxMessages)
	}
	// [summary, recent tail]. The summary is first and system-role.
	if msgs[0].Role != hx.RoleSystem || !strings.HasPrefix(text(msgs[0]), "[compacted ") {
		t.Fatalf("summary not at the front: role=%s text=%q", msgs[0].Role, text(msgs[0]))
	}
	// keep = MaxMessages-1 = 4 recent messages retained: m8,m9,m10,m11.
	wantTail := []string{"m8", "m9", "m10", "m11"}
	if got := len(msgs) - 1; got != len(wantTail) {
		t.Fatalf("retained tail size: got %d want %d (%v)", got, len(wantTail), textsOf(msgs))
	}
	for i, w := range wantTail {
		if text(msgs[i+1]) != w {
			t.Fatalf("tail[%d]: got %q want %q", i, text(msgs[i+1]), w)
		}
	}

	// Supersession never deletes: 12 originals + 1 summary event are all in the log.
	if got := len(sess.Events()); got != 13 {
		t.Fatalf("Events() must retain the whole log (12 + summary = 13); got %d", got)
	}

	// Re-derive after compaction is stable and identical.
	again := sess.DeriveMessages()
	if len(again) != len(msgs) {
		t.Fatalf("re-derive changed the view: %d vs %d", len(again), len(msgs))
	}

	// Now under budget → idempotent no-op, log unchanged.
	compacted2, err := e.MaybeCompact(context.Background(), sess, 0)
	if err != nil {
		t.Fatal(err)
	}
	if compacted2 {
		t.Fatal("expected no compaction once under budget (idempotent)")
	}
	if got := len(sess.Events()); got != 13 {
		t.Fatalf("no-op MaybeCompact must not append: got %d want 13", got)
	}
}

// TestUnderBudgetIdempotent: below budget, MaybeCompact never touches the session.
func TestUnderBudgetIdempotent(t *testing.T) {
	sess := newSession(t)
	e := &compaction.Engine{MaxMessages: 10, MaxTokens: 10_000}
	for i := 0; i < 3; i++ {
		appendMsg(t, sess, "hello")
	}
	for round := 0; round < 2; round++ {
		compacted, err := e.MaybeCompact(context.Background(), sess, 0)
		if err != nil {
			t.Fatal(err)
		}
		if compacted {
			t.Fatalf("round %d: compacted while under budget", round)
		}
	}
	if got := len(sess.Events()); got != 3 {
		t.Fatalf("Events() changed under budget: got %d want 3", got)
	}
	if got := len(sess.DeriveMessages()); got != 3 {
		t.Fatalf("derived view changed under budget: got %d want 3", got)
	}
}

// TestOverBudgetByChars: within the count budget but past the char budget, the
// engine still compacts (the token estimate is respected, not just the count).
func TestOverBudgetByChars(t *testing.T) {
	sess := newSession(t)
	e := &compaction.Engine{MaxMessages: 100, MaxTokens: 10}
	big := strings.Repeat("x", 20) // 20 chars each
	for i := 0; i < 5; i++ {       // 100 chars total, over the 50-char budget
		appendMsg(t, sess, big)
	}

	compacted, err := e.MaybeCompact(context.Background(), sess, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !compacted {
		t.Fatal("expected compaction when over the char budget")
	}
	msgs := sess.DeriveMessages()
	if len(msgs) >= 5 {
		t.Fatalf("derived view did not shrink: got %d", len(msgs))
	}
	if msgs[0].Role != hx.RoleSystem || !strings.HasPrefix(text(msgs[0]), "[compacted ") {
		t.Fatalf("summary not at the front: %q", text(msgs[0]))
	}
	if got := len(sess.Events()); got != 6 {
		t.Fatalf("Events() must retain all 5 originals + summary = 6; got %d", got)
	}
}

// TestCustomSummarizerUsed: a supplied Summarize replaces the degraded default and
// its text lands verbatim in the front summary message.
func TestCustomSummarizerUsed(t *testing.T) {
	sess := newSession(t)
	calls := 0
	e := &compaction.Engine{
		MaxMessages: 3,
		MaxTokens:   1_000_000,
		Summarize: func(_ context.Context, span []hx.Message) (string, error) {
			calls++
			return "CUSTOM-SUMMARY-OF-" + string(rune('0'+len(span))), nil
		},
	}
	for i := 0; i < 8; i++ {
		appendMsg(t, sess, "m")
	}
	compacted, err := e.MaybeCompact(context.Background(), sess, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !compacted {
		t.Fatal("expected compaction")
	}
	if calls != 1 {
		t.Fatalf("custom Summarize called %d times, want 1", calls)
	}
	if got := text(sess.DeriveMessages()[0]); !strings.HasPrefix(got, "CUSTOM-SUMMARY-OF-") {
		t.Fatalf("custom summary text not used: %q", got)
	}
}

// TestDefaultSummarizerContent: the degraded summarizer names the count and quotes
// the first/last of the superseded span — deterministic, no model, no clock.
func TestDefaultSummarizerContent(t *testing.T) {
	sess := newSession(t)
	e := &compaction.Engine{MaxMessages: 4, MaxTokens: 1_000_000}
	appendMsg(t, sess, "FIRST") // oldest — the first line of the compacted span
	for i := 0; i < 6; i++ {
		appendMsg(t, sess, "mid")
	}
	for i := 0; i < 3; i++ { // the recent tail (keep = MaxMessages-1 = 3)
		appendMsg(t, sess, "recent")
	}
	if _, err := e.MaybeCompact(context.Background(), sess, 0); err != nil {
		t.Fatal(err)
	}
	summary := text(sess.DeriveMessages()[0])
	if !strings.HasPrefix(summary, "[compacted ") {
		t.Fatalf("default summary format unexpected: %q", summary)
	}
	if !strings.Contains(summary, "\"FIRST\"") {
		t.Fatalf("default summary should quote the first compacted line: %q", summary)
	}
}

// TestExactTokenBudgetOverheadAware: the model-reported prompt_tokens (not the
// char estimate) drives the trigger, and the keep-budget accounts for the fixed
// overhead (system prompt + tool schemas) so history is actually shrunk.
func TestExactTokenBudgetOverheadAware(t *testing.T) {
	build := func() hx.Session {
		s := newSession(t)
		for i := 0; i < 6; i++ {
			appendMsg(t, s, strings.Repeat("x", 40)) // 40 chars ≈ 10 tokens each; ~60 tokens history
		}
		return s
	}

	// Exact prompt = 300 tokens: ~240 fixed overhead + ~60 history, over the 280
	// budget → compacts, shrinking history to fit alongside the overhead.
	if compacted, err := (&compaction.Engine{MaxTokens: 280}).
		MaybeCompact(context.Background(), build(), 300); err != nil || !compacted {
		t.Fatalf("expected compaction when the EXACT prompt (300) exceeds the budget (280): compacted=%v err=%v", compacted, err)
	}

	// The SAME history with no exact count (estimate ≈ 60 tokens) is well under 280
	// — so it was the exact count, not the estimate, that triggered above.
	if compacted, _ := (&compaction.Engine{MaxTokens: 280}).
		MaybeCompact(context.Background(), build(), 0); compacted {
		t.Fatal("estimate-only (~60 tokens) must not exceed the 280 budget — proves the exact count drove the trigger")
	}
}

// TestQualityAwareKeepsImportantOverRecent: with a Score set, compaction keeps a
// high-scored OLD message and drops trivial NEWER ones — recency alone could not.
func TestQualityAwareKeepsImportantOverRecent(t *testing.T) {
	sess := newSession(t)
	appendMsg(t, sess, "IMPORTANT") // m0 — oldest, but high-scored: must survive
	for i := 0; i < 5; i++ {
		appendMsg(t, sess, "trivial"+strconv.Itoa(i)) // m1..m5 — newer, low-scored
	}
	e := &compaction.Engine{
		MaxTokens: 8, // tiny budget: only a couple messages fit
		Score: func(m hx.Message) int {
			if strings.Contains(text(m), "IMPORTANT") {
				return 10
			}
			return 0
		},
	}

	compacted, err := e.MaybeCompact(context.Background(), sess, 0)
	if err != nil || !compacted {
		t.Fatalf("expected quality-aware compaction: compacted=%v err=%v", compacted, err)
	}

	derived := sess.DeriveMessages()
	got := textsOf(derived)
	// The high-scored OLD message survives despite its age.
	found := false
	for _, s := range got {
		if s == "IMPORTANT" {
			found = true
		}
	}
	if !found {
		t.Fatalf("quality-aware compaction dropped the high-scored old message: %v", got)
	}
	// A trivial NEWER message (m1) was compacted away — recency did not save it.
	for _, s := range got {
		if s == "trivial0" {
			t.Fatalf("a low-scored newer message survived (recency, not quality): %v", got)
		}
	}
	// Summary at the front; the derived view shrank; the log retains everything.
	if len(derived) == 0 || derived[0].Role != hx.RoleSystem || !strings.HasPrefix(text(derived[0]), "[compacted ") {
		t.Fatalf("no compaction summary at the front: %v", got)
	}
	if len(sess.Events()) != 7 { // 6 originals + 1 summary
		t.Fatalf("log must retain all events (6 + summary = 7); got %d", len(sess.Events()))
	}
}

func textsOf(msgs []hx.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = text(m)
	}
	return out
}
