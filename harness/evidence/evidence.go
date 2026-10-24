// Package evidence is the bridge that closes the loop between the agent harness
// and the proof spine (forge-harness-design-v0.1.md §6, §9 stage H3). It installs
// a tool middleware on the harness's ToolRuntime; when a tool returns a result
// carrying an EvidenceHint, the middleware lifts that result into typed
// flg.Evidence, appends it to the append-only record, and re-derives the affected
// obligation's verdict through the resolver. The agent's real work therefore
// feeds the gate: "the tests passed" becomes evidence, and the record decides
// whether the obligation is discharged — the agent never marks its own work done.
//
// The bridge does not import the tools package: it registers its middleware
// through an optional Use interface the resolved ToolRuntime satisfies, so it
// stays decoupled like every other spine consumer.
package evidence

import (
	"fmt"
	"strings"
	"sync"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/obligation"
	"github.com/websines/structor-harness/reason"
	"github.com/websines/structor-harness/reasongate"
	"github.com/websines/structor-harness/record"
	"github.com/websines/structor-harness/resolver"
	"github.com/websines/structor-harness/runtime"
)

// Sink turns tool results into evidence facts and derives verdicts from the
// record. It owns a flg.Store (the evidence lives in the canonical record, not in
// the harness) and the set of obligations the agent is working to discharge.
type Sink struct {
	mu    sync.Mutex
	store *flg.Store
	obls  map[string]flg.Obligation
	seq   int
}

// New returns a Sink whose evidence and obligation facts live in a store backed
// by cas.
func New(cas record.CAS) *Sink {
	return &Sink{store: flg.NewStore(cas), obls: map[string]flg.Obligation{}}
}

// Store exposes the sink's record so a surface or the verified-context provider
// can assemble context from the obligations and evidence the agent is producing.
func (s *Sink) Store() *flg.Store { return s.store }

// Scorer returns a quality-aware compaction Score function that reads the RECORD —
// the Forge-native signal generic harnesses lack. A message that mentions an
// obligation still being worked (open/reopened/contested/falsified/unverifiable)
// scores HIGH: it is load-bearing, keep it. A message that mentions a SETTLED
// obligation (discharged/audit_only) scores LOW: its discussion is safe to compact.
// A message mentioning no known obligation is neutral (recency decides). The score
// sums over every obligation the message names, so a message about several
// still-open claims is protected more strongly.
func (s *Sink) Scorer() func(hx.Message) int {
	return func(m hx.Message) int {
		text := messageText(m)
		s.mu.Lock()
		ids := make([]string, 0, len(s.obls))
		for id := range s.obls {
			if strings.Contains(text, id) {
				ids = append(ids, id)
			}
		}
		s.mu.Unlock()

		score := 0
		for _, id := range ids {
			v, err := s.Verdict(id)
			if err != nil {
				continue
			}
			switch v.State {
			case flg.StateDischarged, flg.StateAuditOnly:
				score -= 2 // settled — the discussion around it is safe to compact
			default: // open / reopened / contested / falsified / unverifiable — still active
				score += 2 // load-bearing — keep it in the derived context
			}
		}
		return score
	}
}

