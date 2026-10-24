// Package context assembles Forge's distinctive per-turn context: a CITED block
// derived from the append-only record and injected into the agent's system prompt
// every turn (forge-harness-design-v0.1.md §2.1/§6, system design §10.4). Unlike a
// static preamble, this block is recomputed on every Assemble, so it is:
//
//   - cited        — every line resolves to a real fact id / path@commit, never a
//     free-text snippet; the block only carries facts admissible as proof
//     (canonical-reached AND active), so nothing inferred is presented as verified.
//   - active-first — only governing (active) facts appear; superseded / audit-only
//     facts are excluded by the supersession resolution over the record.
//   - never-stale  — the store is re-projected on every render, so a supersession
//     appended between turns drops the fact from the NEXT assembled prompt.
//
// The retrieval itself is the no-vector, cited pipeline in internal/query (BM25 →
// centrality → expansion → supersession-sort); this package formats its result as
// the "Verified context (from the record)" block and wires it onto the harness.
//
// Like every other spine consumer, the wiring stays decoupled from the prompt
// package: it resolves hx.KeyPrompt and registers through an OPTIONAL interface the
// concrete SystemPrompt satisfies (its Context method), never importing prompt.
package context

import (
	"fmt"
	"sort"
	"strings"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/query"
	"github.com/websines/structor-harness/runtime"
)

const (
	// DefaultHeading opens the injected block. It names the block's provenance —
	// the record — so the model reads it as verified, cited context, not free chat.
	DefaultHeading = "Verified context (from the record):"
	// DefaultLimit caps how many facts the block carries, matching query's default.
	DefaultLimit = 10
	// DefaultOrder places the block among the prompt's static sections. It sits
	// after the persona/instruction preamble (low orders) and before late-order
	// trailers, so verified context frames the task without displacing the rules.
	DefaultOrder = 50
)

// Provider builds the verified-context block from a flg.Store and a query index.
// The index is the retrieval graph (itself a store projection — swappable for the
// recursive-CTE backend behind the same port); the store is consulted live for the
// supersession resolution, which is what keeps the block never-stale. A non-empty
// Query narrows the block to the facts most relevant to the current work via the
// no-vector retrieval pipeline; the empty query lists the whole active-governing
// set. Either way, only facts admissible as proof (active + canonical-reached) are
// cited — the block never presents an inferred or superseded fact as verified.
type Provider struct {
	store   *flg.Store
	index   query.GraphIndexPort
	query   string
	queryFn func() string // dynamic per-turn query source; when set, ranking is LIVE
	limit   int
	heading string
}

// NewProvider returns a Provider over store and index with the default heading,
// limit and an empty query (the full active-governing set).
func NewProvider(store *flg.Store, index query.GraphIndexPort) *Provider {
	return &Provider{store: store, index: index, limit: DefaultLimit, heading: DefaultHeading}
}

// WithQuery narrows the block to facts relevant to q — the current work — using
// the no-vector retrieval pipeline (route → BM25 → centrality → expansion). The
// empty query (the default) lists the full active set instead. Returns the
// provider for chaining.
func (p *Provider) WithQuery(q string) *Provider { p.query = q; return p }

// WithQuerySource makes retrieval LIVE and RANKED: fn is called every render to
// get the current work (the task / latest turn), and the retrieval index is
// rebuilt from the record each render so freshly-appended facts are retrievable.
// This is what activates the BM25 + centrality ranking instead of the unranked
// active-facts dump — the record actually functioning as ranked memory. A nil fn
// (the default) leaves the static, frozen-index behaviour.
func (p *Provider) WithQuerySource(fn func() string) *Provider { p.queryFn = fn; return p }

// WithLimit caps the number of cited facts in the block (<=0 restores the default).
func (p *Provider) WithLimit(n int) *Provider {
	if n <= 0 {
		n = DefaultLimit
	}
	p.limit = n
	return p
}

// Render produces the cited context block. It is the func registered as the
// prompt's dynamic Context provider, so it runs once per Assemble: it re-projects
// the store for the live supersession facts, retrieves the relevant active facts,
// and formats each as `- <summary> [<citation>]`. An empty result yields the empty
// string (no block injected) rather than a bare heading. A projection failure is
// surfaced loudly — a context that cannot be produced is never silently dropped.
func (p *Provider) Render() (string, error) {
	_, _, sups, err := p.store.Project()
	if err != nil {
		return "", fmt.Errorf("context: project store: %w", err)
	}

	// Effective query + index. With a dynamic query source, retrieval goes LIVE:
	// the query is the current work and the index is rebuilt from the record this
	// render, so ranking (BM25 → centrality → expansion) runs over fresh facts
	// instead of the unranked active-facts dump.
	idx, q := p.index, p.query
	if p.queryFn != nil {
		q = strings.TrimSpace(p.queryFn())
		if fresh, ferr := query.FromStore(p.store, nil); ferr == nil {
			idx = fresh
		}
	}

	items := p.relevant(idx, q, sups)
	// Keep only facts admissible as proof: active AND reached over canonical edges.
	// This is what makes the block "verified" — an inferred hop or a superseded /
	// ambiguous state disqualifies a fact from being cited here (§10.3).
	cited := items[:0]
	for _, it := range items {
		// Supersession facts are audit bookkeeping — the mechanism by which OTHER
		// facts drop out, not context the agent acts on. Never inject them.
		if it.Kind == query.KindSupersession {
			continue
		}
		if it.CanCiteAsProof() {
			cited = append(cited, it)
		}
	}
	if len(cited) == 0 {
		return "", nil
	}

	var b strings.Builder
	b.WriteString(p.heading)
	for _, it := range cited {
		b.WriteString("\n- ")
		b.WriteString(p.summaryOf(idx, it))
		b.WriteString(" [")
		b.WriteString(it.Citation())
		b.WriteString("]")
	}
	return b.String(), nil
}

