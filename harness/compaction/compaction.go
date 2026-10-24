// Package compaction bounds the derived context by supersession (forge-harness-
// design §2.2, §5.4). When a session's derived history outgrows a TOKEN budget,
// the engine summarizes the oldest span and hands the summary back to the session
// as a Compact — which supersedes that span: token cost drops, the append-only log
// keeps every event (Events() is unchanged), and the derived view shrinks to
// [summary, ...recent tail]. Memory efficiency out of the one mechanism the record
// already runs on.
//
// The trigger is context LENGTH, not a raw message count: compaction fires when
// the estimated token size of the derived history exceeds MaxTokens (a fraction of
// the model's context window). MaxMessages is an optional secondary cap.
//
// The summary can be a real LLM summarization (LLMSummarizer) — the model condenses
// the span — with a deterministic fallback so a failed summarization call never
// breaks compaction. Like the evidence bridge, this package drives the session
// through OPTIONAL-INTERFACE assertions over hx types and never imports the concrete
// session package; it touches only hx + stdlib.
package compaction

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/websines/structor-harness/harness/hx"
)

// Budget defaults, used when a field is left zero.
const (
	// defaultMaxTokens is a modest default window budget so context pressure is felt
	// (and compaction exercised) even against a small local model.
	defaultMaxTokens   = 6000
	defaultCharsPerTok = 4 // rough chars→tokens estimate when no exact count is available
)

// Summarizer condenses a span of messages into one summary string. An LLM
// summarizer calls the model; the deterministic default needs neither model nor
// clock. It takes a context so a real summarization call can be cancelled.
type Summarizer func(ctx context.Context, span []hx.Message) (string, error)

// Engine decides WHEN a session's derived history is over its token budget and HOW
// MUCH of the oldest span to replace with one summary. It holds no session state:
// every decision is a pure function of the current derived view (deterministic,
// clock-free), and the summary content is the only part that may involve a model.
type Engine struct {
	// MaxTokens is the primary budget: the estimated token size of the derived
	// history above which compaction fires. 0 → default. Set this to a fraction
	// (e.g. 0.8) of the target model's context window.
	MaxTokens int
	// MaxMessages is an optional secondary cap on derived message count. 0 → off.
	MaxMessages int
	// CharsPerToken is the chars→tokens divisor for the estimate. 0 → default 4.
	CharsPerToken int
	// Summarize renders the superseded span into one summary. nil → deterministic.
	Summarize Summarizer
	// Score makes compaction QUALITY-AWARE (design §2 follow-up). When set, the
	// engine keeps the highest-scored messages that fit the budget and compacts the
	// rest — anywhere in the history, not just the oldest — so a load-bearing old
	// message survives while a stale newer one is dropped. Higher = keep; negative =
	// prefer compacting. nil → the recency-only prefix strategy. The Forge-native
	// scorer reads the record: a message tied to a still-active obligation scores
	// high, one tied to a settled/superseded thread scores low (see evidence.Sink.Scorer).
	Score func(hx.Message) int
}

// compactor / seqIndexer / budgetReader are the optional interfaces a session
// exposes; asserting them (never importing the session type) is the decoupling.
type compactor interface {
	Compact(throughSeq int, summary hx.Message) error
}
type seqIndexer interface {
	DerivedSeqs() []int
}
type budgetReader interface {
	DerivedLen() int
	DerivedChars() int
}

func (e *Engine) maxTokens() int {
	if e.MaxTokens > 0 {
		return e.MaxTokens
	}
	return defaultMaxTokens
}

func (e *Engine) charsPerToken() int {
	if e.CharsPerToken > 0 {
		return e.CharsPerToken
	}
	return defaultCharsPerTok
}

// estTokens estimates tokens from a character count.
func (e *Engine) estTokens(chars int) int { return chars / e.charsPerToken() }

