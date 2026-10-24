package skillstore

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/skill"
	"github.com/websines/structor-harness/learning"
	"github.com/websines/structor-harness/runtime"
)

// registryKey is the local service key the built Registry is published under —
// the SAME literal harness/skill.Component uses, so a consumer that injects
// "skills" and casts to *skill.Registry works against either component. It is a
// package-local literal on purpose (the skill system is not part of the hx spine).
const registryKey = "skills"

// catalogOrder places the <available_skills> block among the prompt's ordered
// sections, matching harness/skill.Component so the admitted catalog renders in
// the same position the filesystem catalog would.
const catalogOrder = 500

// descriptionMax caps a catalog description — the one line always sent to the
// model. It mirrors harness/skill's cap so a record-backed Title renders as a
// bounded one-liner exactly like a parsed frontmatter description.
const descriptionMax = 200

// Component installs the RECORD-backed skill system for one agent scope. Like
// harness/skill.Component it injects the tool runtime + system prompt and, at
// Apply time, builds a skill.Registry — but the registry is sourced from the
// learning record, not the filesystem: only PROMOTED skills that match scope are
// admitted (buildRegistry). It then:
//   - registers the model-invocable `skill` tool (revertible),
//   - registers the always-present catalog as a prompt section when non-empty
//     (revertible),
//   - publishes the *skill.Registry under the local key "skills" (revertible).
//
// Pollution is structural: an un-promoted skill has no promotion fact, so it is
// not in Recall's promoted set and is never activated — regardless of any file
// on disk. All three installations are effects, so unloading withdraws the tool,
// the section, and the service in LIFO order.
func Component(ls *learning.Store, scope string) *runtime.Component {
	return &runtime.Component{
		Name:    "harness/skillstore",
		Inject:  []string{hx.KeyTools, hx.KeyPrompt},
		Provide: []string{registryKey},
		Apply: func(ctx *runtime.Context) error {
			reg := buildRegistry(ls, scope)

			// Register the `skill` tool as a revertible effect (reused verbatim
			// from harness/skill — the tool loads a skill's resident body).
			if err := ctx.Use(func() (func(), error) {
				tr, err := get[hx.ToolRuntime](ctx, hx.KeyTools)
				if err != nil {
					return nil, err
				}
				return tr.Register(skill.SkillTool(reg))
			}); err != nil {
				return err
			}

			// Register the catalog section only when there is something admitted
			// to inject — an empty admitted catalog costs no section.
			if section := skill.CatalogSection(reg); section != "" {
				if err := ctx.Use(func() (func(), error) {
					sp, err := get[hx.SystemPrompt](ctx, hx.KeyPrompt)
					if err != nil {
						return nil, err
					}
					return sp.Section(catalogOrder, section)
				}); err != nil {
					return err
				}
			}

			// Publish the registry last, so it appears only once its tool is in.
			return ctx.Provide(registryKey, reg)
		},
	}
}

// buildRegistry projects the learning record into the admitted, in-scope skill
// registry: it recalls skills for scope, keeps only the PROMOTED ones, and
// AddResident-s each (body kept in memory, no file to re-read) into a fresh
// registry. A candidate has no promotion fact, so it is filtered out here; a
// retired skill is never returned by Recall, so it drops out upstream. This is
// the whole activation gate — the catalog and the `skill` tool see only what
// this returns.
func buildRegistry(ls *learning.Store, scope string) *skill.Registry {
	reg := skill.NewRegistry()
	for _, sk := range ls.Recall(scope) {
		if sk.State != learning.SkillPromoted {
			continue // only ADMITTED skills activate
		}
		reg.AddResident(skill.Skill{
			Name:           SkillName(sk.ID),
			Description:    capLine(sk.Title),
			Body:           sk.Body,
			ModelInvocable: true,
			UserInvocable:  true,
		})
	}
	return reg
}

// Import brings filesystem skills into the record as SOURCES, never as the live
// catalog. It discovers skills across roots and, for each, records a candidate
// (body → CAS) with an "import:<source>" provenance. When trust is set, each
// import is ALSO promoted through a recorded human-attestation decision
// (AcceptHuman) — admitted by an auditable, supersedable acceptance fact, not by
// silent fiat. Untrusted imports stay candidates until separately promoted.
// Returns how many skills were recorded (imported) and how many were admitted
// (admitted, non-zero only when trust is set).
func Import(ls *learning.Store, roots []skill.Root, trust bool) (imported int, admitted int, err error) {
	discovered, err := skill.DiscoverFilesystem(roots)
	if err != nil {
		return 0, 0, err
	}
	// A registry over the discovered skills lets us re-read a body via Load when
	// discovery did not carry one (the catalog projection drops Body); the file
	// is the source of truth for the bytes we address into the CAS.
	reg := skill.NewRegistry()
	for _, s := range discovered {
		reg.Add(s)
	}
	for _, s := range discovered {
		body := s.Body
		if body == "" {
			if full, ok := reg.Load(s.Name); ok {
				body = full.Body
			}
		}
		id := SkillID(s.Name)
		if _, e := ls.AddCandidate(learning.Skill{
			ID:         id,
			Title:      s.Description,
			Body:       body,
			Provenance: []string{"import:" + s.Source},
		}); e != nil {
			return imported, admitted, e
		}
		imported++
		if trust {
			principal := "import:" + rootDirFor(s.Source, roots)
			d := learning.AcceptHuman(principal, "trusted import", true)
			if _, e := ls.Promote(id, d); e != nil {
				return imported, admitted, e
			}
			admitted++
		}
	}
	return imported, admitted, nil
}

// rootDirFor returns the discovery root a source path belongs to (the longest
// root Dir that prefixes source), so a trusted import's promotion is attributed
// to the root that supplied it. Falls back to the file's parent dir when no root
// matches (defensive; discovery always produces sources under a root).
func rootDirFor(source string, roots []skill.Root) string {
	best := ""
	for _, r := range roots {
		if r.Dir == "" {
			continue
		}
		if source == r.Dir || strings.HasPrefix(source, r.Dir+string(filepath.Separator)) {
			if len(r.Dir) > len(best) {
				best = r.Dir
			}
		}
	}
	if best == "" {
		return filepath.Dir(source)
	}
	return best
}

// capLine folds all whitespace to single spaces and caps the result at
// descriptionMax runes (mirrors harness/skill.capOneLine), so a record Title
// becomes a bounded catalog description.
func capLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > descriptionMax {
		return string(r[:descriptionMax-1]) + "…"
	}
	return s
}

// get resolves the service at key and asserts it to T — the by-key resolution
// pattern the spine uses (mirrors harness/skill.get); ctx.Get enforces the
// component's declared confinement.
func get[T any](ctx *runtime.Context, key string) (T, error) {
	var zero T
	v, err := ctx.Get(key)
	if err != nil {
		return zero, err
	}
	t, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("skillstore: service %q has unexpected type %T", key, v)
	}
	return t, nil
}
