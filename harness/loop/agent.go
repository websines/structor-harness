package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/websines/structor-harness/harness/hx"
)

// services is the narrow view of runtime.Context the loop needs: resolve a
// capability by key. *runtime.Context satisfies it (its Get enforces the
// component's declared confinement), and tests satisfy it with a plain map — so
// the driver logic is exercised without a live registry.
type services interface {
	Get(key string) (any, error)
}

// ReactLoopAgent is the concrete hx.Agent: a live driver over one Session. It owns
// its inbox, resolves the model/tool/prompt runtimes from the shared context by
// key, and runs turns on a single background goroutine (the "kick" driver) so the
// session log is only ever appended to from one place. Its zero value is not
// usable; construct it with newAgent.
type ReactLoopAgent struct {
	id      string
	cfg     hx.AgentConfig
	svc     services
	session hx.Session
	inbox   *inbox
	hooks   hx.Hooks

	// system is the prompt assembled by the current step's preStep, read by step.
	// Only the driver goroutine touches it, so no lock is needed.
	system       string
	systemPrefix string
	task         string // driver-owned immutable intent, outside compactable messages

	// lastPromptTokens is the model-reported prompt_tokens of the most recent
	// request (from the stream's usage chunk) — the exact context size that drives
	// the next step's compaction decision. Driver-goroutine-only, no lock.
	lastPromptTokens int

	mu          sync.Mutex
	running     bool
	idleWaiters []chan struct{}
	lastErr     error

	idSeq uint64

	// maxSteps bounds a single turn's model↔tool steps (§4.2 bounded loop
	// termination): a model that keeps owing a step — e.g. a small model stuck
	// re-calling tools — is stopped after this many steps so the turn concludes and
	// the driver quiesces rather than hanging forever. 0 means unbounded.
	maxSteps int

	// Per-activity abort. A turn captures actCtx once; Cancel cancels it, and the
	// next turn gets a fresh one from newActivity.
	cancelMu    sync.Mutex
	actCtx      context.Context
	actCancel   context.CancelFunc
	cancelCause string
}

var agentSeq uint64

// defaultMaxSteps bounds a turn's steps so a runaway model cannot hang the driver.
// High enough for genuine multi-tool work, low enough to break a tool-call loop.
const defaultMaxSteps = 24

// newAgent constructs a driver over sess, resolving further services from svc.
func newAgent(cfg hx.AgentConfig, svc services, sess hx.Session) *ReactLoopAgent {
	maxSteps := cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = defaultMaxSteps
	}
	return &ReactLoopAgent{
		id:       fmt.Sprintf("agent-%d", atomic.AddUint64(&agentSeq, 1)),
		cfg:      cfg,
		task:     cfg.Task,
		svc:      svc,
		session:  sess,
		inbox:    &inbox{},
		maxSteps: maxSteps,
	}
}

// ID returns the agent's stable identity.
func (a *ReactLoopAgent) ID() string { return a.id }

// Send delivers a message into the inbox and, for the waking kinds
// (followup/steer), wakes the driver. Inject lands for the next step but never
// starts an idle driver — it is consumed by a step that runs for another reason.
func (a *ReactLoopAgent) Send(kind hx.DeliverKind, msg hx.Message) {
	if msg.ID == "" {
		msg.ID = a.nextID("msg")
	}
	a.inbox.push(kind, msg)
	if kind != hx.DeliverInject {
		a.wake()
	}
}

// Followup is the common "the user said something" path: a user message that
// drives the next turn and wakes the driver.
func (a *ReactLoopAgent) Followup(text string) {
	a.Send(hx.DeliverFollowup, hx.Message{
		Role:    hx.RoleUser,
		Content: []hx.ContentBlock{{Kind: hx.BlockText, Text: text}},
	})
}

// WhenIdle blocks until the inbox is drained and no turn is in flight, or ctx is
// cancelled. It re-checks under the lock after each wake, so a delivery that
// arrives while it waits keeps it waiting until the driver has truly quiesced.
func (a *ReactLoopAgent) WhenIdle(ctx context.Context) error {
	for {
		a.mu.Lock()
		if !a.running && !a.turnOwed() {
			err := a.lastErr
			a.mu.Unlock()
			return err
		}
		w := make(chan struct{})
		a.idleWaiters = append(a.idleWaiters, w)
		a.mu.Unlock()
		select {
		case <-w:
			// re-check the condition
		case <-ctx.Done():
			a.mu.Lock()
			for i, waiter := range a.idleWaiters {
				if waiter == w {
					a.idleWaiters = append(a.idleWaiters[:i], a.idleWaiters[i+1:]...)
					break
				}
			}
			a.mu.Unlock()
			return ctx.Err()
		}
	}
}

// Cancel aborts the in-flight activity with a cause. Owed work that has not yet
// started proceeds under a fresh context on the next turn.
func (a *ReactLoopAgent) Cancel(cause string) {
	a.cancelMu.Lock()
	defer a.cancelMu.Unlock()
	a.cancelCause = cause
	if a.actCancel != nil {
		a.actCancel()
	}
}