// MaybeCompact compacts the oldest span when the session's derived history exceeds
// the token budget (or the optional message cap), and is a no-op otherwise.
//
// exactTokens is the model-reported prompt_tokens of the LAST request (from the
// endpoint's usage) — the exact size of the context that was actually sent. When
// it is > 0 the trigger uses it (system prompt + tool schemas + messages all
// counted); when it is 0 (e.g. the first turn, before any response) the engine
// falls back to the character estimate over the derived view. Selection of how
// much to keep always uses the per-message estimate (the exact count is a single
// whole-prompt number and cannot be split per message).
func (e *Engine) MaybeCompact(ctx context.Context, sess hx.Session, exactTokens int) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	msgs := sess.DeriveMessages()
	count, chars := e.measure(sess, msgs)
	tokens := exactTokens
	if tokens <= 0 {
		tokens = e.estTokens(chars)
	}
	overTokens := tokens > e.maxTokens()
	overMsgs := e.MaxMessages > 0 && count > e.MaxMessages
	if !overTokens && !overMsgs {
		return false, nil // within budget — nothing owed, no supersession
	}

	idx, ok := sess.(seqIndexer)
	if !ok {
		return false, nil
	}
	cmp, ok := sess.(compactor)
	if !ok {
		return false, nil
	}
	seqs := idx.DerivedSeqs()
	if len(seqs) != len(msgs) {
		return false, nil // inconsistent view; refuse rather than compact the wrong span
	}

	// How many tokens of MESSAGE HISTORY may we keep? The exact prompt is
	// overhead (system prompt + tool schemas — unshrinkable) plus history. When we
	// know the exact size, target overhead + kept-history ≤ budget; otherwise fall
	// back to half the budget over the estimate.
	keepBudget := e.maxTokens() / 2
	if exactTokens > 0 {
		historyTokens := e.estTokens(chars)
		overhead := exactTokens - historyTokens
		if overhead < 0 {
			overhead = 0
		}
		keepBudget = e.maxTokens() - overhead
		// Always retain a little recent context even when the overhead alone blows
		// the budget (a too-small budget for the model's setup), never zero history.
		if floor := e.maxTokens() / 8; keepBudget < floor {
			keepBudget = floor
		}
	}

	// Quality-aware path: keep the highest-scored messages within budget and
	// compact the rest as an arbitrary set (not just an oldest prefix).
	if e.Score != nil {
		return e.compactByScore(ctx, sess, msgs, seqs, keepBudget)
	}

	keep := e.keepWithinTokens(msgs, keepBudget)
	boundary := len(msgs) - keep
	// Compact a whole tool exchange, including every result for a multi-call message.
	for _, group := range exchangeGroups(msgs) {
		if group[0] < boundary && group[len(group)-1] >= boundary {
			boundary = group[len(group)-1] + 1
		}
	}
	if boundary < 1 {
		return false, nil // nothing older than the retained tail to compact
	}

	// throughSeq = the highest Seq in the intended front span. Taking the MAX (not
	// the boundary's Seq) pulls a prior summary — which floats to the front with a
	// HIGH Seq — into the superseded set instead of stranding it.
	throughSeq := seqs[0]
	for i := 1; i < boundary; i++ {
		if seqs[i] > throughSeq {
			throughSeq = seqs[i]
		}
	}

	// Prior summaries can have a higher sequence than later-derived messages.
	// Close the actual sequence selection over complete tool exchanges as well.
	for changed := true; changed; {
		changed = false
		for _, group := range exchangeGroups(msgs) {
			selected := false
			for _, i := range group {
				selected = selected || seqs[i] <= throughSeq
			}
			if selected {
				for _, i := range group {
					if seqs[i] > throughSeq {
						throughSeq = seqs[i]
						changed = true
					}
				}
			}
		}
	}
	var span []hx.Message
	for i := range msgs {
		if seqs[i] <= throughSeq {
			span = append(span, msgs[i])
		}
	}
	if len(span) < 2 {
		return false, nil // replacing one message with one summary saves nothing
	}

	summary := e.summary(ctx, span)
	// Summarization may fall back after an adapter error, but an aborted owner
	// cannot authorize a replacement of the session's derived history.
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := cmp.Compact(throughSeq, summary); err != nil {
		return false, err
	}
	return true, nil
}