// relevant selects the active facts for the block. With a query it runs the full
// no-vector retrieval and returns the governing (active) hits; with no query — or a
// query that found no lexical hit — it returns the entire active-governing set. In
// both cases the result is active-first (superseded/audit facts never appear) and
// cited to a real source.
func (p *Provider) relevant(idx query.GraphIndexPort, q string, sups []flg.SupersessionEvidence) []query.CitedItem {
	if strings.TrimSpace(q) != "" {
		res := query.Retrieve(idx, sups, q, p.limit)
		if len(res.Governing) > 0 {
			return res.Governing
		}
	}
	return p.governingUniverse(idx, sups)
}

// governingUniverse returns a cited item for every ACTIVE node in the index. It
// resolves supersession over the whole node universe from the live store facts —
// so a node the record has superseded is dropped here, the never-stale guarantee —
// and orders the survivors by id for a deterministic, clock-free block.
func (p *Provider) governingUniverse(idx query.GraphIndexPort, sups []flg.SupersessionEvidence) []query.CitedItem {
	ids := idx.Nodes()
	universe := make([]string, len(ids))
	for i, id := range ids {
		universe[i] = string(id)
	}
	resolution := flg.ResolveSupersession(universe, sups)

	var items []query.CitedItem
	for _, id := range ids {
		st := resolution.StateOf(string(id))
		if st == "" {
			st = flg.NodeActive // no evidence ⇒ governing (§5.4)
		}
		if st != flg.NodeActive {
			continue // superseded / audit_only / ambiguous never govern
		}
		n, ok := idx.Node(id)
		if !ok {
			continue
		}
		// A governing node read straight off the record is canonical by construction.
		items = append(items, query.CitedItem{
			ID: id, Kind: n.Kind, State: flg.NodeActive,
			Prov: query.Canonical, Reasons: []string{"governing"},
			Path: n.Path, Commit: n.Commit,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	if p.limit > 0 && len(items) > p.limit {
		items = items[:p.limit]
	}
	return items
}

// summaryOf renders a one-line human summary for a fact: the node's lexical text
// (title / evidence summary), collapsed to a single line, falling back to the fact
// id when the node carries no text.
func (p *Provider) summaryOf(idx query.GraphIndexPort, it query.CitedItem) string {
	if n, ok := idx.Node(it.ID); ok {
		if s := oneLine(n.Text); s != "" {
			return s
		}
	}
	return string(it.ID)
}

// oneLine collapses any run of whitespace (newlines included) to single spaces so a
// multi-line fact summary stays one bullet in the block.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Component mounts the verified-context provider on the harness. It injects the
// SystemPrompt (hx.KeyPrompt), asserts the optional dynamic-context interface the
// concrete SystemPrompt satisfies, and registers the provider as a DYNAMIC context
// section — a revertible effect whose inverse is the registration's own disposer,
// so unloading the component withdraws the block. It provides nothing new: the
// block reaches the model through the prompt it was injected into. queryFn
// (optional) makes retrieval LIVE + RANKED: it is called each turn for the current
// work and the index is rebuilt from the record each render; nil keeps the static
// full-active-set behaviour.
func Component(store *flg.Store, index query.GraphIndexPort, queryFn func() string) *runtime.Component {
	return ComponentAt(store, index, queryFn, DefaultOrder)
}

// ComponentAt lets the native harness place changing record context after its
// stable instructions and skill catalog without changing other compositions.
func ComponentAt(store *flg.Store, index query.GraphIndexPort, queryFn func() string, order int) *runtime.Component {
	p := NewProvider(store, index)
	if queryFn != nil {
		p.WithQuerySource(queryFn)
	}
	return &runtime.Component{
		Name:   "harness/context",
		Inject: []string{hx.KeyPrompt},
		Apply: func(ctx *runtime.Context) error {
			v, err := ctx.Get(hx.KeyPrompt)
			if err != nil {
				return err
			}
			// The SystemPrompt supports dynamic context through an optional interface
			// (its concrete Context method); asserting it keeps this package decoupled
			// from the prompt package, exactly like the evidence bridge and the tools.
			dyn, ok := v.(interface {
				Context(int, func() (string, error)) (func(), error)
			})
			if !ok {
				return fmt.Errorf("context: %s does not support dynamic context providers", hx.KeyPrompt)
			}
			return ctx.Use(func() (func(), error) {
				return dyn.Context(order, p.Render)
			})
		},
	}
}
