package hx

import (
	"errors"
	"fmt"
)

// ErrStepLimit means a turn still owed work when its configured budget ended.
var ErrStepLimit = errors.New("harness: step budget exhausted")

// TurnError preserves the terminal reason separately from provider prose.
type TurnError struct {
	Reason string
	Cause  error
}

func (e *TurnError) Error() string { return fmt.Sprintf("harness: %s: %v", e.Reason, e.Cause) }

func (e *TurnError) Unwrap() error { return e.Cause }

// TurnOutcome is an operational session event, never acceptance evidence.
type TurnOutcome struct {
	Reason string `json:"reason"`
	Steps  int    `json:"steps"`
	Error  string `json:"error,omitempty"`
}
