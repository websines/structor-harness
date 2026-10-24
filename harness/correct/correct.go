// Package correct is the harness self-correction seam: detect a failing loop and
// don't repeat the mistake. It ports two mechanics from the DeepSeek harness (dsh)
// and adds one dsh lacks:
//
//   - RepeatGuard — dsh's repeat-tool loop-breaker. It counts consecutive
//     identical (tool, canonical-args) calls per initiator and, at escalating
//     thresholds ([3,5,8] by default), APPENDS an advisory to the tool result
//     telling the model to re-read the last result, change approach, or finish.
//     It never blocks and never rewrites the call — the decision stays with the
//     model. IMPROVEMENT over dsh: dsh fires only at the exact configured counts
//     and then GOES SILENT past the highest threshold; a chain stuck at 20 repeats
//     gets no further nudge. Here, once the top threshold is crossed the top-tier
//     advisory keeps firing on every further repeat, so a runaway loop is never
//     silently tolerated.
//
//   - RecoveryMessage — dsh's crash-repair idea (feed a typed, side-effect-aware
//     recovery message back to the model on failure, not a bare error) applied to
//     any tool error: retry only when the call is read-only/idempotent, otherwise
//     verify external state or change approach — never retry blindly.
//
//   - FailureMemory — the persistent "avoid this" store dsh does NOT have. dsh's
//     repeat guard is in-memory-per-agent and forgets across compaction/resume,
//     and it only reacts to EXACT repeats. FailureMemory instead records every
//     failed attempt as a structured fact and surfaces recent failures as an
//     "Avoid repeating…" prompt block on later turns, so the model carries the
//     lesson forward even after the exact-repeat chain has reset.
//
// The Component wires all three onto the runtime: the guard and failure-memory
// middleware mount on the ToolRuntime, and the failure Advisory mounts as a
// dynamic prompt-context section so past failures are injected into later turns.
// Every registration is revertible. Standard library only.
package correct

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/runtime"
)

// Tunables. maxReasonChars head-bounds any error excerpt quoted back to the model
// (dsh's argumentsPreviewChars idea); defaultRecentN is how many past failures the
// prompt block surfaces; promptOrder places that block late among the sections.
const (
	maxReasonChars = 500
	defaultRecentN = 5
	promptOrder    = 850

	sourceRepeatGuard = "harness/correct/repeat-guard"
	advisoryMarker    = "[self-correction]"
	advisoryHeader    = "Avoid repeating these past failures — they did not work:"
)

// Sentinel errors returned by Component when a mounted service lacks the optional
// capability the self-correction wiring needs.
var (
	// ErrNoToolMiddleware is returned when KeyTools does not accept middleware.
	ErrNoToolMiddleware = errors.New("correct: tool runtime does not support middleware")
	// ErrNoPromptContext is returned when KeyPrompt has no dynamic context section.
	ErrNoPromptContext = errors.New("correct: system prompt does not support dynamic context sections")
)

// ---------------------------------------------------------------------------
// canonicalization + hashing
// ---------------------------------------------------------------------------

// canonical normalizes raw args to their comparison form: the raw bytes trimmed
// of surrounding whitespace. Two calls whose args differ only in leading/trailing
// whitespace canonicalize identically; anything deeper is deliberately out of
// scope (exact-match detection, like dsh — fuzzy matching is not attempted).
func canonical(args []byte) string { return strings.TrimSpace(string(args)) }

// hashArgs is the sha256 of the canonical args, hex-encoded — the chain key's
// argument half. Identical canonical args hash identically, by construction.
func hashArgs(args []byte) string {
	sum := sha256.Sum256([]byte(canonical(args)))
	return hex.EncodeToString(sum[:])
}

// Signature hashes (tool, canonical-args) into a stable hex digest — the identity
// a Failure carries so the same failing call is recognizable across turns. A NUL
// separator keeps ("ab","c") distinct from ("a","bc").
func Signature(tool string, args []byte) string {
	h := sha256.New()
	h.Write([]byte(tool))
	h.Write([]byte{0})
	h.Write([]byte(canonical(args)))
	return hex.EncodeToString(h.Sum(nil))
}

