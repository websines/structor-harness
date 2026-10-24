// Package reason is a native Datalog reasoning engine: a Go port of semantica's
// datalog_reasoner.py (semi-naive bottom-up fixpoint over Horn clauses), FUSED
// with derivation tracking so every derived fact is proof-carrying.
//
// The algorithm (semantica's, faithfully). Evaluation is semi-naive bottom-up:
// seed a DELTA with all base (EDB) facts; each iteration, for each rule, apply
// it using the delta — for each body position p, body[p] ranges over the delta
// facts while every other body atom ranges over ALL known facts — collecting
// head facts not already known, adding them to the fact set and to the next
// delta; repeat until the delta is empty (the fixpoint). A term is a VARIABLE
// iff it starts with an uppercase letter (semantica's convention), else a
// constant. Unification threads bindings from a rule atom's args onto a ground
// fact's args and instantiates the head. This terminates on finite data and
// supports recursion (e.g. transitive closure).
//
// The Forge augmentation (why this is proof-native). Semantica computes only
// the fact set and keeps any derivation trace separate. Forge is proof-native,
// so here the two are UNIFIED: for each derived fact the engine records the
// FIRST derivation that produced it — which rule fired and which concrete
// ground body facts (premises) it consumed. Base facts are proof leaves (no
// rule, no premises).
//
// Why proof reconstruction terminates. Because semi-naive derives a fact only
// from facts that already existed in a PRIOR iteration, every premise is
// strictly older than the fact it justifies, so the first-derivation graph is a
// DAG. A proof tree therefore reconstructs by plain recursion and always
// terminates; a path-scoped visited-guard is kept purely as a safety net.
//
// Determinism (Forge Law 3). Rules iterate in insertion order and candidate
// facts are sorted by Key before use, so DeriveAll, Facts and every proof tree
// are byte-identical across runs — no dependence on Go map iteration order
// leaks into any output.
package reason

import (
	"fmt"
	"sort"
	"strings"
)

// Term is a single argument of a rule atom. It is a VARIABLE iff it starts with
// an uppercase ASCII letter (semantica's convention); otherwise it is a
// constant.
type Term string

// IsVar reports whether the term is a variable (starts with an uppercase
// letter). An empty term is a constant.
func (t Term) IsVar() bool {
	if len(t) == 0 {
		return false
	}
	c := t[0]
	return c >= 'A' && c <= 'Z'
}

// Atom is a rule head or body condition. Its args may hold variables. Neg marks
// a NEGATED body atom (negation-as-failure); heads and facts are never negated.
type Atom struct {
	Pred string
	Args []Term
	Neg  bool
}

// Fact is a ground atom: every argument is a constant.
type Fact struct {
	Pred string
	Args []string
}

// Key returns the canonical "pred(a,b,c)" identity used for dedup and lookup.
func (f Fact) Key() string {
	return f.Pred + "(" + strings.Join(f.Args, ",") + ")"
}

// Rule is a Horn clause: Head holds when every Body atom holds. Name identifies
// the rule in proof trees (defaults to the head predicate when parsed).
type Rule struct {
	Name string
	Head Atom
	Body []Atom
}

// derivation records the FIRST way a derived fact was produced: the rule that
// fired, the exact ground positive premises it consumed (in body order) and the
// ground negated premises that were justified by ABSENCE (negation-as-failure).
type derivation struct {
	rule        string
	premises    []Fact
	negPremises []Fact
}

// Engine holds the fact index, the rules, the derivation map and a flag marking
// whether the fixpoint has already run.
type Engine struct {
	index    map[string][]Fact     // facts by predicate (base + derived)
	known    map[string]bool       // dedup set keyed by Fact.Key
	byKey    map[string]Fact       // Key -> Fact
	rules    []Rule                // insertion order (stable)
	deriv    map[string]derivation // Key -> first derivation (derived facts only)
	derived  bool                  // Solve has run
	solveErr error                 // error from the (single) Solve run, if any
}

