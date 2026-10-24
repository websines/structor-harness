// Black-box test (package context_test) so it may wire the real prompt and runtime
// packages the way a surface would — the injection path end to end.
package context_test

import (
	"strings"
	"testing"

	"github.com/websines/structor-harness/flg"
	fctx "github.com/websines/structor-harness/harness/context"
	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/prompt"
	"github.com/websines/structor-harness/query"
	"github.com/websines/structor-harness/record"
	"github.com/websines/structor-harness/runtime"
)

// seedStore builds a record with three obligations (one about to be superseded),
// a piece of supporting evidence, and a supersession fact demoting the legacy
// obligation. All three obligations share the term "refund" so retrieval finds them.
func seedStore(t *testing.T) *flg.Store {
	t.Helper()
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := flg.NewStore(cas)

	for _, o := range []flg.Obligation{
		{ID: "O-1101", Title: "refund never exceeds capture", DomainTags: []string{"ledger"}},
		{ID: "O-1102", Title: "refund reconciles with ledger", DomainTags: []string{"ledger"}},
		{ID: "O-9999", Title: "legacy refund cap", DomainTags: []string{"ledger"}},
	} {
		if _, err := s.AppendObligation(o); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AppendEvidence(flg.Evidence{
		ID: "E-1", ObligationID: "O-1101",
		Polarity: flg.PolaritySupports, Method: flg.MethodExecutedTest,
		Reproducibility: flg.ReproDeterministic, Validity: flg.ValidityValid,
		Scope:       flg.Scope{Domain: "ledger", Tags: []string{"ledger"}},
		PrincipalID: "principal:marcus", Commit: "c0ffee", Summary: "property test passes",
	}); err != nil {
		t.Fatal(err)
	}
	// The legacy obligation is superseded, replaced by the active O-1101.
	if _, err := s.AppendSupersession(flg.SupersessionEvidence{
		Subject: "O-9999", Kind: flg.KindSupersededBy, ReplacedBy: []string{"O-1101"},
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func indexOf(t *testing.T, s *flg.Store) query.GraphIndexPort {
	t.Helper()
	ix, err := query.FromStore(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

// The block is cited, active-first, and never carries a superseded fact.
func TestFLGBlockIsCitedActiveFirstAndSupersededExcluded(t *testing.T) {
	s := seedStore(t)
	p := fctx.NewProvider(s, indexOf(t, s))

	block, err := p.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.HasPrefix(block, "Verified context (from the record):") {
		t.Fatalf("missing cited heading: %q", block)
	}
	// The superseded obligation MUST NOT appear; the active ones MUST.
	if strings.Contains(block, "O-9999") {
		t.Fatalf("superseded fact leaked into the block: %q", block)
	}
	if !strings.Contains(block, "O-1101") || !strings.Contains(block, "O-1102") {
		t.Fatalf("active obligations missing from the block: %q", block)
	}
	// Every bullet must carry a bracketed fact id — the citation.
	for _, line := range strings.Split(block, "\n") {
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		open, close := strings.Index(line, "["), strings.Index(line, "]")
		if open < 0 || close <= open+1 {
			t.Fatalf("cited line lacks a bracketed fact id: %q", line)
		}
	}
}

// Never-stale: superseding a fact AFTER the provider was built drops it on the very
// next Render, without rebuilding the index — the block is recomputed from the live
// record every call.
func TestFLGBlockIsNeverStale(t *testing.T) {
	s := seedStore(t)
	p := fctx.NewProvider(s, indexOf(t, s))

	if block, err := p.Render(); err != nil || !strings.Contains(block, "O-1102") {
		t.Fatalf("precondition: O-1102 should be active: err=%v block=%q", err, block)
	}

	// Append a supersession for O-1102 into the record the provider reads.
	if _, err := s.AppendSupersession(flg.SupersessionEvidence{
		Subject: "O-1102", Kind: flg.KindStatusSuperseded,
	}); err != nil {
		t.Fatal(err)
	}

	block, err := p.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(block, "O-1102") {
		t.Fatalf("never-stale broken: fact superseded after build still present: %q", block)
	}
	if !strings.Contains(block, "O-1101") {
		t.Fatalf("active fact wrongly dropped: %q", block)
	}
}

// The retrieval path narrows the block to facts relevant to the current work.
func TestFLGBlockWithQueryRetrieves(t *testing.T) {
	s := seedStore(t)
	p := fctx.NewProvider(s, indexOf(t, s)).WithQuery("refund")

	block, err := p.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(block, "O-1101") {
		t.Fatalf("retrieval missed the relevant active obligation: %q", block)
	}
	if strings.Contains(block, "O-9999") {
		t.Fatalf("retrieval surfaced a superseded fact as verified: %q", block)
	}
}

// The dynamic query source activates LIVE ranked retrieval: even when the index
// passed at construction is STALE (built from a DIFFERENT, empty store), Render
// rebuilds the index from the live record each turn and ranks by the current work,
// so the relevant active fact is retrieved and the superseded one stays excluded.
// This is the activation of the previously-dormant BM25/centrality ranking — with
// no query source the provider would just dump the active set unranked.
func TestFLGBlockLiveRankedRetrieval(t *testing.T) {
	s := seedStore(t)

	emptyCAS, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stale := indexOf(t, flg.NewStore(emptyCAS)) // knows nothing about s's facts

	p := fctx.NewProvider(s, stale).WithQuerySource(func() string { return "refund" })
	block, err := p.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(block, "O-1101") {
		t.Fatalf("live ranked retrieval missed the relevant fact — ranking not active over the record: %q", block)
	}
	if strings.Contains(block, "O-9999") {
		t.Fatalf("live retrieval surfaced a superseded fact as verified: %q", block)
	}
}

// The component injects the block into a REAL SystemPrompt through a REAL registry,
// and removing it withdraws the block (revertible effect).
func TestComponentRegistersThroughRegistry(t *testing.T) {
	s := seedStore(t)
	ix := indexOf(t, s)

	sp := prompt.New()
	promptComp := &runtime.Component{
		Name:    "prompt",
		Provide: []string{hx.KeyPrompt},
		Apply:   func(ctx *runtime.Context) error { return ctx.Provide(hx.KeyPrompt, sp) },
	}

	r := runtime.New()
	r.Load(promptComp, "")
	r.Load(fctx.Component(s, ix, nil), "")

	f := r.Fiber("harness/context")
	if f == nil || f.State() != runtime.StateActive {
		t.Fatalf("context component did not activate against the prompt: %v", f)
	}

	out, err := sp.Assemble(nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if !strings.Contains(out, "Verified context (from the record):") {
		t.Fatalf("assembled prompt missing the verified-context block: %q", out)
	}
	if !strings.Contains(out, "O-1101") || strings.Contains(out, "O-9999") {
		t.Fatalf("block not cited/active-first inside the prompt: %q", out)
	}

	// Removing the component reverts the registration — the block disappears.
	r.Remove("harness/context")
	out2, err := sp.Assemble(nil)
	if err != nil {
		t.Fatalf("Assemble after remove: %v", err)
	}
	if strings.Contains(out2, "Verified context (from the record):") {
		t.Fatalf("context block not withdrawn after component removal: %q", out2)
	}
}

// A prompt that does not support dynamic context makes the component fail loudly,
// not silently no-op — the optional-interface assertion is a real gate.
func TestComponentRejectsPromptWithoutContext(t *testing.T) {
	s := seedStore(t)
	ix := indexOf(t, s)

	// A stand-in prompt provider whose value lacks a Context method.
	badComp := &runtime.Component{
		Name:    "prompt",
		Provide: []string{hx.KeyPrompt},
		Apply:   func(ctx *runtime.Context) error { return ctx.Provide(hx.KeyPrompt, struct{}{}) },
	}
	r := runtime.New()
	r.Load(badComp, "")
	r.Load(fctx.Component(s, ix, nil), "")

	f := r.Fiber("harness/context")
	if f == nil || f.State() == runtime.StateActive {
		t.Fatalf("component should have failed against a prompt without Context, state=%v", f)
	}
	if f.Err() == nil {
		t.Fatal("expected a loud error recorded on the withheld fiber")
	}
}
