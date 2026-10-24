package record

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Bundle export/import is the self-contained canonical bundle of Law 1: a
// directory holding a signed manifest plus every object it names, so a state can
// be verified and replayed with NO network and NO original engine — the
// air-gapped "bundle-only gate replay" (system design §9, §17). The on-disk
// shape is deliberately OCI-layout-like (blobs addressed by digest under blobs/,
// a single top-level manifest) so migrating to real ORAS/OCI is a format match,
// not a rewrite.

const (
	bundleManifestFile = "manifest.json" // the SignedManifest
	bundleBlobsDir     = "blobs"         // one file per object, named by hex digest
)

// ExportBundle writes a self-contained bundle for sm into dir, copying every
// object the manifest names out of src. It fails loudly if any named object is
// missing from src — a bundle with a dangling reference is never written.
func ExportBundle(dir string, sm SignedManifest, src CAS) error {
	if err := VerifyManifest(sm); err != nil {
		return fmt.Errorf("record: refusing to export an unverifiable manifest: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, bundleBlobsDir), 0o755); err != nil {
		return err
	}
	for _, d := range sm.Manifest.Objects {
		b, err := src.Get(d)
		if err != nil {
			return fmt.Errorf("record: object %s named by manifest is missing from source: %w", d, err)
		}
		h, err := d.hex()
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, bundleBlobsDir, h), b, 0o644); err != nil {
			return err
		}
	}
	mb, err := json.MarshalIndent(sm, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, bundleManifestFile), mb, 0o644)
}

// ImportBundle opens a bundle at dir, verifies the manifest signature offline,
// verifies every object's content against its address, loads the objects into
// dst, and returns the verified manifest. Any signature or integrity failure
// aborts the whole import — a partially-trusted bundle is never loaded.
func ImportBundle(dir string, dst CAS) (SignedManifest, error) {
	var sm SignedManifest
	mb, err := os.ReadFile(filepath.Join(dir, bundleManifestFile))
	if err != nil {
		return sm, fmt.Errorf("record: read bundle manifest: %w", err)
	}
	if err := json.Unmarshal(mb, &sm); err != nil {
		return sm, err
	}
	// 1. The signature must verify with the key the bundle carries — no network.
	if err := VerifyManifest(sm); err != nil {
		return sm, err
	}
	// 2. Every named object must be present AND hash to its address.
	for _, d := range sm.Manifest.Objects {
		h, err := d.hex()
		if err != nil {
			return sm, err
		}
		b, err := os.ReadFile(filepath.Join(dir, bundleBlobsDir, h))
		if err != nil {
			return sm, fmt.Errorf("record: bundle missing object %s: %w", d, err)
		}
		if !d.Verify(b) {
			return sm, fmt.Errorf("record: bundle object %s fails integrity", d)
		}
		if _, err := dst.Put(b); err != nil {
			return sm, err
		}
	}
	return sm, nil
}