// messageText concatenates a message's textual content (text, tool args, tool
// results) so the scorer can look for obligation ids anywhere the model mentioned one.
func messageText(m hx.Message) string {
	var b strings.Builder
	for _, blk := range m.Content {
		if blk.Text != "" {
			b.WriteString(blk.Text)
			b.WriteByte('\n')
		}
		if blk.ToolName != "" {
			b.WriteString(blk.ToolName)
			b.WriteByte('\n')
		}
		if len(blk.ToolArgs) > 0 {
			b.Write(blk.ToolArgs)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// RegisterObligation records an obligation the agent is expected to discharge.
// Its verdict is a pure derivation over the evidence the agent produces — the
// harness cannot set it, only supply evidence toward it.
func (s *Sink) RegisterObligation(o flg.Obligation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.store.AppendObligation(o); err != nil {
		return err
	}
	s.obls[o.ID] = o
	return nil
}

// Middleware is the tool-pipeline middleware that records evidence. It runs the
// tool, and if the result carries an EvidenceHint for a known obligation, appends
// the corresponding evidence fact. Recording is best-effort and never changes the
// tool result the model sees (evidence is a side record, not a tool output).
func (s *Sink) Middleware() hx.ToolMiddleware {
	return func(next hx.ToolExecFn) hx.ToolExecFn {
		return func(in hx.ToolExecutionInput) hx.ToolResult {
			res := next(in)
			if res.EvidenceHint != nil {
				s.record(in, res)
			}
			return res
		}
	}
}

// record maps an EvidenceHint onto typed flg.Evidence and appends it. A tool that
// FAILED to run (IsError) yields evidence marked invalid — an infrastructure
// failure is `unverifiable`, never a factual contradiction (the resolver's rule).
// A reproducible hint is a real witness; a non-reproducible one is testimonial
// and, by the five rules, can never auto-falsify.
func (s *Sink) record(in hx.ToolExecutionInput, res hx.ToolResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := res.EvidenceHint

	polarity := flg.PolaritySupports
	if !h.Supports {
		polarity = flg.PolarityFalsifies
	}
	repro := flg.ReproTestimonial
	if h.Reproducible {
		repro = flg.ReproDeterministic
	}
	validity := flg.ValidityValid
	if res.IsError {
		validity = flg.ValidityInvalid
	}
	// Scope the evidence to the obligation's declared domain so an in-scope
	// falsifier actually lands; fall back to the wildcard for an unknown obligation.
	tags := []string{"*"}
	if o, ok := s.obls[h.ObligationID]; ok && len(o.DomainTags) > 0 {
		tags = o.DomainTags
	}

	s.seq++
	ev := flg.Evidence{
		ID:              fmt.Sprintf("EV-%s-%d", h.ObligationID, s.seq),
		ObligationID:    h.ObligationID,
		Polarity:        polarity,
		Method:          flg.Method(h.Method),
		Reproducibility: repro,
		Validity:        validity,
		Scope:           flg.Scope{Domain: "harness", Tags: tags},
		PrincipalID:     in.Initiator,
		Summary:         h.Summary,
	}
	// Best-effort: a failed append must not break the agent's turn.
	_, _ = s.store.AppendEvidence(ev)
}

// Verdict derives the current state of one obligation from the record. It is a
// pure projection: append evidence, ask again, and the state may change — nothing
// is stored as a flag.
func (s *Sink) Verdict(oblID string) (flg.Verdict, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obls, evs, sups, err := s.store.Project()
	if err != nil {
		return flg.Verdict{}, err
	}
	for _, o := range obls {
		if o.ID == oblID {
			return resolver.ResolveObligation(o, evs, sups), nil
		}
	}
	return flg.Verdict{}, fmt.Errorf("evidence: obligation %q not registered", oblID)
}

// VerdictWithProof derives the verdict AND the machine-checkable proof tree that
// justifies it. The State comes from the hand-coded resolver (the Lean-gated
// authority); the proof is produced by running the resolver's five rules AS a
// stratified Datalog program (reasongate) over the SAME record facts — the two
// are verified to agree on the conformance vectors. So the gate no longer merely
// asserts a verdict; it hands back the derivation (including the negative premises
// — "no standing falsifier" — that make a non-monotonic verdict checkable).
func (s *Sink) VerdictWithProof(oblID string) (flg.Verdict, *reason.ProofNode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obls, evs, sups, err := s.store.Project()
	if err != nil {
		return flg.Verdict{}, nil, err
	}
	for _, o := range obls {
		if o.ID == oblID {
			v := resolver.ResolveObligation(o, evs, sups)
			state, proof := reasongate.ResolveViaDatalog(o, evs, sups)
			matched, err := matchingProof(v, state, proof)
			return v, matched, err
		}
	}
	return flg.Verdict{}, nil, fmt.Errorf("evidence: obligation %q not registered", oblID)
}

// Gate reports the informational gate over every registered obligation — what the
// agent would be told is still owed. Never a hard block (Law 5).
func (s *Sink) Gate() (obligation.GateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obls, evs, sups, err := s.store.Project()
	if err != nil {
		return obligation.GateResult{}, err
	}
	return obligation.EvaluateWithResolver(obls, evs, sups, nil), nil
}

// Component mounts the sink on the harness: it injects the ToolRuntime, installs
// the evidence middleware on it (a revertible effect), and provides the sink
// under KeyEvidenceSink so a surface or the agent can read the gate.
func Component(s *Sink) *runtime.Component {
	return &runtime.Component{
		Name:    "harness/evidence",
		Inject:  []string{hx.KeyTools},
		Provide: []string{hx.KeyEvidenceSink},
		Apply: func(ctx *runtime.Context) error {
			v, err := ctx.Get(hx.KeyTools)
			if err != nil {
				return err
			}
			// The ToolRuntime supports middleware through an optional interface;
			// asserting it keeps this package decoupled from the tools package.
			mwUser, ok := v.(interface {
				Use(hx.ToolMiddleware) func()
			})
			if !ok {
				return fmt.Errorf("evidence: %s does not support tool middleware", hx.KeyTools)
			}
			if err := ctx.Use(func() (func(), error) {
				return mwUser.Use(s.Middleware()), nil
			}); err != nil {
				return err
			}
			return ctx.Provide(hx.KeyEvidenceSink, s)
		},
	}
}
