// Package fs is the Forge harness filesystem capability (forge-harness-design
// §H2): the agent's file I/O confined to a Root, plus the model-facing
// read_file / write_file / edit_file tools that ride it.
//
// Two invariants define the capability. First, edits are LITERAL — an
// oldString→newString replacement of a UNIQUE occurrence, never a diff and
// never a silent partial edit (a zero or ambiguous match is refused). Second, a
// write follows a READ-BEFORE-WRITE policy: an agent may create a new file
// freely, but it cannot clobber a file it never read, so a blind overwrite is
// caught at the point of use rather than after the bytes are gone. Writes are
// ATOMIC (temp file in the same dir, fsync, rename), so a concurrent reader
// never observes a half-written file — the same discipline record/cas.go uses
// for CAS objects.
package fs

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/websines/structor-harness/harness/hx"
)

// Local is the on-disk FileSystem confined to Root. Every path an agent names is
// resolved beneath Root and refused if it escapes (a `..` traversal). The reads
// map remembers, per resolved path, the content hash last seen through this
// capability — the ledger read-before-write consults so a write can only land on
// a file this Local has already read (or on a brand-new path).
type Local struct {
	Root string

	mu    sync.Mutex
	reads map[string]string // resolved abs path -> sha256 hex of last-observed content
}

// Compile-time proof that *Local is a full hx.FileSystem.
var _ hx.FileSystem = (*Local)(nil)

// resolve interprets path relative to Root and returns the absolute on-disk path,
// refusing anything that escapes Root. filepath.Join cleans away `..` segments;
// we then confirm the result still lies within Root, so neither a `../` traversal
// nor an absolute path can reach outside the confinement.
func (l *Local) resolve(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("fs: empty path")
	}
	root := filepath.Clean(l.Root)
	abs := filepath.Join(root, path)
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", fmt.Errorf("fs: path %q is unresolvable: %w", path, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("fs: path %q escapes root %q", path, l.Root)
	}
	return abs, nil
}

// ReadText reads a UTF-8 file and records its content hash as "read", so a later
// WriteText/EditText to the same path is permitted. Reading is the ONLY way an
// agent earns the right to overwrite an existing file.
func (l *Local) ReadText(path string) (string, error) {
	abs, err := l.resolve(path)
	if err != nil {
		return "", err
	}
	root, rel, err := l.openRoot(abs)
	if err != nil {
		return "", err
	}
	defer root.Close()
	b, err := root.ReadFile(rel)
	if err != nil {
		return "", fmt.Errorf("fs: read %q: %w", path, err)
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("fs: file %q is not valid UTF-8", path)
	}
	content := string(b)
	l.record(abs, content)
	return content, nil
}

// WriteText writes content to path atomically. Read-before-write is enforced: a
// file that already EXISTS but was never read through this capability is refused
// (an agent must read before it clobbers); creating a NEW file is allowed. On
// success the written content is recorded as read, so an agent that just wrote a
// file knows its bytes and may write it again.
func (l *Local) WriteText(path, content string) error {
	abs, err := l.resolve(path)
	if err != nil {
		return err
	}
	root, rel, err := l.openRoot(abs)
	if err != nil {
		return err
	}
	defer root.Close()
	switch _, statErr := root.Stat(rel); {
	case statErr == nil:
		if !l.wasRead(abs) {
			return fmt.Errorf("fs: refusing to overwrite %q: it was never read (read before write)", path)
		}
	case !os.IsNotExist(statErr):
		return fmt.Errorf("fs: stat %q: %w", path, statErr)
	}
	if err := l.writeAtomic(abs, content); err != nil {
		return fmt.Errorf("fs: write %q: %w", path, err)
	}
	l.record(abs, content)
	return nil
}

// EditText replaces the UNIQUE occurrence of oldStr with newStr and writes the
// result atomically. It is a LITERAL string replacement, not a diff: zero matches
// is "not found", more than one is "ambiguous, N matches", and only an exact
// single match is edited. The read it performs first satisfies read-before-write.
func (l *Local) EditText(path, oldStr, newStr string) error {
	content, err := l.ReadText(path)
	if err != nil {
		return err
	}
	if oldStr == "" {
		return fmt.Errorf("fs: edit %q: oldStr is empty", path)
	}
	switch n := strings.Count(content, oldStr); {
	case n == 0:
		return fmt.Errorf("fs: edit %q: oldStr not found", path)
	case n > 1:
		return fmt.Errorf("fs: edit %q: oldStr is ambiguous, %d matches", path, n)
	}
	updated := strings.Replace(content, oldStr, newStr, 1)
	abs, err := l.resolve(path)
	if err != nil {
		return err
	}
	if err := l.writeAtomic(abs, updated); err != nil {
		return fmt.Errorf("fs: edit %q: %w", path, err)
	}
	l.record(abs, updated)
	return nil
}

// writeAtomic writes content through a temp file in the SAME directory, fsyncs it,
// and renames it over abs — so a reader sees either the old file or the whole new
// one, never a partial write. The temp file is removed on any failure before the
// rename lands, so a failed write leaves no debris behind.
func (l *Local) writeAtomic(abs, content string) error {
	root, rel, err := l.openRoot(abs)
	if err != nil {
		return err
	}
	defer root.Close()
	dir := filepath.Dir(rel)
	if err := root.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmpName := filepath.Join(dir, ".forge-write-"+rand.Text())
	tmp, err := root.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = tmp.Close(); _ = root.Remove(tmpName) }()
	if _, err := tmp.WriteString(content); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := root.Rename(tmpName, rel); err != nil {
		return err
	}
	parent, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}

// record notes the content hash last observed for abs (the read-before-write
// ledger). Lazily initialized so a bare Local{Root:...} literal works.
func (l *Local) record(abs, content string) {
	sum := sha256.Sum256([]byte(content))
	l.mu.Lock()
	if l.reads == nil {
		l.reads = make(map[string]string)
	}
	l.reads[abs] = hex.EncodeToString(sum[:])
	l.mu.Unlock()
}

// wasRead reports whether abs has been read (or written) through this capability.
func (l *Local) wasRead(abs string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.reads[abs]
	return ok
}
