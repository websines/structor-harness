package runtime

// Revertible effects are the runtime's Law 1 (system design §4.15, mechanic 1):
// the mirror of the record's supersession — where every fact carries its own
// undo, every registration carries its tracked inverse. An Effect installs
// something (a registration, a watcher, a binding) and hands back the disposer
// that removes EXACTLY what it installed. Unloading a component is therefore
// structural recovery — replaying the inverses in reverse — not best-effort
// cleanup.
//
// The design is honest about the one thing the runtime cannot check: that a
// supplied inverse actually reverts its effect. That witness is an obligation on
// the component author; in Forge it becomes a checkable deterministic obligation
// on first-party components (§4.15 "Deliberately different"). The accumulator-
// recovery invariant proved over this file is the candidate fifth Lean module.

import "sync"

// Effect is a revertible context transformation. It performs an installation and
// returns the inverse that undoes it. A nil dispose means the effect installed
// nothing to undo (a pure read, say); a non-nil err means the installation
// failed and — by contract — installed nothing, so there is nothing to revert.
type Effect func() (dispose func(), err error)

// disposerStack accumulates the inverses a fiber's effects yield, in install
// order, and reverts them in LIFO order — the last thing installed is the first
// thing undone, so teardown unwinds through exactly the states setup passed
// through. It is a fiber's private effect table (mechanic 1).
type disposerStack struct {
	mu        sync.Mutex
	disposers []func()
	reverted  bool
}

// push runs e and, on success, records its disposer. A failed effect returns its
// error and records nothing — it installed nothing, so the stack is unchanged and
// what came before it stays intact. A nil disposer is tolerated and simply not
// recorded.
func (s *disposerStack) push(e Effect) error {
	dispose, err := e()
	if err != nil {
		return err
	}
	if dispose != nil {
		s.mu.Lock()
		s.disposers = append(s.disposers, dispose)
		s.mu.Unlock()
	}
	return nil
}

// applyAll runs effects in order, recording each disposer. If any effect fails,
// it reverts everything accumulated so far and returns the error, leaving the
// stack empty — partial rollback at the failing boundary (mechanic 5): no
// half-applied step survives an activation that could not complete.
func (s *disposerStack) applyAll(effects []Effect) error {
	for _, e := range effects {
		if err := s.push(e); err != nil {
			s.revert()
			return err
		}
	}
	return nil
}

// revert runs every recorded disposer in LIFO order and clears the stack. It is
// idempotent: a second call reverts nothing (double-dispose is a no-op), so a
// fiber that has already unwound cannot double-free — the accumulator-recovery
// invariant. Reactivation is deliberately NOT reuse: a fiber takes a fresh stack
// each time it activates (auto-reopen — "only a fresh activation brings it back"),
// so this instance stays spent once reverted.
func (s *disposerStack) revert() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reverted {
		return
	}
	s.reverted = true
	for i := len(s.disposers) - 1; i >= 0; i-- {
		s.disposers[i]()
	}
	s.disposers = nil
}
