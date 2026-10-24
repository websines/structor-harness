// Package prompt assembles an agent's system prompt from ordered sections and
// named {{variable}} providers. It implements hx.SystemPrompt: the loop reads it
// by key (hx.KeyPrompt) and calls Assemble once per request to fold the sections
// and interpolate the variables into the one system string the model sees.
//
// Ordering is by explicit section order, ties broken by registration sequence —
// never by a clock (Law 3). Interpolation is strict: an unresolved {{name}} is a
// loud error, never silently left in the output.
package prompt

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/websines/structor-harness/harness/hx"
)

// sectionSep separates rendered sections in the assembled prompt.
const sectionSep = "\n\n"

// section is one registered ordered block of prompt text. seq is the monotonic
// registration id: it uniquely identifies the section (so its disposer removes
// exactly its own entry) AND breaks order ties deterministically, without a clock.
type section struct {
	order int
	seq   int64
	text  string
}

// variable is one registered {{name}} provider. seq guards its disposer against
// removing a same-named registration that replaced it after disposal.
type variable struct {
	seq     int64
	provide func() string
}

// contextProvider is one registered DYNAMIC section: unlike a static section its
// text is not fixed at registration — provide is invoked at every Assemble, so the
// block reflects the state of the record at that turn. order/seq place it in the
// same ordering space as sections (order ascending, seq breaks ties — never a
// clock). A provider error is surfaced loudly by Assemble, never swallowed.
type contextProvider struct {
	order   int
	seq     int64
	provide func() (string, error)
}

// SystemPrompt is the concurrency-safe hx.SystemPrompt implementation.
type SystemPrompt struct {
	mu       sync.Mutex
	next     int64                      // monotonic registration counter (tie-break + disposer identity)
	sections map[int64]*section         // keyed by registration seq
	contexts map[int64]*contextProvider // keyed by registration seq
	vars     map[string]*variable       // keyed by variable name
}

var _ hx.SystemPrompt = (*SystemPrompt)(nil)

// New returns an empty SystemPrompt ready to register sections, dynamic context
// providers, and variables.
func New() *SystemPrompt {
	return &SystemPrompt{
		sections: make(map[int64]*section),
		contexts: make(map[int64]*contextProvider),
		vars:     make(map[string]*variable),
	}
}

// Section registers a static block rendered at the given order (ascending).
// Sections with equal order render in registration order — a deterministic
// tie-break, never a clock. The returned disposer removes exactly this section
// and is safe to call more than once.
func (p *SystemPrompt) Section(order int, text string) (func(), error) {
	p.mu.Lock()
	seq := p.next
	p.next++
	p.sections[seq] = &section{order: order, seq: seq, text: text}
	p.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			delete(p.sections, seq)
			p.mu.Unlock()
		})
	}, nil
}

// Context registers a DYNAMIC section computed at Assemble time. Unlike Section —
// whose text is fixed when it is registered — provide is invoked on every Assemble,
// so the rendered block always reflects the state of the record at that turn (this
// is how verified, never-stale context is folded in). It is ordered in the same
// space as static sections (order ascending, registration order breaking ties), so
// a context block can be placed precisely among the static ones. A provider that
// returns an error fails Assemble loudly — a context that cannot be produced is
// never silently dropped. The returned disposer removes exactly this provider and
// is safe to call more than once.
func (p *SystemPrompt) Context(order int, provide func() (string, error)) (func(), error) {
	if provide == nil {
		return nil, fmt.Errorf("prompt: Context: nil provider")
	}
	p.mu.Lock()
	seq := p.next
	p.next++
	p.contexts[seq] = &contextProvider{order: order, seq: seq, provide: provide}
	p.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			delete(p.contexts, seq)
			p.mu.Unlock()
		})
	}, nil
}

// Variable registers a provider for {{name}}. Re-registering a live name is a
// loud error (dispose it first). The returned disposer removes this provider —
// and only this one, even if a later registration replaced it — and is safe to
// call more than once.
func (p *SystemPrompt) Variable(name string, provide func() string) (func(), error) {
	if name == "" {
		return nil, fmt.Errorf("prompt: Variable: empty name")
	}
	if provide == nil {
		return nil, fmt.Errorf("prompt: Variable %q: nil provider", name)
	}
	p.mu.Lock()
	if _, exists := p.vars[name]; exists {
		p.mu.Unlock()
		return nil, fmt.Errorf("prompt: Variable %q already registered", name)
	}
	seq := p.next
	p.next++
	p.vars[name] = &variable{seq: seq, provide: provide}
	p.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			if v, ok := p.vars[name]; ok && v.seq == seq {
				delete(p.vars, name)
			}
			p.mu.Unlock()
		})
	}, nil
}

