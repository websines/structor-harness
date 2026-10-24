package fs

import (
	"os"
	"path/filepath"
)

// OpenRoot constrains resolution in the OS, including symlinks changed during I/O.
func (l *Local) openRoot(abs string) (*os.Root, string, error) {
	dir := filepath.Clean(l.Root)
	rel, err := filepath.Rel(dir, abs)
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", err
	}
	return root, rel, nil
}
