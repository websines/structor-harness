// Black-box tests (package flg_test) for compare_refs — Rule 5: concurrent edits
// are PREDICTED, not merged. Covers (i) divergence with no overlap, (ii) the same
// change on both sides = overlap not conflict, and (iii) two sides changing one
// logical fact differently = a reported conflict.
package flg_test

import (
	"reflect"
	"testing"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/record"
)

// logicalIDs pulls the ordered LogicalIDs out of a result slice for assertions.
func logicalIDs(refs []flg.FactRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.LogicalID)
	}
	return out
}

// TestCompareRefs_ConflictOverlapDivergent exercises every classification in one
// pass over a shared base:
//
//	O-1 — a and b change it DIFFERENTLY   → divergent + overlap + conflict
//	O-2 — a and b make the SAME change    → overlap, NOT conflict
//	O-3 — neither side touches it         → nothing
//	O-4 — only a adds it (base absent)    → divergent, NOT overlap (b left it alone)
func TestCompareRefs_ConflictOverlapDivergent(t *testing.T) {
	s := newStore(t)

	o1Base := appendObl(t, s, "O-1", "base")
	o1A := appendObl(t, s, "O-1", "a change")
	o1B := appendObl(t, s, "O-1", "b change")
	o2Base := appendObl(t, s, "O-2", "base")
	o2Both := appendObl(t, s, "O-2", "same change")
	o3Base := appendObl(t, s, "O-3", "base")
	o4A := appendObl(t, s, "O-4", "added only by a")

	base := flg.Overlay{Base: []record.Digest{o1Base, o2Base, o3Base}}
	refA := flg.Overlay{
		Base:       []record.Digest{o1Base, o2Base, o3Base},
		Delta:      []record.Digest{o1A, o2Both, o4A},
		Tombstones: []record.Digest{o1Base, o2Base},
	}
	refB := flg.Overlay{
		Base:       []record.Digest{o1Base, o2Base, o3Base},
		Delta:      []record.Digest{o1B, o2Both},
		Tombstones: []record.Digest{o1Base, o2Base},
	}

	res, err := flg.CompareRefs(s, refA, refB, &base)
	if err != nil {
		t.Fatal(err)
	}

	if got := logicalIDs(res.Divergent); !reflect.DeepEqual(got, []string{"O-1", "O-4"}) {
		t.Errorf("divergent: want [O-1 O-4], got %v", got)
	}
	if got := logicalIDs(res.Overlap); !reflect.DeepEqual(got, []string{"O-1", "O-2"}) {
		t.Errorf("overlap: want [O-1 O-2], got %v", got)
	}
	if got := logicalIDs(res.Conflicts); !reflect.DeepEqual(got, []string{"O-1"}) {
		t.Errorf("conflicts: want [O-1], got %v", got)
	}

	// The conflict carries both sides' differing addresses vs the base (Rule 5).
	c := res.Conflicts[0]
	if c.A != o1A || c.B != o1B || c.Base != o1Base {
		t.Errorf("conflict O-1 addresses wrong: A=%s B=%s Base=%s", c.A, c.B, c.Base)
	}
	if c.A == c.B {
		t.Error("a conflict must have two DIFFERENT sides")
	}
}

// TestCompareRefs_NoBase: without a base, divergence is still computed (the raw
// branch diff), but overlap and conflicts are base-relative and stay empty.
func TestCompareRefs_NoBase(t *testing.T) {
	s := newStore(t)
	o1Base := appendObl(t, s, "O-1", "base")
	o1A := appendObl(t, s, "O-1", "a change")
	o1B := appendObl(t, s, "O-1", "b change")

	refA := flg.Overlay{Base: []record.Digest{o1Base}, Delta: []record.Digest{o1A}, Tombstones: []record.Digest{o1Base}}
	refB := flg.Overlay{Base: []record.Digest{o1Base}, Delta: []record.Digest{o1B}, Tombstones: []record.Digest{o1Base}}

	res, err := flg.CompareRefs(s, refA, refB, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := logicalIDs(res.Divergent); !reflect.DeepEqual(got, []string{"O-1"}) {
		t.Errorf("divergent: want [O-1], got %v", got)
	}
	if len(res.Overlap) != 0 || len(res.Conflicts) != 0 {
		t.Errorf("no base ⇒ no overlap/conflicts, got overlap=%v conflicts=%v", res.Overlap, res.Conflicts)
	}
}

// TestCompareRefs_IdenticalRefs: two refs with the same effective facts diverge in
// nothing and conflict in nothing (a fact inherited unchanged has one address).
func TestCompareRefs_IdenticalRefs(t *testing.T) {
	s := newStore(t)
	o1 := appendObl(t, s, "O-1", "base")
	o2 := appendObl(t, s, "O-2", "base")

	base := flg.Overlay{Base: []record.Digest{o1, o2}}
	ref := flg.Overlay{Base: []record.Digest{o1, o2}}

	res, err := flg.CompareRefs(s, ref, ref, &base)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Divergent) != 0 || len(res.Overlap) != 0 || len(res.Conflicts) != 0 {
		t.Errorf("identical refs must compare clean, got %+v", res)
	}
}