func (e *Engine) measure(sess hx.Session, msgs []hx.Message) (count, chars int) {
	if br, ok := sess.(budgetReader); ok {
		return br.DerivedLen(), br.DerivedChars()
	}
	return len(msgs), totalChars(msgs)
}

// setCompactor is the optional session seam for QUALITY-AWARE compaction: it
// supersedes an arbitrary set of message Seqs (not just an oldest prefix). Asserted
// (never imported) like every other session seam.
type setCompactor interface {
	CompactSet(supersededSeqs []int, summary hx.Message) error
}

// compactByScore keeps the highest-priority messages that fit keepBudget and
// supersedes the rest as a set. Priority is Score dominant, recency (position)
// breaking ties — so a stale message anywhere in the history is dropped while a
// load-bearing old one survives. Requires the session to support CompactSet;
// absent it, declines rather than falling back to a lossy prefix cut.
func (e *Engine) compactByScore(ctx context.Context, sess hx.Session, msgs []hx.Message, seqs []int, keepBudget int) (bool, error) {
	setter, ok := sess.(setCompactor)
	if !ok {
		return false, nil
	}
	groups := exchangeGroups(msgs)
	prio := make([]int, len(groups))
	cost := make([]int, len(groups))
	for g, indices := range groups {
		for _, i := range indices {
			p := e.Score(msgs[i])*(len(msgs)+1) + i
			if i == indices[0] || p > prio[g] {
				prio[g] = p
			}
			cost[g] += messageChars(msgs[i])
		}
	}
	order := make([]int, len(groups))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return prio[order[a]] > prio[order[b]] })
	kept := make([]bool, len(msgs))
	chars, count := 0, 0
	for _, g := range order {
		if (chars+cost[g])/e.charsPerToken() > keepBudget {
			continue
		}
		if e.MaxMessages > 0 && count+len(groups[g]) > e.MaxMessages-1 {
			continue
		}
		chars += cost[g]
		count += len(groups[g])
		for _, i := range groups[g] {
			kept[i] = true
		}
	}
	var supersededSeqs []int
	var span []hx.Message
	for i := range msgs {
		if !kept[i] {
			supersededSeqs = append(supersededSeqs, seqs[i])
			span = append(span, msgs[i])
		}
	}
	if len(supersededSeqs) < 2 {
		return false, nil
	}
	summary := e.summary(ctx, span)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := setter.CompactSet(supersededSeqs, summary); err != nil {
		return false, err
	}
	return true, nil
}

// keepWithinTokens walks from the newest message backward and keeps as many as fit
// tokenBudget (and the optional message cap), reserving one slot for the summary.
func (e *Engine) keepWithinTokens(msgs []hx.Message, tokenBudget int) int {
	cpt := e.charsPerToken()
	msgCap := e.MaxMessages - 1 // one slot reserved for the summary
	keep, chars := 0, 0
	for i := len(msgs) - 1; i >= 0; i-- {
		c := messageChars(msgs[i])
		if (chars+c)/cpt > tokenBudget {
			break
		}
		if e.MaxMessages > 0 && keep+1 > msgCap {
			break
		}
		chars += c
		keep++
	}
	return keep
}

// summary renders the span into one system message. It prefers Summarize (a real
// LLM summary) but falls back to the deterministic summarizer on any error or empty
// result — compaction must never fail because a summarization model call did.
func (e *Engine) summary(ctx context.Context, span []hx.Message) hx.Message {
	text := defaultSummarize(span)
	if e.Summarize != nil {
		if s, err := e.Summarize(ctx, span); err == nil && strings.TrimSpace(s) != "" {
			text = s
		}
	}
	return hx.Message{
		Role:    hx.RoleSystem,
		Content: []hx.ContentBlock{{Kind: hx.BlockText, Text: text}},
	}
}

