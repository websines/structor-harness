package record_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/websines/structor-harness/record"
)

func TestCASPutRefusesCorruptExistingObject(t *testing.T) {
	root := t.TempDir()
	cas, err := record.NewLocalCAS(root)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("original object")
	id, err := cas.Put(data)
	if err != nil {
		t.Fatal(err)
	}
	hex := strings.TrimPrefix(string(id), "sha256:")
	if err := os.WriteFile(filepath.Join(root, hex[:2], hex), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cas.Put(data); err == nil {
		t.Fatal("acknowledged an existing object whose bytes fail integrity")
	}
}
