package preset

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/websines/structor-harness/harness/app"
	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/prompt"
	"github.com/websines/structor-harness/harness/tools"
)

// toolReturning is a tool whose result Value is the JSON string ret, so an Execute
// test can tell WHICH body ran (base's vs. an overlay's shadowing version).
func toolReturning(name, ret string) hx.ToolDefinition {
	return hx.ToolDefinition{
		Schema: hx.ToolSchema{Name: name, Description: name},
		Execute: func(rc hx.ToolRunContext, args []byte) (hx.ToolResult, error) {
			v, _ := json.Marshal(ret)
			return hx.ToolResult{Value: v}, nil
		},
	}
}

// schemaNames returns the sorted set of tool names in a Schemas() result.
func schemaNames(schemas []hx.ToolSchema) []string {
	out := make([]string, 0, len(schemas))
	for _, s := range schemas {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

// decodeStr asserts res is not an error result and returns its JSON-string Value.
func decodeStr(t *testing.T, res hx.ToolResult) string {
	t.Helper()
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Value)
	}
	var s string
	if err := json.Unmarshal(res.Value, &s); err != nil {
		t.Fatalf("unmarshal result value %q: %v", res.Value, err)
	}
	return s
}

// newBase builds a real shared base: one common tool + one BASE prompt section, the
// substrate both agents' presets layer over.
func newBase(t *testing.T) (*tools.Runtime, *prompt.SystemPrompt) {
	t.Helper()
	bt := tools.New(tools.Config{})
	if _, err := bt.Register(toolReturning("common", "base-common")); err != nil {
		t.Fatalf("register base common: %v", err)
	}
	bp := prompt.New()
	if _, err := bp.Section(0, "BASE"); err != nil {
		t.Fatalf("register base section: %v", err)
	}
	return bt, bp
}

// alpha / beta presets used across the per-agent tests.
func alphaPreset() Preset {
	return Preset{AgentID: "alpha", App: app.App{
		Name:     "alpha-app",
		Tools:    []hx.ToolDefinition{toolReturning("toolA", "toolA-ran")},
		Sections: []app.Section{{Order: 10, Text: "SEC-A"}},
	}}
}

func betaPreset() Preset {
	return Preset{AgentID: "beta", App: app.App{
		Name:     "beta-app",
		Tools:    []hx.ToolDefinition{toolReturning("toolB", "toolB-ran")},
		Sections: []app.Section{{Order: 10, Text: "SEC-B"}},
	}}
}

// TestPerAgentToolSets: two agents over ONE shared base each see common PLUS their
// own tool and NOT the other agent's — per-agent tool sets.
func TestPerAgentToolSets(t *testing.T) {
	bt, bp := newBase(t)

	alphaTools, _, err := alphaPreset().Apply(bt, bp)
	if err != nil {
		t.Fatalf("alpha Apply: %v", err)
	}
	betaTools, _, err := betaPreset().Apply(bt, bp)
	if err != nil {
		t.Fatalf("beta Apply: %v", err)
	}

	if got, want := schemaNames(alphaTools.Schemas()), []string{"common", "toolA"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("alpha Schemas = %v, want %v", got, want)
	}
	if got, want := schemaNames(betaTools.Schemas()), []string{"common", "toolB"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("beta Schemas = %v, want %v", got, want)
	}

	// The shared base is untouched by either scope: still just {common}.
	if got, want := schemaNames(bt.Schemas()), []string{"common"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("shared base Schemas mutated = %v, want %v", got, want)
	}
}

// TestPerAgentPromptSections: each agent's Assemble contains BASE plus its own
// section and NOT the other's.
func TestPerAgentPromptSections(t *testing.T) {
	bt, bp := newBase(t)

	_, alphaPrompt, err := alphaPreset().Apply(bt, bp)
	if err != nil {
		t.Fatalf("alpha Apply: %v", err)
	}
	_, betaPrompt, err := betaPreset().Apply(bt, bp)
	if err != nil {
		t.Fatalf("beta Apply: %v", err)
	}

	alphaStr, err := alphaPrompt.Assemble(nil)
	if err != nil {
		t.Fatalf("alpha Assemble: %v", err)
	}
	if !strings.Contains(alphaStr, "BASE") || !strings.Contains(alphaStr, "SEC-A") {
		t.Fatalf("alpha prompt = %q, want BASE + SEC-A", alphaStr)
	}
	if strings.Contains(alphaStr, "SEC-B") {
		t.Fatalf("alpha prompt leaked beta's SEC-B: %q", alphaStr)
	}

	betaStr, err := betaPrompt.Assemble(nil)
	if err != nil {
		t.Fatalf("beta Assemble: %v", err)
	}
	if !strings.Contains(betaStr, "BASE") || !strings.Contains(betaStr, "SEC-B") {
		t.Fatalf("beta prompt = %q, want BASE + SEC-B", betaStr)
	}
	if strings.Contains(betaStr, "SEC-A") {
		t.Fatalf("beta prompt leaked alpha's SEC-A: %q", betaStr)
	}

	// The shared base prompt is untouched: it assembles to just BASE.
	baseStr, err := bp.Assemble(nil)
	if err != nil {
		t.Fatalf("base Assemble: %v", err)
	}
	if baseStr != "BASE" {
		t.Fatalf("shared base prompt mutated = %q, want %q", baseStr, "BASE")
	}
}