// LLMSummarizer builds a Summarizer that asks a model to condense the span. It
// flattens the span into a plain transcript (so tool-call/tool-result structure
// can't confuse the request) and asks for a terse summary that preserves the
// load-bearing content — decisions, tool results, established facts, open
// obligations. rt/provider/model select which model does the summarizing (often
// the same one driving the agent, or a cheaper one).
func LLMSummarizer(rt hx.LlmRuntime, provider, model string) Summarizer {
	return func(ctx context.Context, span []hx.Message) (string, error) {
		req := hx.GenerateOptions{
			Provider: provider,
			Model:    model,
			System:   "You compress conversation history. Given a transcript, produce a terse summary that PRESERVES decisions made, tool calls and their results, facts established, and any obligations or checks still open. Omit greetings and small talk. Do not add information that is not in the transcript.",
			Messages: []hx.Message{{
				Role:    hx.RoleUser,
				Content: []hx.ContentBlock{{Kind: hx.BlockText, Text: "Summarize this transcript in a few sentences:\n\n" + renderTranscript(span)}},
			}},
			MaxTokens: 400,
		}
		// This flattened transcript is disposable. Cache only the shared
		// summarizer instructions, avoiding a write premium on the transcript.
		req.PromptCache = &hx.PromptCache{SystemPrefix: req.System}
		ch, err := rt.Stream(ctx, req)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for chunk := range ch {
			switch chunk.Kind {
			case hx.ChunkTextDelta:
				b.WriteString(chunk.Delta)
			case hx.ChunkFinish:
				if chunk.Finish == hx.FinishError {
					return "", fmt.Errorf("compaction summarizer: %s", chunk.Err)
				}
			}
		}
		out := strings.TrimSpace(b.String())
		if out == "" {
			return "", fmt.Errorf("compaction summarizer: empty summary")
		}
		return fmt.Sprintf("[compacted %d earlier messages] %s", len(span), out), nil
	}
}

// renderTranscript flattens a span into a plain-text transcript for summarization.
func renderTranscript(span []hx.Message) string {
	var b strings.Builder
	for _, m := range span {
		role := string(m.Role)
		for _, blk := range m.Content {
			switch blk.Kind {
			case hx.BlockText, hx.BlockReasoning:
				if blk.Text != "" {
					fmt.Fprintf(&b, "%s: %s\n", role, blk.Text)
				}
			case hx.BlockToolCall:
				fmt.Fprintf(&b, "%s: [called tool %s]\n", role, blk.ToolName)
			case hx.BlockToolResult:
				res := blk.Text
				if len(res) > 300 {
					res = res[:300]
				}
				fmt.Fprintf(&b, "tool-result: %s\n", res)
			}
		}
	}
	return b.String()
}

// defaultSummarize is the degraded, deterministic fallback summarizer.
func defaultSummarize(span []hx.Message) string {
	n := len(span)
	if n == 0 {
		return "[compacted 0 earlier messages]"
	}
	first := firstLine(firstText(span[0]))
	if n == 1 {
		return fmt.Sprintf("[compacted 1 earlier message: %q]", first)
	}
	last := firstLine(firstText(span[n-1]))
	return fmt.Sprintf("[compacted %d earlier messages: %q … %q]", n, first, last)
}

func firstText(m hx.Message) string {
	for _, b := range m.Content {
		if (b.Kind == hx.BlockText || b.Kind == hx.BlockReasoning) && b.Text != "" {
			return b.Text
		}
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const width = 80
	if len(s) > width {
		s = s[:width]
	}
	return s
}

func totalChars(msgs []hx.Message) int {
	n := 0
	for _, m := range msgs {
		n += messageChars(m)
	}
	return n
}

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
