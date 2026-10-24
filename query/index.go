package query

import (
	"sort"
	"strings"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/record"
)

// index.go is the in-memory GraphIndexPort (port.go): it materializes the whole
// FLG projection in process and answers every read from maps. It is the
// reference the recursive-CTE backend is a conformance swap for — small graphs
// traverse here, large ones behind the same port over Postgres.
//
// The graph is derived PURELY from a flg.Store projection (obligations, evidence,
// supersessions) plus an optional Spine (the theorem→lemma structure the current
// projection cannot yet carry). No clock, no ordering input: the same projection
// always builds the same graph, and ProjectionHash pins it.

// Spine is the canonical decomposition the flg projection does not yet encode:
// an Obligation carries its LemmaID, but nothing above the lemma. Spec ingestion
// supplies theorem grouping here (Theorem→Lemma decomposes-to). It is OPTIONAL —
// with no Spine the lemma is the top of the visible lineage.
type Spine struct {
	// LemmaToTheorem maps a lemma id to its parent theorem id (decomposes-to).
	LemmaToTheorem map[string]string
	// TheoremText gives a theorem node its lexical/display text, by theorem id.
	TheoremText map[string]string
	// LemmaText gives a lemma node its text, by lemma id (else the id is used).
	LemmaText map[string]string
}

// InMemoryIndex implements GraphIndexPort over an in-process projection.
type InMemoryIndex struct {
	nodes         map[NodeID]Node
	out           map[NodeID][]Edge
	in            map[NodeID][]Edge
	all           []Edge
	ids           []NodeID
	ph            record.Digest
	supersessions []flg.SupersessionEvidence
}

var _ GraphIndexPort = (*InMemoryIndex)(nil)

// FromStore projects a flg.Store and builds the in-memory index. The projection
// hash it pins to is the store's own ProjectionHash — the value declared queries
// bind their results to (§10.3). spine may be nil.
func FromStore(s *flg.Store, spine *Spine) (*InMemoryIndex, error) {
	obls, evs, sups, err := s.Project()
	if err != nil {
		return nil, err
	}
	return FromProjection(obls, evs, sups, s.ProjectionHash(), spine), nil
}

// FromProjection builds the index from an already-derived projection and the
// projection hash it is pinned to. This is the path a test (or a replayed
// checkpoint) uses to build over a fixed fact set.
func FromProjection(obls []flg.Obligation, evs []flg.Evidence, sups []flg.SupersessionEvidence, ph record.Digest, spine *Spine) *InMemoryIndex {
	b := &builder{
		ix: &InMemoryIndex{
			nodes: map[NodeID]Node{},
			out:   map[NodeID][]Edge{},
			in:    map[NodeID][]Edge{},
			ph:    ph,
		},
		anchorOf: map[NodeID]string{},
	}
	b.addObligations(obls, spine)
	b.addEvidence(evs)
	b.ix.supersessions = cloneSupersessions(sups)
	b.addSupersessions(sups)
	b.addInferredCoAnchors(obls)
	b.freeze()
	return b.ix
}

type builder struct {
	ix       *InMemoryIndex
	anchorOf map[NodeID]string // obligation id -> anchor file, for evidence citations
}

func (b *builder) node(id NodeID, kind NodeKind, text, path, commit string, fact record.Digest) {
	// A node may be referenced before it is defined (e.g. a lemma named by an
	// obligation). The first concrete definition wins; a placeholder never
	// overwrites real metadata.
	if existing, ok := b.ix.nodes[id]; ok && existing.Kind != "" && kind == "" {
		return
	}
	if existing, ok := b.ix.nodes[id]; ok && existing.Text != "" && text == "" {
		return
	}
	b.ix.nodes[id] = Node{ID: id, Kind: kind, Text: text, Path: path, Commit: commit, FactID: fact}
}

func (b *builder) edge(from, to NodeID, kind EdgeKind, prov Provenance, reason string) {
	e := Edge{From: from, To: to, Kind: kind, Prov: prov, Reason: reason}
	b.ix.out[from] = append(b.ix.out[from], e)
	b.ix.in[to] = append(b.ix.in[to], e)
	b.ix.all = append(b.ix.all, e)
}

func (b *builder) addObligations(obls []flg.Obligation, spine *Spine) {
	// Resolve concrete definitions before parent placeholders, regardless of delivery order.
	for _, o := range obls {
		var text strings.Builder
		text.WriteString(o.Title)
		if o.Anchor.Symbol != "" {
			text.WriteString(" ")
			text.WriteString(o.Anchor.Symbol)
		}
		for _, tag := range o.DomainTags {
			text.WriteString(" ")
			text.WriteString(tag)
		}
		b.node(NodeID(o.ID), KindObligation, text.String(), o.Anchor.File, "", "")
	}
	for _, o := range obls {
		oid := NodeID(o.ID)
		text := o.Title
		if o.Anchor.Symbol != "" {
			text += " " + o.Anchor.Symbol
		}
		for _, t := range o.DomainTags {
			text += " " + t
		}
		b.node(oid, KindObligation, text, o.Anchor.File, "", "")
		b.anchorOf[oid] = o.Anchor.File
		if o.LemmaID == "" {
			continue
		}
		lid := NodeID(o.LemmaID)
		ltext := o.LemmaID
		if spine != nil {
			if t, ok := spine.LemmaText[o.LemmaID]; ok {
				ltext = t
			}
		}
		if _, defined := b.ix.nodes[lid]; !defined {
			b.node(lid, KindLemma, ltext, "", "", "")
		}
		// A lemma decomposes-to its obligation — declared spec structure, canonical.
		b.edge(lid, oid, EdgeDecomposesTo, Canonical, "lemma decomposes to obligation")
		if spine != nil {
			if tid, ok := spine.LemmaToTheorem[o.LemmaID]; ok && tid != "" {
				thid := NodeID(tid)
				ttext := tid
				if t, ok := spine.TheoremText[tid]; ok {
					ttext = t
				}
				b.node(thid, KindTheorem, ttext, "", "", "")
				b.edge(thid, lid, EdgeDecomposesTo, Canonical, "theorem decomposes to lemma")
			}
		}
	}
}

