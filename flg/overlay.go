// overlay.go is the overlay-ref algebra (system design §5 "Team overlays", §12.1):
// a personal branch graph over a commit-pinned base. An overlay owns no truth of
// its own — it names a base fact set and records only its DELTA (the facts it adds
// or changes) and its TOMBSTONES (the base facts it hides). Its governing fact set
// is a pure function of those three address sets: base minus tombstones plus delta.
//
// LAW 3 (no recency): the effective set is a function of WHICH facts are present,
// never of when they arrived — there is no clock or ordering input here, so a
// resolver reading an overlay can never key on recency. Deterministic: every output
// is content-address sorted, so the same overlay always yields the same fact set.
package flg

import (
	"encoding/json"
	"sort"

	"github.com/websines/structor-harness/record"
)

// Overlay is a personal branch graph over a commit-pinned base fact set. Base,
// Delta and Tombstones are sets of fact addresses (fact_ids) that resolve against
// one flg.Store. A change to a base fact is a Delta fact carrying that fact's
// LogicalID; a deletion is a Tombstone naming the base fact's address. A plain
// base fact set (no delta, no tombstones) is just an Overlay with only Base set.
type Overlay struct {
	// Base is the inherited fact set — the commit-pinned base graph.
	Base []record.Digest
	// Delta is the facts this overlay adds or changes (new revisions).
	Delta []record.Digest
	// Tombstones are base facts hidden in this overlay (deletions), by address.
	Tombstones []record.Digest
}

// EffectiveFactSet returns the overlay's governing fact addresses: base minus
// tombstones plus delta, deduplicated and content-address sorted. Pure and
// clock-free — the same overlay always yields the identical set (system design §5).
func (o Overlay) EffectiveFactSet() []record.Digest {
	tomb := digestSet(o.Tombstones)
	set := make(map[record.Digest]bool, len(o.Base)+len(o.Delta))
	for _, d := range o.Base {
		if !tomb[d] {
			set[d] = true // a base fact survives unless a tombstone hides it
		}
	}
	for _, d := range o.Delta {
		set[d] = true // delta adds the new / changed facts on top of the base
	}
	return sortedDigests(set)
}

// EffectiveByLogical projects the effective fact set to one fact per LogicalID —
// the shape compare_refs reasons over (system design §8 Rule 5). Two overlays that
// carry a byte-identical revision of a logical fact map it to the SAME fact_id,
// because the stored envelope excludes ref and clock (store.go, Law 3): the fact
// address is ref-independent, so a fact inherited unchanged reads identically on
// every overlay. When a base fact and a delta fact share a LogicalID (a change
// recorded without an explicit tombstone) the delta revision shadows the base one;
// any residual tie breaks on the smaller address, so the projection is deterministic.
func (o Overlay) EffectiveByLogical(s *Store) (map[string]record.Digest, error) {
	inDelta := digestSet(o.Delta)
	out := map[string]record.Digest{}
	for _, d := range o.EffectiveFactSet() {
		lid, err := s.logicalID(d)
		if err != nil {
			return nil, err
		}
		cur, seen := out[lid]
		if !seen {
			out[lid] = d
			continue
		}
		// Same logical fact reached twice: delta shadows base; else smaller address wins.
		switch {
		case inDelta[d] && !inDelta[cur]:
			out[lid] = d
		case inDelta[d] == inDelta[cur] && d < cur:
			out[lid] = d
		}
	}
	return out, nil
}

// logicalID reads the LogicalID of a stored fact by its address. It decodes the
// on-CAS envelope (store.go storedFact) directly: the fact address is ref- and
// clock-independent, so the LogicalID it yields is stable across overlays.
func (s *Store) logicalID(d record.Digest) (string, error) {
	b, err := s.cas.Get(d)
	if err != nil {
		return "", err
	}
	var f storedFact
	if err := json.Unmarshal(b, &f); err != nil {
		return "", err
	}
	return f.LogicalID, nil
}

// digestSet collects addresses into a membership set (dedup + O(1) lookup).
func digestSet(in []record.Digest) map[record.Digest]bool {
	set := make(map[record.Digest]bool, len(in))
	for _, d := range in {
		set[d] = true
	}
	return set
}

// sortedDigests returns the set's members in content-address order — the sort that
// makes every overlay output deterministic (no clock, no insertion order).
func sortedDigests(set map[record.Digest]bool) []record.Digest {
	out := make([]record.Digest, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
