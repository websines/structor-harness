package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var linuxRuntimeRoots = [...]string{"/usr", "/bin", "/sbin", "/lib", "/lib64"}

// Preserve merged-/usr aliases instead of mounting a second directory over an
// alias. Only fixed runtime roots are inherited; host /home, /root, /tmp and
// /etc are absent unless the caller explicitly declared a path there.
func linuxRuntimeBindings(hostRoot string) ([]string, error) {
	var bindings []string
	for _, path := range linuxRuntimeRoots {
		source := filepath.Join(hostRoot, path)
		info, err := os.Lstat(source)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("sandbox: inspect fixed runtime %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(source)
			if err != nil {
				return nil, fmt.Errorf("sandbox: inspect runtime alias %s: %w", path, err)
			}
			resolved := target
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(filepath.Dir(path), resolved)
			}
			resolved = filepath.Clean(resolved)
			allowed := false
			for _, root := range linuxRuntimeRoots {
				if resolved == root || strings.HasPrefix(resolved, root+"/") {
					allowed = true
				}
			}
			if !allowed || resolved == path {
				return nil, fmt.Errorf("sandbox: runtime alias %s escapes the fixed runtime", path)
			}
			bindings = append(bindings, "--symlink", target, path)
			continue
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("sandbox: fixed runtime %s is not a directory", path)
		}
		bindings = append(bindings, "--ro-bind", source, path)
	}
	// The dynamic loader cache is the only default host /etc file. Inheriting
	// all /etc would expose unrelated credentials/configuration to commands.
	cache := filepath.Join(hostRoot, "/etc/ld.so.cache")
	info, err := os.Lstat(cache)
	if err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("sandbox: dynamic loader cache is not a regular file")
		}
		bindings = append(bindings, "--ro-bind", cache, "/etc/ld.so.cache")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("sandbox: inspect dynamic loader cache: %w", err)
	}
	return bindings, nil
}
