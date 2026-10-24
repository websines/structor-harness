package loop

import (
	"sync"

	"github.com/websines/structor-harness/harness/hx"
)

// inbox is the per-agent delivery queue. A delivery carries a message and the
// DeliverKind that decides WHEN it lands and WHETHER it wakes the driver:
//
//   - DeliverFollowup — a new turn's user input; claimed at a turn's first step.
//   - DeliverSteer    — a mid-turn nudge; claimed at the next step (any step).
//   - DeliverInject   — extra context (e.g. a tool's AdditionalContext); claimed
//     at the next step, but never wakes an idle driver on its own.
//
// The canonical Inbox lives in the (parallel-built) harness/agent package; this is
// the minimal in-package equivalent the driver needs to build and test alone.
// Order among queued items is preserved — the queue is FIFO, never time-sorted.
type inbox struct {
	mu    sync.Mutex
	items []delivery
}

type delivery struct {
	kind hx.DeliverKind
	msg  hx.Message
}

// push appends a delivery. It never blocks and never wakes the driver — waking is
// the agent's decision (see ReactLoopAgent.Send), which depends on the kind.
func (in *inbox) push(kind hx.DeliverKind, msg hx.Message) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.items = append(in.items, delivery{kind: kind, msg: msg})
}

// claim removes and returns the messages a step should consume this iteration, in
// queue order. On the first step of a turn (includeFollowup) it drains every
// pending delivery; on a follow-on step it takes only steer/inject and leaves
// followups queued so they drive the NEXT turn (followup = "next turn").
func (in *inbox) claim(includeFollowup bool) []hx.Message {
	in.mu.Lock()
	defer in.mu.Unlock()
	var out []hx.Message
	var keep []delivery
	for _, d := range in.items {
		if d.kind == hx.DeliverFollowup && !includeFollowup {
			keep = append(keep, d)
			continue
		}
		out = append(out, d.msg)
	}
	in.items = keep
	return out
}

// empty reports whether nothing at all is queued.
func (in *inbox) empty() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return len(in.items) == 0
}

// owed reports whether a delivery is queued that should drive the driver — a
// followup (next turn) or a steer (next step). Inject alone never counts: it is
// the "no wake" kind, consumed opportunistically by a step that runs for another
// reason.
func (in *inbox) owed() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, d := range in.items {
		if d.kind == hx.DeliverFollowup || d.kind == hx.DeliverSteer {
			return true
		}
	}
	return false
}