// New returns an empty engine.
func New() *Engine {
	return &Engine{
		index: map[string][]Fact{},
		known: map[string]bool{},
		byKey: map[string]Fact{},
		deriv: map[string]derivation{},
	}
}

// AddFact registers a base (EDB) fact. Duplicates are ignored.
func (e *Engine) AddFact(f Fact) {
	key := f.Key()
	if e.known[key] {
		return
	}
	e.known[key] = true
	e.byKey[key] = f
	e.index[f.Pred] = append(e.index[f.Pred], f)
}

// AddRule registers a rule. Rules fire in the order they are added.
func (e *Engine) AddRule(r Rule) {
	e.rules = append(e.rules, r)
}

// DeriveAll runs the stratified fixpoint once, discarding any stratification
// error. It is idempotent (repeat calls are no-ops) and is the entry point for
// positive-only programs: with no negation the program has a single stratum and
// reduces exactly to the original semi-naive behaviour.
func (e *Engine) DeriveAll() { _ = e.Solve() }

// Solve runs STRATIFIED negation-as-failure evaluation once and reports whether
// the program was stratifiable. It is idempotent: after the first call it
// returns the same result without re-evaluating.
//
// Stratification. A predicate dependency graph is built from the rules: a body
// atom with predicate P under a head predicate H yields a POSITIVE edge P->H
// (requiring stratum(H) >= stratum(P)) or, when the atom is negated, a NEGATIVE
// edge (requiring stratum(H) >= stratum(P)+1). EDB predicates (never a head) sit
// at stratum 0. If the max-constraint fixpoint does not stabilise, some
// predicate negatively depends on itself and the program is rejected.
//
// Evaluation proceeds stratum by stratum in increasing order, running the SAME
// semi-naive fixpoint as before but firing only rules whose head is in the
// current stratum. A negated body atom is a FILTER against the already-complete
// lower strata: it is instantiated under the current bindings (all its vars must
// already be bound by earlier positive atoms) and the branch succeeds iff the
// resulting ground fact is NOT known.
func (e *Engine) Solve() error {
	if e.derived {
		return e.solveErr
	}
	e.derived = true
	e.solveErr = e.solve()
	return e.solveErr
}

func (e *Engine) solve() error {
	strata, err := e.computeStrata()
	if err != nil {
		return err
	}
	maxLevel := 0
	for _, s := range strata {
		if s > maxLevel {
			maxLevel = s
		}
	}
	for level := 0; level <= maxLevel; level++ {
		var levelRules []Rule
		for _, r := range e.rules {
			if strata[r.Head.Pred] == level {
				levelRules = append(levelRules, r)
			}
		}
		if len(levelRules) == 0 {
			continue
		}
		if err := e.evalStratum(levelRules); err != nil {
			return err
		}
	}
	return nil
}

// evalStratum runs the semi-naive fixpoint over exactly the given rules (all
// with head predicates in one stratum). Facts of lower strata are already
// complete, so negated atoms filter correctly against e.known.
func (e *Engine) evalStratum(rules []Rule) error {
	// Seed the delta with all currently-known facts (base + lower strata).
	delta := e.snapshot()

	for factCount(delta) > 0 {
		all := e.snapshot() // facts known through the previous round
		next := map[string][]Fact{}

		for _, rule := range rules {
			body := rule.Body
			if len(body) == 0 {
				continue // a rule with no body derives nothing
			}
			// Semi-naive: exactly one POSITIVE body position draws from the
			// delta. Negated positions are never a delta source.
			for _, p := range positivePositions(body) {
				err := e.joinBody(rule, 0, p, delta, all, map[string]string{}, nil, nil,
					func(bindings map[string]string, premises, negPremises []Fact) {
						head, ok := instantiate(rule.Head, bindings)
						if !ok {
							return
						}
						key := head.Key()
						if e.known[key] {
							return // already known; keep the FIRST derivation
						}
						e.recordDerived(head, rule.Name, premises, negPremises)
						next[head.Pred] = append(next[head.Pred], head)
					})
				if err != nil {
					return err
				}
			}
		}

		sortFactMap(next)
		delta = next
	}
	return nil
}

