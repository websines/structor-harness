package fs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReviewSymlinkEscape(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("outside secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	fs := &Local{Root: root}
	if _, err := fs.ReadText("linked/secret"); err == nil {
		t.Fatal("read crossed the configured root")
	}
	if err := fs.WriteText("linked/new", "outside write"); err == nil {
		t.Fatal("write crossed the configured root")
	}
	if err := fs.EditText("linked/secret", "secret", "changed"); err == nil {
		t.Fatal("edit crossed the configured root")
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
		t.Fatalf("out-of-root file was created: %v", err)
	}
	b, err := os.ReadFile(secret) //nolint:gosec // The path belongs to this test's temporary directory.
	if err != nil || string(b) != "outside secret" {
		t.Fatalf("out-of-root content changed: %q %v", b, err)
	}
}
