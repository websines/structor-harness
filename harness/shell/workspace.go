package shell

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Tool caches remain within the declared workspace instead of requesting write
// access to the developer's global Go cache or system temporary directory.
// WorkspaceEnvironment keeps temporary files and Go build caches inside the
// workspace authorized by the execution sandbox. It preserves other inherited
// settings, including toolchain and module configuration.
func WorkspaceEnvironment(workDir string) ([]string, error) {
	return WorkspaceEnvironmentFrom(workDir, os.Environ())
}

// WorkspaceEnvironmentFrom uses a captured environment rather than inheriting
// provider credentials or developer settings into experimental shell tools.
func WorkspaceEnvironmentFrom(workDir string, base []string) ([]string, error) {
	if workDir == "" {
		return nil, nil
	}
	dir, err := filepath.Abs(workDir)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("shell: open workspace: %w", err)
	}
	defer root.Close()
	for _, path := range []string{".forge/tool-cache/tmp", ".forge/tool-cache/go"} {
		if err := root.MkdirAll(path, 0o700); err != nil {
			return nil, fmt.Errorf("shell: create workspace cache: %w", err)
		}
	}
	values := map[string]string{"TMPDIR": filepath.Join(dir, ".forge/tool-cache/tmp"),
		"GOTMPDIR": filepath.Join(dir, ".forge/tool-cache/tmp"), "GOCACHE": filepath.Join(dir, ".forge/tool-cache/go")}
	environment := make([]string, 0, len(base)+len(values))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := values[key]; !replaced {
			environment = append(environment, entry)
		}
	}
	for _, key := range []string{"TMPDIR", "GOTMPDIR", "GOCACHE"} {
		environment = append(environment, key+"="+values[key])
	}
	return environment, nil
}