// computeStrata assigns each predicate a stratum by iterating the max-constraints
// to a fixpoint. It returns an error naming an offending predicate when the
// program is not stratifiable (a predicate negatively depends on itself).
func (e *Engine) computeStrata() (map[string]int, error) {
	type edge struct {
		from, to string
		delta    int
	}
	preds := map[string]bool{}
	var edges []edge
	for _, r := range e.rules {
		preds[r.Head.Pred] = true
		for _, b := range r.Body {
			preds[b.Pred] = true
			d := 0
			if b.Neg {
				d = 1
			}
			edges = append(edges, edge{from: b.Pred, to: r.Head.Pred, delta: d})
		}
	}
	stratum := make(map[string]int, len(preds))
	for p := range preds {
		stratum[p] = 0
	}
	// Bellman-Ford-style relaxation: a valid stratification settles within
	// len(preds) passes, so a change on pass len(preds)+1 proves a negative cycle.
	n := len(preds)
	for pass := 0; pass <= n; pass++ {
		changed := false
		for _, ed := range edges {
			if want := stratum[ed.from] + ed.delta; want > stratum[ed.to] {
				stratum[ed.to] = want
				changed = true
			}
		}
		if !changed {
			return stratum, nil
		}
	}
	// Not stabilised: report the predicate driven to the highest stratum (it sits
	// on the negative cycle), chosen deterministically.
	names := make([]string, 0, len(stratum))
	for p := range stratum {
		names = append(names, p)
	}
	sort.Strings(names)
	offender, best := names[0], -1
	for _, p := range names {
		if stratum[p] > best {
			best, offender = stratum[p], p
		}
	}
	return nil, fmt.Errorf("reason: program is not stratifiable: predicate %q negatively depends on itself", offender)
}

// positivePositions returns the indices of the non-negated body atoms — the only
// positions eligible to be the semi-naive delta source.
func positivePositions(body []Atom) []int {
	var out []int
	for i := range body {
		if !body[i].Neg {
			out = append(out, i)
		}
	}
	return out
}

// recordDerived stores a newly derived fact and its first derivation.
func (e *Engine) recordDerived(f Fact, ruleName string, premises, negPremises []Fact) {
	key := f.Key()
	if e.known[key] {
		return
	}
	e.known[key] = true
	e.byKey[key] = f
	e.index[f.Pred] = append(e.index[f.Pred], f)
	e.deriv[key] = derivation{rule: ruleName, premises: premises, negPremises: negPremises}
}

// joinBody recursively joins the body atoms. A POSITIVE position deltaPos draws
// candidates from delta; every other positive position draws from all. A NEGATED
// atom is not a candidate source: it is instantiated under the current bindings
// (all its vars must already be bound, else the rule is unsafe) and the branch
// continues iff the resulting ground fact is absent, recording it as a negative
// premise. On a full match it calls emit with the threaded bindings, the ordered
// positive premises and the ordered negative premises.
func (e *Engine) joinBody(
	rule Rule, idx, deltaPos int,
	delta, all map[string][]Fact,
	bindings map[string]string, premises, negPremises []Fact,
	emit func(map[string]string, []Fact, []Fact),
) error {
	body := rule.Body
	if idx == len(body) {
		emit(bindings, premises, negPremises)
		return nil
	}
	atom := body[idx]
	if atom.Neg {
		// Negation-as-failure filter. Every var must already be bound.
		ground, ok := instantiate(atom, bindings)
		if !ok {
			return fmt.Errorf("reason: unsafe negation in rule %s", rule.Name)
		}
		if e.known[ground.Key()] {
			return nil // the negated fact is known: this branch fails (prune).
		}
		// Absent: the branch succeeds. Record it as a negative premise; consume
		// no candidate. Fresh backing array so siblings never share.
		np := append(negPremises[:len(negPremises):len(negPremises)], ground)
		return e.joinBody(rule, idx+1, deltaPos, delta, all, bindings, premises, np, emit)
	}
	var candidates []Fact
	if idx == deltaPos {
		candidates = delta[atom.Pred]
	} else {
		candidates = all[atom.Pred]
	}
	for _, f := range candidates {
		nextBindings, ok := unify(atom, f, bindings)
		if !ok {
			continue
		}
		// Fresh backing array per branch so siblings never share premises.
		nextPremises := append(premises[:len(premises):len(premises)], f)
		if err := e.joinBody(rule, idx+1, deltaPos, delta, all, nextBindings, nextPremises, negPremises, emit); err != nil {
			return err
		}
	}
	return nil
}

