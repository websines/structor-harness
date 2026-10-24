package skill

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/websines/structor-harness/harness/hx"
)

// catalogIntro and the closing guidance frame the always-present catalog. The rule
// against double-loading mirrors dsh: a user-injected skill body appears inline, so
// the model must follow that block rather than re-load through the tool.
const (
	catalogIntro = "A skill is a reusable set of task-specific instructions. The following skills are available in this session:"
	catalogRules = "If the user names a skill, or the task clearly matches a skill's description, call the `skill` tool with the exact skill name before taking task actions, then follow the returned instructions. This catalog lists summaries only; do not follow a skill's instructions until it has been loaded. A user may also invoke a skill directly with `/name`; its instructions then appear inline — follow them and do not reload that skill through the tool."
)

// CatalogSection renders the <available_skills> block (name + description per
// model-invocable skill) for injection as a prompt section. It returns "" when no
// model-invocable skill exists, so an empty catalog costs nothing.
func CatalogSection(r *Registry) string {
	cat := r.Catalog()
	if len(cat) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(catalogIntro)
	b.WriteString("\n\n<available_skills>\n")
	for _, s := range cat {
		b.WriteString("- `")
		b.WriteString(s.Name)
		b.WriteString("`: ")
		b.WriteString(s.Description)
		b.WriteString("\n")
	}
	b.WriteString("</available_skills>\n\n")
	b.WriteString(catalogRules)
	return b.String()
}

// SkillTool returns the model-invocable `skill` tool. Given {"name":"..."} it loads
// the named skill and returns its rendered instructions as the tool result — the
// instructions the model then follows. An unknown name, or one whose skill is not
// model-invocable, returns a distinct IsError result. The body NEVER returns a Go
// error: every failure is a ToolResult{IsError:true}.
func SkillTool(r *Registry) hx.ToolDefinition {
	return hx.ToolDefinition{
		Schema: hx.ToolSchema{
			Name:        "skill",
			Description: "Load a skill by its exact kebab-case name (from the available skills listing) and return its full instructions to follow.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "Exact kebab-case skill name from the available skills listing.",
					},
				},
				"required":             []any{"name"},
				"additionalProperties": false,
			},
		},
		Concurrent: true, // pure read of instructions
		Execute: func(_ hx.ToolRunContext, args []byte) (hx.ToolResult, error) {
			var in struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return skillErr("skill: invalid arguments: %v", err), nil
			}
			name := strings.TrimSpace(in.Name)
			if name == "" {
				return skillErr("skill: empty name"), nil
			}
			s, ok := r.Load(name)
			if !ok {
				return skillErr("skill %q is unknown or no longer available", name), nil
			}
			if !s.ModelInvocable {
				return skillErr("skill %q is not available for model invocation", name), nil
			}
			rendered := renderSkillContent(s)
			val, err := json.Marshal(map[string]any{"name": s.Name, "content": rendered})
			if err != nil {
				return skillErr("skill: encode result: %v", err), nil
			}
			return hx.ToolResult{
				Value:   val,
				Content: []hx.ContentBlock{{Kind: hx.BlockText, Text: rendered}},
			}, nil
		},
	}
}

// ExtractUserInvocation finds a whitespace-bounded `/skill-name` token in userMsg
// and returns that skill with its body loaded, for inline injection. It works for
// ModelInvocable=false skills — the only entry point for those — but honors
// UserInvocable, so a user-disabled skill is not injected. Returns false when no
// token names a known, user-invocable skill.
func ExtractUserInvocation(userMsg string, r *Registry) (Skill, bool) {
	for _, tok := range strings.Fields(userMsg) {
		if len(tok) < 2 || tok[0] != '/' {
			continue
		}
		name := tok[1:]
		s, ok := r.Load(name)
		if !ok || !s.UserInvocable {
			continue
		}
		return s, true
	}
	return Skill{}, false
}

// renderSkillContent renders one loaded skill as the canonical <skill_content>
// block: an escaped name attribute, optional resource hint, and the verbatim body.
// This is the single shape both the tool result and user-explicit injection use.
func renderSkillContent(s Skill) string {
	var b strings.Builder
	b.WriteString("<skill_content name=\"")
	b.WriteString(escapeAttr(s.Name))
	b.WriteString("\">\n")
	if s.Source != "" {
		b.WriteString("<skill_resources>\nBase file for this skill: ")
		b.WriteString(s.Source)
		b.WriteString("\nResolve paths this skill mentions against its directory. Load referenced resources only as needed.\n</skill_resources>\n\n")
	}
	b.WriteString("<skill_instructions>\n")
	b.WriteString(s.Body)
	b.WriteString("\n</skill_instructions>\n</skill_content>")
	return b.String()
}

// escapeAttr escapes the minimal set for an XML attribute value.
func escapeAttr(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
	)
	return r.Replace(s)
}

// skillErr builds a ToolResult{IsError:true} whose Value is a JSON object carrying
// the message, so a downstream reader always gets valid JSON — never a panic.
func skillErr(format string, a ...any) hx.ToolResult {
	msg := fmt.Sprintf(format, a...)
	b, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: msg})
	return hx.ToolResult{IsError: true, Value: b}
}
