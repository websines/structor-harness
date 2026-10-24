package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
)

type openAIParameterTransport func(*http.Request) (*http.Response, error)

func (f openAIParameterTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestOpenAIModelRequestParameters(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, endpoint, model string
		luna                  bool
	}{
		{"official Luna", "https://api.openai.com/v1", "gpt-6-luna", true},
		{"official Luna snapshot", "https://api.openai.com/v1/", "gpt-6-luna-fixture-snapshot", true},
		{"legacy official model", "https://api.openai.com/v1", "legacy-fixture", false},
		{"compatible provider", "https://provider.example/v1", "gpt-6-luna", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			adapter := &OpenAIAdapter{BaseURL: tc.endpoint, DefaultModel: tc.model, APIKey: "fixture-key", HTTP: &http.Client{Transport: openAIParameterTransport(func(req *http.Request) (*http.Response, error) {
				called = true
				var body map[string]any
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body["model"] != tc.model || req.Header.Get("Authorization") != "Bearer fixture-key" {
					t.Fatal("wrong request identity")
				}
				if tc.luna {
					if body["reasoning_effort"] != "none" || body["max_completion_tokens"] != float64(32) || body["max_tokens"] != nil {
						t.Fatal("official Luna tool-call parameters unsupported")
					}
				} else if body["max_tokens"] != float64(32) || body["reasoning_effort"] != nil || body["max_completion_tokens"] != nil {
					t.Fatal("unrelated compatible request shape changed")
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))}, nil
			})}}
			stream, err := adapter.Stream(context.Background(), hx.GenerateOptions{MaxTokens: 32})
			if err != nil {
				t.Fatal(err)
			}
			for range stream {
			}
			if !called {
				t.Fatal("request was not dispatched")
			}
		})
	}
}
