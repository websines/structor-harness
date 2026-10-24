package session

import (
	"encoding/json"

	"github.com/websines/structor-harness/harness/hx"
)

// DeriveMessages projects the LLM message history from the log's SURFACE events
// (user/message, assistant/message, tool/result) — lifecycle markers and
// assistant/chunk deltas contribute nothing. The projection is cached and advanced
// INCREMENTALLY: each call folds only the events appended since the last call
// (from deriveNext to the head's end) and advances the cursor, so the cost is
// O(new events), never O(whole log). This is the session's analogue of the
// resolver projecting verdicts from facts.
func (s *session) DeriveMessages() []hx.Message {
	for ; s.deriveNext < len(s.head); s.deriveNext++ {
		ev := s.head[s.deriveNext]
		if !ev.Surface {
			continue
		}
		// Supersession (design §2.2): a compacted-away event is in the log but not in
		// the derived view. New events are always newer than the watermark, so this
		// skip never fires on the incremental tail — the incremental cost is unchanged.
		if s.superseded(ev.Seq) {
			continue
		}
		// A compaction summary is placed at the FRONT by rederive (it replaces the
		// oldest span), never appended in Seq order here. Summaries only ever enter the
		// log via Compact, which rederives and advances the cursor past them, so this
		// guard is belt-and-suspenders.
		if ev.Kind == evCompactionSummary {
			continue
		}
		if msg, ok := messageFromEvent(ev); ok {
			s.msgCache = append(s.msgCache, msg)
		}
	}
	return s.msgCache
}

// rederive rebuilds the message cache from the whole log after a compaction changed
// its shape. Active compaction summaries (Seq > the watermark) float to the FRONT —
// each stands in for the oldest span it replaced — followed by the retained tail in
// Seq order; the superseded span (Seq <= the watermark) is dropped. The fold cursor
// is reset to the head end so subsequent Append/DeriveMessages stay incremental.
// Called only from Compact — a rare operation — so its O(head) cost is not on the
// per-turn path.
func (s *session) rederive() {
	var leading, body []hx.Message
	for i := range s.head {
		ev := s.head[i]
		if !ev.Surface || s.superseded(ev.Seq) {
			continue
		}
		msg, ok := messageFromEvent(ev)
		if !ok {
			continue
		}
		if ev.Kind == evCompactionSummary {
			leading = append(leading, msg)
		} else {
			body = append(body, msg)
		}
	}
	s.msgCache = append(leading, body...)
	s.deriveNext = len(s.head)
}

// DerivedSeqs returns the Seq of the source event behind each derived message, in
// the SAME order DeriveMessages yields them (active summaries first, then the
// retained tail). It is the map the compaction engine needs to turn a
// derived-position span into a Seq watermark for Compact — expressed in ints so
// the engine never imports this package (it walks hx types only).
func (s *session) DerivedSeqs() []int {
	s.DeriveMessages() // ensure the head is fully folded before we mirror its order
	var leading, body []int
	for i := range s.head {
		ev := s.head[i]
		if !ev.Surface || s.superseded(ev.Seq) {
			continue
		}
		if _, ok := messageFromEvent(ev); !ok {
			continue
		}
		if ev.Kind == evCompactionSummary {
			leading = append(leading, ev.Seq)
		} else {
			body = append(body, ev.Seq)
		}
	}
	return append(leading, body...)
}

// DerivedLen reports how many messages the current derived view holds — the
// message-count budget the compaction engine reads.
func (s *session) DerivedLen() int { return len(s.DeriveMessages()) }

// DerivedChars reports the total character count across the derived view — the
// token-estimate budget the compaction engine reads (chars as a deterministic,
// clock-free proxy for tokens).
func (s *session) DerivedChars() int {
	total := 0
	for _, m := range s.DeriveMessages() {
		total += messageChars(m)
	}
	return total
}

// messageChars estimates a message's size as the sum of ALL its blocks' payloads
// — the token proxy the derived budget is measured in. It counts tool-call args
// and tool-result content, not just text: in a tool-heavy session those are the
// bulk of the context, and ignoring them made the budget wildly under-count.
func messageChars(m hx.Message) int {
	n := 0
	for _, b := range m.Content {
		switch b.Kind {
		case hx.BlockText, hx.BlockReasoning:
			n += len(b.Text)
		case hx.BlockToolResult:
			n += len(b.Text)
		case hx.BlockToolCall:
			n += len(b.ToolName) + len(b.ToolArgs)
		}
	}
	return n
}

// Header returns the current folded request header — the payload of the latest
// request/header event. Folded incrementally like DeriveMessages, over its own
// cursor: only events appended since the last call are scanned, so a header read
// never re-walks the whole log. Latest wins by Seq order, never by clock (Law 3).
func (s *session) Header() hx.RequestHeader {
	for ; s.headerNext < len(s.head); s.headerNext++ {
		ev := s.head[s.headerNext]
		if ev.Kind != hx.EvRequestHeader {
			continue
		}
		var h hx.RequestHeader
		if err := json.Unmarshal(ev.Data, &h); err == nil {
			s.header = h
		}
	}
	return s.header
}

// messageFromEvent decodes a surface event's payload into an hx.Message and stamps
// its provenance (Source = the event's FactID, the fact that produced it). A
// surface event carries a JSON-encoded hx.Message as its Data. A payload that does
// not decode is skipped, not projected.
func messageFromEvent(ev hx.SessionEvent) (hx.Message, bool) {
	var m hx.Message
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		return hx.Message{}, false
	}
	if m.Source == "" {
		m.Source = ev.FactID
	}
	return m, true
}
