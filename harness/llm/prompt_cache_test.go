package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
)

func captureCacheRequest(t *testing.T, adapter OpenAIAdapter, opts hx.GenerateOptions) map[string]any {
	t.Helper()
	var body map[string]any
	adapter.HTTP = &http.Client{Transport: openAIParameterTransport(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")),
		}, nil
	})}
	stream, err := adapter.Stream(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	drain(stream)
	return body
}

func decodeCacheMessages(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	encoded, err := json.Marshal(body["messages"])
	if err != nil {
		t.Fatal(err)
	}
	var result []map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPromptCachePreservesHistoryAcrossChangingContext(t *testing.T) {
	adapter := OpenAIAdapter{
		BaseURL: "https://api.openai.com/v1", DefaultModel: "gpt-6-luna", NativeCaching: true,
		CacheScope: CacheScopeKey("company", "alice"),
	}
	opts := hx.GenerateOptions{
		System:      "Shared instructions\n\nWorkspace A; current fact A",
		PromptCache: &hx.PromptCache{SystemPrefix: "Shared instructions", ReuseHistory: true},
		Messages:    []hx.Message{{Role: hx.RoleUser, Content: []hx.ContentBlock{{Kind: hx.BlockText, Text: "Task"}}}},
		Tools:       []hx.ToolSchema{{Name: "z"}, {Name: "a"}},
	}
	first := captureCacheRequest(t, adapter, opts)
	opts.System = "Shared instructions\n\nWorkspace B; current fact B"
	second := captureCacheRequest(t, adapter, opts)
	left, right := decodeCacheMessages(t, first), decodeCacheMessages(t, second)
	if len(left) != 3 || !reflect.DeepEqual(left[:2], right[:2]) {
		t.Fatalf("mutable context changed reusable prefix: %v / %v", left, right)
	}
	if left[2]["role"] != "developer" || left[2]["content"] != "\n\nWorkspace A; current fact A" ||
		right[2]["content"] != "\n\nWorkspace B; current fact B" {
		t.Fatal("current context lost, stale, or demoted below developer priority")
	}
	if first["prompt_cache_key"] != second["prompt_cache_key"] || first["prompt_cache_key"] != adapter.CacheScope {
		t.Fatal("cache scope changed between related requests")
	}
	if !reflect.DeepEqual(first["prompt_cache_options"], map[string]any{"mode": "implicit", "ttl": "30m"}) {
		t.Fatal("agent history caching or TTL missing")
	}
	parts, ok := left[0]["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatal("shared prefix is not a standalone content block")
	}
	part, ok := parts[0].(map[string]any)
	if !ok || part["text"] != "Shared instructions" ||
		!reflect.DeepEqual(part["prompt_cache_breakpoint"], map[string]any{"mode": "explicit"}) {
		t.Fatal("shared prefix breakpoint missing")
	}
	tools, ok := first["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatal("tools lost")
	}
	encoded, _ := json.Marshal(tools[0])
	if !strings.Contains(string(encoded), `"name":"a"`) || opts.Tools[0].Name != "z" {
		t.Fatal("wire tools are unstable or caller tool order was mutated")
	}
}

func TestPromptCacheProviderAndModelCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, model  string
		enabled, key, explicit bool
	}{
		{name: "modern", endpoint: "https://api.openai.com/v1/", model: "gpt-6.1-sol", enabled: true, key: true, explicit: true},
		{name: "modern snapshot", endpoint: "https://api.openai.com/v1", model: "gpt-6-luna-2026-10-01", enabled: true, key: true, explicit: true},
		{name: "5.6", endpoint: "https://api.openai.com/v1", model: "gpt-5.6-sol", enabled: true, key: true, explicit: true},
		{name: "older", endpoint: "https://api.openai.com/v1", model: "gpt-5.4", enabled: true, key: true},
		{name: "4.1", endpoint: "https://api.openai.com/v1", model: "gpt-4.1", enabled: true, key: true},
		{name: "unknown official", endpoint: "https://api.openai.com/v1", model: "future-model", enabled: true},
		{name: "compatible", endpoint: "https://proxy.example/v1", model: "gpt-6-luna", enabled: true},
		{name: "disabled", endpoint: "https://api.openai.com/v1", model: "gpt-6-luna"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := captureCacheRequest(t, OpenAIAdapter{
				BaseURL: tc.endpoint, DefaultModel: tc.model, NativeCaching: tc.enabled, CacheScope: "opaque-scope",
			}, hx.GenerateOptions{
				System: "instructions", PromptCache: &hx.PromptCache{SystemPrefix: "instructions"},
			})
			_, key := body["prompt_cache_key"]
			_, options := body["prompt_cache_options"]
			if key != tc.key || options != tc.explicit {
				t.Fatalf("unsupported/missing cache parameters: %v", body)
			}
			if body["prompt_cache_retention"] != nil {
				t.Fatal("organization retention policy was overridden")
			}
			if !tc.explicit {
				msgs := decodeCacheMessages(t, body)
				if len(msgs) != 1 || msgs[0]["role"] != "system" || msgs[0]["content"] != "instructions" {
					t.Fatal("legacy wire format changed")
				}
			} else if !reflect.DeepEqual(body["prompt_cache_options"], map[string]any{"mode": "explicit", "ttl": "30m"}) {
				t.Fatal("one-shot request writes disposable input")
			}
		})
	}
}

