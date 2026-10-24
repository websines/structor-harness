package llm

import (
	"strings"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
)

func TestReviewSSERequiresTerminalMarker(t *testing.T) {
	partial := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"write_file","arguments":"{}"}}]}}]}` + "\n\n"
	finish, err := scanSSE(strings.NewReader(partial), func(hx.StreamChunk) bool { return true })
	if err == nil && finish != hx.FinishError && finish != hx.FinishAborted {
		t.Fatalf("stream ended without finish_reason or [DONE] but was accepted: finish=%q err=%v", finish, err)
	}
}
