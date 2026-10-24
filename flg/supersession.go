// Supersession subresolver (system design §5.4, port of flg-core supersession.py).
//
// Given the entities in a scope and the SupersessionEvidence recorded against
// them, classify each into one of four states and resolve replacement chains
// transitively:
//
//	active     — governing truth (the default; no evidence says otherwise)
//	superseded — invalidated/replaced by explicit evidence (may name active heads)
//	audit_only — explicitly excluded from active context ("audit only"), kept for history
//	ambiguous  — evidence conflicts or cannot resolve → a REPAIR signal, never a guessed verdict
//
// Design guarantees:
//   - Recency is not authority. Resolve takes no timestamps or ordering — there
//     is no input a recency heuristic could key on. A node is active unless
//     explicit evidence demotes it.
//   - Audit history never governs. Active() returns ONLY active nodes.
//   - Ambiguity → repair. Cycles (Tarjan SCC), dangling replacements and
//     contradictory markers are surfaced as Ambiguity rows, not resolved by fiat.
//
// Pure and engine-neutral: no DB, no I/O, fully golden-fixture testable.
package flg

import "sort"

// NodeState is the resolved classification of one entity.
type NodeState string

const (
	NodeActive     NodeState = "active"
	NodeSuperseded NodeState = "superseded"
	NodeAuditOnly  NodeState = "audit_only"
	NodeAmbiguous  NodeState = "ambiguous"
)

// auditKinds mean "kept for audit, not governing"; supersedeKinds mean
// "invalidated / replaced".
var auditKinds = map[SupersessionKind]bool{
	KindExcludeMarker: true,
}

var supersedeKinds = map[SupersessionKind]bool{
	KindStatusSuperseded:      true,
	KindSupersededBy:          true,
	KindCrossTypeInvalidation: true,
	KindDecision:              true,
}

// NodeVerdict is the resolved state of one entity, with its replacement heads
// and cited evidence.
type NodeVerdict struct {
	EntityID   string
	State      NodeState
	ReplacedBy []string // resolved ACTIVE heads (transitive), if any
	Reasons    []string // evidence kinds / problem that drove the verdict
	Citations  []string // evidence citations (artifact ids / paths)
}

// Ambiguity is a violating row: evidence that cannot be resolved and needs repair.
type Ambiguity struct {
	EntityID string
	Problem  string
	Detail   string
}

// Resolution is the full classification of a scope plus ambiguities needing repair.
type Resolution struct {
	Verdicts    []NodeVerdict
	Ambiguities []Ambiguity
}

// ByID indexes verdicts by entity id.
func (r Resolution) ByID() map[string]NodeVerdict {
	out := make(map[string]NodeVerdict, len(r.Verdicts))
	for _, v := range r.Verdicts {
		out[v.EntityID] = v
	}
	return out
}

// StateOf returns the resolved state of an entity (empty if unknown).
func (r Resolution) StateOf(entityID string) NodeState {
	for _, v := range r.Verdicts {
		if v.EntityID == entityID {
			return v.State
		}
	}
	return ""
}

func (r Resolution) idsOf(state NodeState) []string {
	var out []string
	for _, v := range r.Verdicts {
		if v.State == state {
			out = append(out, v.EntityID)
		}
	}
	sort.Strings(out)
	return out
}

// Active is the governing set — ACTIVE only. audit_only/superseded/ambiguous never govern.
func (r Resolution) Active() []string     { return r.idsOf(NodeActive) }
func (r Resolution) Superseded() []string { return r.idsOf(NodeSuperseded) }
func (r Resolution) AuditOnly() []string  { return r.idsOf(NodeAuditOnly) }
func (r Resolution) Ambiguous() []string  { return r.idsOf(NodeAmbiguous) }

type supersedeInfo struct {
	targets   []string
	reasons   []string
	citations []string
}

// unresolvable is an internal signal that a supersede chain cannot reach an
// active head → ambiguous.
type unresolvable struct {
	problem string
	detail  string
}

