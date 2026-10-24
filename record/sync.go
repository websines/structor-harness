package record

import (
	"os"
	"path/filepath"
)

func syncDirectory(path string) error {
	directory, err := os.Open(path) //nolint:gosec // Internal caller supplies the configured CAS root or a validated digest shard.
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// A concurrent writer may have renamed this object without syncing its directory
// yet. Every successful Put therefore acknowledges durable publication itself.
func (c *LocalCAS) acknowledge(d Digest, path string) (Digest, error) {
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return "", err
	}
	if err := syncDirectory(c.Root); err != nil {
		return "", err
	}
	return d, nil
}
