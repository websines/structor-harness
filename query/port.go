package query

import "github.com/websines/structor-harness/record"

// port.go defines GraphIndexPort — THE read seam over the FLG (system design
// §4.8, §10.2). The engine talks to this port, never to SQL. Two production
// backends implement it without changing a single caller:
//
//   - Recursive-CTE-over-Postgres (baseline): the §5.5 tables are the source
//     projection. Nodes() is `SELECT id FROM entities WHERE (workspace,ref)=…`;
//     Out(id)/In(id) are single-row edge lookups; a full traversal is one
//     `WITH RECURSIVE walk AS (… UNION ALL … JOIN edges …)`. This backend is
//     also the conformance ORACLE — the in-memory impl (index.go) must return
//     byte-identical traversals against it.
//   - Graph index (optional, e.g. a pgGraph candidate): the same port, backed by
//     a derived traversal structure, qualified in later. Strictly optional; the
//     tables remain the rebuildable source, so it can never become load-bearing.
//
// Because the port hides the backend, this package ships the in-memory impl now
// and names the CTE swap here; nothing above the port learns which one it got.

// NodeID is a stable identity for a graph node — an obligation id, evidence id,
// lemma/theorem id, or a supersession fact address. It is the logical id of a
// fact, never a clock or row number.
type NodeID string

// NodeKind tags what a node is (a subset of the §5.2 node types this layer walks).
type NodeKind string

const (
	KindTheorem      NodeKind = "theorem"
	KindLemma        NodeKind = "lemma"
	KindObligation   NodeKind = "obligation"
	KindEvidence     NodeKind = "evidence"
	KindSupersession NodeKind = "supersession"
)

// EdgeKind tags a graph edge (the §5.3 edge vocabulary this layer traverses).
type EdgeKind string

const (
	// EdgeDecomposesTo is the spec spine: Theorem→Lemma→Obligation. Canonical.
	EdgeDecomposesTo EdgeKind = "decomposes_to"
	// EdgeSupports / EdgeFalsifies: Evidence→Obligation, by polarity. Canonical.
	EdgeSupports  EdgeKind = "supports"
	EdgeFalsifies EdgeKind = "falsifies"
	// EdgeSupersedes is THE spine (§5.3): any→any, carrying a supersession kind.
	// It points newer→older (a replacement / a supersession fact → the subject it
	// demotes), so a forward walk over it is a walk backward in time. Canonical.
	EdgeSupersedes EdgeKind = "supersedes"
	// EdgeCoAnchored is an INFERRED structural link between obligations that share
	// a code anchor file. It is a heuristic, not a recorded fact — present so the
	// canonical-vs-inferred gate (§10.3) has something to refuse as proof.
	EdgeCoAnchored EdgeKind = "co_anchored"
)

// Provenance is the canonical-vs-inferred label every hop carries (§10.3). Only a
// canonical edge — one recorded in the append-only FLG — is admissible as proof.
// An inferred edge may inform retrieval but a gate refuses it as evidence.
type Provenance string

const (
	// Canonical: the edge is a stored fact (decomposes-to from the obligation's
	// declared lemma, supports/falsifies from the evidence, supersedes from a
	// SupersessionEvidence). Admissible as proof.
	Canonical Provenance = "canonical"
	// Inferred: the edge was derived heuristically (co-anchoring, graph
	// expansion). NEVER admissible as proof; a gate refuses it.
	Inferred Provenance = "inferred"
)

// Node is a vertex in the FLG projection. Path/Commit pin it to a concrete
// source location so every retrieved item resolves to a real path@commit (§10.4).
type Node struct {
	ID   NodeID
	Kind NodeKind
	// Text is the lexical document for retrieval (title / summary / tags).
	Text string
	// Path is the anchor file the node lives at; Commit pins it, when known.
	Path   string
	Commit string
	// FactID is the append-only fact address this node projects from, when it has
	// one (supersession facts do; obligations/evidence are named by their id).
	FactID record.Digest
}

// Edge is one directed, provenance-labelled hop. Reason carries the supersession
// kind or decomposition rationale, so an audit hop is self-describing.
type Edge struct {
	From   NodeID
	To     NodeID
	Kind   EdgeKind
	Prov   Provenance
	Reason string
}

// GraphIndexPort is the read abstraction over the FLG (§10.2). Every method is
// deterministic and clock-free: Nodes() and every adjacency list come back in a
// fixed order, so a traversal over the port is reproducible regardless of
// backend. The recursive-CTE and graph-index backends both satisfy this contract.
type GraphIndexPort interface {
	// Node returns the node for id and whether it is present.
	Node(id NodeID) (Node, bool)
	// Nodes returns every node id, sorted — the retrieval / PageRank universe.
	Nodes() []NodeID
	// Out returns id's outgoing edges in deterministic order.
	Out(id NodeID) []Edge
	// In returns id's incoming edges in deterministic order.
	In(id NodeID) []Edge
	// Edges returns every edge in deterministic order (PageRank / expansion input).
	Edges() []Edge
	// ProjectionHash is the digest every declared-query result is pinned to
	// (§10.3): the single value evidence binds to, and the drift detector.
	ProjectionHash() record.Digest
}
