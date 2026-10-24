package preset

import (
	"fmt"
	"sort"
	"strings"

	"github.com/websines/structor-harness/harness/app"
	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/prompt"
	"github.com/websines/structor-harness/harness/tools"
)

// sectionSep joins the base's assembled prompt and this scope's assembled sections.
// It matches internal/harness/prompt's own between-section separator, so a base
// section and a scope section read the same as two base sections would.
const sectionSep = "\n\n"

// ScopedTools is a per-agent tool VIEW over a shared base. It answers with the base
// runtime's tools PLUS this scope's own overlay, the overlay shadowing the base on a
// name clash. It satisfies hx.ToolRuntime, so an agent can use it anywhere the base
// runtime is used. The overlay is a private tools.Runtime: registering onto the
// scope NEVER mutates the shared base, which is what keeps one agent's tools out of
// another agent's view.
//
// Scope decides which body runs, not with what authority it runs: a scoped tool
// still executes with full ambient access (capability confinement needs cordis
// mechanic 7 — see the package doc).
type ScopedTools struct {
	base  hx.ToolRuntime // shared, read-only from here; never mutated
	layer *tools.Runtime // this agent's private overlay
}

// Compile-time proof a ScopedTools is usable wherever the base runtime is.
var _ hx.ToolRuntime = (*ScopedTools)(nil)

// NewScopedTools returns a fresh view over base with an empty overlay. base must be
// non-nil (it is the fallback for every unshadowed name).
func NewScopedTools(base hx.ToolRuntime) *ScopedTools {
	return &ScopedTools{base: base, layer: tools.New(tools.Config{})}
}

// Register adds a tool to THIS scope's overlay and returns its disposer. The shared
// base is untouched, so the tool is visible only to this agent. Overlay registration
// carries the base runtime's own validation (empty name, missing Execute body,
// duplicate within the overlay, reserved names are rejected).
func (s *ScopedTools) Register(def hx.ToolDefinition) (dispose func(), err error) {
	return s.layer.Register(def)
}

