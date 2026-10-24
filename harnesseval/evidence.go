package harnesseval

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/native"
)

type eventView struct {
	Seq       int          `json:"seq"`
	Kind      hx.EventKind `json:"kind"`
	Text      string       `json:"text"`
	Truncated bool         `json:"truncated"`
}
type transcriptView struct {
	Text           string      `json:"text"`
	Events         []eventView `json:"events"`
	OriginalEvents int         `json:"originalEvents"`
	Omitted        int         `json:"omitted"`
}

// RenderTranscript keeps complete original evidence in CAS and supplies bounded
// behavioral events to the editor instead of thousands of token-level chunks.
func RenderTranscript(data []byte) (json.RawMessage, error) {
	var execution native.Result
	if err := json.Unmarshal(data, &execution); err != nil {
		return nil, err
	}
	view := transcriptView{OriginalEvents: len(execution.Events)}
	view.Text, _ = excerpt(execution.Text, 4096)
	for _, e := range execution.Events {
		var text string
		switch e.Kind {
		case hx.EvToolCall:
			var call hx.ContentBlock
			if json.Unmarshal(e.Data, &call) != nil {
				continue
			}
			text = call.ToolName + " " + string(call.ToolArgs)
		case hx.EvToolResult:
			var message hx.Message
			if json.Unmarshal(e.Data, &message) != nil {
				continue
			}
			for _, b := range message.Content {
				text += b.ToolName + " " + b.Text + "\n"
			}
		case hx.EvTurnEnd, hx.EvStepEnd:
			text = string(e.Data)
		default:
			continue
		}
		shown, truncated := excerpt(text, 2048)
		view.Events = append(view.Events, eventView{Seq: e.Seq, Kind: e.Kind, Text: shown, Truncated: truncated})
	}
	if len(view.Events) > 64 {
		view.Omitted = len(view.Events) - 64
		view.Events = append(view.Events[:8], view.Events[len(view.Events)-56:]...)
	}
	return json.Marshal(view)
}

func RenderArtifact(data []byte) (json.RawMessage, error) {
	var source Source
	if err := json.Unmarshal(data, &source); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	view := struct {
		Files     Source   `json:"files"`
		Truncated []string `json:"truncated,omitempty"`
	}{Files: Source{}}
	remaining := 16384
	for _, key := range keys {
		text, truncated := excerpt(source[key], min(remaining, 8192))
		view.Files[key] = text
		remaining -= len(text)
		if truncated {
			view.Truncated = append(view.Truncated, key)
		}
	}
	return json.Marshal(view)
}

func excerpt(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	if limit <= 0 {
		return "", true
	}
	s = s[:limit]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return strings.TrimSpace(s), true
}
