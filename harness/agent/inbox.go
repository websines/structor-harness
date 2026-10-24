package agent

import (
	"sync"

	"github.com/websines/structor-harness/harness/hx"
)

// Inbox is an agent's owed-input queue, split into two ordered lists by delivery
// scope: next-turn (followup) and next-step (steer, inject). The loop's
// ReactLoopAgent embeds an Inbox; the loop pushes deliveries in and drains them
// at turn/step boundaries.
//
// Construction is hx-independent (NewInbox takes nothing) so the loop can build
// one without wiring, while Push/Claim carry the hx delivery vocabulary. wake is
// a coalescing signal (buffer 1): a push that must rouse an idle driver
// (followup, steer) sends one non-blocking tick; the driver selects on Wake().
// inject deliberately does not wake — it piggybacks on activity already running.
type Inbox struct {
	mu   sync.Mutex
	turn []hx.Message // next-turn deliveries (followup)
	step []hx.Message // next-step deliveries (steer, inject)
	wake chan struct{}
}

// NewInbox returns an empty Inbox with an armed (buffered) wake signal.
func NewInbox() *Inbox {
	return &Inbox{wake: make(chan struct{}, 1)}
}

// Push files msg into the queue its kind targets and, for waking kinds (followup,
// steer), ticks the wake signal. An unrecognized kind is filed to the next-step
// queue and woken, so a delivered message is never silently dropped.
func (in *Inbox) Push(kind hx.DeliverKind, msg hx.Message) {
	wake := true
	in.mu.Lock()
	switch kind {
	case hx.DeliverFollowup:
		in.turn = append(in.turn, msg)
	case hx.DeliverSteer:
		in.step = append(in.step, msg)
	case hx.DeliverInject:
		in.step = append(in.step, msg)
		wake = false
	default:
		in.step = append(in.step, msg)
	}
	in.mu.Unlock()
	if wake {
		in.signal()
	}
}

// ClaimStep drains and returns the next-step deliveries (steer, inject) in the
// order pushed, clearing that queue. Called at each step boundary; returns nil
// when the queue is empty.
func (in *Inbox) ClaimStep() []hx.Message {
	in.mu.Lock()
	defer in.mu.Unlock()
	out := in.step
	in.step = nil
	return out
}

// ClaimTurn drains BOTH queues and returns them as one turn-entry batch:
// next-step deliveries first (they were bound for the imminent step), then
// next-turn deliveries. Both queues are cleared. Called at a turn boundary;
// returns nil when nothing is owed.
func (in *Inbox) ClaimTurn() []hx.Message {
	in.mu.Lock()
	defer in.mu.Unlock()
	if len(in.step) == 0 && len(in.turn) == 0 {
		return nil
	}
	out := make([]hx.Message, 0, len(in.step)+len(in.turn))
	out = append(out, in.step...)
	out = append(out, in.turn...)
	in.step = nil
	in.turn = nil
	return out
}

// Pending reports whether any delivery is owed in either queue — the loop uses
// it to decide whether it is idle (nothing owed) or must keep working.
func (in *Inbox) Pending() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return len(in.step) > 0 || len(in.turn) > 0
}

// Wake returns the receive end of the coalescing wake signal. The driver selects
// on it to rouse when a waking delivery arrives.
func (in *Inbox) Wake() <-chan struct{} {
	return in.wake
}

// signal ticks the wake channel without blocking; a pending tick coalesces
// multiple pushes into one wake (the driver drains the queues, not the channel).
func (in *Inbox) signal() {
	select {
	case in.wake <- struct{}{}:
	default:
	}
}
