// Package skilleval makes a skill's EVALS travel WITH the skill as FACTS in the
// append-only record (skill-up §19.4: declarative test cases), replacing the
// in-code selfimprove.MapEvals. An eval is no longer a Go closure wired at
// startup — it is a declarative Spec recorded as an Obligation fact in the same
// content-addressed store the skill itself lives in, so the held-out set is
// reconstructable from the record (Law 1) and scoped to its skill by a domain
// tag. A Provider projects those facts back out and hands selfimprove exactly the
// EvalProvider it already consumes: no core-loop change, and no import cycle
// (this package imports selfimprove; selfimprove does not import this).
package skilleval

import (
	"strings"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/harness/selfimprove"
	"github.com/websines/structor-harness/learning"
)

// LemmaEval is the LemmaID stamped on every eval Obligation. It is DISTINCT from
// learning's lemmas ("learning.skill" / "learning.failure"), so an eval fact
// never collides with a skill or failure fact when the shared record is
// projected — the projection tells them apart by this discriminator.
const LemmaEval = "skill/eval"

// The two grader kinds. An empty Spec.Kind defaults to KindContains. The kind is
// stored on the fact's Anchor.Symbol so the grader can recover it per-case when
// the record is projected (learning.Case carries no kind of its own).
const (
	KindContains    = "contains"
	KindNotContains = "not_contains"
)

// Spec is one declarative eval case authored alongside a skill. Want is the
// substring the skill BODY must contain (KindContains) or must not contain
// (KindNotContains). A Spec with an empty Name or empty Want is skipped by
// Record — an unnamed or wantless case can neither be keyed nor graded.
type Spec struct {
	Name string // case name — becomes the Obligation Title and learning.Case.Name
	Want string // the substring the body is graded against
	Kind string // "contains" (default) | "not_contains"
}

// resolveKind normalizes an empty kind to the default, KindContains.
func resolveKind(kind string) string {
	if kind == "" {
		return KindContains
	}
	return kind
}

// Record appends ONE Obligation fact per Spec into the record, so a skill's
// evals live in the CAS beside the skill. The fact is keyed
// "eval:<skillName>:<spec.Name>" and tagged "skill:<skillName>" so Provider can
// select exactly this skill's evals; the resolved kind rides on Anchor.Symbol
// and the graded substring on AcceptancePolicy. Empty-Name or empty-Want specs
// are skipped.
func Record(store *flg.Store, skillName string, specs []Spec) error {
	for _, spec := range specs {
		if spec.Name == "" || spec.Want == "" {
			continue
		}
		ob := flg.Obligation{
			ID:               "eval:" + skillName + ":" + spec.Name,
			LemmaID:          LemmaEval,
			Title:            spec.Name,
			Anchor:           flg.Anchor{Symbol: resolveKind(spec.Kind)},
			AcceptancePolicy: spec.Want,
			DomainTags:       []string{"skill:" + skillName},
		}
		if _, err := store.AppendObligation(ob); err != nil {
			return err
		}
	}
	return nil
}

// Provider serves skill evals out of the record, implementing
// selfimprove.EvalProvider. It holds no truth of its own (Law 1): every eval is
// projected fresh from the append-only store on each lookup.
type Provider struct {
	store *flg.Store
}

// NewProvider returns a Provider backed by the given record.
func NewProvider(store *flg.Store) *Provider { return &Provider{store: store} }

// Provider must satisfy the interface selfimprove already consumes.
var _ selfimprove.EvalProvider = (*Provider)(nil)

// EvalsFor projects the record and selects the eval facts for skill name: those
// with LemmaID == LemmaEval carrying the domain tag "skill:<name>". Each becomes
// a held-out learning.Case, and a single grader dispatches per-case by the kind
// recorded on that case's fact — resolved through a name→kind map built here and
// closed over, since learning.Case carries no kind. Returns ok=true only when at
// least one eval exists for the skill; otherwise (Evals{}, false), so a skill
// with no declared evals is skipped by a self-improvement pass (no fiat).
func (p *Provider) EvalsFor(name string) (selfimprove.Evals, bool) {
	if p == nil || p.store == nil {
		return selfimprove.Evals{}, false
	}
	obls, _, _, err := p.store.Project()
	if err != nil {
		return selfimprove.Evals{}, false
	}

	tag := "skill:" + name
	var heldOut []learning.Case
	kinds := make(map[string]string) // case name -> grader kind
	seen := make(map[string]bool)    // dedupe by logical id (first projection wins)
	for _, ob := range obls {
		if ob.LemmaID != LemmaEval || !hasTag(ob.DomainTags, tag) {
			continue
		}
		if seen[ob.ID] {
			continue
		}
		seen[ob.ID] = true
		heldOut = append(heldOut, learning.Case{Name: ob.Title, Want: ob.AcceptancePolicy})
		kinds[ob.Title] = resolveKind(ob.Anchor.Symbol)
	}
	if len(heldOut) == 0 {
		return selfimprove.Evals{}, false
	}

	eval := func(sk learning.Skill, c learning.Case) bool {
		body := strings.ToLower(sk.Body)
		want := strings.ToLower(c.Want)
		switch kinds[c.Name] {
		case KindNotContains:
			return !strings.Contains(body, want)
		default: // KindContains (and the normalized default)
			return strings.Contains(body, want)
		}
	}
	return selfimprove.Evals{HeldOut: heldOut, Eval: eval}, true
}

// hasTag reports whether want is among tags.
func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}
