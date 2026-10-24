// compare.go implements compare_refs — Rule 5 of the resolver contract (system
// design §8): "Concurrent edits are predicted, not merged." Two overlay refs are
// compared over a shared base BEFORE either merges, so a merge collision surfaces
// as a prediction, never as something discovered after the fact. It is the graph-
// level companion of the PR gate's scope-to-the-change step (§7.5) and of cycles
// being predicted at load (§4.9) rather than deadlocking at runtime.
//
// The comparison is by LogicalID → fact address. Because a stored fact's address
// excludes ref and clock (store.go, Law 3), a fact inherited unchanged carries the
// SAME address on both refs while a fact one side edited carries a DIFFERENT one.
// So address inequality per LogicalID is exactly "this logical fact differs here".
package flg

import (
	"sort"

	"github.com/websines/structor-harness/record"
)

// FactRef is one logical fact compared across refs. A, B and Base hold that fact's
// address on side a, side b and the base, or "" where the fact is absent on that
// ref (an add or a delete). A conflict carries both sides' differing addresses.
type FactRef struct {
	LogicalID string
	A         record.Digest // fact address on side a, "" if absent
	B         record.Digest // fact address on side b, "" if absent
	Base      record.Digest // fact address on the base, "" if absent
}

// CompareResult is the one-pass outcome of comparing two refs (system design §8
// Rule 5). Every slice is ordered by LogicalID, so the result is deterministic:
//
//	Divergent — logical facts that differ between a and b: the raw branch diff.
//	            Needs no base.
//	Overlap   — logical facts BOTH refs changed relative to the base ("what are we
//	            both touching"); the same change and a different change both count.
//	            Base-relative — empty when no base is given.
//	Conflicts — the subset of Overlap the two refs changed DIFFERENTLY: a predicted
//	            merge collision, surfaced before either merges. Base-relative.
type CompareResult struct {
	Divergent []FactRef
	Overlap   []FactRef
	Conflicts []FactRef
}

// CompareRefs compares overlay refs a and b over store s, optionally against a
// common base (nil = no base). Divergent needs no base; Overlap and Conflicts are
// base-relative and stay empty when base is nil. A typical call passes both
// branches' shared base (e.g. the pinned base commit) to predict merge collisions
// between two feature refs before either merges (Rule 5). No clock, no ordering:
// the result depends only on which facts each ref effectively carries.
func CompareRefs(s *Store, a, b Overlay, base *Overlay) (CompareResult, error) {
	ea, err := a.EffectiveByLogical(s)
	if err != nil {
		return CompareResult{}, err
	}
	eb, err := b.EffectiveByLogical(s)
	if err != nil {
		return CompareResult{}, err
	}
	var ebase map[string]record.Digest
	if base != nil {
		if ebase, err = base.EffectiveByLogical(s); err != nil {
			return CompareResult{}, err
		}
	}

	ids := map[string]bool{}
	for id := range ea {
		ids[id] = true
	}
	for id := range eb {
		ids[id] = true
	}
	for id := range ebase {
		ids[id] = true
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)

	var res CompareResult
	for _, id := range ordered {
		ha, hb, hbase := ea[id], eb[id], ebase[id]
		entry := FactRef{LogicalID: id, A: ha, B: hb, Base: hbase}
		if ha != hb {
			res.Divergent = append(res.Divergent, entry) // the raw branch diff
		}
		if base != nil && ha != hbase && hb != hbase {
			res.Overlap = append(res.Overlap, entry) // both refs changed it vs base
			if ha != hb {
				res.Conflicts = append(res.Conflicts, entry) // ...and disagree → collision
			}
		}
	}
	return res, nil
}