func TestPromptCacheKeepsLargeTaskReusableAcrossCompaction(t *testing.T) {
	adapter := OpenAIAdapter{BaseURL: "https://api.openai.com/v1", DefaultModel: "gpt-6-luna", NativeCaching: true}
	task := "immutable authorized task"
	anchor := "\n\nCurrent authorized task (retain this intent across compaction):\n" + task
	opts := hx.GenerateOptions{
		System:      "shared\n\ncurrent facts A" + anchor,
		PromptCache: &hx.PromptCache{SystemPrefix: "shared", Task: task, ReuseHistory: true},
		Messages: []hx.Message{
			{Role: hx.RoleUser, Content: []hx.ContentBlock{{Kind: hx.BlockText, Text: task}}},
			{Role: hx.RoleAssistant, Content: []hx.ContentBlock{{Kind: hx.BlockText, Text: "prior answer"}}},
		},
	}
	before := decodeCacheMessages(t, captureCacheRequest(t, adapter, opts))
	opts.System = "shared\n\ncurrent facts B" + anchor
	opts.Messages = []hx.Message{{Role: hx.RoleSystem, Content: []hx.ContentBlock{{Kind: hx.BlockText, Text: "compacted history"}}}}
	after := decodeCacheMessages(t, captureCacheRequest(t, adapter, opts))
	if len(before) != 4 || len(after) != 4 || !reflect.DeepEqual(before[:2], after[:2]) ||
		before[2]["role"] != "assistant" || before[2]["content"] != "prior answer" ||
		after[2]["content"] != "compacted history" || after[3]["content"] != "\n\ncurrent facts B" {
		t.Fatal("task not cached once, history altered, or fresh facts lost across compaction")
	}
	encoded, _ := json.Marshal(before[1])
	if !strings.Contains(string(encoded), `"prompt_cache_breakpoint":{"mode":"explicit"}`) ||
		!strings.Contains(string(encoded), task) {
		t.Fatal("immutable task breakpoint missing")
	}
	// A followup that merely starts with the task must never be removed.
	opts.Messages = []hx.Message{{Role: hx.RoleUser, Content: []hx.ContentBlock{{Kind: hx.BlockText, Text: task + " and a followup"}}}}
	followup := decodeCacheMessages(t, captureCacheRequest(t, adapter, opts))
	if followup[2]["role"] != "user" || followup[2]["content"] != task+" and a followup" {
		t.Fatal("partial-match followup removed")
	}
	// Middleware may replace the task anchor: stale metadata cannot truncate it.
	opts.System = "shared\n\nreplacement task"
	changed := decodeCacheMessages(t, captureCacheRequest(t, adapter, opts))
	if len(changed) != 3 || changed[1]["role"] != "user" || changed[2]["content"] != "\n\nreplacement task" {
		t.Fatal("stale task metadata removed actual instructions or history")
	}
}

func TestPromptCacheRejectsStalePrefixMetadata(t *testing.T) {
	body := captureCacheRequest(t, OpenAIAdapter{
		BaseURL: "https://api.openai.com/v1", DefaultModel: "gpt-6-luna", NativeCaching: true,
	}, hx.GenerateOptions{
		System:      "middleware replaced instructions",
		PromptCache: &hx.PromptCache{SystemPrefix: "old instructions", ReuseHistory: true},
	})
	msgs := decodeCacheMessages(t, body)
	if len(msgs) != 1 || msgs[0]["content"] != "middleware replaced instructions" || msgs[0]["role"] != "system" {
		t.Fatal("stale prefix truncated or replaced the actual prompt")
	}
}

func TestCacheScopeKeyIdentityAndCollisionBoundaries(t *testing.T) {
	base := CacheScopeKey("company-a", "alice")
	if base != CacheScopeKey("company-a", "alice") ||
		base == CacheScopeKey("company-b", "alice") || base == CacheScopeKey("company-a", "bob") ||
		CacheScopeKey("ab", "c") == CacheScopeKey("a", "bc") ||
		strings.Contains(base, "alice") || strings.Contains(base, "company-a") {
		t.Fatal("cache accounting identity is unstable, ambiguous, or exposed")
	}
}