// excerpt trims s and head-bounds it at max, appending an omitted-count marker
// when it truncates (dsh's capped-preview style) so an error blob cannot ride
// unbounded into the next request.
func excerpt(s string, max int) string {
	s = strings.TrimSpace(s)
	if max > 0 && len(s) > max {
		return s[:max] + fmt.Sprintf("… (+%d more chars)", len(s)-max)
	}
	return s
}

// ---------------------------------------------------------------------------
// RepeatGuard — the loop-breaker
// ---------------------------------------------------------------------------

// callKey is the chain key: a tool name plus the hash of its canonical arguments.
type callKey struct {
	tool string
	hash string
}

// RepeatGuard is a tool middleware that breaks failing loops. It counts
// consecutive identical (tool name, canonical args) invocations per initiator
// and, at escalating thresholds, APPENDS an advisory to the tool result — it
// never blocks and never rewrites the call. Unlike dsh it does not go silent past
// the highest threshold: the top-tier advisory keeps firing so a runaway loop is
// always surfaced. Safe for concurrent use.
type RepeatGuard struct {
	mu         sync.Mutex
	thresholds []int              // ascending, deduplicated, each >= 2
	last       map[string]callKey // per-initiator last observed call
	count      map[string]int     // per-initiator consecutive-repeat count
}

// NewRepeatGuard returns a guard with the default thresholds [3,5,8].
func NewRepeatGuard() *RepeatGuard {
	return &RepeatGuard{
		thresholds: normalizeThresholds([]int{3, 5, 8}),
		last:       make(map[string]callKey),
		count:      make(map[string]int),
	}
}

