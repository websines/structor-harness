// This file implements scope-to-the-change (system design §4.3, §7.5 step 2,
// §8.2): the ratchet that makes coverage follow activity. A change declares the
// domain tags and files it touches; Scope selects exactly the registered
// obligations that change pulls in — "the one or two Theorems covering the
// behaviour it affects ... Nothing else." Pure set arithmetic: no clock, no I/O.
package obligation

import "github.com/websines/structor-harness/flg"

// Change is the touched surface of a PR, already reduced to graph terms by
// compare_refs (§7.5 step 2). It carries no diff text and no timestamp — only the
// domain tags and file paths the change lands on.
type Change struct {
	// Tags are the domain tags the change affects. An obligation is touched when
	// its DomainTags intersect these. The wildcard "*" touches every obligation
	// (mirrors the resolver's in-scope wildcard).
	Tags []string
	// Files are the changed file paths. An obligation is touched when its anchor
	// file is among them — the anchor pins the leaf to a code span (§4.8), so a
	// diff to that span pulls the leaf into scope even where no tag was declared.
	Files []string
}

// Scope selects the registered obligations touched by c: the union of tag-
// intersection and anchor-file match, deduplicated and sorted by id (it reads
// through Registry.All, which is already id-sorted). Retired scopes are included,
// not dropped — "stale, kept, surfaced" (§4.3); the gate and exposure report then
// treat them as the non-governing audit_only they are, rather than hiding them.
func Scope(reg *Registry, c Change) []flg.Obligation {
	tags := map[string]bool{}
	wildcard := false
	for _, t := range c.Tags {
		if t == "*" {
			wildcard = true
		}
		tags[t] = true
	}
	files := map[string]bool{}
	for _, f := range c.Files {
		files[f] = true
	}

	var out []flg.Obligation
	for _, o := range reg.All() {
		if wildcard || touchesTags(o, tags) || files[o.Anchor.File] {
			out = append(out, o)
		}
	}
	return out
}

// ScopeIDs is Scope reduced to obligation ids, in the same stable order.
func ScopeIDs(reg *Registry, c Change) []string {
	touched := Scope(reg, c)
	out := make([]string, 0, len(touched))
	for _, o := range touched {
		out = append(out, o.ID)
	}
	return out
}

// touchesTags reports whether any of the obligation's declared domain tags is in
// the change's tag set.
func touchesTags(o flg.Obligation, tags map[string]bool) bool {
	for _, d := range o.DomainTags {
		if tags[d] {
			return true
		}
	}
	return false
}
