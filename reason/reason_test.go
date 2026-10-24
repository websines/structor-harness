// Black-box tests (package reason_test): drive the engine through its exported
// API only, so every assertion is about observable behaviour — the derived fact
// set, proof trees and query bindings.
package reason_test

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/websines/structor-harness/reason"
)

// buildAncestors builds the canonical transitive-closure program:
// parent(a,b), parent(b,c), parent(c,d) plus the two ancestor rules, then runs
// the fixpoint.
func buildAncestors(t *testing.T) *reason.Engine {
	t.Helper()
	e := reason.New()
	for _, s := range []string{"parent(a, b)", "parent(b, c)", "parent(c, d)"} {
		f, err := reason.ParseFact(s)
		if err != nil {
			t.Fatalf("ParseFact(%q): %v", s, err)
		}
		e.AddFact(f)
	}
	for _, s := range []string{
		"ancestor(X, Y) :- parent(X, Y).",
		"ancestor(X, Z) :- parent(X, Y), ancestor(Y, Z).",
	} {
		r, err := reason.ParseRule(s)
		if err != nil {
			t.Fatalf("ParseRule(%q): %v", s, err)
		}
		e.AddRule(r)
	}
	e.DeriveAll()
	return e
}

func mustFact(t *testing.T, s string) reason.Fact {
	t.Helper()
	f, err := reason.ParseFact(s)
	if err != nil {
		t.Fatalf("ParseFact(%q): %v", s, err)
	}
	return f
}

func ancestorKeys(facts []reason.Fact) []string {
	var out []string
	for _, f := range facts {
		if f.Pred == "ancestor" {
			out = append(out, f.Key())
		}
	}
	sort.Strings(out)
	return out
}

func leaves(n *reason.ProofNode) []*reason.ProofNode {
	if len(n.Premises) == 0 {
		return []*reason.ProofNode{n}
	}
	var out []*reason.ProofNode
	for _, p := range n.Premises {
		out = append(out, leaves(p)...)
	}
	return out
}

func countNodes(n *reason.ProofNode) int {
	c := 1
	for _, p := range n.Premises {
		c += countNodes(p)
	}
	return c
}

// 1. Transitive closure: ancestor(a,d) is derived and the FULL ancestor set is
// exactly the transitive closure of the a->b->c->d chain. DeriveAll returning
// is the termination assertion.
func TestTransitiveClosure(t *testing.T) {
	e := buildAncestors(t)

	if !e.Has(mustFact(t, "ancestor(a, d)")) {
		t.Fatal("expected ancestor(a,d) to be derived")
	}

	want := []string{
		"ancestor(a,b)", "ancestor(a,c)", "ancestor(a,d)",
		"ancestor(b,c)", "ancestor(b,d)", "ancestor(c,d)",
	}
	got := ancestorKeys(e.Facts())
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ancestor set = %v, want %v", got, want)
	}
}

// 2. Proof tree: the proof of ancestor(a,c) cites the ancestor rule, its
// premises are exactly parent(a,b) + ancestor(b,c), and every leaf is a base
// parent fact with Rule == "".
func TestProofTree(t *testing.T) {
	e := buildAncestors(t)

	node, ok := e.Proof(mustFact(t, "ancestor(a, c)"))
	if !ok {
		t.Fatal("expected a proof for ancestor(a,c)")
	}
	if node.Fact.Key() != "ancestor(a,c)" {
		t.Fatalf("root fact = %s, want ancestor(a,c)", node.Fact.Key())
	}
	if node.Rule != "ancestor" {
		t.Fatalf("root rule = %q, want ancestor", node.Rule)
	}

	if len(node.Premises) != 2 {
		t.Fatalf("want exactly 2 premises, got %d", len(node.Premises))
	}
	premKeys := []string{node.Premises[0].Fact.Key(), node.Premises[1].Fact.Key()}
	sort.Strings(premKeys)
	if premKeys[0] != "ancestor(b,c)" || premKeys[1] != "parent(a,b)" {
		t.Fatalf("premises = %v, want [ancestor(b,c) parent(a,b)]", premKeys)
	}

	for _, leaf := range leaves(node) {
		if leaf.Fact.Pred != "parent" {
			t.Fatalf("leaf %s is not a base parent fact", leaf.Fact.Key())
		}
		if leaf.Rule != "" {
			t.Fatalf("base leaf %s has Rule=%q, want empty", leaf.Fact.Key(), leaf.Rule)
		}
		if len(leaf.Premises) != 0 {
			t.Fatalf("base leaf %s has premises", leaf.Fact.Key())
		}
	}
}