// normalizeThresholds drops values below 2 and duplicates, sorts ascending, and
// falls back to the default when nothing survives — the list is never empty.
func normalizeThresholds(in []int) []int {
	seen := make(map[int]bool, len(in))
	out := make([]int, 0, len(in))
	for _, t := range in {
		if t < 2 || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Ints(out)
	if len(out) == 0 {
		return []int{3, 5, 8}
	}
	return out
}

// observe records one call and returns its consecutive-repeat count for the
// initiator: a call identical to the previous one increments the counter, a
// different call resets it to 1.
func (g *RepeatGuard) observe(initiator, tool string, args []byte) int {
	key := callKey{tool: tool, hash: hashArgs(args)}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last[initiator] == key {
		g.count[initiator]++
	} else {
		g.last[initiator] = key
		g.count[initiator] = 1
	}
	return g.count[initiator]
}

// Middleware wraps the next dispatch fn. It counts this call, runs it, and — when
// the count trips a threshold — appends the advisory to the returned result's
// Value AND stages it as injected context for the next step. It never short-
// circuits: next always runs, and the advisory rides the real result.
func (g *RepeatGuard) Middleware() hx.ToolMiddleware {
	return func(next hx.ToolExecFn) hx.ToolExecFn {
		return func(in hx.ToolExecutionInput) hx.ToolResult {
			n := g.observe(in.Initiator, in.Name, in.Args)
			res := next(in)
			if adv := advisoryFor(in.Name, n, g.thresholds); adv != "" {
				res.Value = withAdvisory(res.Value, adv)
				res.AdditionalContext = append(res.AdditionalContext, advisoryMessage(adv))
			}
			return res
		}
	}
}

// tierFor maps a repeat count to an advisory tier, or -1 for "no advisory".
// A count equal to the i-th threshold is tier i; a count PAST the highest
// threshold stays at the top tier (never silent); counts between thresholds and
// below the first draw nothing.
func tierFor(n int, thresholds []int) int {
	if len(thresholds) == 0 {
		return -1
	}
	top := len(thresholds) - 1
	if n > thresholds[top] {
		return top
	}
	for i, t := range thresholds {
		if n == t {
			return i
		}
	}
	return -1
}

// advisoryFor renders the escalating advisory for a repeat count, or "" when no
// tier applies. The three tiers intensify: notice → warning → ALERT.
func advisoryFor(tool string, n int, thresholds []int) string {
	switch tierFor(n, thresholds) {
	case -1:
		return ""
	case 0:
		return fmt.Sprintf("Repeat notice: you have called %q with identical arguments %d times in a row. "+
			"Read the previous result carefully before calling again — if the task is not complete, change your "+
			"approach or arguments instead of repeating the same call.", tool, n)
	case 1:
		return fmt.Sprintf("Repeat warning: %q has now run with the SAME arguments %d times in a row and is making "+
			"no progress. Do not call it with these exact arguments again. Inspect the latest result and pick a "+
			"different action, different arguments, or finish the task if you already have enough evidence.", tool, n)
	default:
		return fmt.Sprintf("Repeat ALERT: %q has run with identical arguments %d times in a row — this is a stuck "+
			"loop. Stop repeating this call. You must change strategy now: try a materially different approach, "+
			"gather different evidence, or conclude the task. Calling it again with these arguments will not change "+
			"the outcome.", tool, n)
	}
}

// withAdvisory appends the advisory to a model-visible tool result value as a
// clearly-marked trailing block, so the nudge rides the result without depending
// on its shape. It never mutates the caller's slice.
func withAdvisory(value []byte, advisory string) []byte {
	return []byte(string(value) + "\n\n" + advisoryMarker + " " + advisory)
}

// advisoryMessage stages the advisory as an injected synthetic user message — the
// faithful port of dsh's additionalContexts delivery, source-attributed for audit.
func advisoryMessage(text string) hx.Message {
	return hx.Message{
		Role:    hx.RoleUser,
		Content: []hx.ContentBlock{{Kind: hx.BlockText, Text: text}},
		Source:  sourceRepeatGuard,
	}
}

// ---------------------------------------------------------------------------
// FailureMemory — the persistent "avoid this" store
// ---------------------------------------------------------------------------

// Failure is one recorded failed attempt. Signature is the hash of (tool, args)
// or of an obligation — the identity that ties recurrences of the same mistake
// together.
type Failure struct {
	Action    string // what was attempted (e.g. the tool name)
	Reason    string // why it failed (a bounded excerpt)
	Signature string // hash of (tool,args) or (obligation)
}

// FailureMemory is an append-only record of failed attempts, surfaced as
// "avoid this" context on later turns. Safe for concurrent use.
type FailureMemory struct {
	mu       sync.Mutex
	failures []Failure
}

// NewFailureMemory returns an empty failure memory.
func NewFailureMemory() *FailureMemory { return &FailureMemory{} }

// Record appends a failure. Append-only: nothing is ever evicted or overwritten,
// so the store is a faithful history of what has been tried and failed.
func (m *FailureMemory) Record(f Failure) {
	m.mu.Lock()
	m.failures = append(m.failures, f)
	m.mu.Unlock()
}

// Recent returns up to n most recent failures, newest first. n <= 0 returns nil.
func (m *FailureMemory) Recent(n int) []Failure {
	if n <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	start := 0
	if len(m.failures) > n {
		start = len(m.failures) - n
	}
	tail := m.failures[start:]
	out := make([]Failure, len(tail))
	for i, f := range tail {
		out[len(tail)-1-i] = f // reverse into newest-first
	}
	return out
}

// Advisory renders the n most recent failures as a prompt block:
//
//	Avoid repeating these past failures — they did not work:
//	- <action> failed because <reason>
//
// It returns "" when there is nothing to warn about, so an empty memory injects
// no block.
func (m *FailureMemory) Advisory(n int) string {
	fs := m.Recent(n)
	if len(fs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(advisoryHeader)
	for _, f := range fs {
		b.WriteString("\n- ")
		b.WriteString(f.Action)
		b.WriteString(" failed because ")
		b.WriteString(f.Reason)
	}
	return b.String()
}

// Middleware records a failure whenever the wrapped tool returns IsError: Action
// is the tool name, Reason a bounded excerpt of the error value, Signature the
// hash of (tool, args). A successful call records nothing. The tool result the
// model sees is never altered — the memory is a side record.
func (m *FailureMemory) Middleware() hx.ToolMiddleware {
	return func(next hx.ToolExecFn) hx.ToolExecFn {
		return func(in hx.ToolExecutionInput) hx.ToolResult {
			res := next(in)
			if res.IsError {
				m.Record(Failure{
					Action:    in.Name,
					Reason:    excerpt(string(res.Value), maxReasonChars),
					Signature: Signature(in.Name, in.Args),
				})
			}
			return res
		}
	}
}

// ---------------------------------------------------------------------------
// RecoveryMessage — crash-repair applied to any tool error
// ---------------------------------------------------------------------------

// RecoveryMessage formats a tool error into a model-actionable, idempotency-aware
// recovery message instead of a bare error. An idempotent (read-only) tool is
// safe to retry; a non-idempotent one may have already caused side effects, so
// the model is told to verify external state or change approach — never to retry
// blindly. This is dsh's crash-repair recovery text applied to live tool errors.
func RecoveryMessage(tool string, idempotent bool, errExcerpt string) string {
	reason := excerpt(errExcerpt, maxReasonChars)
	if idempotent {
		return fmt.Sprintf("The tool %q failed: %s. This tool is read-only/idempotent, so it is safe to retry — "+
			"but only if the same call could plausibly succeed now (for example a transient error). If the inputs "+
			"were wrong, change them rather than retrying the identical call.", tool, reason)
	}
	return fmt.Sprintf("The tool %q failed: %s. This tool is NOT idempotent and may have already caused side "+
		"effects. Do not retry blindly: first verify external state to determine what actually happened, then "+
		"either change your approach or ask before trying again.", tool, reason)
}

// ---------------------------------------------------------------------------
// Component — wire it all onto the runtime
// ---------------------------------------------------------------------------

// Component mounts the self-correction seam: it installs the repeat-guard and
// failure-memory middleware on the ToolRuntime (KeyTools) and registers the
// failure Advisory as a dynamic prompt-context section (KeyPrompt), so past
// failures are injected into later turns. It injects KeyTools + KeyPrompt and
// provides nothing; every registration is a revertible effect.
func Component() *runtime.Component {
	guard := NewRepeatGuard()
	mem := NewFailureMemory()

	return &runtime.Component{
		Name:   "harness/correct",
		Inject: []string{hx.KeyTools, hx.KeyPrompt},
		Apply: func(ctx *runtime.Context) error {
			tools, err := get[hx.ToolRuntime](ctx, hx.KeyTools)
			if err != nil {
				return err
			}
			sp, err := get[hx.SystemPrompt](ctx, hx.KeyPrompt)
			if err != nil {
				return err
			}

			// The ToolRuntime accepts middleware through an optional interface;
			// asserting it keeps this package decoupled from the tools package.
			mwUser, ok := tools.(interface {
				Use(hx.ToolMiddleware) func()
			})
			if !ok {
				return ErrNoToolMiddleware
			}
			// Guard outermost (it must see every repeat), failure-memory inner
			// (it records the real result). Both are revertible effects.
			if err := ctx.Use(func() (func(), error) {
				return mwUser.Use(guard.Middleware()), nil
			}); err != nil {
				return err
			}
			if err := ctx.Use(func() (func(), error) {
				return mwUser.Use(mem.Middleware()), nil
			}); err != nil {
				return err
			}

			// The SystemPrompt exposes dynamic sections through an optional
			// interface; the Advisory is recomputed at every Assemble so later
			// turns always carry the freshest "avoid this" block.
			pctx, ok := sp.(interface {
				Context(int, func() (string, error)) (func(), error)
			})
			if !ok {
				return ErrNoPromptContext
			}
			return ctx.Use(func() (func(), error) {
				return pctx.Context(promptOrder, func() (string, error) {
					return mem.Advisory(defaultRecentN), nil
				})
			})
		},
	}
}

// get resolves the service at key and asserts it to T — the by-key resolution the
// spine uses instead of importing a concrete sibling. ctx.Get enforces the
// component's declared confinement.
func get[T any](ctx *runtime.Context, key string) (T, error) {
	var zero T
	v, err := ctx.Get(key)
	if err != nil {
		return zero, err
	}
	t, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("correct: service %q has unexpected type %T", key, v)
	}
	return t, nil
}
