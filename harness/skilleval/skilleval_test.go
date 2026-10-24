package skilleval

import (
	"testing"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/learning"
	"github.com/websines/structor-harness/record"
)

// newStore builds a real record-backed flg.Store over a local-disk CAS.
func newStore(t *testing.T) *flg.Store {
	t.Helper()
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalCAS: %v", err)
	}
	return flg.NewStore(cas)
}

// Record two specs (one contains, one not_contains) and confirm EvalsFor returns
// them as declarative held-out cases whose grader passes a conforming body and
// fails a non-conforming one. Also confirms empty-Name/empty-Want specs are
// skipped by Record.
func TestRecordAndEvalsForGrades(t *testing.T) {
	store := newStore(t)
	specs := []Spec{
		{Name: "mentions-idempotent", Want: "idempotent", Kind: KindContains},
		{Name: "no-todo", Want: "TODO", Kind: KindNotContains},
		{Name: "", Want: "unnamed-skipped"},  // no name -> skipped
		{Name: "wantless-skipped", Want: ""}, // no want -> skipped
	}
	if err := Record(store, "writer", specs); err != nil {
		t.Fatalf("Record: %v", err)
	}

	ev, ok := NewProvider(store).EvalsFor("writer")
	if !ok {
		t.Fatal("EvalsFor(writer): want ok=true")
	}
	if len(ev.HeldOut) != 2 {
		t.Fatalf("HeldOut: want 2 cases (empties skipped), got %d", len(ev.HeldOut))
	}
	if ev.Eval == nil {
		t.Fatal("Eval grader is nil")
	}

	// A body that CONTAINS the contains-want AND LACKS the not_contains-want
	// must pass every case; a body that fails both must fail every case.
	good := learning.Skill{Body: "This routine is idempotent and safe to retry."}
	bad := learning.Skill{Body: "Handle retries. TODO: make this idem-safe later."}
	for _, c := range ev.HeldOut {
		if !ev.Eval(good, c) {
			t.Errorf("conforming body should PASS case %q", c.Name)
		}
		if ev.Eval(bad, c) {
			t.Errorf("non-conforming body should FAIL case %q", c.Name)
		}
	}
}

// A skill with no eval facts recorded must yield ok=false so a self-improvement
// pass skips it (nothing to beat, no fiat).
func TestEvalsForNoFacts(t *testing.T) {
	store := newStore(t)
	if ev, ok := NewProvider(store).EvalsFor("ghost"); ok {
		t.Fatalf("EvalsFor(ghost): want ok=false, got ok=true with %d cases", len(ev.HeldOut))
	}
}

// Eval facts are scoped to their skill by domain tag: skill A's evals must never
// surface for skill B.
func TestEvalsForDomainTagScoping(t *testing.T) {
	store := newStore(t)
	if err := Record(store, "alpha", []Spec{{Name: "a1", Want: "alpha-word"}}); err != nil {
		t.Fatalf("Record alpha: %v", err)
	}
	if err := Record(store, "beta", []Spec{{Name: "b1", Want: "beta-word"}}); err != nil {
		t.Fatalf("Record beta: %v", err)
	}
	p := NewProvider(store)

	ev, ok := p.EvalsFor("alpha")
	if !ok || len(ev.HeldOut) != 1 {
		t.Fatalf("alpha: want ok=true with 1 case, got ok=%v n=%d", ok, len(ev.HeldOut))
	}
	if ev.HeldOut[0].Name != "a1" || ev.HeldOut[0].Want != "alpha-word" {
		t.Errorf("alpha case leaked/wrong: got name=%q want=%q", ev.HeldOut[0].Name, ev.HeldOut[0].Want)
	}

	// beta's single eval must be independently scoped, never mixed with alpha's.
	evB, okB := p.EvalsFor("beta")
	if !okB || len(evB.HeldOut) != 1 || evB.HeldOut[0].Want != "beta-word" {
		t.Fatalf("beta: want its own 1 case, got ok=%v cases=%v", okB, evB.HeldOut)
	}
}

// The default kind (empty Spec.Kind) grades as "contains".
func TestDefaultKindIsContains(t *testing.T) {
	store := newStore(t)
	if err := Record(store, "def", []Spec{{Name: "c1", Want: "needle"}}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	ev, ok := NewProvider(store).EvalsFor("def")
	if !ok || len(ev.HeldOut) != 1 {
		t.Fatalf("def: want 1 case, got ok=%v n=%d", ok, len(ev.HeldOut))
	}
	c := ev.HeldOut[0]
	if !ev.Eval(learning.Skill{Body: "a needle in a haystack"}, c) {
		t.Error("default kind should pass a body that contains the want")
	}
	if ev.Eval(learning.Skill{Body: "only hay here"}, c) {
		t.Error("default kind should fail a body that lacks the want")
	}
}