// ResolveSupersession classifies every entity in entityIDs from the explicit
// evidence (no recency).
func ResolveSupersession(entityIDs []string, evidence []SupersessionEvidence) Resolution {
	universe := map[string]bool{}
	for _, id := range entityIDs {
		universe[id] = true
	}

	// group evidence by subject, preserving subject order for determinism
	bySubject := map[string][]SupersessionEvidence{}
	var subjectOrder []string
	for _, ev := range evidence {
		if _, seen := bySubject[ev.Subject]; !seen {
			subjectOrder = append(subjectOrder, ev.Subject)
		}
		bySubject[ev.Subject] = append(bySubject[ev.Subject], ev)
	}

	verdicts := map[string]NodeVerdict{}
	var ambiguities []Ambiguity
	auditSubjects := map[string]bool{}
	supersede := map[string]supersedeInfo{}

	// --- 1. per-subject candidate from explicit evidence kinds ---------------
	for _, subject := range subjectOrder {
		evs := bySubject[subject]
		citations := sortedUnique(func() []string {
			var cs []string
			for _, e := range evs {
				cs = append(cs, e.Evidence...)
			}
			return cs
		}())

		if !universe[subject] {
			ambiguities = append(ambiguities, Ambiguity{
				EntityID: subject,
				Problem:  "evidence_for_missing_entity",
				Detail:   "evidence targets unknown entity " + subject,
			})
			continue
		}

		hasExclude, hasSupersede := false, false
		for _, e := range evs {
			if auditKinds[e.Kind] {
				hasExclude = true
			}
			if supersedeKinds[e.Kind] {
				hasSupersede = true
			}
		}

		if hasExclude && hasSupersede {
			verdicts[subject] = NodeVerdict{
				EntityID:  subject,
				State:     NodeAmbiguous,
				Reasons:   []string{"exclude_supersede_conflict"},
				Citations: citations,
			}
			ambiguities = append(ambiguities, Ambiguity{
				EntityID: subject,
				Problem:  "exclude_supersede_conflict",
				Detail:   "subject has both an exclude/audit marker and supersession evidence",
			})
			continue
		}
		if hasExclude {
			verdicts[subject] = NodeVerdict{
				EntityID:  subject,
				State:     NodeAuditOnly,
				Reasons:   sortedUniqueKinds(evs, nil),
				Citations: citations,
			}
			auditSubjects[subject] = true
			continue
		}

		var targets []string
		for _, e := range evs {
			if supersedeKinds[e.Kind] {
				targets = append(targets, e.ReplacedBy...)
			}
		}
		supersede[subject] = supersedeInfo{
			targets:   sortedUnique(targets),
			reasons:   sortedUniqueKinds(evs, supersedeKinds),
			citations: citations,
		}
	}

	// --- 2. cycles among supersededBy edges have no active head → ambiguous ---
	graph := map[string][]string{}
	for s, info := range supersede {
		for _, t := range info.targets {
			if _, ok := supersede[t]; ok {
				graph[s] = append(graph[s], t)
			}
		}
	}
	cycleNodes := cycleNodesOf(graph, supersede)
	for node := range cycleNodes {
		verdicts[node] = NodeVerdict{
			EntityID:  node,
			State:     NodeAmbiguous,
			Reasons:   []string{"supersession_cycle"},
			Citations: supersede[node].citations,
		}
		ambiguities = append(ambiguities, Ambiguity{
			EntityID: node,
			Problem:  "supersession_cycle",
			Detail:   "participates in a supersededBy cycle (no active head)",
		})
	}

	// --- 3. resolve transitive replacement heads for the rest ----------------
	// stable order over subjects
	var supSubjects []string
	for s := range supersede {
		supSubjects = append(supSubjects, s)
	}
	sort.Strings(supSubjects)
	for _, subject := range supSubjects {
		if cycleNodes[subject] {
			continue
		}
		info := supersede[subject]
		if len(info.targets) == 0 { // e.g. status_superseded / decision with no named replacement
			verdicts[subject] = NodeVerdict{
				EntityID:  subject,
				State:     NodeSuperseded,
				Reasons:   info.reasons,
				Citations: info.citations,
			}
			continue
		}
		heads, un := walkHeads(subject, supersede, universe, auditSubjects, cycleNodes)
		if un != nil {
			verdicts[subject] = NodeVerdict{
				EntityID:  subject,
				State:     NodeAmbiguous,
				Reasons:   []string{un.problem},
				Citations: info.citations,
			}
			ambiguities = append(ambiguities, Ambiguity{
				EntityID: subject,
				Problem:  un.problem,
				Detail:   un.detail,
			})
			continue
		}
		verdicts[subject] = NodeVerdict{
			EntityID:   subject,
			State:      NodeSuperseded,
			ReplacedBy: sortedUnique(heads),
			Reasons:    info.reasons,
			Citations:  info.citations,
		}
	}

	// --- 4. everything else is ACTIVE (no evidence ⇒ governing) --------------
	for eid := range universe {
		if _, ok := verdicts[eid]; !ok {
			verdicts[eid] = NodeVerdict{EntityID: eid, State: NodeActive}
		}
	}

	var ids []string
	for id := range verdicts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	ordered := make([]NodeVerdict, 0, len(ids))
	for _, id := range ids {
		ordered = append(ordered, verdicts[id])
	}
	return Resolution{Verdicts: ordered, Ambiguities: ambiguities}
}

