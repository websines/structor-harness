package evidence

import (
	"testing"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/record"
)

// TestRecordScorer proves the Forge-native compaction scorer reads the record: a
// message mentioning a still-active obligation scores HIGH (keep it), the same
// message scores LOW once that obligation is discharged (its context is settled and
// compactable), and a message mentioning no obligation is neutral (recency decides).
func TestRecordScorer(t *testing.T) {
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sink := New(cas)
	if err := sink.RegisterObligation(flg.Obligation{
		ID: "O-1", Title: "refund never exceeds capture", AcceptancePolicy: "p", DomainTags: []string{"payments"},
	}); err != nil {
		t.Fatal(err)
	}
	score := sink.Scorer()
	msg := func(text string) hx.Message {
		return hx.Message{Content: []hx.ContentBlock{{Kind: hx.BlockText, Text: text}}}
	}

	// Open obligation → mentioning it is load-bearing → score high.
	if s := score(msg("still working on O-1 here")); s <= 0 {
		t.Fatalf("an OPEN obligation's message should score high (keep), got %d", s)
	}
	// No obligation mentioned → neutral (recency decides).
	if s := score(msg("just some chatter")); s != 0 {
		t.Fatalf("a message with no obligation should be neutral, got %d", s)
	}

	// Discharge O-1 with supporting, reproducible, in-scope evidence.
	sink.record(hx.ToolExecutionInput{Initiator: "principal:a"}, hx.ToolResult{
		EvidenceHint: &hx.EvidenceHint{ObligationID: "O-1", Supports: true, Method: "executed_test", Reproducible: true, Summary: "passed"},
	})
	if v, _ := sink.Verdict("O-1"); v.State != flg.StateDischarged {
		t.Fatalf("precondition: O-1 should be discharged, got %s", v.State)
	}

	// Now the same message scores LOW — a settled obligation's discussion is
	// compactable. The scorer re-read the record, not a stale flag.
	if s := score(msg("still working on O-1 here")); s >= 0 {
		t.Fatalf("a DISCHARGED obligation's message should score low (compactable), got %d", s)
	}
}
