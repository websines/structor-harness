// Package skillsmith adds the model-callable skill MAKER and IMPROVER (v0.1
// design §4). Both author/refine skills and route them through the record's
// admission gate: make_skill records a new candidate, improve_skill records a
// new content-addressed version and supersedes the prior one — neither activates
// a skill (that is a separate Promote). The tools are total functions over a
// learning.Store: a bad argument becomes a ToolResult{IsError:true}, never a Go
// error and never a panic (matching harness/fs). Ids are minted through the
// shared skillstore convention so a made/improved skill is the same skill
// skillstore admits and activates.
package skillsmith

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/skillstore"
	"github.com/websines/structor-harness/learning"
	"github.com/websines/structor-harness/runtime"
)

// Tools returns the two skillsmith tools bound to ls: make_skill (author a new
// candidate) and improve_skill (record a new version + supersede the old one).
func Tools(ls *learning.Store) []hx.ToolDefinition {
	return []hx.ToolDefinition{
		makeSkillTool(ls),
		improveSkillTool(ls),
	}
}

// Component installs make_skill + improve_skill on the tool runtime. It injects
// hx.KeyTools and registers both tools as a single revertible effect (all-or-
// nothing, LIFO teardown), mirroring harness/fs.Component.
func Component(ls *learning.Store) *runtime.Component {
	return &runtime.Component{
		Name:   "harness/skillsmith",
		Inject: []string{hx.KeyTools},
		Apply: func(ctx *runtime.Context) error {
			return ctx.Use(func() (func(), error) {
				v, err := ctx.Get(hx.KeyTools)
				if err != nil {
					return nil, err
				}
				tr, ok := v.(hx.ToolRuntime)
				if !ok {
					return nil, fmt.Errorf("skillsmith: %q is not an hx.ToolRuntime", hx.KeyTools)
				}
				return registerTools(ls, tr)
			})
		},
	}
}

// makeSkillTool authors a NEW skill and records it as a candidate. It is not
// active until separately promoted — the description says so, since the model
// must not assume authoring == activating.
func makeSkillTool(ls *learning.Store) hx.ToolDefinition {
	return hx.ToolDefinition{
		Schema: hx.ToolSchema{
			Name: "make_skill",
			Description: "Author a NEW reusable skill and record it as a CANDIDATE in the governed skill store. " +
				"The skill is NOT active until it is separately promoted through the acceptance gate — authoring a skill does not activate it.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "kebab-case skill name (the unique key).",
					},
					"summary": map[string]any{
						"type":        "string",
						"description": "One-line summary shown in the skill catalog.",
					},
					"body": map[string]any{
						"type":        "string",
						"description": "The full skill instructions (markdown).",
					},
					"provenance": map[string]any{
						"type":        "string",
						"description": "Optional origin note (run id, source) recorded on the fact.",
					},
				},
				"required":             []any{"name", "summary", "body"},
				"additionalProperties": false,
			},
		},
		Execute: func(_ hx.ToolRunContext, args []byte) (hx.ToolResult, error) {
			var in struct {
				Name       string `json:"name"`
				Summary    string `json:"summary"`
				Body       string `json:"body"`
				Provenance string `json:"provenance"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return smithErr("make_skill: invalid arguments: %v", err), nil
			}
			name := strings.TrimSpace(in.Name)
			if name == "" {
				return smithErr("make_skill: name is required"), nil
			}
			if strings.TrimSpace(in.Body) == "" {
				return smithErr("make_skill: body is required"), nil
			}
			var prov []string
			if p := strings.TrimSpace(in.Provenance); p != "" {
				prov = []string{p}
			}
			id := skillstore.SkillID(name)
			if _, err := ls.AddCandidate(learning.Skill{
				ID:         id,
				Title:      in.Summary,
				Body:       in.Body,
				Provenance: prov,
			}); err != nil {
				return smithErr("make_skill: %v", err), nil
			}
			return okResult(map[string]any{
				"skill_id": id,
				"name":     name,
				"state":    "candidate",
			})
		},
	}
}

// improveSkillTool refines an existing skill as a NEW VERSION, not an edit: it
// records the improved body under a content-addressed VersionID (a fresh
// candidate) and retires the prior version (a supersession — kept for history,
// never deleted). Admission of the new version is a separate Promote.
func improveSkillTool(ls *learning.Store) hx.ToolDefinition {
	return hx.ToolDefinition{
		Schema: hx.ToolSchema{
			Name: "improve_skill",
			Description: "Refine an existing skill by recording a NEW VERSION (content-addressed) and retiring the prior version " +
				"(a supersession — never an in-place edit or a deletion). The new version is a CANDIDATE and is NOT active until it is separately promoted.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "kebab-case name of the existing skill to improve.",
					},
					"new_body": map[string]any{
						"type":        "string",
						"description": "The improved full skill instructions (markdown) — recorded as a new version.",
					},
					"reason": map[string]any{
						"type":        "string",
						"description": "Why the skill is being superseded; recorded on both the new version's provenance and the retirement.",
					},
				},
				"required":             []any{"name", "new_body", "reason"},
				"additionalProperties": false,
			},
		},
		Execute: func(_ hx.ToolRunContext, args []byte) (hx.ToolResult, error) {
			var in struct {
				Name    string `json:"name"`
				NewBody string `json:"new_body"`
				Reason  string `json:"reason"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return smithErr("improve_skill: invalid arguments: %v", err), nil
			}
			name := strings.TrimSpace(in.Name)
			if name == "" {
				return smithErr("improve_skill: name is required"), nil
			}
			if strings.TrimSpace(in.NewBody) == "" {
				return smithErr("improve_skill: new_body is required"), nil
			}
			oldID, oldTitle, ok := findLive(ls, name)
			if !ok {
				return smithErr("improve_skill: no live skill named %q to improve", name), nil
			}
			title := oldTitle
			if strings.TrimSpace(title) == "" {
				title = "improved: " + name
			}
			newID := skillstore.VersionID(name, in.NewBody)
			if newID == oldID {
				return okResult(map[string]any{"new_skill_id": oldID, "retired": "", "state": "unchanged"})
			}
			if _, err := ls.AddCandidate(learning.Skill{
				ID:         newID,
				Title:      title,
				Body:       in.NewBody,
				Provenance: []string{in.Reason},
			}); err != nil {
				return smithErr("improve_skill: record new version: %v", err), nil
			}
			if _, err := ls.Retire(oldID, in.Reason); err != nil {
				return smithErr("improve_skill: retire prior version: %v", err), nil
			}
			return okResult(map[string]any{
				"new_skill_id": newID,
				"retired":      oldID,
				"state":        "candidate",
			})
		},
	}
}

