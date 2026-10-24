package query

import (
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/websines/structor-harness/flg"
)

// retrieval.go is no-vector retrieval (system design §10.4), a port of flg-core's
// retrieval pipeline (retrieval.py / lexical.py / ranking.py). The SHAPE is
// ported, not embeddings:
//
//	route → code-aware BM25 → PageRank centrality re-rank → graph expansion →
//	supersession-sort (active first, superseded labelled) → CITED results.
//
// Every emitted item resolves to a real fact id / path@commit, and carries the
// provenance of the edge it was reached by. A gate refuses to cite an item
// reached only through an INFERRED edge as proof (§10.3): retrieval may surface
// it, but it can never stand as evidence.

// --- code-aware BM25 (port of lexical.py) -----------------------------------

// reWord splits text into words; sub-word splitting (camelCase / snake_case /
// digit runs) is done in subwords() because RE2 lacks the lookahead lexical.py
// relies on.
var reWord = regexp.MustCompile(`[A-Za-z0-9]+`)

// tokenize lowercases text into whole words PLUS their sub-words, split on
// camelCase / snake_case / digit boundaries, so a query for "user" matches
// "getUserId", "user_id" and "USER" alike (lexical.py). The whole form is kept
// too, for exact hits.
func tokenize(text string) []string {
	var tokens []string
	for _, word := range reWord.FindAllString(text, -1) {
		lowered := strings.ToLower(word)
		tokens = append(tokens, lowered)
		for _, part := range subwords(word) {
			low := strings.ToLower(part)
			if low != lowered {
				tokens = append(tokens, low)
			}
		}
	}
	return tokens
}

// subwords splits one word on camelCase / PascalCase / ALLCAPS / digit-run
// boundaries. The Python source uses a lookahead the Go regexp engine (RE2) does
// not support, so we emit the boundary pieces directly.
func subwords(word string) []string {
	var out []string
	var cur strings.Builder
	runes := []rune(word)
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	classOf := func(r rune) int {
		switch {
		case r >= 'A' && r <= 'Z':
			return 1 // upper
		case r >= 'a' && r <= 'z':
			return 2 // lower
		default:
			return 3 // digit
		}
	}
	for i, r := range runes {
		if i > 0 {
			prev := classOf(runes[i-1])
			c := classOf(r)
			// boundary: class change, OR ALLCAPS→CamelCase (UPPER followed by
			// UPPER then lower, e.g. "HTTPServer" → "HTTP","Server").
			boundary := c != prev
			if prev == 1 && c == 1 && i+1 < len(runes) && classOf(runes[i+1]) == 2 {
				boundary = true
			}
			// keep a leading uppercase attached to the lowercase run it heads.
			if prev == 1 && c == 2 {
				boundary = false
			}
			if boundary {
				flush()
			}
		}
		cur.WriteRune(r)
	}
	flush()
	return out
}

// bm25 is an in-memory Okapi BM25 index over {docID: text}. k1 saturates term
// frequency; b normalizes for length (the standard 1.5 / 0.75). Ties break by
// ascending doc id, so ranking is deterministic (lexical.py).
type bm25 struct {
	k1, b  float64
	freqs  map[NodeID]map[string]int
	length map[NodeID]int
	df     map[string]int
	avgdl  float64
}

func buildBM25(docs map[NodeID]string) *bm25 {
	ix := &bm25{
		k1: 1.5, b: 0.75,
		freqs:  map[NodeID]map[string]int{},
		length: map[NodeID]int{},
		df:     map[string]int{},
	}
	total := 0
	for id, text := range docs {
		toks := tokenize(text)
		f := map[string]int{}
		for _, t := range toks {
			f[t]++
		}
		ix.freqs[id] = f
		ix.length[id] = len(toks)
		total += len(toks)
		for term := range f {
			ix.df[term]++
		}
	}
	if len(ix.length) > 0 {
		ix.avgdl = float64(total) / float64(len(ix.length))
	}
	return ix
}

func (ix *bm25) idf(term string) float64 {
	n := float64(len(ix.freqs))
	df := float64(ix.df[term])
	return math.Log(1 + (n-df+0.5)/(df+0.5))
}

