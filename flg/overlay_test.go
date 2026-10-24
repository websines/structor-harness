// Black-box tests (package flg_test) for the overlay-ref algebra: the effective
// fact set (base minus tombstones plus delta) and its per-LogicalID projection.
package flg_test

import (
	"reflect"
	"sort"
	"testing"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/record"
)

// appendObl appends an obligation revision and returns its fact address. Two calls
// with the same id but different titles share a LogicalID and yield distinct
// addresses — a "change" to one logical fact.
func appendObl(t *testing.T, s *flg.Store, id, title string) record.Digest {
	t.Helper()
	fv, err := s.AppendObligation(flg.Obligation{ID: id, Title: title})
	if err != nil {
		t.Fatal(err)
	}
	return fv.FactID
}

func newStore(t *testing.T) *flg.Store {
	t.Helper()
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return flg.NewStore(cas)
}

func contains(ds []record.Digest, d record.Digest) bool {
	for _, x := range ds {
		if x == d {
			return true
		}
	}
	return false
}

// TestEffectiveFactSet_TombstoneHidesDeltaAdds is the overlay set math: a tombstone
// hides a base fact, a delta adds one, and the result is deduped and sorted.
func TestEffectiveFactSet_TombstoneHidesDeltaAdds(t *testing.T) {
	s := newStore(t)
	x := appendObl(t, s, "O-1", "x")
	y := appendObl(t, s, "O-2", "y")
	z := appendObl(t, s, "O-3", "z")
	w := appendObl(t, s, "O-4", "w")

	o := flg.Overlay{
		Base:       []record.Digest{x, y, z},
		Delta:      []record.Digest{w},
		Tombstones: []record.Digest{y}, // hide the base fact y
	}
	got := o.EffectiveFactSet()

	if len(got) != 3 {
		t.Fatalf("want 3 effective facts, got %d: %v", len(got), got)
	}
	if contains(got, y) {
		t.Error("tombstoned base fact y must not appear in the effective set")
	}
	for _, d := range []record.Digest{x, z, w} {
		if !contains(got, d) {
			t.Errorf("effective set missing expected fact %s", d)
		}
	}
	if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i] < got[j] }) {
		t.Errorf("effective set must be content-address sorted, got %v", got)
	}
}

// TestEffectiveFactSet_Deterministic: insertion order of the base/delta slices does
// not change the effective set — it depends only on which facts are present (Law 3).
func TestEffectiveFactSet_Deterministic(t *testing.T) {
	s := newStore(t)
	a := appendObl(t, s, "O-1", "a")
	b := appendObl(t, s, "O-2", "b")
	c := appendObl(t, s, "O-3", "c")

	o1 := flg.Overlay{Base: []record.Digest{a, b, c}}
	o2 := flg.Overlay{Base: []record.Digest{c, a, b, a}} // shuffled + a duplicate
	if !reflect.DeepEqual(o1.EffectiveFactSet(), o2.EffectiveFactSet()) {
		t.Fatalf("effective set is order/dup sensitive: %v vs %v",
			o1.EffectiveFactSet(), o2.EffectiveFactSet())
	}
}

// TestEffectiveByLogical_DeltaShadowsBase: a delta revision of a base fact wins on
// its LogicalID whether or not the old version is also tombstoned.
func TestEffectiveByLogical_DeltaShadowsBase(t *testing.T) {
	s := newStore(t)
	base := appendObl(t, s, "O-1", "base version")
	changed := appendObl(t, s, "O-1", "changed version") // same LogicalID, new content
	other := appendObl(t, s, "O-2", "unchanged")

	// Change recorded as delta only (no tombstone): delta must still shadow base.
	o := flg.Overlay{Base: []record.Digest{base, other}, Delta: []record.Digest{changed}}
	eff, err := o.EffectiveByLogical(s)
	if err != nil {
		t.Fatal(err)
	}
	if eff["O-1"] != changed {
		t.Fatalf("delta revision should shadow base for O-1: got %s want %s", eff["O-1"], changed)
	}
	if eff["O-2"] != other {
		t.Fatalf("unchanged fact O-2 wrong: got %s want %s", eff["O-2"], other)
	}

	// Same change recorded with an explicit tombstone: identical projection.
	oTomb := flg.Overlay{
		Base: []record.Digest{base, other}, Delta: []record.Digest{changed},
		Tombstones: []record.Digest{base},
	}
	effTomb, err := oTomb.EffectiveByLogical(s)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(eff, effTomb) {
		t.Fatalf("delta-only and tombstoned change should project identically: %v vs %v", eff, effTomb)
	}
}
