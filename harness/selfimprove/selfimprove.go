// Package selfimprove wires the SkillOpt loop into the AGENT LIFECYCLE: a tool
// failure during a run automatically schedules an OFFLINE, validation-gated
// self-improvement pass over the admitted skills. It rides the same tool-middleware
// seam memory.FailureMiddleware uses (no core loop change), so it fires as part of
// the agent's normal operation — but the pass itself runs on its own goroutine
// (single-flight), so it never blocks the agent, and it is bounded: only skills
// that carry declared evals can be optimized, and a skill improves only if the
// epoch strictly beats its held-out set (SkillOpt / learning.Accept). No fiat, and
// no wall clock — the trigger is the failure, not a timer (Law 3).
package selfimprove

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/skillstore"
	"github.com/websines/structor-harness/learning"
	"github.com/websines/structor-harness/runtime"
	"github.com/websines/structor-harness/skillopt"
)

// keySelfImprove is the local service key the component publishes the Service under.
// Like the skill system, self-improvement is not part of the hx spine, so it does
// not add a key to hx/keys.go.
const keySelfImprove = "selfimprove"

// Evals is a skill's held-out validation set plus its grader — the skill-up idea
// that evals travel WITH the skill. A skill with no Evals cannot be auto-improved
// (there is nothing to beat), so it is left untouched — never promoted by fiat.
type Evals struct {
	HeldOut []learning.Case
	Eval    func(learning.Skill, learning.Case) bool
}

// EvalProvider supplies a skill's evals by name. Returns ok=false when a skill has
// no declared evals (and so is skipped by a self-improvement pass).
type EvalProvider interface {
	EvalsFor(name string) (Evals, bool)
}

// MapEvals is the simplest EvalProvider: an in-memory registry keyed by skill name.
type MapEvals map[string]Evals

// EvalsFor looks up a skill's evals in the map.
func (m MapEvals) EvalsFor(name string) (Evals, bool) { e, ok := m[name]; return e, ok }

// Service runs the offline self-improvement pass. It holds no truth of its own: the
// learning store is the record, the optimizer proposes edits, and the evals gate
// admission. A pass is single-flight — a failure that arrives while a pass runs is
// dropped rather than piling up more model calls.
type Service struct {
	ls       *learning.Store
	opt      skillopt.Optimizer
	evals    EvalProvider
	maxEdits int

	// everyN is the count-based cadence: the automatic failure-triggered pass fires
	// only on every Nth failure (1 = every failure, the default). It is a COUNT, not
	// a clock — there is no wall-time timer here (Law 3: the trigger is the failure).
	everyN int
	// failN is the running count of tool failures seen by Middleware, compared modulo
	// everyN to decide when the automatic pass fires. Atomic: the middleware runs on
	// the caller's goroutine and failures may race.
	failN int64

	inflight atomic.Bool
	wg       sync.WaitGroup
	mu       sync.Mutex
	last     []skillopt.SweepReport

	// trigger schedules a pass; overridable in tests to observe the firing without
	// spawning a goroutine. Defaults to scheduleAsync.
	trigger func()
}

// New builds a Service over the learning store, an optimizer (e.g. a
// skillopt.LLMRewriteOptimizer or a StaticOptimizer), the per-skill evals, and a
// per-skill edit budget (maxEdits, <1 defaults to 1).
func New(ls *learning.Store, opt skillopt.Optimizer, evals EvalProvider, maxEdits int) *Service {
	if maxEdits < 1 {
		maxEdits = 1
	}
	s := &Service{ls: ls, opt: opt, evals: evals, maxEdits: maxEdits, everyN: 1}
	s.trigger = s.scheduleAsync
	return s
}

// WithCadence throttles the AUTOMATIC failure-triggered pass to fire only on every
// Nth tool failure (count-based, not a clock). N<1 is treated as 1 (fire on every
// failure — the current default). It returns s so it composes with New, e.g.
// selfimprove.New(...).WithCadence(3). The explicit Trigger escape hatch bypasses
// this cadence entirely.
func (s *Service) WithCadence(everyNFailures int) *Service {
	if everyNFailures < 1 {
		everyNFailures = 1
	}
	s.everyN = everyNFailures
	return s
}

// Trigger is the explicit "flush" escape hatch: it schedules a pass regardless of
// the cadence counter, still respecting single-flight (a pass already in flight
// coalesces). Use it to force a pass no matter the cadence — e.g. at the end of a
// run. It calls the same overridable trigger the middleware uses, so an override in
// a test observes it too.
func (s *Service) Trigger() {
	if s.trigger != nil {
		s.trigger()
	}
}