// 3. Proof of a base fact: a single leaf node with Rule == "".
func TestProofOfBaseFact(t *testing.T) {
	e := buildAncestors(t)

	node, ok := e.Proof(mustFact(t, "parent(a, b)"))
	if !ok {
		t.Fatal("expected a proof for the base fact parent(a,b)")
	}
	if node.Fact.Key() != "parent(a,b)" {
		t.Fatalf("fact = %s, want parent(a,b)", node.Fact.Key())
	}
	if node.Rule != "" {
		t.Fatalf("base fact Rule = %q, want empty", node.Rule)
	}
	if len(node.Premises) != 0 {
		t.Fatalf("base fact has %d premises, want 0", len(node.Premises))
	}
}

// 4. No spurious derivations: a non-entailed fact is absent from Facts() and
// Proof returns false.
func TestNoSpuriousDerivations(t *testing.T) {
	e := buildAncestors(t)
	da := mustFact(t, "ancestor(d, a)")

	if e.Has(da) {
		t.Fatal("ancestor(d,a) must not be entailed")
	}
	for _, f := range e.Facts() {
		if f.Key() == "ancestor(d,a)" {
			t.Fatal("ancestor(d,a) leaked into Facts()")
		}
	}
	if _, ok := e.Proof(da); ok {
		t.Fatal("Proof(ancestor(d,a)) must return false")
	}
}

// 5. Recursion terminates on a cycle in the data: parent(a,b), parent(b,a) plus
// the ancestor rules halt, and ancestor(a,a) is derived with a finite valid
// proof.
func TestRecursionTerminatesOnCycle(t *testing.T) {
	e := reason.New()
	for _, s := range []string{"parent(a, b)", "parent(b, a)"} {
		e.AddFact(mustFact(t, s))
	}
	for _, s := range []string{
		"ancestor(X, Y) :- parent(X, Y).",
		"ancestor(X, Z) :- parent(X, Y), ancestor(Y, Z).",
	} {
		r, err := reason.ParseRule(s)
		if err != nil {
			t.Fatalf("ParseRule(%q): %v", s, err)
		}
		e.AddRule(r)
	}

	done := make(chan struct{})
	go func() {
		e.DeriveAll()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("DeriveAll did not terminate on cyclic data")
	}

	aa := mustFact(t, "ancestor(a, a)")
	if !e.Has(aa) {
		t.Fatal("expected ancestor(a,a) to be derived")
	}
	node, ok := e.Proof(aa)
	if !ok {
		t.Fatal("expected a proof for ancestor(a,a)")
	}
	if n := countNodes(node); n < 2 {
		t.Fatalf("proof for ancestor(a,a) too small (%d nodes):\n%s", n, node.String())
	}
	// The proof must bottom out in base facts.
	for _, leaf := range leaves(node) {
		if leaf.Rule != "" {
			t.Fatalf("proof leaf %s is not a base fact", leaf.Fact.Key())
		}
	}
}

// 6. Determinism: building the same program twice yields byte-identical Facts()
// and byte-identical proof trees.
func TestDeterminism(t *testing.T) {
	e1 := buildAncestors(t)
	e2 := buildAncestors(t)

	var k1, k2 []string
	for _, f := range e1.Facts() {
		k1 = append(k1, f.Key())
	}
	for _, f := range e2.Facts() {
		k2 = append(k2, f.Key())
	}
	if strings.Join(k1, "\n") != strings.Join(k2, "\n") {
		t.Fatalf("Facts() not deterministic:\n%v\n%v", k1, k2)
	}

	ad := mustFact(t, "ancestor(a, d)")
	n1, _ := e1.Proof(ad)
	n2, _ := e2.Proof(ad)
	if n1.String() != n2.String() {
		t.Fatalf("proof not deterministic:\n%q\n%q", n1.String(), n2.String())
	}

	// Surface an example proof tree in `go test -v` output.
	t.Logf("proof of ancestor(a,d):\n%s", n1.String())
}