func (b *builder) addEvidence(evs []flg.Evidence) {
	for _, e := range evs {
		eid := NodeID(e.ID)
		path := b.anchorOf[NodeID(e.ObligationID)] // the code the witness lands on
		b.node(eid, KindEvidence, e.Summary, path, e.Commit, "")
		kind := EdgeSupports
		if e.Polarity == flg.PolarityFalsifies {
			kind = EdgeFalsifies
		}
		// Evidence→Obligation is declared on the evidence fact — canonical.
		b.edge(eid, NodeID(e.ObligationID), kind, Canonical, string(e.Method))
	}
}

func (b *builder) addSupersessions(sups []flg.SupersessionEvidence) {
	for _, s := range sups {
		// The supersession fact itself is a node (its citations are its text), so
		// an audit walk can show WHAT demoted a fact, not merely that it was.
		sid := NodeID(s.ID)
		if sid == "" {
			sid = NodeID("sup:" + s.Subject) // anonymous conformance vector fallback
		}
		text := string(s.Kind)
		for _, c := range s.Evidence {
			text += " " + c
		}
		b.node(sid, KindSupersession, text, "", "", record.Digest(s.ID))
		// The supersession fact supersedes its subject — the spine edge, canonical.
		b.edge(sid, NodeID(s.Subject), EdgeSupersedes, Canonical, string(s.Kind))
		// Each named replacement head also supersedes the subject: the new fact is
		// what governs now. This is the edge replay walks backward in time.
		for _, head := range s.ReplacedBy {
			b.edge(NodeID(head), NodeID(s.Subject), EdgeSupersedes, Canonical, string(s.Kind))
		}
	}
}

// addInferredCoAnchors links obligations that share an anchor file with an
// INFERRED co_anchored edge. It is a heuristic (co-location is not a recorded
// relation), so it is labelled Inferred and a gate refuses it as proof. Within a
// file group (sorted by id) the first obligation is linked to each of the rest —
// a deterministic star, not an O(n^2) clique.
func (b *builder) addInferredCoAnchors(obls []flg.Obligation) {
	byFile := map[string][]string{}
	for _, o := range obls {
		if o.Anchor.File == "" {
			continue
		}
		byFile[o.Anchor.File] = append(byFile[o.Anchor.File], o.ID)
	}
	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		group := byFile[f]
		if len(group) < 2 {
			continue
		}
		sort.Strings(group)
		hub := group[0]
		for _, other := range group[1:] {
			b.edge(NodeID(hub), NodeID(other), EdgeCoAnchored, Inferred, "shares anchor file "+f)
			b.edge(NodeID(other), NodeID(hub), EdgeCoAnchored, Inferred, "shares anchor file "+f)
		}
	}
}

// freeze sorts every adjacency list and the id/edge universes into a fixed order,
// so the port's reads are deterministic irrespective of build order.
func (b *builder) freeze() {
	less := func(a, c Edge) bool {
		if a.From != c.From {
			return a.From < c.From
		}
		if a.To != c.To {
			return a.To < c.To
		}
		if a.Kind != c.Kind {
			return a.Kind < c.Kind
		}
		return a.Reason < c.Reason
	}
	for id := range b.ix.out {
		es := b.ix.out[id]
		sort.Slice(es, func(i, j int) bool { return less(es[i], es[j]) })
	}
	for id := range b.ix.in {
		es := b.ix.in[id]
		sort.Slice(es, func(i, j int) bool { return less(es[i], es[j]) })
	}
	sort.Slice(b.ix.all, func(i, j int) bool { return less(b.ix.all[i], b.ix.all[j]) })
	b.ix.ids = make([]NodeID, 0, len(b.ix.nodes))
	for id := range b.ix.nodes {
		b.ix.ids = append(b.ix.ids, id)
	}
	sort.Slice(b.ix.ids, func(i, j int) bool { return b.ix.ids[i] < b.ix.ids[j] })
}

// --- GraphIndexPort ---------------------------------------------------------

func (ix *InMemoryIndex) Node(id NodeID) (Node, bool) { n, ok := ix.nodes[id]; return n, ok }

func (ix *InMemoryIndex) Nodes() []NodeID {
	return append([]NodeID(nil), ix.ids...)
}

func (ix *InMemoryIndex) Out(id NodeID) []Edge {
	return append([]Edge(nil), ix.out[id]...)
}

func (ix *InMemoryIndex) In(id NodeID) []Edge {
	return append([]Edge(nil), ix.in[id]...)
}

func (ix *InMemoryIndex) Edges() []Edge {
	return append([]Edge(nil), ix.all...)
}

func (ix *InMemoryIndex) ProjectionHash() record.Digest { return ix.ph }
