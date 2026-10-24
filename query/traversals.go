package query

import "sort"

// traversals.go is the three hot traversals (system design §10.1) over a
// GraphIndexPort. All three are bounded-hop and deterministic: neighbours are
// visited in a fixed order, every returned slice is sorted, and no timestamp is
// an input. Over the CTE backend each is one `WITH RECURSIVE` walk; here they are
// the same walk in memory.
//
//	Audit  — Theorem→Lemma→Obligation→Evidence + EVERY supersedes edge. The
//	         lineage, for "show me the state on the day of the incident" (§7.6).
//	Impact — from a changed spec fact / obligation, the SET of everything it
//	         touches. The drift blast radius stamped on a spec change (§7.3).
//	Replay — the supersession chain backward from a point (§9): what replaced what.

// DefaultMaxHops bounds a traversal when a caller passes 0. Audit is quoted at
// 5–6 hops across years (§10.1); we allow a little headroom.
const DefaultMaxHops = 8

func hops(maxHops int) int {
	if maxHops <= 0 {
		return DefaultMaxHops
	}
	return maxHops
}

// step is one reachable neighbour and the real recorded edge that reaches it.
// Keeping the true edge (not a synthesized one) means every hop reports its
// actual provenance, so the canonical-vs-inferred gate sees the real label.
type step struct {
	to  NodeID
	via Edge
}

// lineageDown yields the lineage BELOW a node: its decomposition children
// (decomposes-to, downward) and the evidence that witnesses it (supports /
// falsifies, which point evidence→obligation, so we read them incoming). This is
// the shared spine of Audit and Impact.
func lineageDown(ix GraphIndexPort, id NodeID) []step {
	var steps []step
	for _, e := range ix.Out(id) {
		if e.Kind == EdgeDecomposesTo {
			steps = append(steps, step{to: e.To, via: e})
		}
	}
	for _, e := range ix.In(id) {
		if e.Kind == EdgeSupports || e.Kind == EdgeFalsifies {
			steps = append(steps, step{to: e.From, via: e})
		}
	}
	return steps
}

// bfs walks from seed using expand, up to maxHops, visiting neighbours in sorted
// order. It returns the visited ids in BFS order (seed first) and every edge
// traversed, both deterministic. A node is visited once, at its shortest hop.
func bfs(ix GraphIndexPort, seed NodeID, maxHops int, expand func(GraphIndexPort, NodeID) []step) (order []NodeID, edges []Edge) {
	limit := hops(maxHops)
	seen := map[NodeID]bool{seed: true}
	order = []NodeID{seed}
	frontier := []NodeID{seed}
	for depth := 0; depth < limit && len(frontier) > 0; depth++ {
		var next []NodeID
		for _, cur := range frontier {
			steps := expand(ix, cur)
			sort.Slice(steps, func(i, j int) bool {
				if steps[i].to != steps[j].to {
					return steps[i].to < steps[j].to
				}
				return steps[i].via.Kind < steps[j].via.Kind
			})
			for _, s := range steps {
				edges = append(edges, s.via)
				if seen[s.to] {
					continue
				}
				seen[s.to] = true
				order = append(order, s.to)
				next = append(next, s.to)
			}
		}
		frontier = next
	}
	return order, dedupEdges(edges)
}

// --- Audit ------------------------------------------------------------------

// AuditResult is the lineage of a root fact: the nodes on it, every edge walked
// (decomposition, evidence AND every supersedes edge incident to the lineage),
// and the deepest hop reached. Nodes and Edges are sorted for a stable render.
type AuditResult struct {
	Root  NodeID
	Nodes []NodeID
	Edges []Edge
}

// auditExpand walks the lineage down AND across every supersedes edge touching a
// node, so "every supersedes edge" (§7.6) is on the result and the superseding
// facts / replacement heads are themselves reached.
func auditExpand(ix GraphIndexPort, id NodeID) []step {
	steps := lineageDown(ix, id)
	for _, e := range ix.Out(id) {
		if e.Kind == EdgeSupersedes {
			steps = append(steps, step{to: e.To, via: e})
		}
	}
	for _, e := range ix.In(id) {
		if e.Kind == EdgeSupersedes {
			steps = append(steps, step{to: e.From, via: e})
		}
	}
	return steps
}

