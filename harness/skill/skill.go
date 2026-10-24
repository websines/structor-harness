package skill

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// descriptionMax caps a catalog description. The description is the one line that
// is ALWAYS sent to the model (the cheap catalog), so it is bounded; the body is
// not.
const descriptionMax = 200

// Skill is a named bundle of INSTRUCTIONS (distinct from a tool, which executes).
// The catalog phase carries only Name/Description (Body empty); Body is filled by
// Registry.Load, which re-reads Source.
type Skill struct {
	Name           string // kebab-case, unique key
	Description    string // one line, capped — goes in the always-present catalog
	WhenToUse      string
	ModelInvocable bool   // default true; false when frontmatter has disable-model-invocation
	UserInvocable  bool   // default true
	Body           string // the markdown instructions (loaded lazily)
	Source         string // file path (provenance)
	Rank           int    // discovery root rank (lower wins)
}

// Registry holds discovered skills keyed by name. Providers register sources by
// Add-ing the skills they found; a read merges them with nearest-rank-wins on a
// duplicate name. Safe for concurrent use.
type Registry struct {
	mu     sync.Mutex
	byName map[string]Skill
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Skill)}
}

// Add records a discovered skill. On a duplicate name the nearest rank (lower Rank)
// wins; a tie keeps the first-registered entry. A skill with an empty name is
// ignored. The stored entry is the catalog projection — its Body is dropped, so
// the catalog stays cheap and the body is re-read lazily by Load.
func (r *Registry) Add(s Skill) {
	if s.Name == "" {
		return
	}
	s.Body = "" // catalog phase: body is loaded lazily from Source
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.byName[s.Name]; ok {
		if s.Rank < existing.Rank {
			r.byName[s.Name] = s
		}
		return
	}
	r.byName[s.Name] = s
}

// AddResident records a skill whose Body is already in memory — the shape a
// record-backed source (the learning store, an admitted skill loaded from the CAS)
// registers, as opposed to a filesystem skill whose body is re-read lazily from
// Source. Unlike Add, it KEEPS the Body and leaves Source empty, so Load returns the
// resident body directly via its Source=="" branch. Nearest-rank-wins on a duplicate
// name, same as Add.
func (r *Registry) AddResident(s Skill) {
	if s.Name == "" {
		return
	}
	s.Source = "" // resident: body is authoritative in-memory, not a file to re-read
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.byName[s.Name]; ok {
		if s.Rank < existing.Rank {
			r.byName[s.Name] = s
		}
		return
	}
	r.byName[s.Name] = s
}

