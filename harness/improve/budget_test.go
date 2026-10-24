package improve

import (
	"sync"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/llm"
	"math"
)

func TestBudgetConcurrentReservations(t *testing.T) {
	b, err := NewBudget(Limits{Input: 1000, Output: 1000, USD: 1, Calls: 10}, Rates{Input: 1, Output: 1})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() { _, _ = b.reserve(100, 100) })
	}
	wg.Wait()
	s := b.Snapshot()
	if s.Calls != 10 || s.Input != 1000 || s.Output != 1000 {
		t.Fatalf("overspent %+v", s)
	}
}

func TestBudgetDetailedCostAndResolvedModel(t *testing.T) {
	b, err := NewBudget(Limits{Input: 100000, Output: 100000, USD: 5, Calls: 10}, Rates{Input: 1.25, Output: 5, Actual: &llm.TokenRates{Input: 1, CachedInput: .1, CacheWrite: 1.25, Output: 5}, ActualMaxInput: 10000})
	if err != nil {
		t.Fatal(err)
	}
	r, err := b.reserve(2000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	reads, writes := 500, 100
	b.settle(r, &hx.Usage{Model: "resolved-model", InputTokens: 1000, OutputTokens: 100, CachedInputTokens: &reads, CacheWriteTokens: &writes})
	s := b.Snapshot()
	if s.CostEstimate == nil || math.Abs(s.CostEstimate.TotalUSD-.001075) > 1e-12 || len(s.Models) != 1 || s.Models[0] != "resolved-model" {
		t.Fatalf("incomplete billing metadata: %+v", s)
	}
}
func TestGuardRetainsUnknownUsage(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(map[bool]string{true: "known", false: "unknown"}[known], func(t *testing.T) {
			b, _ := NewBudget(Limits{Input: 100000, Output: 1000, USD: 1, Calls: 2}, Rates{Input: 1, Output: 1})
			chunks := []hx.StreamChunk{}
			if known {
				chunks = append(chunks, hx.StreamChunk{Kind: hx.ChunkUsage, Usage: &hx.Usage{InputTokens: 25, OutputTokens: 5}})
			}
			chunks = append(chunks, hx.StreamChunk{Kind: hx.ChunkFinish, Finish: hx.FinishStop})
			g := Guard{Adapter: &llm.FakeAdapter{Script: [][]hx.StreamChunk{chunks}}, Budgets: []*Budget{b}, MaxOutput: 100}
			stream, err := g.Stream(t.Context(), hx.GenerateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for range stream {
			}
			s := b.Snapshot()
			if known && (s.Input != 25 || s.Output != 5 || s.Unknown != 0) {
				t.Fatalf("usage %+v", s)
			}
			if !known && (s.Input < 1024 || s.Output != 100 || s.Unknown != 1) {
				t.Fatalf("missing usage became zero %+v", s)
			}
		})
	}
}
