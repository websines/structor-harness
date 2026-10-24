package improve

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"sync"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/llm"
)

var ErrBudget = errors.New("improve: budget exhausted")

type Limits struct {
	Input  int64   `json:"input"`
	Output int64   `json:"output"`
	USD    float64 `json:"usd"`
	Calls  int     `json:"calls"`
}
type Rates struct {
	Input          float64         `json:"input"`
	Output         float64         `json:"output"`
	Actual         *llm.TokenRates `json:"actual,omitempty"`
	ActualMaxInput int             `json:"actualMaxInput,omitempty"`
}
type Spending struct {
	Input        int64             `json:"input"`
	Output       int64             `json:"output"`
	USD          float64           `json:"usd"`
	Calls        int               `json:"calls"`
	Unknown      int               `json:"unknown"`
	CostEstimate *llm.CostEstimate `json:"costEstimate,omitempty"`
	Models       []string          `json:"models,omitempty"`
}
type Budget struct {
	mu          sync.Mutex
	limits      Limits
	rates       Rates
	spending    Spending
	usage       []hx.Usage
	unknownCost bool
	exhausted   bool
}
type reservation struct {
	input, output int64
	usd           float64
}

func NewBudget(limits Limits, rates Rates) (*Budget, error) {
	if limits.Input < 1 || limits.Output < 1 || limits.Calls < 1 || limits.USD <= 0 || !finite(limits.USD) || rates.Input < 0 || rates.Output < 0 || !finite(rates.Input) || !finite(rates.Output) {
		return nil, errors.New("improve: invalid budget/rates")
	}
	if rates.Actual != nil && (!rates.Actual.Valid() || rates.Actual.Input > rates.Input || rates.Actual.CacheWrite > rates.Input || rates.Actual.CachedInput > rates.Input || rates.Actual.Output > rates.Output) {
		return nil, errors.New("improve: actual pricing exceeds reservation rates")
	}
	return &Budget{limits: limits, rates: rates}, nil
}
func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }
func (b *Budget) reserve(input, output int64) (reservation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := reservation{input: input, output: output, usd: (float64(input)*b.rates.Input + float64(output)*b.rates.Output) / 1e6}
	s := b.spending

	if b.exhausted || input < 1 || output < 1 || s.Input+input > b.limits.Input || s.Output+output > b.limits.Output || s.USD+r.usd > b.limits.USD || s.Calls >= b.limits.Calls {
		b.exhausted = true
		return r, ErrBudget
	}
	b.spending.Input += input
	b.spending.Output += output
	b.spending.USD += r.usd
	b.spending.Calls++
	return r, nil
}
func (b *Budget) refund(r reservation) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spending.Input -= r.input
	b.spending.Output -= r.output
	b.spending.USD -= r.usd
	b.spending.Calls--
}
func (b *Budget) settle(r reservation, u *hx.Usage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if u == nil || u.InputTokens < 0 || u.OutputTokens < 0 {
		b.spending.Unknown++
		b.unknownCost = true
		return
	}
	b.usage = append(b.usage, *u)
	if b.rates.ActualMaxInput > 0 && u.InputTokens > b.rates.ActualMaxInput {
		b.unknownCost = true
	}
	b.spending.Input += int64(u.InputTokens) - r.input
	b.spending.Output += int64(u.OutputTokens) - r.output
	b.spending.USD += (float64(u.InputTokens)*b.rates.Input+float64(u.OutputTokens)*b.rates.Output)/1e6 - r.usd
}
func (b *Budget) Snapshot() Spending {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.spending
	models := map[string]bool{}
	for _, call := range b.usage {
		if call.Model != "" {
			models[call.Model] = true
		}
	}
	for model := range models {
		s.Models = append(s.Models, model)
	}
	sort.Strings(s.Models)
	if !b.unknownCost {
		s.CostEstimate = llm.EstimateCost(b.usage, b.rates.Actual)
	}
	return s
}

// Restore admits only retained spending, before any resumed model work.
func (b *Budget) Restore(s Spending) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s.Input < 0 || s.Output < 0 || s.Calls < 0 || s.Unknown < 0 || s.USD < 0 || !finite(s.USD) {
		return errors.New("improve: invalid retained spending")
	}
	b.spending.Input += s.Input
	b.spending.Output += s.Output
	b.spending.Calls += s.Calls
	b.spending.Unknown += s.Unknown
	b.spending.USD += s.USD
	b.unknownCost = true // restored global totals are reservations, not call detail
	return nil
}

func (b *Budget) Exhausted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.spending
	l := b.limits
	return b.exhausted || s.Input >= l.Input || s.Output >= l.Output || s.Calls >= l.Calls || s.USD >= l.USD
}

// Guard wraps every model call, including compaction and editors. Reservations
// include serialized prompt/tool bytes as a conservative input token bound.
// Missing provider usage retains the reservation; it is never recorded as zero.
type Guard struct {
	Adapter   hx.LlmAdapter
	Budgets   []*Budget
	MaxOutput int
	wg        sync.WaitGroup
}

// Wait joins accounting for all streams after the trial stops dispatching.
func (g *Guard) Wait() { g.wg.Wait() }

func (g *Guard) Route() string { return g.Adapter.Route() }
func (g *Guard) Stream(ctx context.Context, opts hx.GenerateOptions) (<-chan hx.StreamChunk, error) {
	if g.Adapter == nil || len(g.Budgets) == 0 || g.MaxOutput < 1 {
		return nil, errors.New("improve: call guard not configured")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.MaxTokens == 0 || opts.MaxTokens > g.MaxOutput {
		opts.MaxTokens = g.MaxOutput
	}
	data, err := json.Marshal(opts)
	if err != nil {
		return nil, err
	}
	reservations := make([]reservation, 0, len(g.Budgets))
	for i, b := range g.Budgets {
		r, reserveErr := b.reserve(int64(len(data)+1024), int64(opts.MaxTokens))
		if reserveErr != nil {
			for j := 0; j < i; j++ {
				g.Budgets[j].refund(reservations[j])
			}
			return nil, reserveErr
		}
		reservations = append(reservations, r)
	}
	stream, err := g.Adapter.Stream(ctx, opts)
	if err != nil {
		for i, b := range g.Budgets {
			b.settle(reservations[i], nil)
		}
		return nil, err
	}
	out := make(chan hx.StreamChunk)
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		defer close(out)
		var usage *hx.Usage
		defer func() {
			for i, b := range g.Budgets {
				b.settle(reservations[i], usage)
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case c, ok := <-stream:
				if !ok {
					return
				}
				if c.Kind == hx.ChunkUsage && c.Usage != nil {
					copy := *c.Usage
					usage = &copy
				}
				select {
				case out <- c:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}
