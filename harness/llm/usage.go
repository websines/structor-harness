package llm

import (
	"math"

	"github.com/websines/structor-harness/harness/hx"
)

// UsageSummary includes every provider-reported call. Missing details leave the
// corresponding aggregate unknown, including the cache-read fraction.
type UsageSummary struct {
	ReportedCalls     int      `json:"reported_calls"`
	InputTokens       int      `json:"input_tokens"`
	OutputTokens      int      `json:"output_tokens"`
	CachedInputTokens *int     `json:"cached_input_tokens,omitempty"`
	CacheWriteTokens  *int     `json:"cache_write_tokens,omitempty"`
	ReasoningTokens   *int     `json:"reasoning_tokens,omitempty"`
	CacheReadFraction *float64 `json:"cache_read_fraction,omitempty"`
}

func SummarizeUsage(calls []hx.Usage) UsageSummary {
	result := UsageSummary{ReportedCalls: len(calls)}
	reads, writes, reasoning := 0, 0, 0
	readsKnown, writesKnown, reasoningKnown := len(calls) > 0, len(calls) > 0, len(calls) > 0
	for _, call := range calls {
		result.InputTokens += call.InputTokens
		result.OutputTokens += call.OutputTokens
		if call.CachedInputTokens == nil {
			readsKnown = false
		} else {
			reads += *call.CachedInputTokens
		}
		if call.CacheWriteTokens == nil {
			writesKnown = false
		} else {
			writes += *call.CacheWriteTokens
		}
		if call.ReasoningTokens == nil {
			reasoningKnown = false
		} else {
			reasoning += *call.ReasoningTokens
		}
	}
	if readsKnown {
		result.CachedInputTokens = &reads
		if result.InputTokens > 0 {
			fraction := float64(reads) / float64(result.InputTokens)
			result.CacheReadFraction = &fraction
		}
	}
	if writesKnown {
		result.CacheWriteTokens = &writes
	}
	if reasoningKnown {
		result.ReasoningTokens = &reasoning
	}
	return result
}

// TokenRates are operator-supplied USD prices per million tokens for the configured
// model, processing tier, and context band. No inferred or stale prices are used.
type TokenRates struct {
	Input       float64 `json:"input"`
	CachedInput float64 `json:"cached_input"`
	CacheWrite  float64 `json:"cache_write"`
	Output      float64 `json:"output"`
}

func (r TokenRates) Valid() bool {
	for _, rate := range []float64{r.Input, r.CachedInput, r.CacheWrite, r.Output} {
		if rate < 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
			return false
		}
	}
	return true
}

type CostEstimate struct {
	Rates             TokenRates `json:"rates_usd_per_million"`
	TotalUSD          float64    `json:"total_usd"`
	WithoutCachingUSD float64    `json:"without_caching_usd"`
	SavingsUSD        float64    `json:"savings_usd"`
}

// EstimateCost keeps cache writes separate from ordinary input and counts output
// (including reasoning) exactly once. Savings may be negative on a cold write.
// Unknown/inconsistent details produce no dollar estimate, not a fabricated zero.
func EstimateCost(calls []hx.Usage, rates *TokenRates) *CostEstimate {
	if rates == nil || !rates.Valid() || len(calls) == 0 {
		return nil
	}
	result := &CostEstimate{Rates: *rates}
	for _, call := range calls {
		if call.CachedInputTokens == nil || call.CacheWriteTokens == nil {
			return nil
		}
		reads, writes := *call.CachedInputTokens, *call.CacheWriteTokens
		if reads < 0 || writes < 0 || call.InputTokens < reads ||
			call.InputTokens-reads < writes || call.OutputTokens < 0 {
			return nil
		}
		ordinary := call.InputTokens - reads - writes
		result.TotalUSD += (float64(ordinary)*rates.Input + float64(reads)*rates.CachedInput +
			float64(writes)*rates.CacheWrite + float64(call.OutputTokens)*rates.Output) / 1e6
		result.WithoutCachingUSD +=
			(float64(call.InputTokens)*rates.Input + float64(call.OutputTokens)*rates.Output) / 1e6
	}
	result.SavingsUSD = result.WithoutCachingUSD - result.TotalUSD
	return result
}
