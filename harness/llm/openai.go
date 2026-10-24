package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/websines/structor-harness/harness/hx"
)

// OpenAIAdapter speaks the OpenAI chat-completions streaming protocol to ANY
// compatible URL: a LiteLLM proxy, DeepSeek, OpenAI itself, or a local vLLM /
// Ollama. Official OpenAI options are gated by endpoint/model; compatible
// endpoints retain the generic protocol. It is the "direct provider" seam of
// design §7: HTTP POST + SSE, no SDK.
type OpenAIAdapter struct {
	BaseURL      string // e.g. "https://api.openai.com/v1" — "/chat/completions" is appended
	APIKey       string // sent as "Authorization: Bearer {APIKey}" when non-empty
	DefaultModel string // used when GenerateOptions.Model is empty
	HTTP         *http.Client
	// NativeCaching opts into official OpenAI cache controls. Compatible endpoints
	// keep their existing wire format. CacheScope is opaque operator-owned identity.
	NativeCaching bool
	CacheScope    string
	// ObserveUsage receives all billed calls, including compaction requests which
	// do not flow through the agent's session chunks. The observer must be safe for
	// concurrent streams and must not block on the stream it is observing.
	ObserveUsage func(hx.GenerateOptions, hx.Usage)
}

var _ hx.LlmAdapter = (*OpenAIAdapter)(nil)

// Route is the provider id this adapter answers to.
func (a *OpenAIAdapter) Route() string { return "openai-compatible" }

// --- wire types (request) ----------------------------------------------------

type oaiRequest struct {
	Model               string            `json:"model"`
	Messages            []oaiMessage      `json:"messages"`
	Tools               []oaiTool         `json:"tools,omitempty"`
	Stream              bool              `json:"stream"`
	StreamOptions       *oaiStreamOptions `json:"stream_options,omitempty"`
	MaxTokens           int               `json:"max_tokens,omitempty"`
	MaxCompletionTokens int               `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string            `json:"reasoning_effort,omitempty"`
	PromptCacheKey      string            `json:"prompt_cache_key,omitempty"`
	PromptCacheOptions  *oaiCacheOptions  `json:"prompt_cache_options,omitempty"`
}

// oaiStreamOptions asks the endpoint to emit a final usage chunk in the stream —
// the exact prompt_tokens the model counted, which drives the compaction budget.
// The official API requires this; most compatible endpoints (LM Studio) honor it.
type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    any           `json:"content,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type oaiToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function oaiFunctionCall `json:"function"`
}

type oaiFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaiTool struct {
	Type     string          `json:"type"`
	Function oaiToolFunction `json:"function"`
}

type oaiToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// --- wire types (streamed response) ------------------------------------------

type oaiStreamChunk struct {
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
	Usage   *oaiUsage   `json:"usage"`
}

type oaiChoice struct {
	Delta        oaiDelta `json:"delta"`
	FinishReason *string  `json:"finish_reason"`
}

type oaiDelta struct {
	Content          string             `json:"content"`
	ReasoningContent string             `json:"reasoning_content"`
	ToolCalls        []oaiDeltaToolCall `json:"tool_calls"`
}

type oaiDeltaToolCall struct {
	Index    int               `json:"index"`
	ID       string            `json:"id"`
	Function *oaiDeltaFunction `json:"function"`
}

type oaiDeltaFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	PromptDetails    *struct {
		CachedTokens     *int `json:"cached_tokens"`
		CacheWriteTokens *int `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails *struct {
		ReasoningTokens *int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// --- request mapping ---------------------------------------------------------

// mapMessages projects the harness conversation onto OpenAI messages. The system
// prompt (if any) leads; then each hx.Message maps by role.
func mapMessages(system string, msgs []hx.Message) []oaiMessage {
	out := make([]oaiMessage, 0, len(msgs)+1)
	if system != "" {
		out = append(out, oaiMessage{Role: "system", Content: system})
	}
	for _, m := range msgs {
		out = append(out, mapMessage(m)...)
	}
	return out
}

// mapMessage maps one hx.Message. A tool role fans out to one OpenAI "tool"
// message per tool-result block (each keyed by the call id it answers). Any other
// role becomes a single message whose text blocks concatenate into content and
// whose tool-call blocks become tool_calls.
func mapMessage(m hx.Message) []oaiMessage {
	if m.Role == hx.RoleTool {
		var out []oaiMessage
		for _, b := range m.Content {
			if b.Kind != hx.BlockToolResult {
				continue
			}
			var content any
			if b.Text != "" {
				content = b.Text
			}
			out = append(out, oaiMessage{
				Role:       "tool",
				ToolCallID: b.ToolCallID,
				Content:    content,
			})
		}
		return out
	}

	msg := oaiMessage{Role: string(m.Role)}
	var text strings.Builder
	for _, b := range m.Content {
		switch b.Kind {
		case hx.BlockText:
			text.WriteString(b.Text)
		case hx.BlockToolCall:
			args := string(b.ToolArgs)
			if args == "" {
				args = "{}"
			}
			msg.ToolCalls = append(msg.ToolCalls, oaiToolCall{
				ID:       b.ToolCallID,
				Type:     "function",
				Function: oaiFunctionCall{Name: b.ToolName, Arguments: args},
			})
			// reasoning / image blocks are not replayed as input.
		}
	}
	if text.Len() != 0 {
		msg.Content = text.String()
	}
	return []oaiMessage{msg}
}

func mapTools(tools []hx.ToolSchema) []oaiTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]oaiTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, oaiTool{
			Type: "function",
			Function: oaiToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	return out
}

// --- Stream ------------------------------------------------------------------

// Stream issues one chat-completions call and streams the response as chunks. It
// returns a channel closed by a terminal ChunkFinish; ctx cancellation aborts the
// underlying HTTP request and yields ChunkFinish{FinishAborted}. Errors before
// the first byte are returned as a typed *hx.LlmFailure; errors mid-stream arrive
// as ChunkFinish{FinishError}.
func (a *OpenAIAdapter) Stream(ctx context.Context, opts hx.GenerateOptions) (<-chan hx.StreamChunk, error) {
	model := opts.Model
	if model == "" {
		model = a.DefaultModel
	}
	request := oaiRequest{
		Model:         model,
		Messages:      mapMessages(opts.System, opts.Messages),
		Tools:         mapTools(opts.Tools),
		Stream:        true,
		StreamOptions: &oaiStreamOptions{IncludeUsage: true},
		MaxTokens:     opts.MaxTokens,
	}
	// GPT-6 Luna's Chat Completions tool calls require reasoning_effort=none.
	// Its output budget uses max_completion_tokens. Keep compatible providers'
	// existing request shape unchanged; this applies only to the official API.
	if strings.TrimRight(a.BaseURL, "/") == "https://api.openai.com/v1" && (model == "gpt-6-luna" || strings.HasPrefix(model, "gpt-6-luna-")) {
		request.ReasoningEffort = "none"
		request.MaxCompletionTokens, request.MaxTokens = opts.MaxTokens, 0
	}
	a.applyPromptCache(&request, opts)
	body, err := json.Marshal(request)
	if err != nil {
		return nil, &hx.LlmFailure{Kind: "provider", Message: "llm: marshal request: " + err.Error()}
	}

	url := strings.TrimRight(a.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, &hx.LlmFailure{Kind: "network", Message: "llm: build request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if a.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.APIKey)
	}

	client := a.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &hx.LlmFailure{Kind: "aborted", Message: "llm: request aborted: " + ctx.Err().Error()}
		}
		return nil, &hx.LlmFailure{Kind: "network", Message: "llm: request failed: " + err.Error(), Retryable: true}
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, httpFailure(resp.StatusCode, readSnippet(resp.Body))
	}

	out := make(chan hx.StreamChunk)
	go a.pump(ctx, resp, out, opts)
	return out, nil
}

// pump owns the channel lifecycle: it scans the SSE body into chunks and emits a
// single terminal ChunkFinish before closing. ctx cancellation takes precedence
// over any scan error (an aborted request is FinishAborted, not FinishError).
func (a *OpenAIAdapter) pump(ctx context.Context, resp *http.Response, out chan<- hx.StreamChunk, opts hx.GenerateOptions) {
	defer close(out)
	defer resp.Body.Close()

	emit := func(c hx.StreamChunk) bool {
		if c.Kind == hx.ChunkUsage && c.Usage != nil && a.ObserveUsage != nil {
			a.ObserveUsage(opts, *c.Usage)
		}
		select {
		case <-ctx.Done():
			return false
		case out <- c:
			return true
		}
	}

	fin, err := scanSSE(resp.Body, emit)

	switch {
	case ctx.Err() != nil:
		trySend(ctx, out, hx.StreamChunk{Kind: hx.ChunkFinish, Finish: hx.FinishAborted})
	case err != nil:
		trySend(ctx, out, hx.StreamChunk{Kind: hx.ChunkFinish, Finish: hx.FinishError, Err: err.Error()})
	default:
		if fin == "" {
			fin = hx.FinishStop
		}
		trySend(ctx, out, hx.StreamChunk{Kind: hx.ChunkFinish, Finish: fin})
	}
}

// trySend delivers one terminal chunk, giving up if ctx is already done and the
// consumer is no longer draining (so the goroutine never leaks).
func trySend(ctx context.Context, out chan<- hx.StreamChunk, c hx.StreamChunk) {
	select {
	case out <- c:
	case <-ctx.Done():
	}
}

// --- SSE parsing -------------------------------------------------------------

// scanSSE reads an OpenAI-compatible SSE body line by line with a bufio.Scanner
// and folds each `data: {json}` payload into zero or more StreamChunks passed to
// emit. It stops at `data: [DONE]` or EOF. tool-call ids and names are
// accumulated by tool_calls index across fragments, so a later argument-only
// fragment still carries the id/name the assembler needs. It does NOT emit the
// terminal finish — it returns the finish reason it saw so the caller emits
// exactly one. emit returning false stops the scan (ctx cancelled).
//
// Only the current line is ever held in memory: the scan is O(1) in the response
// size, which is what keeps the adapter memory-bounded (design §5.5).
func scanSSE(r io.Reader, emit func(hx.StreamChunk) bool) (hx.FinishReason, error) {
	var fin hx.FinishReason
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // grow up to 1 MiB for a long data line

	ids := map[int]string{}
	names := map[int]string{}

	// Block-index allocation. The OpenAI stream separates its content streams only
	// by KIND (text vs reasoning_content) and separates tool calls by their own
	// tool_calls[].index — it never emits a single unified per-block index. The fold
	// downstream groups deltas by StreamChunk.BlockIndex, so if we left every delta
	// at the zero value a reasoning model (e.g. LFM2's reasoning_content preamble)
	// followed by a tool call would collide at index 0: the tool-call fields would
	// be folded onto the reasoning block, whose Kind stays BlockReasoning, and the
	// loop's tool-call filter would miss it. So allocate a distinct, STABLE
	// BlockIndex per logical stream (text, reasoning, each tool call) in
	// first-appearance order. This also keeps two parallel tool calls from merging.
	nextBlock := 0
	blockOf := map[string]int{}
	blockIndex := func(key string) int {
		if i, ok := blockOf[key]; ok {
			return i
		}
		i := nextBlock
		nextBlock++
		blockOf[key] = i
		return i
	}

	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue // blank separators, comments, and event: lines are ignored
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "[DONE]" {
			return fin, nil
		}

		var chunk oaiStreamChunk
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue // a malformed line is skipped, never fatal to the stream
		}

		if chunk.Usage != nil {
			usage := hx.Usage{
				Model:        chunk.Model,
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
			}
			if details := chunk.Usage.PromptDetails; details != nil {
				usage.CachedInputTokens = details.CachedTokens
				usage.CacheWriteTokens = details.CacheWriteTokens
			}
			if details := chunk.Usage.CompletionDetails; details != nil {
				usage.ReasoningTokens = details.ReasoningTokens
			}
			if !emit(hx.StreamChunk{Kind: hx.ChunkUsage, Usage: &usage}) {
				return fin, nil
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		ch := chunk.Choices[0]

		if ch.Delta.Content != "" {
			if !emit(hx.StreamChunk{Kind: hx.ChunkTextDelta, BlockIndex: blockIndex("text"), Delta: ch.Delta.Content}) {
				return fin, nil
			}
		}
		if ch.Delta.ReasoningContent != "" {
			if !emit(hx.StreamChunk{Kind: hx.ChunkReasoningDelta, BlockIndex: blockIndex("reasoning"), Delta: ch.Delta.ReasoningContent}) {
				return fin, nil
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			if tc.ID != "" {
				ids[tc.Index] = tc.ID
			}
			var args string
			if tc.Function != nil {
				if tc.Function.Name != "" {
					names[tc.Index] = tc.Function.Name
				}
				args = tc.Function.Arguments
			}
			if !emit(hx.StreamChunk{
				Kind:       hx.ChunkToolCallDelta,
				BlockIndex: blockIndex(fmt.Sprintf("tool:%d", tc.Index)),
				ToolCallID: ids[tc.Index],
				ToolName:   names[tc.Index],
				ArgsDelta:  args,
			}) {
				return fin, nil
			}
		}
		if ch.FinishReason != nil {
			fin = mapFinish(*ch.FinishReason)
		}
	}
	if err := sc.Err(); err != nil {
		return fin, err
	}
	if fin == "" {
		return fin, io.ErrUnexpectedEOF
	}
	return fin, nil
}

func mapFinish(reason string) hx.FinishReason {
	switch reason {
	case "tool_calls", "function_call":
		return hx.FinishToolCalls
	case "length":
		return hx.FinishLength
	case "stop":
		return hx.FinishStop
	default:
		return hx.FinishStop
	}
}

// --- error mapping -----------------------------------------------------------

func readSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 2048))
	return strings.TrimSpace(string(b))
}

func httpFailure(status int, body string) *hx.LlmFailure {
	kind, retryable := "provider", false
	switch {
	case status == http.StatusTooManyRequests:
		kind, retryable = "rate_limit", true
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		kind = "auth"
	case status >= 500:
		retryable = true
	}
	msg := fmt.Sprintf("llm: openai-compatible HTTP %d", status)
	if body != "" {
		msg += ": " + body
	}
	return &hx.LlmFailure{Kind: kind, Message: msg, Retryable: retryable}
}