// TestShadowing: an overlay tool named the same as a base tool wins for that agent
// in Schemas() and Execute, while the base's tool of that name is unchanged for a
// direct base user.
func TestShadowing(t *testing.T) {
	bt, bp := newBase(t) // base "common" returns "base-common"

	shadow := Preset{AgentID: "alpha", App: app.App{
		Name:  "shadow-app",
		Tools: []hx.ToolDefinition{toolReturning("common", "alpha-common")},
	}}
	alphaTools, _, err := shadow.Apply(bt, bp)
	if err != nil {
		t.Fatalf("shadow Apply: %v", err)
	}

	// Schemas still lists a single "common" (no duplicate), and it is the overlay's.
	if got, want := schemaNames(alphaTools.Schemas()), []string{"common"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("shadowed Schemas = %v, want %v", got, want)
	}

	// Execute through the scope runs the OVERLAY's common.
	if got := decodeStr(t, alphaTools.Execute(hx.ToolExecutionInput{Name: "common", Args: []byte(`{}`)})); got != "alpha-common" {
		t.Fatalf("scoped Execute(common) = %q, want %q", got, "alpha-common")
	}

	// The shared base's common is UNCHANGED for a direct base user.
	if got := decodeStr(t, bt.Execute(hx.ToolExecutionInput{Name: "common", Args: []byte(`{}`)})); got != "base-common" {
		t.Fatalf("base Execute(common) = %q, want %q (base mutated by overlay)", got, "base-common")
	}
}

// TestExecuteRouting: a scope's own tool runs through the scope; a tool outside the
// scope's view is absent (an error result), never routed to another agent's tool.
func TestExecuteRouting(t *testing.T) {
	bt, bp := newBase(t)

	alphaTools, _, err := alphaPreset().Apply(bt, bp)
	if err != nil {
		t.Fatalf("alpha Apply: %v", err)
	}
	// beta.Apply over the same base to prove toolB exists somewhere, yet stays
	// invisible to alpha.
	if _, _, err := betaPreset().Apply(bt, bp); err != nil {
		t.Fatalf("beta Apply: %v", err)
	}

	// toolA runs through alpha's view.
	if got := decodeStr(t, alphaTools.Execute(hx.ToolExecutionInput{Name: "toolA", Args: []byte(`{}`)})); got != "toolA-ran" {
		t.Fatalf("alpha Execute(toolA) = %q, want %q", got, "toolA-ran")
	}
	// common (base) still runs through alpha's view via base fallback.
	if got := decodeStr(t, alphaTools.Execute(hx.ToolExecutionInput{Name: "common", Args: []byte(`{}`)})); got != "base-common" {
		t.Fatalf("alpha Execute(common) = %q, want %q", got, "base-common")
	}
	// toolB is NOT in alpha's view: an error result, not beta's tool.
	res := alphaTools.Execute(hx.ToolExecutionInput{Name: "toolB", Args: []byte(`{}`)})
	if !res.IsError {
		t.Fatalf("alpha Execute(toolB) should be an error result (absent from alpha's view), got %s", res.Value)
	}
	// And Concurrent for an out-of-view tool resolves via the base (unknown => false).
	if alphaTools.Concurrent("toolB") {
		t.Fatalf("alpha Concurrent(toolB) should be false: toolB is not in alpha's view")
	}
}

// TestApplyNilBase: Apply defends against nil base services.
func TestApplyNilBase(t *testing.T) {
	if _, _, err := (alphaPreset()).Apply(nil, prompt.New()); err == nil {
		t.Fatalf("Apply with nil base tools should error")
	}
	if _, _, err := (alphaPreset()).Apply(tools.New(tools.Config{}), nil); err == nil {
		t.Fatalf("Apply with nil base prompt should error")
	}
}
