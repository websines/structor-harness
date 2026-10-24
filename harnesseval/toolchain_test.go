package harnesseval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCacheCopiesCannotPoisonAnotherWorkspace(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "object"), []byte("trusted standard library"), 0600); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(t.TempDir(), "cache")
	second := filepath.Join(t.TempDir(), "cache")
	if err := copyCache(t.Context(), source, first); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "object"), []byte("candidate overwrite"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := copyCache(t.Context(), source, second); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(source, "object"), filepath.Join(second, "object")} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "trusted standard library" {
			t.Fatalf("cache poisoning: %s %s %v", path, data, err)
		}
	}
}
