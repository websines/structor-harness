// Package record is the canonical substrate (system design §4, Law 1): content-
// addressed storage, signed manifests and self-verifying bundle export. The
// projection is derived and rebuildable; the record is canonical.
//
// This file implements M2 on the DECLARED-DEGRADED demo profile (system design
// §17): a local-disk CAS and self-signed manifests, behind ports whose real
// implementations (OCI/ORAS, cosign/KMS, Rekor) swap in for a customer
// deployment WITHOUT changing callers. Every degradation is named in the
// manifest it produces (see manifest.go Profile), so a demo bundle can never
// pass for a customer-grade one.
package record

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Digest is a content address: "sha256:" + lowercase hex. Because it is derived
// from bytes, identical content always yields one address (dedup + integrity).
type Digest string

// ComputeDigest returns the content address of b. This is the ONLY way a Digest
// is minted — an address you did not compute from bytes is not an address.
func ComputeDigest(b []byte) Digest {
	sum := sha256.Sum256(b)
	return Digest("sha256:" + hex.EncodeToString(sum[:]))
}

// Verify reports whether b actually hashes to d — the integrity check every
// read performs, so a corrupted or substituted object is caught, never served.
func (d Digest) Verify(b []byte) bool { return ComputeDigest(b) == d }

func (d Digest) hex() (string, error) {
	s, ok := strings.CutPrefix(string(d), "sha256:")
	if !ok || len(s) != sha256.Size*2 || strings.ToLower(s) != s {
		return "", fmt.Errorf("record: malformed digest %q", d)
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("record: malformed digest %q: %w", d, err)
	}
	return s, nil
}

// CAS is the content-addressed store port. A customer deployment binds this to an
// OCI registry via ORAS; the demo profile binds it to LocalCAS below. No method
// takes a timestamp — an object is named by what it is, never by when it arrived.
type CAS interface {
	// Put stores b and returns its address. Putting identical bytes twice is a
	// no-op that returns the same address (content addressing = idempotent).
	Put(b []byte) (Digest, error)
	// Get returns the bytes for d, verifying integrity before returning them.
	Get(d Digest) ([]byte, error)
	// Has reports whether d is present.
	Has(d Digest) bool
}

// ErrNotFound is returned by Get when an address is absent — surfaced as a named
// availability gap (a preservation failure), never silently filled.
var ErrNotFound = errors.New("record: object not found in CAS")

// LocalCAS is the degraded-profile CAS: one file per object under Root, named by
// its hex digest. Immutable by construction — a write is a no-op if the object
// already exists, and the name is the hash so content can never drift from name.
type LocalCAS struct{ Root string }

// NewLocalCAS creates (if needed) a local-disk CAS rooted at dir.
func NewLocalCAS(dir string) (*LocalCAS, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("record: create CAS root: %w", err)
	}
	return &LocalCAS{Root: dir}, nil
}

func (c *LocalCAS) path(d Digest) (string, error) {
	h, err := d.hex()
	if err != nil {
		return "", err
	}
	// Shard by the first two hex chars to keep directories small.
	return filepath.Join(c.Root, h[:2], h), nil
}

// Put stores b and returns its content address.
func (c *LocalCAS) Put(b []byte) (Digest, error) {
	d := ComputeDigest(b)
	p, err := c.path(d)
	if err != nil {
		return "", err
	}
	if _, err := c.Get(d); err == nil {
		return c.acknowledge(d, p)
	} else if !errors.Is(err, ErrNotFound) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	// Each writer owns its temporary file; identical puts may run concurrently.
	f, err := os.CreateTemp(filepath.Dir(p), ".cas-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(f.Name()) }() //nolint:gosec // CreateTemp creates this name in the validated digest shard.
	defer func() { _ = f.Close() }()
	if _, err := f.Write(b); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(f.Name(), p); err != nil { //nolint:gosec // Source is CreateTemp-owned; target is derived from a validated SHA-256 digest.
		// Some platforms cannot replace a destination another writer published.
		if _, readErr := c.Get(d); readErr == nil {
			return c.acknowledge(d, p)
		}
		return "", err
	}
	return c.acknowledge(d, p)
}

// Get returns the bytes for d, verifying the content matches the address.
func (c *LocalCAS) Get(d Digest) ([]byte, error) {
	p, err := c.path(d)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, d)
	}
	if err != nil {
		return nil, err
	}
	if !d.Verify(b) {
		return nil, fmt.Errorf("record: integrity failure — %s does not match its content", d)
	}
	return b, nil
}

// Has reports whether d is present.
func (c *LocalCAS) Has(d Digest) bool {
	p, err := c.path(d)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}