// wake starts the driver goroutine if it is not already running. The running flag
// is flipped under a.mu, and kick clears it (also under a.mu) only once no work is
// owed — so there is no lost wakeup: a delivery either is seen by the running kick
// or restarts it here.
func (a *ReactLoopAgent) wake() {
	if a.claimDriver() {
		go a.kick()
	}
}

// claimDriver installs the first activity before the asynchronous driver can
// start. Cancellation in that interval must still abort the claimed turn.
func (a *ReactLoopAgent) claimDriver() bool {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return false
	}
	a.lastErr = nil
	// Install the activity before publishing the driver as running. An immediate
	// Cancel must target this activity rather than be overwritten by the driver.
	a.newActivity()
	a.running = true
	a.mu.Unlock()
	return true
}

// kick is the driver: it runs turns while any is owed, then quiesces. A contained
// failure at this boundary is recovered — the driver stops, it does not panic the
// process. Between turns, and before deciding to stop, it re-checks the inbox
// under the lock so a delivery racing the shutdown is not dropped.
func (a *ReactLoopAgent) kick() {
	defer a.recoverDriver()
	for {
		if a.turn() {
			a.newActivity()
			continue
		}
		a.mu.Lock()
		if a.turnOwed() {
			a.newActivity()
			a.mu.Unlock()
			continue
		}
		a.Cancel("activity completed")
		a.running = false
		waiters := a.idleWaiters
		a.idleWaiters = nil
		a.mu.Unlock()
		closeAll(waiters)
		return
	}
}

// recoverDriver contains a panic raised anywhere under a turn: it records the
// failure, marks the driver idle, and releases any WhenIdle waiters so callers do
// not block forever behind a crashed turn.
func (a *ReactLoopAgent) recoverDriver() {
	if r := recover(); r != nil {
		a.mu.Lock()
		a.Cancel("driver panicked")
		a.lastErr = fmt.Errorf("loop: driver recovered from panic: %v", r)
		a.running = false
		waiters := a.idleWaiters
		a.idleWaiters = nil
		a.mu.Unlock()
		closeAll(waiters)
	}
}

// turnOwed reports whether the inbox holds work that should drive another turn.
func (a *ReactLoopAgent) turnOwed() bool { return a.inbox.owed() }

// newActivity installs a fresh per-activity context for the next turn, cancelling
// any prior (already-finished) one.
func (a *ReactLoopAgent) newActivity() {
	a.cancelMu.Lock()
	defer a.cancelMu.Unlock()
	if a.actCancel != nil {
		a.actCancel()
	}
	a.actCtx, a.actCancel = context.WithCancel(context.Background())
}

// activityCtx returns the current turn's abort context.
func (a *ReactLoopAgent) activityCtx() context.Context {
	a.cancelMu.Lock()
	defer a.cancelMu.Unlock()
	if a.actCtx == nil {
		return context.Background()
	}
	return a.actCtx
}

// fail records a non-fatal driver error (surfaced for diagnostics; the driver ends
// the turn cleanly rather than propagating).
func (a *ReactLoopAgent) fail(err error) {
	if err == nil {
		return
	}
	a.mu.Lock()
	a.lastErr = err
	a.mu.Unlock()
}

// nextID mints a per-agent, monotonically counted id. It is a counter, never a
// timestamp — ordering of truth is the log's Seq, not any clock (Law 3).
func (a *ReactLoopAgent) nextID(prefix string) string {
	return fmt.Sprintf("%s-%s-%d", a.id, prefix, atomic.AddUint64(&a.idSeq, 1))
}

// initiator is the actor a tool call is stamped with — the verified principal if
// the agent runs under one, else the agent id.
func (a *ReactLoopAgent) initiator() string {
	if a.cfg.PrincipalID != "" {
		return a.cfg.PrincipalID
	}
	return a.id
}

// emit appends one event to the session log, recording (never panicking on) an
// append failure.
func (a *ReactLoopAgent) emit(kind hx.EventKind, data []byte, surface bool) {
	if _, err := a.session.Append(kind, data, surface); err != nil {
		a.fail(fmt.Errorf("loop: append %s: %w", kind, err))
	}
}

// resolve fetches the service at key from svc and asserts it to T, the pattern the
// loop uses everywhere instead of importing a concrete package.
func resolve[T any](svc services, key string) (T, error) {
	var zero T
	v, err := svc.Get(key)
	if err != nil {
		return zero, err
	}
	t, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("loop: service %q has unexpected type %T", key, v)
	}
	return t, nil
}

// encode marshals a value for an event payload; on the (unexpected) failure of a
// plain in-house struct it yields nil rather than aborting the turn.
func encode(v any) []byte {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func closeAll(waiters []chan struct{}) {
	for _, w := range waiters {
		close(w)
	}
}

// Compile-time proof that ReactLoopAgent is a full hx.Agent.
var _ hx.Agent = (*ReactLoopAgent)(nil)
