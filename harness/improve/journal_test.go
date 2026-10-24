package improve

import (
	"testing"
)

func TestJournalOriginalOperationIsImmutable(t *testing.T) {
	j := testJournal(t)
	d, err := j.Record(t.Context(), "stage-intent", map[string]string{"task": "one"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = j.Record(t.Context(), "stage-intent", map[string]string{"task": "two"}); err == nil {
		t.Fatal("replaced original intent")
	}
	var value map[string]string
	got, err := j.Read("stage-intent", &value)
	if err != nil || got != d || value["task"] != "one" {
		t.Fatalf("read %v %v", value, err)
	}
}