// Schemas returns the model-facing schemas of base ∪ overlay, the overlay winning on
// a name clash, sorted by name so the tool list a request carries is deterministic —
// matching the base runtime's own contract.
func (s *ScopedTools) Schemas() []hx.ToolSchema {
	layer := s.layer.Schemas()
	shadowed := make(map[string]struct{}, len(layer))
	out := make([]hx.ToolSchema, 0, len(layer))
	for _, sc := range layer {
		shadowed[sc.Name] = struct{}{}
		out = append(out, sc)
	}
	for _, sc := range s.base.Schemas() {
		if _, hidden := shadowed[sc.Name]; !hidden {
			out = append(out, sc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Execute runs one call, preferring an overlay tool of that name and falling back to
// the base. A name in neither resolves to the base, which returns an "unknown tool"
// error result — so a tool outside this agent's view is absent, never another
// agent's tool.
func (s *ScopedTools) Execute(in hx.ToolExecutionInput) hx.ToolResult {
	if s.inLayer(in.Name) {
		return s.layer.Execute(in)
	}
	return s.base.Execute(in)
}

// Concurrent reports whether a named tool may join a parallel group, resolving the
// same way Execute does: the overlay's answer for a shadowed name, else the base's.
func (s *ScopedTools) Concurrent(name string) bool {
	if s.inLayer(name) {
		return s.layer.Concurrent(name)
	}
	return s.base.Concurrent(name)
}

// inLayer reports whether name is registered in this scope's overlay. The overlay is
// the single source of truth for its own membership, so this stays correct across
// overlay registrations and disposals.
func (s *ScopedTools) inLayer(name string) bool {
	for _, sc := range s.layer.Schemas() {
		if sc.Name == name {
			return true
		}
	}
	return false
}

// ScopedPrompt is a per-agent prompt VIEW over a shared base: the base's sections
// PLUS this scope's own. It satisfies hx.SystemPrompt. Sections added here land on a
// private overlay prompt, so one agent's sections never appear in another's Assemble.
type ScopedPrompt struct {
	base  hx.SystemPrompt      // shared, read-only from here; never mutated
	layer *prompt.SystemPrompt // this agent's private overlay
}

// Compile-time proof a ScopedPrompt is usable wherever the base prompt is.
var _ hx.SystemPrompt = (*ScopedPrompt)(nil)

// NewScopedPrompt returns a fresh view over base with an empty overlay. base must be
// non-nil (it supplies the shared sections every agent sees).
func NewScopedPrompt(base hx.SystemPrompt) *ScopedPrompt {
	return &ScopedPrompt{base: base, layer: prompt.New()}
}

// Section registers an ordered section on THIS scope's overlay and returns its
// disposer. The shared base is untouched, so the section is visible only to this
// agent's Assemble.
func (p *ScopedPrompt) Section(order int, text string) (dispose func(), err error) {
	return p.layer.Section(order, text)
}

// Variable registers a {{name}} provider on this scope's overlay. It resolves only
// for this scope's own sections at Assemble time (base sections resolve against base
// variables); see the package doc on the layer boundary for variables.
func (p *ScopedPrompt) Variable(name string, provide func() string) (dispose func(), err error) {
	return p.layer.Variable(name, provide)
}

// Assemble renders the base's assembly followed by this scope's own sections, joined
// by the section separator. Both layers receive the same request-time vars. The
// result contains every base section plus this agent's, and none of any other
// agent's. (Cross-layer ordering strictly by section order is not possible through
// the opaque base interface, which exposes only Assemble — the base renders as one
// block, the scope's sections as the next.)
func (p *ScopedPrompt) Assemble(vars map[string]string) (string, error) {
	baseStr, err := p.base.Assemble(vars)
	if err != nil {
		return "", err
	}
	layerStr, err := p.layer.Assemble(vars)
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, 2)
	if baseStr != "" {
		parts = append(parts, baseStr)
	}
	if layerStr != "" {
		parts = append(parts, layerStr)
	}
	return strings.Join(parts, sectionSep), nil
}

// Preset composes an App onto a shared base for ONE agent, producing that agent's
// private scoped views. AgentID names the agent the preset belongs to (used for
// identity and error context); App carries the tools and sections to overlay.
type Preset struct {
	AgentID string
	App     app.App
}

// Apply layers p.App onto fresh scoped views of the shared base and returns them —
// the agent's private view. It registers every App tool onto a new ScopedTools(base)
// and every App section onto a new ScopedPrompt(base), leaving the shared base
// untouched so other agents' views are unaffected.
//
// The overlay registration disposers are intentionally dropped: a preset's whole
// scope IS the agent's private view, discarded wholesale when the agent goes away.
// Nothing was contributed to the shared base, so there is nothing to revert onto it.
func (p Preset) Apply(baseTools hx.ToolRuntime, basePrompt hx.SystemPrompt) (*ScopedTools, *ScopedPrompt, error) {
	if baseTools == nil {
		return nil, nil, fmt.Errorf("preset %q: base tool runtime is nil", p.AgentID)
	}
	if basePrompt == nil {
		return nil, nil, fmt.Errorf("preset %q: base system prompt is nil", p.AgentID)
	}

	st := NewScopedTools(baseTools)
	for _, td := range p.App.Tools {
		if _, err := st.Register(td); err != nil {
			return nil, nil, fmt.Errorf("preset %q: register tool %q: %w", p.AgentID, td.Schema.Name, err)
		}
	}

	sp := NewScopedPrompt(basePrompt)
	for _, sec := range p.App.Sections {
		if _, err := sp.Section(sec.Order, sec.Text); err != nil {
			return nil, nil, fmt.Errorf("preset %q: register section (order %d): %w", p.AgentID, sec.Order, err)
		}
	}

	return st, sp, nil
}
