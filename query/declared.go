package query

import (
	"encoding/json"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/record"
)

// declared.go is declared query sets (system design §10.3): a NAMED, VERSIONED
// program of fixed-verb queries whose result is pinned to a projection_hash.
// This is the shape gate evidence takes (§7.5) — free-text retrieval is the wrong
// shape for a gate; a declared set is deterministic and reproducible.
//
// The three affordances of §10.3, all here:
//
//   - Declared query sets — a fixed-verb program (no free text; the verbs are an
//     enum), run over a projection, producing a canonically-serialized result.
//   - Projection-hash pinning — the result binds to the index's ProjectionHash.
//     The SAME preserved inputs yield BYTE-IDENTICAL bytes and hash; a single new
//     fact changes the projection hash, so the pin (and the bytes) change.
//   - Canonical-vs-inferred labelling — every hop carries provenance; Proof()
//     refuses inferred edges, so a gate can never cite a heuristic as evidence.

// Verb is the fixed vocabulary a declared query set may use. There is no free-text
// verb: a gate program is a finite, auditable set of traversals.
type Verb string

const (
	VerbAudit    Verb = "audit"
	VerbImpact   Verb = "impact"
	VerbReplay   Verb = "replay"
	VerbRetrieve Verb = "retrieve"
)

// Step is one fixed-verb query. Seed is the node a traversal starts at; Query and
// Limit apply to VerbRetrieve; MaxHops bounds a traversal (0 = default).
type Step struct {
	Verb    Verb   `json:"verb"`
	Seed    NodeID `json:"seed,omitempty"`
	MaxHops int    `json:"max_hops,omitempty"`
	Query   string `json:"query,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

// QuerySet is a named, versioned program — e.g. `gate-context` / `v1` (§10.3).
type QuerySet struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Steps   []Step `json:"steps"`
}

// ProgramHash digests the program identity + steps, so a change to the program is
// a different pin even against the same projection. Deterministic: Go marshals
// struct fields in declaration order and map keys sorted, and QuerySet holds only
// slices/scalars, so the bytes are stable.
func (qs QuerySet) ProgramHash() record.Digest {
	b, _ := json.Marshal(qs)
	return record.ComputeDigest(b)
}

// provEdge is an edge as it appears in a result: endpoints, kind, and provenance.
type provEdge struct {
	From   NodeID     `json:"from"`
	To     NodeID     `json:"to"`
	Kind   EdgeKind   `json:"kind"`
	Prov   Provenance `json:"prov"`
	Reason string     `json:"reason,omitempty"`
}

func provEdges(edges []Edge) []provEdge {
	out := make([]provEdge, 0, len(edges))
	for _, e := range edges {
		out = append(out, provEdge{From: e.From, To: e.To, Kind: e.Kind, Prov: e.Prov, Reason: e.Reason})
	}
	return out
}

// StepResult is one step's deterministic output. It records node ids (sorted),
// every edge with its provenance, and — for retrieval — the ranked item ids in
// order. It deliberately carries NO float scores: ranks (an ordering of ids) are
// byte-stable where raw floats would risk representation drift, so the pin stays
// reproducible.
type StepResult struct {
	Verb   Verb       `json:"verb"`
	Seed   NodeID     `json:"seed,omitempty"`
	Query  string     `json:"query,omitempty"`
	Nodes  []NodeID   `json:"nodes"`
	Edges  []provEdge `json:"edges"`
	Ranked []NodeID   `json:"ranked,omitempty"`
}

// PinnedResult is a declared query set's result, pinned to the projection it ran
// against. Bytes is the canonical serialization; ResultHash digests it. Two runs
// over the same projection produce identical Bytes, ResultHash and ProjectionHash.
type PinnedResult struct {
	Program        string
	Version        string
	ProgramHash    record.Digest
	ProjectionHash record.Digest
	ResultHash     record.Digest
	Steps          []StepResult
	Bytes          []byte
}

// pinnedEnvelope is the exact structure that gets serialized to Bytes. The
// projection hash is INSIDE it, so any projection change flips ResultHash even if
// a traversal happened not to reach the new fact — the result is pinned to the
// projection, per §10.3.
type pinnedEnvelope struct {
	Program        string       `json:"program"`
	Version        string       `json:"version"`
	ProgramHash    string       `json:"program_hash"`
	ProjectionHash string       `json:"projection_hash"`
	Steps          []StepResult `json:"steps"`
}

// Run executes a declared query set over the index, returning a projection-pinned,
// canonically-serialized result. Deterministic by construction: every traversal
// is ordered, every node/edge slice is sorted, and the envelope is marshaled with
// encoding/json (struct order preserved, map keys sorted). Same projection →
// byte-identical Bytes; a new fact → a new ProjectionHash and new Bytes.
func Run(ix GraphIndexPort, qs QuerySet) PinnedResult {
	steps := make([]StepResult, 0, len(qs.Steps))
	for _, s := range qs.Steps {
		steps = append(steps, runStep(ix, s))
	}
	env := pinnedEnvelope{
		Program:        qs.Name,
		Version:        qs.Version,
		ProgramHash:    string(qs.ProgramHash()),
		ProjectionHash: string(ix.ProjectionHash()),
		Steps:          steps,
	}
	// json.Marshal is deterministic here: struct fields keep declaration order and
	// there are no maps in the envelope, so the same inputs give identical bytes.
	bytes, _ := json.Marshal(env)
	return PinnedResult{
		Program:        qs.Name,
		Version:        qs.Version,
		ProgramHash:    qs.ProgramHash(),
		ProjectionHash: ix.ProjectionHash(),
		ResultHash:     record.ComputeDigest(bytes),
		Steps:          steps,
		Bytes:          bytes,
	}
}

func runStep(ix GraphIndexPort, s Step) StepResult {
	switch s.Verb {
	case VerbAudit:
		a := Audit(ix, s.Seed, s.MaxHops)
		return StepResult{Verb: s.Verb, Seed: s.Seed, Nodes: a.Nodes, Edges: provEdges(a.Edges)}
	case VerbImpact:
		r := Impact(ix, s.Seed, s.MaxHops)
		return StepResult{Verb: s.Verb, Seed: s.Seed, Nodes: r.Touched, Edges: provEdges(r.Edges)}
	case VerbReplay:
		r := Replay(ix, s.Seed, s.MaxHops)
		nodes := append([]NodeID(nil), r.Chain...)
		sortIDs(nodes)
		return StepResult{Verb: s.Verb, Seed: s.Seed, Nodes: nodes, Edges: provEdges(r.Edges)}
	case VerbRetrieve:
		var sups []flg.SupersessionEvidence
		if snapshot, ok := ix.(interface {
			Supersessions() []flg.SupersessionEvidence
		}); ok {
			sups = snapshot.Supersessions()
		}
		res := Retrieve(ix, sups, s.Query, s.Limit)
		ranked := make([]NodeID, 0, len(res.Governing))
		nodes := make([]NodeID, 0, len(res.Governing))
		var edges []provEdge
		for _, it := range res.Governing {
			ranked = append(ranked, it.ID)
			nodes = append(nodes, it.ID)
			// carry the provenance the item was reached with, so the gate can see
			// an inferred-reached hit even at the declared-query layer.
			edges = append(edges, provEdge{From: s.Seed, To: it.ID, Kind: "retrieval", Prov: it.Prov, Reason: string(it.State)})
		}
		sortIDs(nodes)
		return StepResult{Verb: s.Verb, Query: s.Query, Nodes: nodes, Edges: edges, Ranked: ranked}
	default:
		return StepResult{Verb: s.Verb, Seed: s.Seed, Nodes: []NodeID{}, Edges: []provEdge{}}
	}
}

// --- the canonical-vs-inferred gate -----------------------------------------

// Proof splits a pinned result's edges into the canonical set (admissible as
// evidence) and the refused inferred set. A gate cites ProofEdges; it REFUSES to
// cite anything in Refused (§10.3: "a gate refuses inferred edges as proof").
type Proof struct {
	ProofEdges []provEdge
	Refused    []provEdge
}

// Gate applies the canonical-vs-inferred rule across every step's edges.
func (p PinnedResult) Gate() Proof {
	var out Proof
	for _, st := range p.Steps {
		for _, e := range st.Edges {
			if e.Prov == Canonical {
				out.ProofEdges = append(out.ProofEdges, e)
			} else {
				out.Refused = append(out.Refused, e)
			}
		}
	}
	return out
}

// RefusesInferred reports whether the gate had to refuse any inferred edge — true
// when the program's result crossed a heuristic hop that may not stand as proof.
func (p PinnedResult) RefusesInferred() bool { return len(p.Gate().Refused) > 0 }