// RunOnce runs one self-improvement pass: for each ADMITTED (promoted) skill that
// carries evals, it sweeps a SkillOpt epoch over the record's harvested failures
// (the workings) gated by the skill's held-out set. Skills without evals are
// skipped. Returns the per-skill reports (nil when nothing was eligible).
func (s *Service) RunOnce(ctx context.Context) []skillopt.SweepReport {
	if s.ls == nil || s.opt == nil || s.evals == nil {
		return nil
	}
	fails := skillopt.HarvestFailures(s.ls, "") // the record's failure memory = the workings
	var items []skillopt.SweepItem
	for _, name := range admittedNames(s.ls) {
		ev, ok := s.evals.EvalsFor(name)
		if !ok || len(ev.HeldOut) == 0 || ev.Eval == nil {
			continue // no evals → cannot beat anything → skip (no fiat)
		}
		items = append(items, skillopt.SweepItem{
			Name:     name,
			Rollouts: fails,
			MaxEdits: s.maxEdits,
			HeldOut:  ev.HeldOut,
			Eval:     ev.Eval,
		})
	}
	if len(items) == 0 {
		return nil
	}
	return skillopt.Sweep(ctx, s.ls, s.opt, items)
}

// scheduleAsync starts a pass on its own goroutine unless one is already running
// (single-flight): a burst of failures coalesces into at most one in-flight pass.
func (s *Service) scheduleAsync() {
	if !s.inflight.CompareAndSwap(false, true) {
		return // a pass is already running; drop this trigger
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.inflight.Store(false)
		reports := s.RunOnce(context.Background())
		s.mu.Lock()
		s.last = reports
		s.mu.Unlock()
	}()
}

// Wait blocks until any in-flight pass finishes — a convenience for a driver or a
// demo that wants to observe the outcome of the automatic pass.
func (s *Service) Wait() { s.wg.Wait() }

// Last returns the reports from the most recent completed pass.
func (s *Service) Last() []skillopt.SweepReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// Middleware returns a tool middleware that schedules a self-improvement pass on a
// count-based cadence: it counts every wrapped tool error and calls the (single-
// flight) schedule only on every Nth failure (N = the cadence set by WithCadence,
// default 1 = every failure). It never blocks the tool — the pass runs on its own
// goroutine — and it never sleeps: the throttle is the count modulo N, not a clock.
func Middleware(s *Service) hx.ToolMiddleware {
	return func(next hx.ToolExecFn) hx.ToolExecFn {
		return func(in hx.ToolExecutionInput) hx.ToolResult {
			res := next(in)
			if res.IsError && s != nil && s.trigger != nil {
				every := s.everyN
				if every < 1 {
					every = 1
				}
				n := atomic.AddInt64(&s.failN, 1)
				if n%int64(every) == 0 {
					s.trigger()
				}
			}
			return res
		}
	}
}

// Component installs the self-improvement seam on the runtime: it injects the tool
// runtime, adds the failure-triggered middleware (via the same optional Use
// interface memory.Component uses), and publishes the Service under a local key.
func Component(s *Service) *runtime.Component {
	return &runtime.Component{
		Name:    "harness/selfimprove",
		Inject:  []string{hx.KeyTools},
		Provide: []string{keySelfImprove},
		Apply: func(ctx *runtime.Context) error {
			tr, err := get[hx.ToolRuntime](ctx, hx.KeyTools)
			if err != nil {
				return err
			}
			// Install the middleware if the runtime supports a middleware chain
			// (the concrete tools runtime does); otherwise the service is still
			// provided for explicit RunOnce use.
			if mwUser, ok := tr.(interface {
				Use(hx.ToolMiddleware) func()
			}); ok {
				if err := ctx.Use(func() (func(), error) {
					return mwUser.Use(Middleware(s)), nil
				}); err != nil {
					return err
				}
			}
			return ctx.Provide(keySelfImprove, s)
		},
	}
}

// admittedNames returns the distinct names of promoted (admitted) skills — the set
// a self-improvement pass may optimize. Derived from the record; never a cache.
func admittedNames(ls *learning.Store) []string {
	seen := make(map[string]bool)
	var out []string
	for _, sk := range ls.Recall("") {
		if sk.State != learning.SkillPromoted {
			continue
		}
		n := skillstore.SkillName(sk.ID)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// get resolves a service by key and asserts it to T — the by-key pattern the spine
// uses instead of importing a concrete sibling.
func get[T any](ctx *runtime.Context, key string) (T, error) {
	var zero T
	v, err := ctx.Get(key)
	if err != nil {
		return zero, err
	}
	t, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("selfimprove: service %q has unexpected type %T", key, v)
	}
	return t, nil
}