// Catalog returns the model-invocable skills as cheap entries (name + description,
// no body), sorted by name. This is the always-present surface rendered into the
// prompt and used to gate the `skill` tool.
func (r *Registry) Catalog() []Skill {
	r.mu.Lock()
	out := make([]Skill, 0, len(r.byName))
	for _, s := range r.byName {
		if !s.ModelInvocable {
			continue
		}
		out = append(out, Skill{
			Name:           s.Name,
			Description:    s.Description,
			ModelInvocable: s.ModelInvocable,
			UserInvocable:  s.UserInvocable,
			Source:         s.Source,
			Rank:           s.Rank,
			// Body and WhenToUse intentionally omitted — the catalog is name+description.
		})
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// All returns every registered skill (catalog projection, no body), sorted by name.
func (r *Registry) All() []Skill {
	r.mu.Lock()
	out := make([]Skill, 0, len(r.byName))
	for _, s := range r.byName {
		out = append(out, s)
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Load returns the full skill, re-reading and reparsing Body from Source on every
// call (the lazy body — body edits need no cache invalidation). The registry key
// name and discovery rank are preserved; description/whenToUse/invocation/body come
// from the fresh parse. Returns false for an unknown name. Policy is NOT enforced
// here: Load is the trusted primitive, and each surface applies its own predicate.
func (r *Registry) Load(name string) (Skill, bool) {
	r.mu.Lock()
	s, ok := r.byName[name]
	r.mu.Unlock()
	if !ok {
		return Skill{}, false
	}
	if s.Source == "" {
		return s, true
	}
	full, err := parseSkill(s.Source, s.Name, s.Rank)
	if err != nil {
		// The file went away or turned unreadable between discovery and load;
		// fall back to the stored metadata (still no body) rather than vanishing.
		return s, true
	}
	full.Name = s.Name // the registry key is authoritative for the name
	full.Rank = s.Rank
	full.Source = s.Source
	return full, true
}

// parseSkill reads path and produces a fully-loaded Skill (Body included). name is
// the fallback used when frontmatter omits `name` (the file/dir stem). The returned
// name is always kebab-cased.
func parseSkill(path, defaultName string, rank int) (Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, err
	}
	front, body := splitFrontmatter(string(data))
	name := front["name"]
	if strings.TrimSpace(name) == "" {
		name = defaultName
	}
	name = kebab(name)
	if name == "" {
		return Skill{}, fmt.Errorf("skill: %s: could not derive a name", path)
	}
	return Skill{
		Name:           name,
		Description:    capOneLine(front["description"]),
		WhenToUse:      strings.TrimSpace(front["whentouse"]),
		ModelInvocable: !parseBool(front["disable-model-invocation"], false),
		UserInvocable:  parseBool(front["user-invocable"], true),
		Body:           strings.TrimLeft(body, "\n"),
		Source:         path,
		Rank:           rank,
	}, nil
}

// splitFrontmatter parses a leading `---\n … \n---` block of `key: value` lines and
// returns the (lower-cased-key) map plus the body — everything after the closing
// `---`. A file that does not start with `---`, or whose block is never closed, has
// no frontmatter: the whole content is the body.
func splitFrontmatter(content string) (map[string]string, string) {
	front := make(map[string]string)
	content = strings.TrimPrefix(content, "\ufeff") // tolerate a UTF-8 BOM

	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // allow long lines/bodies

	if !sc.Scan() {
		return front, ""
	}
	if strings.TrimSpace(strings.TrimRight(sc.Text(), "\r")) != "---" {
		return front, content // no frontmatter
	}

	closed := false
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "---" {
			closed = true
			break
		}
		if idx := strings.IndexByte(line, ':'); idx >= 0 {
			key := strings.ToLower(strings.TrimSpace(line[:idx]))
			val := strings.TrimSpace(line[idx+1:])
			val = strings.Trim(val, `"'`)
			if key != "" {
				front[key] = val
			}
		}
	}
	if !closed {
		return make(map[string]string), content // unterminated block: treat as body
	}

	var body []string
	for sc.Scan() {
		body = append(body, strings.TrimRight(sc.Text(), "\r"))
	}
	return front, strings.Join(body, "\n")
}

// parseBool reads a YAML-ish boolean, accepting the case-insensitive forms
// true/false, yes/no, on/off, 1/0. An empty or unrecognized value yields def.
func parseBool(s string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return def
	case "true", "yes", "on", "1":
		return true
	case "false", "no", "off", "0":
		return false
	default:
		return def
	}
}

// kebab normalizes s to kebab-case: lower-cased, with spaces/underscores/hyphens
// collapsed to single hyphens, all other characters dropped, and leading/trailing
// hyphens trimmed.
func kebab(s string) string {
	var b strings.Builder
	prevHyphen := true // leading-suppress: no hyphen until a real char is written
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevHyphen = false
		case r == ' ' || r == '\t' || r == '_' || r == '-':
			if !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		default:
			// drop anything else
		}
	}
	return strings.Trim(b.String(), "-")
}

// capOneLine folds all whitespace (including newlines) to single spaces and caps
// the result at descriptionMax runes, appending an ellipsis when it is cut.
func capOneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > descriptionMax {
		return string(r[:descriptionMax-1]) + "…"
	}
	return s
}
