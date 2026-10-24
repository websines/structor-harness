package reason

import "strings"

// ProofNode is one step of a machine-checkable proof tree. Rule names the rule
// that derived Fact from Premises; an empty Rule marks a base (EDB) fact, which
// is always a leaf. Neg marks a NEGATIVE leaf: a negated premise justified by the
// ABSENCE of Fact (negation-as-failure); a negative leaf has no children.
type ProofNode struct {
	Fact     Fact
	Rule     string
	Premises []*ProofNode
	Neg      bool
}

// Proof reconstructs the proof tree for a known fact. It returns (nil, false)
// when the fact is not in the engine's fact set. Base facts return a single
// leaf node with Rule == "".
//
// Reconstruction recurses over the FIRST-derivation DAG, which is acyclic
// because every premise was derived in an earlier iteration than the fact it
// supports; a path-scoped visited set guards against any pathological cycle so
// the walk is always finite.
func (e *Engine) Proof(f Fact) (*ProofNode, bool) {
	key := f.Key()
	if !e.known[key] {
		return nil, false
	}
	return e.buildProof(key, map[string]bool{}), true
}

func (e *Engine) buildProof(key string, onPath map[string]bool) *ProofNode {
	fact := e.byKey[key]
	d, isDerived := e.deriv[key]
	if !isDerived {
		// Base fact: a leaf with no rule and no premises.
		return &ProofNode{Fact: fact, Rule: ""}
	}
	if onPath[key] {
		// Safety net only — the first-derivation graph is a DAG, so this
		// never fires in practice. Truncate to keep the tree finite.
		return &ProofNode{Fact: fact, Rule: d.rule}
	}
	onPath[key] = true
	var premises []*ProofNode
	for _, p := range d.premises {
		premises = append(premises, e.buildProof(p.Key(), onPath))
	}
	// Negative premises render AFTER the positive ones, each a childless leaf
	// justified by absence.
	for _, np := range d.negPremises {
		premises = append(premises, &ProofNode{Fact: np, Neg: true})
	}
	delete(onPath, key) // path-scoped: allow shared premises in sibling branches
	return &ProofNode{Fact: fact, Rule: d.rule, Premises: premises}
}

// String renders the proof tree as an indented, human-readable listing. Each
// line shows the fact's canonical key followed by its justification: "[base]"
// for an EDB leaf, "[rule: NAME]" for a derived node, and a negative leaf as
// "¬pred(a,b)  [absent]".
func (n *ProofNode) String() string {
	var b strings.Builder
	n.write(&b, 0)
	return b.String()
}

func (n *ProofNode) write(b *strings.Builder, depth int) {
	for i := 0; i < depth; i++ {
		b.WriteString("  ")
	}
	if n.Neg {
		b.WriteString("¬")
		b.WriteString(n.Fact.Key())
		b.WriteString("  [absent]\n")
		return
	}
	b.WriteString(n.Fact.Key())
	if n.Rule == "" {
		b.WriteString("  [base]")
	} else {
		b.WriteString("  [rule: ")
		b.WriteString(n.Rule)
		b.WriteString("]")
	}
	b.WriteString("\n")
	for _, p := range n.Premises {
		p.write(b, depth+1)
	}
}
