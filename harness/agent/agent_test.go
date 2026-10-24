package agent

import (
	"context"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
)

// --- fakes -------------------------------------------------------------------

type fakeAgent struct{ id string }

func (f *fakeAgent) ID() string                      { return f.id }
func (f *fakeAgent) Send(hx.DeliverKind, hx.Message) {}
func (f *fakeAgent) Followup(string)                 {}
func (f *fakeAgent) WhenIdle(context.Context) error  { return nil }
func (f *fakeAgent) Cancel(string)                   {}

type fakeHandle struct {
	a        hx.Agent
	disposed bool
}

func (h *fakeHandle) Agent() hx.Agent { return h.a }
func (h *fakeHandle) Dispose()        { h.disposed = true }

type fakeFactory struct {
	created []hx.AgentConfig
}

func (f *fakeFactory) Create(cfg hx.AgentConfig) (hx.AgentHandle, error) {
	f.created = append(f.created, cfg)
	return &fakeHandle{a: &fakeAgent{id: cfg.SessionID}}, nil
}

// --- registry ----------------------------------------------------------------

func TestCreateWithoutFactoryErrors(t *testing.T) {
	r := New()
	if _, err := r.Create(hx.AgentConfig{}); err == nil {
		t.Fatal("expected an error creating with no factory set")
	}
}

func TestSetFactoryThenCreateRoutesToIt(t *testing.T) {
	r := New()
	f := &fakeFactory{}
	dispose, err := r.SetFactory(f)
	if err != nil {
		t.Fatalf("SetFactory: %v", err)
	}

	h, err := r.Create(hx.AgentConfig{SessionID: "s1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(f.created) != 1 || f.created[0].SessionID != "s1" {
		t.Fatalf("factory not routed: %+v", f.created)
	}
	if h.Agent().ID() != "s1" {
		t.Fatalf("handle agent id = %q, want s1", h.Agent().ID())
	}
	if got := r.Live(); len(got) != 1 {
		t.Fatalf("live agents = %d, want 1", len(got))
	}

	// Dispose untracks the agent and delegates to the underlying handle.
	inner := h.(*trackedHandle).inner.(*fakeHandle)
	h.Dispose()
	if !inner.disposed {
		t.Fatal("underlying handle not disposed")
	}
	if got := r.Live(); len(got) != 0 {
		t.Fatalf("live agents after dispose = %d, want 0", len(got))
	}
	h.Dispose() // idempotent second dispose must not double-dispose or panic

	// After disposing the factory, Create errors again (previous factory restored).
	dispose()
	if _, err := r.Create(hx.AgentConfig{SessionID: "s2"}); err == nil {
		t.Fatal("expected an error after the factory was disposed")
	}
}

func TestWithInitiatorSetsAndRestoresNested(t *testing.T) {
	r := New()
	a1 := &fakeAgent{id: "a1"}
	a2 := &fakeAgent{id: "a2"}

	if _, ok := r.CurrentInitiator(); ok {
		t.Fatal("expected no initiator initially")
	}

	err := r.WithInitiator(a1, func() error {
		if cur, ok := r.CurrentInitiator(); !ok || cur != a1 {
			t.Fatalf("outer initiator = %v (ok=%v), want a1", cur, ok)
		}
		if inErr := r.WithInitiator(a2, func() error {
			if cur, ok := r.CurrentInitiator(); !ok || cur != a2 {
				t.Fatalf("inner initiator = %v (ok=%v), want a2", cur, ok)
			}
			return nil
		}); inErr != nil {
			return inErr
		}
		// The inner call must restore the outer initiator.
		if cur, ok := r.CurrentInitiator(); !ok || cur != a1 {
			t.Fatalf("post-inner initiator = %v (ok=%v), want a1", cur, ok)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithInitiator: %v", err)
	}
	if _, ok := r.CurrentInitiator(); ok {
		t.Fatal("expected no initiator after the outer call returns")
	}
}

func TestWithInitiatorRestoresOnPanic(t *testing.T) {
	r := New()
	a := &fakeAgent{id: "a"}
	func() {
		defer func() { _ = recover() }()
		_ = r.WithInitiator(a, func() error { panic("boom") })
	}()
	if _, ok := r.CurrentInitiator(); ok {
		t.Fatal("initiator leaked after a panic in fn")
	}
}

// --- inbox -------------------------------------------------------------------

func TestInboxOrdersStepBeforeTurnAndWakes(t *testing.T) {
	in := NewInbox()

	in.Push(hx.DeliverInject, hx.Message{ID: "inject"})   // step, no wake
	in.Push(hx.DeliverSteer, hx.Message{ID: "steer"})     // step, wake
	in.Push(hx.DeliverFollowup, hx.Message{ID: "follow"}) // turn, wake

	// A waking push (steer/followup) signaled the driver.
	select {
	case <-in.Wake():
	default:
		t.Fatal("expected a wake signal after steer/followup push")
	}

	batch := in.ClaimTurn()
	want := []string{"inject", "steer", "follow"} // both step deliveries, then the turn one
	if len(batch) != len(want) {
		t.Fatalf("batch = %v, want ids %v", batch, want)
	}
	for i, id := range want {
		if batch[i].ID != id {
			t.Fatalf("batch[%d].ID = %q, want %q", i, batch[i].ID, id)
		}
	}
	if in.Pending() {
		t.Fatal("inbox should be empty after ClaimTurn")
	}
}

func TestInjectDoesNotWake(t *testing.T) {
	in := NewInbox()
	in.Push(hx.DeliverInject, hx.Message{ID: "x"})

	select {
	case <-in.Wake():
		t.Fatal("inject must not wake the driver")
	default:
	}
	// It is still owed as a next-step delivery.
	if step := in.ClaimStep(); len(step) != 1 || step[0].ID != "x" {
		t.Fatalf("ClaimStep = %v, want a single inject", step)
	}
}

func TestClaimStepLeavesTurnQueue(t *testing.T) {
	in := NewInbox()
	in.Push(hx.DeliverSteer, hx.Message{ID: "s"})
	in.Push(hx.DeliverFollowup, hx.Message{ID: "f"})

	if step := in.ClaimStep(); len(step) != 1 || step[0].ID != "s" {
		t.Fatalf("ClaimStep = %v, want [s]", step)
	}
	if turn := in.ClaimTurn(); len(turn) != 1 || turn[0].ID != "f" {
		t.Fatalf("ClaimTurn = %v, want [f]", turn)
	}
}