// Facts returns every fact (base and derived) sorted by Key for determinism.
func (e *Engine) Facts() []Fact {
	out := make([]Fact, 0, len(e.byKey))
	for _, f := range e.byKey {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// Has reports whether the fact is known (base or derived).
func (e *Engine) Has(f Fact) bool {
	return e.known[f.Key()]
}

// Query matches pred(args...) against every known fact of that predicate and
// arity, returning one binding map per match holding the value bound to each
// variable position. Constant positions must match. Results are ordered by the
// matched fact's Key.
func (e *Engine) Query(pred string, args ...Term) []map[string]string {
	atom := Atom{Pred: pred, Args: args}
	facts := append([]Fact(nil), e.index[pred]...)
	sort.Slice(facts, func(i, j int) bool { return facts[i].Key() < facts[j].Key() })

	var results []map[string]string
	for _, f := range facts {
		bindings, ok := unify(atom, f, map[string]string{})
		if !ok {
			continue
		}
		proj := map[string]string{}
		for _, a := range args {
			if a.IsVar() {
				proj[string(a)] = bindings[string(a)]
			}
		}
		results = append(results, proj)
	}
	return results
}

// unify matches a (possibly variable-bearing) atom against a ground fact,
// returning the extended bindings on success. The input bindings are not
// mutated.
func unify(atom Atom, fact Fact, bindings map[string]string) (map[string]string, bool) {
	if atom.Pred != fact.Pred || len(atom.Args) != len(fact.Args) {
		return nil, false
	}
	next := make(map[string]string, len(bindings)+len(atom.Args))
	for k, v := range bindings {
		next[k] = v
	}
	for i, arg := range atom.Args {
		val := fact.Args[i]
		if arg.IsVar() {
			name := string(arg)
			if bound, ok := next[name]; ok {
				if bound != val {
					return nil, false
				}
			} else {
				next[name] = val
			}
		} else if string(arg) != val {
			return nil, false
		}
	}
	return next, true
}

// instantiate grounds a head atom under bindings. It fails if any head variable
// is unbound (the rule is not range-restricted for this match).
func instantiate(head Atom, bindings map[string]string) (Fact, bool) {
	args := make([]string, len(head.Args))
	for i, a := range head.Args {
		if a.IsVar() {
			v, ok := bindings[string(a)]
			if !ok {
				return Fact{}, false
			}
			args[i] = v
		} else {
			args[i] = string(a)
		}
	}
	return Fact{Pred: head.Pred, Args: args}, true
}

// snapshot returns a per-predicate copy of the current fact index, each slice
// sorted by Key for deterministic iteration.
func (e *Engine) snapshot() map[string][]Fact {
	out := make(map[string][]Fact, len(e.index))
	for pred, facts := range e.index {
		cp := make([]Fact, len(facts))
		copy(cp, facts)
		sort.Slice(cp, func(i, j int) bool { return cp[i].Key() < cp[j].Key() })
		out[pred] = cp
	}
	return out
}

// sortFactMap sorts each predicate's slice by Key in place.
func sortFactMap(m map[string][]Fact) {
	for _, facts := range m {
		sort.Slice(facts, func(i, j int) bool { return facts[i].Key() < facts[j].Key() })
	}
}

// factCount totals the facts across a per-predicate map.
func factCount(m map[string][]Fact) int {
	n := 0
	for _, v := range m {
		n += len(v)
	}
	return n
}
