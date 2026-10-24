package record

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalCAS_PutGetIntegrity(t *testing.T) {
	cas, err := NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("a refund never exceeds its capture")
	d, err := cas.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(d), "sha256:") {
		t.Fatalf("digest not content-addressed: %s", d)
	}
	// Idempotent: putting identical bytes returns the same address.
	if d2, _ := cas.Put(body); d2 != d {
		t.Fatalf("content addressing not idempotent: %s != %s", d2, d)
	}
	got, err := cas.Get(d)
	if err != nil || string(got) != string(body) {
		t.Fatalf("get mismatch: %v %q", err, got)
	}
	if _, err := cas.Get("sha256:deadbeef"); err == nil {
		t.Fatal("expected error getting absent object")
	}
}

func TestManifest_SignVerify(t *testing.T) {
	signer, err := NewSelfSigner("principal:ci-bot")
	if err != nil {
		t.Fatal(err)
	}
	sm, err := signer.Sign(Manifest{Commit: "c0ffee", ProjectionHash: "sha256:abc"})
	if err != nil {
		t.Fatal(err)
	}
	if sm.Manifest.Profile != ProfileDegraded {
		t.Fatalf("expected degraded profile stamp, got %q", sm.Manifest.Profile)
	}
	if err := VerifyManifest(sm); err != nil {
		t.Fatalf("valid manifest failed to verify: %v", err)
	}
	// Tamper with the signed content — verification must fail.
	sm.Manifest.Commit = "tampered"
	if err := VerifyManifest(sm); err == nil {
		t.Fatal("tampered manifest verified — signature is not binding")
	}
}

func TestBundle_RoundTripAndTamper(t *testing.T) {
	src, _ := NewLocalCAS(t.TempDir())
	da, _ := src.Put([]byte("object-a"))
	db, _ := src.Put([]byte("object-b"))

	signer, _ := NewSelfSigner("principal:ci-bot")
	sm, err := signer.Sign(Manifest{
		Commit:         "c0ffee",
		ProjectionHash: "sha256:proj",
		Objects:        []Digest{da, db},
	})
	if err != nil {
		t.Fatal(err)
	}

	bundleDir := t.TempDir()
	if err := ExportBundle(bundleDir, sm, src); err != nil {
		t.Fatal(err)
	}

	// Import into a FRESH, empty CAS with no network — the offline replay path.
	dst, _ := NewLocalCAS(t.TempDir())
	got, err := ImportBundle(bundleDir, dst)
	if err != nil {
		t.Fatalf("clean bundle failed to import: %v", err)
	}
	if got.Manifest.Commit != "c0ffee" || !dst.Has(da) || !dst.Has(db) {
		t.Fatal("import did not fully load the preservation set")
	}

	// Tamper with a blob in the bundle — import must reject the whole thing.
	h := strings.TrimPrefix(string(da), "sha256:")
	if err := os.WriteFile(filepath.Join(bundleDir, bundleBlobsDir, h), []byte("corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst2, _ := NewLocalCAS(t.TempDir())
	if _, err := ImportBundle(bundleDir, dst2); err == nil {
		t.Fatal("tampered bundle imported — integrity is not enforced")
	}
}