// Audit walks the full lineage of root (Theorem→Lemma→Obligation→Evidence) and
// includes every supersedes edge on it. It is the §7.6 audit query: seconds, with
// an auditor in the room, every hop's provenance visible.
func Audit(ix GraphIndexPort, root NodeID, maxHops int) AuditResult {
	order, edges := bfs(ix, root, maxHops, auditExpand)
	nodes := append([]NodeID(nil), order...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i] < nodes[j] })
	return AuditResult{Root: root, Nodes: nodes, Edges: edges}
}

// SupersedesEdges returns just the supersedes edges on the lineage — the "every
// supersedes edge included" guarantee, isolated for a caller that wants the spine.
func (a AuditResult) SupersedesEdges() []Edge {
	var out []Edge
	for _, e := range a.Edges {
		if e.Kind == EdgeSupersedes {
			out = append(out, e)
		}
	}
	return out
}

// --- Impact -----------------------------------------------------------------

// ImpactResult is the drift blast radius of a change: every obligation and piece
// of evidence the changed fact touches (§7.3). Touched is sorted and excludes the
// changed node itself.
type ImpactResult struct {
	Changed NodeID
	Touched []NodeID
	Edges   []Edge
}

// Impact is the reverse (drift) traversal: from a changed spec fact / obligation,
// walk decomposition down to obligations and follow each obligation's evidence,
// returning the SET of everything touched. This is the set that receives
// cross_type_invalidation on a spec change (§7.3) — the entire drift story.
func Impact(ix GraphIndexPort, changed NodeID, maxHops int) ImpactResult {
	order, edges := bfs(ix, changed, maxHops, lineageDown)
	touched := make([]NodeID, 0, len(order))
	for _, id := range order {
		if id != changed {
			touched = append(touched, id)
		}
	}
	sort.Slice(touched, func(i, j int) bool { return touched[i] < touched[j] })
	return ImpactResult{Changed: changed, Touched: touched, Edges: edges}
}

// TouchedOfKind filters the blast radius to one node kind (e.g. only obligations).
func (r ImpactResult) TouchedOfKind(ix GraphIndexPort, kind NodeKind) []NodeID {
	var out []NodeID
	for _, id := range r.Touched {
		if n, ok := ix.Node(id); ok && n.Kind == kind {
			out = append(out, id)
		}
	}
	return out
}

// --- Replay -----------------------------------------------------------------

// ReplayResult is the supersession chain backward from a point: the nodes in
// order from the given point back through what it replaced, and the supersedes
// edges walked. Chain[0] is the starting point.
type ReplayResult struct {
	From  NodeID
	Chain []NodeID
	Edges []Edge
}

// replayExpand follows supersedes edges in the newer→older direction: an active
// head's Out supersedes edge reaches the subject it demoted, and so on down the
// chain.
func replayExpand(ix GraphIndexPort, id NodeID) []step {
	var steps []step
	for _, e := range ix.Out(id) {
		if e.Kind == EdgeSupersedes {
			steps = append(steps, step{to: e.To, via: e})
		}
	}
	return steps
}

// Replay walks supersession chains backward from a point (§9): from a fact that
// governs now, follow the supersedes spine to what it replaced, bounded and
// ordered. Chain preserves discovery order (newest → oldest); Edges are the
// supersedes hops with their kinds.
func Replay(ix GraphIndexPort, from NodeID, maxHops int) ReplayResult {
	order, edges := bfs(ix, from, maxHops, replayExpand)
	return ReplayResult{From: from, Chain: order, Edges: edges}
}

// --- helpers ----------------------------------------------------------------

// dedupEdges removes repeated edges (a node reached twice re-reports its edge)
// and returns them in the sorted order the port already guarantees.
func dedupEdges(edges []Edge) []Edge {
	seen := map[Edge]bool{}
	var out []Edge
	for _, e := range edges {
		if seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		if out[i].To != out[j].To {
			return out[i].To < out[j].To
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}
