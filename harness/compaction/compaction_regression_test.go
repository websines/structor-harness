package compaction_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/websines/structor-harness/harness/compaction"
	"github.com/websines/structor-harness/harness/hx"
)

func TestReviewCompactionPreservesToolExchange(t *testing.T) {
	for _, strategy := range []string{"prefix", "score"} {
		for _, calls := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/%d", strategy, calls), func(t *testing.T) {
				sess := newSession(t)
				assistant := hx.Message{Role: hx.RoleAssistant}
				msgs := []hx.Message{{Role: hx.RoleUser, Content: []hx.ContentBlock{{Kind: hx.BlockText, Text: "Please inspect."}}}}
				var results []hx.Message
				for i := range calls {
					id := fmt.Sprintf("read-%d", i)
					assistant.Content = append(assistant.Content, hx.ContentBlock{Kind: hx.BlockToolCall, ToolCallID: id, ToolName: "read_file", ToolArgs: []byte("{}")})
					results = append(results, hx.Message{Role: hx.RoleTool, Content: []hx.ContentBlock{{Kind: hx.BlockToolResult, ToolCallID: id, Text: "package main"}}})
				}
				msgs = append(msgs, assistant)
				msgs = append(msgs, results...)
				msgs = append(msgs, hx.Message{Role: hx.RoleAssistant, Content: []hx.ContentBlock{{Kind: hx.BlockText, Text: "Done."}}})
				for _, msg := range msgs {
					data, err := json.Marshal(msg)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = sess.Append(hx.EvUserMessage, data, true); err != nil {
						t.Fatal(err)
					}
				}
				engine := &compaction.Engine{MaxMessages: 3, MaxTokens: 10000}
				if strategy == "score" {
					engine.Score = func(msg hx.Message) int {
						if msg.Role == hx.RoleTool {
							return 100
						}
						if msg.Content[0].Kind == hx.BlockToolCall {
							return -100
						}
						return 0
					}
				}
				compacted, err := engine.MaybeCompact(context.Background(), sess, 0)
				if err != nil || !compacted {
					t.Fatalf("compaction: %v %v", compacted, err)
				}
				pending := map[string]bool{}
				for _, msg := range sess.DeriveMessages() {
					for _, block := range msg.Content {
						if block.Kind == hx.BlockToolCall {
							pending[block.ToolCallID] = true
						}
						if block.Kind == hx.BlockToolResult {
							if !pending[block.ToolCallID] {
								t.Fatalf("orphan result %q", block.ToolCallID)
							}
							delete(pending, block.ToolCallID)
						}
					}
				}
				if len(pending) != 0 {
					t.Fatalf("missing results: %v", pending)
				}
			})
		}
	}
}
