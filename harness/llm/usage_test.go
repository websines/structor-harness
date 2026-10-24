package llm

import (
	"math"
	"strings"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
)

func tokenCount(n int) *int { return &n }

func TestUsageDetailsSurviveSSEAndRemainUnknownWhenAbsent(t *testing.T) {
	for _, tc := range []struct {
		name, details string
		known         bool
	}{
		{name: "reported", details: `,"prompt_tokens_details":{"cached_tokens":900,"cache_write_tokens":100},"completion_tokens_details":{"reasoning_tokens":3}`, known: true},
		{name: "legacy absent"},
		{name: "null", details: `,"prompt_tokens_details":null,"completion_tokens_details":null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sse := `data: {"choices":[],"usage":{"prompt_tokens":1200,"completion_tokens":10` + tc.details + "}}\n\ndata: [DONE]\n\n"
			var got hx.Usage
			_, err := scanSSE(strings.NewReader(sse), func(chunk hx.StreamChunk) bool {
				if chunk.Usage != nil {
					got = *chunk.Usage
				}
				return true
			})
			if err != nil || got.InputTokens != 1200 || got.OutputTokens != 10 {
				t.Fatalf("usage lost: %+v %v", got, err)
			}
			if tc.known {
				if got.CachedInputTokens == nil || *got.CachedInputTokens != 900 ||
					got.CacheWriteTokens == nil || *got.CacheWriteTokens != 100 ||
					got.ReasoningTokens == nil || *got.ReasoningTokens != 3 {
					t.Fatalf("cache or reasoning accounting lost: %+v", got)
				}
			} else if got.CachedInputTokens != nil || got.CacheWriteTokens != nil || got.ReasoningTokens != nil {
				t.Fatal("missing details reported as measured zero")
			}
		})
	}
}

func TestSummarizeUsageIncludesColdWritesAndAuxiliaryCalls(t *testing.T) {
	calls := []hx.Usage{
		{InputTokens: 1000, OutputTokens: 100, CachedInputTokens: tokenCount(0), CacheWriteTokens: tokenCount(1000)},
		{InputTokens: 1200, OutputTokens: 100, CachedInputTokens: tokenCount(1000), CacheWriteTokens: tokenCount(200)},
		{InputTokens: 800, OutputTokens: 80, CachedInputTokens: tokenCount(0), CacheWriteTokens: tokenCount(0)},
	}
	summary := SummarizeUsage(calls)
	if summary.ReportedCalls != 3 || summary.InputTokens != 3000 || summary.OutputTokens != 280 ||
		summary.CachedInputTokens == nil || *summary.CachedInputTokens != 1000 ||
		summary.CacheWriteTokens == nil || *summary.CacheWriteTokens != 1200 ||
		summary.ReasoningTokens != nil || summary.CacheReadFraction == nil ||
		math.Abs(*summary.CacheReadFraction-1.0/3) > 1e-9 {
		t.Fatalf("incorrect aggregate: %+v", summary)
	}
	calls = append(calls, hx.Usage{InputTokens: 100})
	unknown := SummarizeUsage(calls)
	if unknown.CachedInputTokens != nil || unknown.CacheWriteTokens != nil || unknown.CacheReadFraction != nil ||
		unknown.InputTokens != 3100 {
		t.Fatal("partial measurements presented as complete totals")
	}
}

func TestEstimateCostAccountsForWritesOutputAndNegativeSavings(t *testing.T) {
	rates := &TokenRates{Input: 2, CachedInput: 0.2, CacheWrite: 2.5, Output: 10}
	call := hx.Usage{InputTokens: 1000, OutputTokens: 100, CachedInputTokens: tokenCount(0), CacheWriteTokens: tokenCount(1000)}
	cold := EstimateCost([]hx.Usage{call}, rates)
	if cold == nil || math.Abs(cold.TotalUSD-0.0035) > 1e-9 || math.Abs(cold.SavingsUSD+0.0005) > 1e-9 {
		t.Fatalf("cold write premium hidden or double counted: %+v", cold)
	}
	call.CachedInputTokens, call.CacheWriteTokens = tokenCount(800), tokenCount(100)
	call.ReasoningTokens = tokenCount(50)
	warm := EstimateCost([]hx.Usage{call}, rates)
	if warm == nil || math.Abs(warm.TotalUSD-0.00161) > 1e-9 || math.Abs(warm.SavingsUSD-0.00139) > 1e-9 {
		t.Fatalf("cache/output/ordinary input accounted incorrectly: %+v", warm)
	}
	for _, tc := range []struct {
		name  string
		usage hx.Usage
		rates *TokenRates
	}{
		{name: "missing rates", usage: call},
		{name: "missing usage", usage: hx.Usage{InputTokens: 1000}, rates: rates},
		{name: "overcount", usage: hx.Usage{InputTokens: 100, CachedInputTokens: tokenCount(80), CacheWriteTokens: tokenCount(30)}, rates: rates},
		{name: "negative rate", usage: call, rates: &TokenRates{Input: -1}},
		{name: "NaN", usage: call, rates: &TokenRates{Input: math.NaN()}},
		{name: "infinity", usage: call, rates: &TokenRates{Input: math.Inf(1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if EstimateCost([]hx.Usage{tc.usage}, tc.rates) != nil {
				t.Fatal("fabricated a dollar estimate from invalid or incomplete data")
			}
		})
	}
}