// scoreMap returns the BM25 score of every doc sharing ≥1 query term (lexical.py).
func (ix *bm25) scoreMap(query string) map[NodeID]float64 {
	out := map[NodeID]float64{}
	terms := tokenize(query)
	if len(terms) == 0 || len(ix.freqs) == 0 {
		return out
	}
	for id, f := range ix.freqs {
		dl := float64(ix.length[id])
		var score float64
		for _, term := range terms {
			tf := float64(f[term])
			if tf == 0 {
				continue
			}
			denom := tf + ix.k1*(1-ix.b+ix.b*dl/ix.avgdl)
			score += ix.idf(term) * (tf * (ix.k1 + 1)) / denom
		}
		if score > 0 {
			out[id] = score
		}
	}
	return out
}

// search ranks docs sharing ≥1 query term, descending score then ascending id.
func (ix *bm25) search(query string) []NodeID { return rankByScore(ix.scoreMap(query)) }

// --- PageRank centrality (port of ranking.py) -------------------------------

// pagerank is stationary PageRank over src→dst edges: a node many edges point at
// ranks higher. Dangling nodes redistribute uniformly; deterministic power
// iteration, fixed order, converges by L1 delta (ranking.py).
func pagerank(edges []Edge, nodes []NodeID) map[NodeID]float64 {
	universe := map[NodeID]bool{}
	var order []NodeID
	add := func(id NodeID) {
		if !universe[id] {
			universe[id] = true
			order = append(order, id)
		}
	}
	for _, id := range nodes {
		add(id)
	}
	for _, e := range edges {
		add(e.From)
		add(e.To)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	n := len(order)
	if n == 0 {
		return map[NodeID]float64{}
	}
	out := map[NodeID][]NodeID{}
	for _, e := range edges {
		out[e.From] = append(out[e.From], e.To)
	}
	const damping = 0.85
	const maxIter = 100
	const tol = 1e-9
	rank := map[NodeID]float64{}
	for _, id := range order {
		rank[id] = 1.0 / float64(n)
	}
	base := (1.0 - damping) / float64(n)
	for iter := 0; iter < maxIter; iter++ {
		var danglingMass float64
		for _, id := range order {
			if len(out[id]) == 0 {
				danglingMass += rank[id]
			}
		}
		dangling := damping * danglingMass / float64(n)
		next := map[NodeID]float64{}
		for _, id := range order {
			next[id] = base + dangling
		}
		for _, id := range order {
			deg := len(out[id])
			if deg == 0 {
				continue
			}
			share := damping * rank[id] / float64(deg)
			for _, v := range out[id] {
				next[v] += share
			}
		}
		var delta float64
		for _, id := range order {
			delta += math.Abs(next[id] - rank[id])
		}
		rank = next
		if delta < tol {
			break
		}
	}
	return rank
}

// --- query router (port of retrieval.route) ---------------------------------

// QueryMode tunes the lexical/centrality blend and the expansion depth.
type QueryMode string

const (
	ModeOverview    QueryMode = "overview"    // broad "how does X work"
	ModeSpecific    QueryMode = "specific"    // a name/id lookup
	ModeExploratory QueryMode = "exploratory" // trace / impact / lineage
)

var exploratoryCues = []string{
	"trace", "why", "impact", "affect", "depend", "downstream",
	"upstream", "relate", "lineage", "reach", "flow", "call",
}

var reID = regexp.MustCompile(`^[A-Za-z][\w.-]*-\d+$`)

// route classifies a query by simple, explainable heuristics (retrieval.py).
func route(query string) QueryMode {
	q := strings.ToLower(query)
	for _, cue := range exploratoryCues {
		if strings.Contains(q, cue) {
			return ModeExploratory
		}
	}
	if reID.MatchString(strings.TrimSpace(query)) || len(strings.Fields(q)) <= 2 {
		return ModeSpecific
	}
	return ModeOverview
}

var modeWeights = map[QueryMode][2]float64{
	ModeOverview:    {0.5, 0.5},
	ModeSpecific:    {0.9, 0.1},
	ModeExploratory: {0.6, 0.4},
}

var modeDepth = map[QueryMode]int{
	ModeOverview:    1,
	ModeSpecific:    0,
	ModeExploratory: 2,
}

// --- results ----------------------------------------------------------------

// CitedItem is one retrieved fact: cited to a real path@commit, scored, labelled
// with its supersession state, and carrying the provenance of the edge it was
// reached by. Prov is Canonical for a direct (lexical/central) hit and for a hit
// reached over a recorded edge; it is Inferred when reached only over a heuristic
// edge — the label the proof gate keys on.
type CitedItem struct {
	ID      NodeID
	Kind    NodeKind
	State   flg.NodeState
	Score   float64
	Reasons []string
	Prov    Provenance
	Path    string
	Commit  string
}

// Citation renders the item as `id@path@commit` (or the parts it has) — always a
// real, resolvable pin, never a free-text snippet (§10.4).
func (c CitedItem) Citation() string {
	out := string(c.ID)
	if c.Path != "" {
		out += "@" + c.Path
	}
	if c.Commit != "" {
		out += "@" + c.Commit
	}
	return out
}

// CanCiteAsProof reports whether the item may stand as evidence: it must be
// governing (active) AND reached over canonical edges only. An inferred hop, or a
// superseded/ambiguous state, disqualifies it (§10.3).
func (c CitedItem) CanCiteAsProof() bool {
	return c.Prov == Canonical && c.State == flg.NodeActive
}

// RetrievalResult is the served context: governing (active) items first, then
// audit history (superseded / audit-only), each cited and labelled.
type RetrievalResult struct {
	Query     string
	Mode      QueryMode
	Governing []CitedItem
	Audit     []CitedItem
}

// ProofCitations returns only the items admissible as evidence — active and
// canonical-reached. This is what a gate cites; everything else is advisory.
func (r RetrievalResult) ProofCitations() []CitedItem {
	var out []CitedItem
	for _, c := range r.Governing {
		if c.CanCiteAsProof() {
			out = append(out, c)
		}
	}
	return out
}

// --- the pipeline -----------------------------------------------------------

// Retrieve runs the no-vector pipeline over the index and the supersession
// resolution, returning cited, active-first results (§10.4). limit caps the
// governing set; sups are the supersession facts the projection carries (used for
// the active-first sort). Passing 0 for limit uses a default of 10.
func Retrieve(ix GraphIndexPort, sups []flg.SupersessionEvidence, query string, limit int) RetrievalResult {
	if limit <= 0 {
		limit = 10
	}
	mode := route(query)
	ids := ix.Nodes()

	// supersession classification over the whole node universe: active governs,
	// superseded / audit_only is history, ambiguous is a defect (excluded).
	universe := make([]string, len(ids))
	for i, id := range ids {
		universe[i] = string(id)
	}
	resolution := flg.ResolveSupersession(universe, sups)
	stateOf := func(id NodeID) flg.NodeState {
		if s := resolution.StateOf(string(id)); s != "" {
			return s
		}
		return flg.NodeActive // no evidence ⇒ governing (§5.4)
	}

	// 1. code-aware BM25 candidates.
	docs := map[NodeID]string{}
	for _, id := range ids {
		if n, ok := ix.Node(id); ok {
			docs[id] = string(id) + " " + string(n.Kind) + " " + n.Text
		}
	}
	lexical := buildBM25(docs).scoreMap(query)

	// 2. PageRank centrality over the graph.
	ranks := pagerank(ix.Edges(), ids)
	prInScope := map[NodeID]float64{}
	for _, id := range ids {
		if v, ok := ranks[id]; ok {
			prInScope[id] = v
		}
	}

	lexNorm := normalize(lexical)
	prNorm := normalize(prInScope)
	w := modeWeights[mode]

	scores := map[NodeID]float64{}
	reasons := map[NodeID][]string{}
	prov := map[NodeID]Provenance{}
	for id := range lexical {
		scores[id] = w[0]*lexNorm[id] + w[1]*prNorm[id]
		reasons[id] = []string{"lexical"}
		prov[id] = Canonical // a direct lexical hit is a first-class, citable seed
		if prNorm[id] > 0 {
			reasons[id] = append(reasons[id], "pagerank")
		}
	}
	// broad queries with no lexical hit fall back to the most central nodes.
	if len(scores) == 0 && mode != ModeSpecific {
		for id, pr := range prNorm {
			scores[id] = w[1] * pr
			reasons[id] = []string{"pagerank"}
			prov[id] = Canonical
		}
	}

	seeds := rankByScore(scores)
	if len(seeds) > limit {
		seeds = seeds[:limit]
	}
	selected := map[NodeID]float64{}
	for _, id := range seeds {
		selected[id] = scores[id]
	}

	// 3. graph expansion over both edge directions, depth by mode. A node reached
	// over an inferred edge inherits INFERRED provenance — the gate refuses it as
	// proof even though it stays in the served context.
	depth := modeDepth[mode]
	if depth > 0 {
		expandNeighborhood(ix, seeds, depth, func(id NodeID, score float64, p Provenance) {
			if _, already := selected[id]; already {
				return
			}
			selected[id] = score
			reasons[id] = []string{"expanded"}
			prov[id] = p
		}, selected)
	}

	// 4. build items, supersession-sorted: active governs; the rest is history.
	var governing, audit []CitedItem
	for id, score := range selected {
		n, ok := ix.Node(id)
		if !ok {
			continue
		}
		st := stateOf(id)
		item := CitedItem{
			ID: id, Kind: n.Kind, State: st, Score: score,
			Reasons: reasons[id], Prov: prov[id], Path: n.Path, Commit: n.Commit,
		}
		switch st {
		case flg.NodeActive:
			governing = append(governing, item)
		case flg.NodeSuperseded, flg.NodeAuditOnly:
			audit = append(audit, item)
			// ambiguous items are defects — excluded from the body.
		}
	}
	sortItems(governing)
	sortItems(audit)
	if len(governing) > limit {
		governing = governing[:limit]
	}
	return RetrievalResult{Query: query, Mode: mode, Governing: governing, Audit: audit}
}

// expandNeighborhood is BFS out from seeds over UNDIRECTED graph edges, depth
// hops (retrieval.expand_neighborhood). Each newly reached node is reported once
// at its shortest hop with a decayed score and the provenance of the WEAKEST edge
// on the path that reached it — so a path crossing one inferred edge is inferred.
func expandNeighborhood(ix GraphIndexPort, seeds []NodeID, depth int, emit func(NodeID, float64, Provenance), seedScores map[NodeID]float64) {
	// adjacency: undirected, remembering each edge's provenance.
	type adj struct {
		to   NodeID
		prov Provenance
	}
	neighbors := map[NodeID][]adj{}
	for _, e := range ix.Edges() {
		neighbors[e.From] = append(neighbors[e.From], adj{e.To, e.Prov})
		neighbors[e.To] = append(neighbors[e.To], adj{e.From, e.Prov})
	}
	seen := map[NodeID]bool{}
	pathProv := map[NodeID]Provenance{}
	pathScore := map[NodeID]float64{}
	for _, s := range seeds {
		seen[s] = true
		pathProv[s] = Canonical
		pathScore[s] = seedScores[s]
	}
	frontier := append([]NodeID(nil), seeds...)
	for hop := 0; hop < depth && len(frontier) > 0; hop++ {
		var next []NodeID
		for _, cur := range frontier {
			as := neighbors[cur]
			sort.Slice(as, func(i, j int) bool { return as[i].to < as[j].to })
			for _, a := range as {
				if seen[a.to] {
					continue
				}
				seen[a.to] = true
				p := Canonical
				if pathProv[cur] == Inferred || a.prov == Inferred {
					p = Inferred // weakest link governs the path's admissibility
				}
				pathProv[a.to] = p
				pathScore[a.to] = pathScore[cur] * math.Pow(0.5, float64(hop+1))
				emit(a.to, pathScore[a.to], p)
				next = append(next, a.to)
			}
		}
		frontier = next
	}
}

// --- small helpers ----------------------------------------------------------

func normalize(scores map[NodeID]float64) map[NodeID]float64 {
	var top float64
	for _, v := range scores {
		if v > top {
			top = v
		}
	}
	out := make(map[NodeID]float64, len(scores))
	if top <= 0 {
		for k := range scores {
			out[k] = 0
		}
		return out
	}
	for k, v := range scores {
		out[k] = v / top
	}
	return out
}

// rankByScore orders ids by descending score, ascending id — the stable order
// BM25 and the seed selection share.
func rankByScore(scores map[NodeID]float64) []NodeID {
	ids := make([]NodeID, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if scores[ids[i]] != scores[ids[j]] {
			return scores[ids[i]] > scores[ids[j]]
		}
		return ids[i] < ids[j]
	})
	return ids
}

func sortIDs(ids []NodeID) {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
}

func sortItems(items []CitedItem) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Score != items[j].Score {
			return items[i].Score > items[j].Score
		}
		return items[i].ID < items[j].ID
	})
}