// walkHeads collects the transitive active replacement heads of subject, or
// returns the first reason it is unresolvable.
func walkHeads(subject string, supersede map[string]supersedeInfo, universe, auditSubjects, cycleNodes map[string]bool) ([]string, *unresolvable) {
	heads := map[string]bool{}
	un := walk(subject, supersede, universe, auditSubjects, cycleNodes, heads, map[string]bool{})
	if un != nil {
		return nil, un
	}
	var out []string
	for h := range heads {
		out = append(out, h)
	}
	return out, nil
}

func walk(node string, supersede map[string]supersedeInfo, universe, auditSubjects, cycleNodes, heads, seen map[string]bool) *unresolvable {
	seen = cloneSet(seen)
	seen[node] = true
	for _, target := range supersede[node].targets {
		switch {
		case !universe[target]:
			return &unresolvable{"dangling_replacement", "replacement " + target + " does not exist"}
		case cycleNodes[target]:
			return &unresolvable{"replacement_in_cycle", "replacement " + target + " is in a cycle"}
		case auditSubjects[target]:
			return &unresolvable{"replaced_by_audit_only", "replacement " + target + " is audit-only"}
		}
		if info, ok := supersede[target]; ok {
			if len(info.targets) == 0 {
				return &unresolvable{"replacement_superseded_without_head", "replacement " + target + " is superseded with no active replacement"}
			}
			if seen[target] { // defensive: cycles already removed in step 2
				return &unresolvable{"supersession_cycle", "cycle via " + target}
			}
			if un := walk(target, supersede, universe, auditSubjects, cycleNodes, heads, seen); un != nil {
				return un
			}
		} else {
			heads[target] = true // a non-superseded, non-audit, existing node → an active head
		}
	}
	return nil
}

// cycleNodesOf runs Tarjan SCC and returns nodes in any strongly-connected
// component of size > 1, plus self-loops.
func cycleNodesOf(graph map[string][]string, supersede map[string]supersedeInfo) map[string]bool {
	index := map[string]int{}
	low := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	result := map[string]bool{}
	counter := 0

	var strongconnect func(v string)
	strongconnect = func(v string) {
		index[v] = counter
		low[v] = counter
		counter++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range graph[v] {
			if _, ok := index[w]; !ok {
				strongconnect(w)
				if low[w] < low[v] {
					low[v] = low[w]
				}
			} else if onStack[w] {
				if index[w] < low[v] {
					low[v] = index[w]
				}
			}
		}
		if low[v] == index[v] {
			var component []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				component = append(component, w)
				if w == v {
					break
				}
			}
			selfLoop := false
			for _, w := range graph[v] {
				if w == v {
					selfLoop = true
					break
				}
			}
			if len(component) > 1 || selfLoop {
				for _, w := range component {
					result[w] = true
				}
			}
		}
	}

	// deterministic vertex order
	var vertices []string
	for v := range supersede {
		vertices = append(vertices, v)
	}
	sort.Strings(vertices)
	for _, v := range vertices {
		if _, ok := index[v]; !ok {
			strongconnect(v)
		}
	}
	return result
}

// --- small pure helpers ---------------------------------------------------

func sortedUnique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func sortedUniqueKinds(evs []SupersessionEvidence, filter map[SupersessionKind]bool) []string {
	var kinds []string
	for _, e := range evs {
		if filter == nil || filter[e.Kind] {
			kinds = append(kinds, string(e.Kind))
		}
	}
	return sortedUnique(kinds)
}

func cloneSet(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