// renderable is one ordered unit of the assembled prompt: either a static section
// (text set, dyn nil) or a dynamic context provider (dyn set). Both kinds share one
// ordering space, so a dynamic block can be positioned precisely among the static
// sections. Interleaving them here — rather than appending context at a fixed
// position — is what lets the FLG context block sit exactly where it belongs.
type renderable struct {
	order int
	seq   int64
	text  string                 // static section body (interpolated)
	dyn   func() (string, error) // dynamic context provider (rendered as-is)
}

// Assemble renders the registered sections and dynamic context providers in order
// and interpolates every {{name}} reference in the static sections from vars
// (request-time, highest precedence) then from the registered variable providers.
// An unresolved reference is a loud error, and so is a context provider that fails.
//
// A static Section carries text fixed at registration; a Context provider is
// invoked HERE, once per Assemble, so its block reflects the record as of this
// turn. This is the whole point of the split: the FLG context stays never-stale
// because it is recomputed every assemble, not frozen when it was registered.
func (p *SystemPrompt) Assemble(vars map[string]string) (string, error) {
	body, _, err := p.AssembleWithCachePrefix(vars)
	return body, err
}

// AssembleWithCachePrefix renders once and identifies only the leading literal
// sections as reusable. Dynamic context and variable interpolation end the prefix,
// even when their current value is empty; each provider still runs exactly once.
func (p *SystemPrompt) AssembleWithCachePrefix(vars map[string]string) (string, string, error) {
	// Snapshot under the lock, then build without it: provider funcs (variables AND
	// context providers) run outside the lock, so a provider may safely touch the
	// prompt without deadlocking.
	p.mu.Lock()
	ordered := make([]renderable, 0, len(p.sections)+len(p.contexts))
	for _, s := range p.sections {
		ordered = append(ordered, renderable{order: s.order, seq: s.seq, text: s.text})
	}
	for _, c := range p.contexts {
		ordered = append(ordered, renderable{order: c.order, seq: c.seq, dyn: c.provide})
	}
	providers := make(map[string]func() string, len(p.vars))
	for name, v := range p.vars {
		providers[name] = v.provide
	}
	p.mu.Unlock()

	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].order != ordered[j].order {
			return ordered[i].order < ordered[j].order
		}
		return ordered[i].seq < ordered[j].seq // deterministic tie-break, no clock
	})

	// One memo shared across sections keeps each {{variable}} provider invoked at
	// most once per Assemble, however many sections reference it.
	memo := make(map[string]string)
	var body strings.Builder
	prefixEnd := 0
	stable := true
	for i, r := range ordered {
		if r.dyn != nil || strings.Contains(r.text, "{{") {
			stable = false
		}
		if i > 0 {
			body.WriteString(sectionSep)
		}
		if r.dyn != nil {
			// Dynamic context is produced fresh at assemble time. Its output is final:
			// like a substituted variable value it is not re-scanned for {{...}}, so a
			// fact summary that happens to contain braces can never break assembly.
			s, err := r.dyn()
			if err != nil {
				return "", "", fmt.Errorf("prompt: context provider (order %d): %w", r.order, err)
			}
			body.WriteString(s)
			continue
		}
		s, err := interpolate(r.text, vars, providers, memo)
		if err != nil {
			return "", "", err
		}
		body.WriteString(s)
		if stable {
			prefixEnd = body.Len()
		}
	}
	full := body.String()
	return full, full[:prefixEnd], nil
}

// interpolate replaces every {{name}} in text with its resolved value in a single
// left-to-right pass (substituted values are never re-scanned, so a value that
// happens to contain braces is inert). memo is shared across the whole Assemble so
// a provider is invoked at most once per Assemble, only if its name is referenced.
func interpolate(text string, vars map[string]string, providers map[string]func() string, memo map[string]string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(text); {
		open := strings.Index(text[i:], "{{")
		if open < 0 {
			out.WriteString(text[i:])
			break
		}
		out.WriteString(text[i : i+open])
		nameStart := i + open + 2
		end := strings.Index(text[nameStart:], "}}")
		if end < 0 {
			return "", fmt.Errorf("prompt: unterminated {{ near offset %d", i+open)
		}
		raw := text[nameStart : nameStart+end]
		name := strings.TrimSpace(raw)
		if name == "" {
			return "", fmt.Errorf("prompt: empty variable reference {{%s}}", raw)
		}
		val, err := resolve(name, vars, providers, memo)
		if err != nil {
			return "", err
		}
		out.WriteString(val)
		i = nameStart + end + 2
	}
	return out.String(), nil
}

// resolve looks name up in the request vars first, then the registered providers.
// A name matched by neither is a loud error — the strict-interpolation contract.
func resolve(name string, vars map[string]string, providers map[string]func() string, memo map[string]string) (string, error) {
	if v, ok := vars[name]; ok {
		return v, nil
	}
	if v, ok := memo[name]; ok {
		return v, nil
	}
	if provide, ok := providers[name]; ok {
		v := provide()
		memo[name] = v
		return v, nil
	}
	return "", fmt.Errorf("prompt: unresolved variable %q (no request var or registered provider)", name)
}