// findLive locates the current live skill for name and returns its id + title.
// A name is an exact identity (not a fuzzy query), so it scans recalled skills
// and matches by SkillName(id)==name, preferring a Promoted version over a
// Candidate one. It first tries a scoped Recall(name) (the design's mechanism),
// then falls back to the exact-identity scan over all live skills — so a skill
// whose text does not happen to contain its own name is still found.
func findLive(ls *learning.Store, name string) (id, title string, ok bool) {
	pick := func(skills []learning.Skill) (string, string, bool) {
		var gotID, gotTitle string
		found := false
		for _, sk := range skills {
			if skillstore.SkillName(sk.ID) != name {
				continue
			}
			if sk.State == learning.SkillPromoted {
				return sk.ID, sk.Title, true // a promoted version wins outright
			}
			if !found {
				gotID, gotTitle, found = sk.ID, sk.Title, true
			}
		}
		return gotID, gotTitle, found
	}
	if id, title, ok = pick(ls.Recall(name)); ok {
		return id, title, true
	}
	return pick(ls.Recall(""))
}

// registerTools registers both tools on tr and returns a single disposer that
// withdraws them in LIFO order; a failed registration rolls back the ones
// already installed, so the tool surface is all-or-nothing (mirrors harness/fs).
func registerTools(ls *learning.Store, tr hx.ToolRuntime) (dispose func(), err error) {
	defs := Tools(ls)
	disposers := make([]func(), 0, len(defs))
	revert := func() {
		for i := len(disposers) - 1; i >= 0; i-- {
			disposers[i]()
		}
	}
	for _, def := range defs {
		d, err := tr.Register(def)
		if err != nil {
			revert()
			return nil, fmt.Errorf("skillsmith: register %q: %w", def.Schema.Name, err)
		}
		if d != nil {
			disposers = append(disposers, d)
		}
	}
	return revert, nil
}

// okResult marshals v into a successful (model-visible JSON) ToolResult; an
// encode failure degrades to an error result rather than a panic.
func okResult(v map[string]any) (hx.ToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return smithErr("skillsmith: encode result: %v", err), nil
	}
	return hx.ToolResult{Value: b}, nil
}

// smithErr builds a ToolResult{IsError:true} whose Value is a JSON object
// carrying the message, so a downstream reader always gets valid JSON.
func smithErr(format string, a ...any) hx.ToolResult {
	msg := fmt.Sprintf(format, a...)
	b, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: msg})
	return hx.ToolResult{IsError: true, Value: b}
}
