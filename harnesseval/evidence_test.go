package harnesseval

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/native"
)

func TestRenderTranscriptRetainsBehaviorAndDeclaresTruncation(t *testing.T) {
	result := native.Result{Text: "done"}
	for i := 0; i < 1000; i++ {
		result.Events = append(result.Events, hx.SessionEvent{Seq: i, Kind: hx.EvAssistantChunk, Data: []byte(strings.Repeat("x", 100))})
	}
	for i := 1000; i < 1080; i++ {
		payload, _ := json.Marshal(hx.ContentBlock{ToolName: "read_file", ToolArgs: []byte(`{"path":"main.go"}`)})
		result.Events = append(result.Events, hx.SessionEvent{Seq: i, Kind: hx.EvToolCall, Data: payload})
	}
	data, _ := json.Marshal(result)
	rendered, err := RenderTranscript(data)
	if err != nil {
		t.Fatal(err)
	}
	var view transcriptView
	if err = json.Unmarshal(rendered, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Events) != 64 || view.Omitted != 16 || view.OriginalEvents != 1080 || !strings.Contains(view.Events[0].Text, "main.go") || view.Events[63].Seq != 1079 {
		t.Fatalf("bad evidence view: %+v", view)
	}
}
