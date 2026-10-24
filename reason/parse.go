package reason

import (
	"fmt"
	"regexp"
	"strings"
)

// atomRe matches "pred(args)" where args is any run without nested parentheses.
var atomRe = regexp.MustCompile(`^([A-Za-z0-9_]+)\s*\(([^()]*)\)$`)

// tokenRe validates a single predicate/constant/variable token.
var tokenRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// ParseFact parses a ground atom such as "parent(tom, bob)". A trailing '.' is
// stripped. It rejects any atom containing a variable (a term starting with an
// uppercase letter).
func ParseFact(s string) (Fact, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ".")
	atom, err := parseAtom(s)
	if err != nil {
		return Fact{}, err
	}
	args := make([]string, len(atom.Args))
	for i, a := range atom.Args {
		if a.IsVar() {
			return Fact{}, fmt.Errorf("reason: fact %q contains variable %q", s, a)
		}
		args[i] = string(a)
	}
	return Fact{Pred: atom.Pred, Args: args}, nil
}

// ParseRule parses a Horn clause such as
//
//	"ancestor(X, Y) :- parent(X, Y)."
//	"ancestor(X, Z) :- parent(X, Y), ancestor(Y, Z)."
//
// A trailing '.' is stripped, body atoms are comma-separated, and the rule's
// Name defaults to its head predicate.
func ParseRule(s string) (Rule, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ".")

	idx := strings.Index(s, ":-")
	if idx < 0 {
		return Rule{}, fmt.Errorf("reason: rule %q missing ':-'", s)
	}
	head, err := parseAtom(strings.TrimSpace(s[:idx]))
	if err != nil {
		return Rule{}, err
	}

	var body []Atom
	for _, atomStr := range splitAtoms(strings.TrimSpace(s[idx+2:])) {
		atomStr = strings.TrimSpace(atomStr)
		if atomStr == "" {
			continue
		}
		// A body atom prefixed with "not " or "!" is negated (negation-as-
		// failure). Heads and facts are never negated.
		neg := false
		switch {
		case strings.HasPrefix(atomStr, "not "):
			neg = true
			atomStr = strings.TrimSpace(atomStr[len("not "):])
		case strings.HasPrefix(atomStr, "!"):
			neg = true
			atomStr = strings.TrimSpace(atomStr[len("!"):])
		}
		a, err := parseAtom(atomStr)
		if err != nil {
			return Rule{}, err
		}
		a.Neg = neg
		body = append(body, a)
	}
	if len(body) == 0 {
		return Rule{}, fmt.Errorf("reason: rule %q has an empty body", s)
	}
	return Rule{Name: head.Pred, Head: head, Body: body}, nil
}

// parseAtom parses "pred(t1, t2, ...)" into an Atom whose terms may be
// variables or constants.
func parseAtom(s string) (Atom, error) {
	s = strings.TrimSpace(s)
	m := atomRe.FindStringSubmatch(s)
	if m == nil {
		return Atom{}, fmt.Errorf("reason: cannot parse atom %q", s)
	}
	pred := m[1]
	inner := strings.TrimSpace(m[2])
	var args []Term
	if inner != "" {
		for _, p := range strings.Split(inner, ",") {
			p = strings.TrimSpace(p)
			if !tokenRe.MatchString(p) {
				return Atom{}, fmt.Errorf("reason: invalid term %q in atom %q", p, s)
			}
			args = append(args, Term(p))
		}
	}
	return Atom{Pred: pred, Args: args}, nil
}

// splitAtoms splits a rule body on top-level commas, keeping commas that sit
// inside an atom's parentheses attached to that atom.
func splitAtoms(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}
