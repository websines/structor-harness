package hx

// The session log — the single source of truth. In Forge each SessionEvent is
// appended as a fact in flg.Store (content-addressed, append-only, actor-stamped,
// commit-anchored). deriveMessages projects LLM history from the log. This
// interface is what the session package provides; the loop consumes it by key.

// EventKind discriminates a SessionEvent. The ~12 core kinds a single turn uses.
type EventKind string

const (
	EvTurnStart        EventKind = "turn/start"
	EvTurnEnd          EventKind = "turn/end"
	EvStepStart        EventKind = "step/start"
	EvStepEnd          EventKind = "step/end"
	EvUserMessage      EventKind = "user/message"
	EvAssistantChunk   EventKind = "assistant/chunk"
	EvAssistantMessage EventKind = "assistant/message"
	EvToolCall         EventKind = "tool/call"
	EvToolResult       EventKind = "tool/result"
	EvSteeringMessage  EventKind = "steering/message"
	EvRequestHeader    EventKind = "request/header"
)

// SessionEvent is one entry in the append-only log. Seq is the contiguous
// position (ordering is by Seq, never by clock — Law 3). Data is the kind's
// payload as JSON. Surface reports whether this event contributes to the derived
// message history (user/assistant/tool messages do; chunks and lifecycle markers
// do not). FactID is the store fact_id once appended (empty before).
type SessionEvent struct {
	Seq     int       `json:"seq"`
	Kind    EventKind `json:"kind"`
	Data    []byte    `json:"data"`
	Surface bool      `json:"surface"`
	FactID  string    `json:"fact_id,omitempty"`
	// PrincipalID stamps who produced it (the initiating agent's verified principal).
	PrincipalID string `json:"principal_id,omitempty"`
	// Commit anchors the event to code state (Law 4).
	Commit string `json:"commit,omitempty"`
}

// RequestHeader is the folded epoch state a model request is reconstructed from.
type RequestHeader struct {
	Provider    string
	Model       string
	System      string
	PromptCache *PromptCache `json:",omitempty"`
}

// Session is one conversation's append-only log + its message projection.
type Session interface {
	ID() string
	// Append records an event as a fact and returns it with Seq/FactID assigned.
	Append(kind EventKind, data []byte, surface bool) (SessionEvent, error)
	// Events returns the log in Seq order (may page from the store; callers should
	// not assume all are resident — memory efficiency).
	Events() []SessionEvent
	// DeriveMessages projects the LLM message history from surface events,
	// cached incrementally (O(new events)).
	DeriveMessages() []Message
	// Header returns the current folded request header.
	Header() RequestHeader
}

// SessionStore is the ctx.Get(KeySession) service.
type SessionStore interface {
	// Create makes a fresh in-memory-headed session backed by the record.
	Create(id string) (Session, error)
	Get(id string) (Session, bool)
}
