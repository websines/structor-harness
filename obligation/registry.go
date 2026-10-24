// This file implements the obligation registry (system design §4.3): a pure,
// in-memory catalogue of leaf obligations the rest of M5 reads. It holds NO
// mutable "status" column — discharge state is a derivation the resolver
// computes (§8, Law 3), never a field written here. The registry answers only
// two questions: "which obligations exist?" and "which does a change touch?"
package obligation

import (
	"errors"
	"fmt"
	"sort"

	"github.com/websines/structor-harness/flg"
)

// Registry is an in-memory set of registered obligations, indexed for the two
// lookups M5 needs: by id (to join a verdict back to its leaf) and by domain tag
// (scope-to-the-change). Pure state — no clock, no I/O, no derived status.
type Registry struct {
	byID  map[string]flg.Obligation
	byTag map[string]map[string]bool // tag -> set of obligation ids
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		byID:  map[string]flg.Obligation{},
		byTag: map[string]map[string]bool{},
	}
}

// ErrEmptyID and ErrDuplicate name the two ways a registration is refused. An
// obligation with no id cannot be joined to a verdict; a duplicate id would let
// one leaf silently shadow another.
var (
	ErrEmptyID   = errors.New("obligation: cannot register an obligation with an empty id")
	ErrDuplicate = errors.New("obligation: an obligation with this id is already registered")
)

// Register adds obl to the registry. It refuses an empty id and a duplicate id
// rather than overwriting — a new obligation version is a new id, never an
// in-place mutation (§4.3: obligations are versioned alongside code).
func (r *Registry) Register(obl flg.Obligation) error {
	if obl.ID == "" {
		return ErrEmptyID
	}
	if _, ok := r.byID[obl.ID]; ok {
		return fmt.Errorf("%w: %s", ErrDuplicate, obl.ID)
	}
	r.byID[obl.ID] = obl
	for _, t := range obl.DomainTags {
		if r.byTag[t] == nil {
			r.byTag[t] = map[string]bool{}
		}
		r.byTag[t][obl.ID] = true
	}
	return nil
}

// Lookup returns the obligation with id and whether it was registered.
func (r *Registry) Lookup(id string) (flg.Obligation, bool) {
	o, ok := r.byID[id]
	return o, ok
}

// Len is the number of registered obligations.
func (r *Registry) Len() int { return len(r.byID) }

// All returns every registered obligation, sorted by id so every read is stable.
func (r *Registry) All() []flg.Obligation {
	out := make([]flg.Obligation, 0, len(r.byID))
	for _, o := range r.byID {
		out = append(out, o)
	}
	sortObligations(out)
	return out
}

// ByTag returns every obligation whose declared domain includes tag, sorted by
// id. This is the association the scope selector rides (§4.3 scope-to-the-change).
func (r *Registry) ByTag(tag string) []flg.Obligation {
	ids := r.byTag[tag]
	out := make([]flg.Obligation, 0, len(ids))
	for id := range ids {
		out = append(out, r.byID[id])
	}
	sortObligations(out)
	return out
}

// sortObligations orders obligations by id so every registry read is byte-stable.
func sortObligations(os []flg.Obligation) {
	sort.Slice(os, func(i, j int) bool { return os[i].ID < os[j].ID })
}
