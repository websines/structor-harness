package skill

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Root is one discovery root: a directory scanned for skills, and the rank that
// breaks duplicate-name ties (lower wins). Ranks encode precedence — project roots
// rank below user libraries so a project skill shadows a same-named user one.
type Root struct {
	Dir  string
	Rank int
}

// Default root ranks: project skills shadow user libraries, which shadow the
// shared agents directory.
const (
	RankProject = 100 // <project>/.forge/skills
	RankUser    = 400 // $HOME/.forge/skills
	RankShared  = 500 // $HOME/.agents/skills
)

// DiscoverFilesystem scans ranked roots and returns every skill found — including
// duplicates across roots, which Registry.Add resolves by rank. A skill is either
// a directory bundle (<root>/<name>/SKILL.md) or a flat file (<root>/<name>.md);
// nested trees are deliberately not searched. A root that does not exist is valid
// empty state and skipped. A file that fails to parse is skipped, not fatal; only
// an unexpected directory-read error aborts discovery. The result is sorted by
// (rank, name, source) for determinism.
func DiscoverFilesystem(roots []Root) ([]Skill, error) {
	var found []Skill
	for _, root := range roots {
		entries, err := os.ReadDir(root.Dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // absent root = empty
			}
			return nil, fmt.Errorf("skill: read root %q: %w", root.Dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				// Directory bundle: <name>/SKILL.md.
				path := filepath.Join(root.Dir, name, "SKILL.md")
				if info, err := os.Stat(path); err != nil || info.IsDir() {
					continue
				}
				if s, err := parseSkill(path, name, root.Rank); err == nil {
					found = append(found, s)
				}
				continue
			}
			// Flat file: <name>.md (case-insensitive extension).
			if !strings.EqualFold(filepath.Ext(name), ".md") {
				continue
			}
			stem := strings.TrimSuffix(name, filepath.Ext(name))
			path := filepath.Join(root.Dir, name)
			if s, err := parseSkill(path, stem, root.Rank); err == nil {
				found = append(found, s)
			}
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].Rank != found[j].Rank {
			return found[i].Rank < found[j].Rank
		}
		if found[i].Name != found[j].Name {
			return found[i].Name < found[j].Name
		}
		return found[i].Source < found[j].Source
	})
	return found, nil
}

// DefaultRoots returns the standard precedence for projectDir: the project's
// ./.forge/skills (RankProject), the user library $HOME/.forge/skills (RankUser),
// and the shared $HOME/.agents/skills (RankShared). The user and shared roots are
// omitted when the home directory cannot be resolved, so they never collapse onto
// a relative path.
func DefaultRoots(projectDir string) []Root {
	roots := []Root{{Dir: filepath.Join(projectDir, ".forge", "skills"), Rank: RankProject}}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots,
			Root{Dir: filepath.Join(home, ".forge", "skills"), Rank: RankUser},
			Root{Dir: filepath.Join(home, ".agents", "skills"), Rank: RankShared},
		)
	}
	return roots
}