// 8. Stratified negation: unreachable(X,Y) holds for exactly the node pairs with
// no path, and a program whose predicate negatively depends on itself is
// rejected by Solve.
func TestStratifiedNegation(t *testing.T) {
	e := reason.New()
	for _, s := range []string{"node(a)", "node(b)", "edge(a, b)"} {
		e.AddFact(mustFact(t, s))
	}
	for _, s := range []string{
		"reachable(X, Y) :- edge(X, Y).",
		"reachable(X, Z) :- edge(X, Y), reachable(Y, Z).",
		"unreachable(X, Y) :- node(X), node(Y), not reachable(X, Y).",
	} {
		r, err := reason.ParseRule(s)
		if err != nil {
			t.Fatalf("ParseRule(%q): %v", s, err)
		}
		e.AddRule(r)
	}
	if err := e.Solve(); err != nil {
		t.Fatalf("Solve() on stratifiable program returned error: %v", err)
	}

	// b cannot reach a, so unreachable(b,a) is derived.
	ba := mustFact(t, "unreachable(b, a)")
	if !e.Has(ba) {
		t.Fatal("expected unreachable(b,a) to be derived")
	}
	// a reaches b, so unreachable(a,b) must NOT be derived.
	if e.Has(mustFact(t, "unreachable(a, b)")) {
		t.Fatal("unreachable(a,b) must not be derived (a reaches b)")
	}

	// The proof of unreachable(b,a) cites the negated premise as an absent leaf.
	node, ok := e.Proof(ba)
	if !ok {
		t.Fatal("expected a proof for unreachable(b,a)")
	}
	var negLeaf *reason.ProofNode
	for _, p := range node.Premises {
		if p.Neg {
			negLeaf = p
		}
	}
	if negLeaf == nil {
		t.Fatalf("proof of unreachable(b,a) has no negative leaf:\n%s", node.String())
	}
	if negLeaf.Fact.Key() != "reachable(b,a)" {
		t.Fatalf("negative leaf = %s, want reachable(b,a)", negLeaf.Fact.Key())
	}
	if len(negLeaf.Premises) != 0 {
		t.Fatal("a negative leaf must have no children")
	}
	if !strings.Contains(node.String(), "¬reachable(b,a)  [absent]") {
		t.Fatalf("proof render missing absent negative premise:\n%s", node.String())
	}
	t.Logf("proof of unreachable(b,a):\n%s", node.String())

	// A program where p negatively depends on itself is not stratifiable.
	bad := reason.New()
	bad.AddFact(mustFact(t, "q(a)"))
	r, err := reason.ParseRule("p(X) :- q(X), not p(X).")
	if err != nil {
		t.Fatalf("ParseRule: %v", err)
	}
	bad.AddRule(r)
	if err := bad.Solve(); err == nil {
		t.Fatal("Solve() must reject a non-stratifiable program (p depends negatively on itself)")
	} else {
		t.Logf("non-stratifiable program correctly rejected: %v", err)
	}
}

// 7. Query: Query("ancestor", a, Y) returns the bindings Y=b, Y=c, Y=d.
func TestQuery(t *testing.T) {
	e := buildAncestors(t)

	res := e.Query("ancestor", reason.Term("a"), reason.Term("Y"))
	got := map[string]bool{}
	for _, b := range res {
		got[b["Y"]] = true
	}
	for _, want := range []string{"b", "c", "d"} {
		if !got[want] {
			t.Fatalf("Query missing binding Y=%s; got %v", want, res)
		}
	}
	if len(res) != 3 {
		t.Fatalf("Query returned %d bindings, want 3: %v", len(res), res)
	}
}
