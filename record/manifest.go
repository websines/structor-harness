package record

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

// Profile names how a manifest was produced, so the record is honest about its
// own assurance level. A degraded manifest can never be mistaken for a
// customer-grade one — the degradation travels with the bytes (system design §17).
type Profile string

const (
	// ProfileDegraded — the demo profile: local-disk CAS, self-signed manifests,
	// single principal, no external transparency anchor.
	ProfileDegraded Profile = "degraded-demo"
	// ProfileCustomer — the target: customer CAS, cosign/KMS signatures, Rekor
	// anchor. Not implemented on this profile; named here as the swap target.
	ProfileCustomer Profile = "customer-grade"
)

// Manifest is the signed, canonical description of a promoted state (§4, §9). It
// names — by content address — every object the state depends on, pins the code
// commit and the derived projection hash, and records who signed it and under
// which profile. It carries NO timestamp: Law 3 holds for the record itself.
type Manifest struct {
	// Profile is the assurance level under which this manifest was produced.
	Profile Profile `json:"profile"`
	// Commit is the code commit this state is anchored to (Law 4).
	Commit string `json:"commit"`
	// ProjectionHash is the digest of the derived projection this manifest
	// certifies (see flg.Store.ProjectionHash). Recomputing the projection from
	// Objects must reproduce this exactly — the Law 1 rebuildability check.
	ProjectionHash string `json:"projection_hash"`
	// Objects is the preservation set: every CAS address this state requires.
	// Sorted, so the manifest bytes are deterministic and themselves addressable.
	Objects []Digest `json:"objects"`
	// PrincipalID is the verified principal that signed this manifest. On the
	// degraded profile it is a single local identity; never a display string.
	PrincipalID string `json:"principal_id"`
	// Note carries any named degradations or human context.
	Note string `json:"note,omitempty"`
	// CheckpointMetadata binds the roles, parent and other preservation metadata.
	CheckpointMetadata Digest `json:"checkpoint_metadata,omitempty"`
}

// canonicalBytes serializes the manifest deterministically (sorted object set,
// fixed field order) so the same manifest always yields the same bytes — and
// therefore the same signature and the same content address.
func (m Manifest) canonicalBytes() ([]byte, error) {
	c := m
	c.Objects = append([]Digest(nil), m.Objects...)
	sort.Slice(c.Objects, func(i, j int) bool { return c.Objects[i] < c.Objects[j] })
	return json.Marshal(c)
}

// CanonicalBytes exposes the exact signable serialization — the same bytes
// SelfSigner.Sign hands to ed25519.Sign and the same bytes VerifyManifest
// reconstructs. It exists so a Signer implemented OUTSIDE this package (the
// cosign-backed signer in internal/attest) can sign the identical payload
// without re-deriving the canonical form; the unexported canonicalBytes stays
// the single source of truth for how a manifest is serialized.
func (m Manifest) CanonicalBytes() ([]byte, error) { return m.canonicalBytes() }

// SigningMethod identifies which signer produced a SignedManifest's signature so
// VerifyManifest can select the matching verification path. The zero value (empty
// string) is the original ed25519 self-signed profile: it is kept as the default
// so every manifest written before the cosign swap existed still verifies, and so
// a self-signed manifest serializes byte-for-byte as it always did.
type SigningMethod string

const (
	// MethodEd25519 is the degraded-profile ed25519 self-signature emitted by
	// SelfSigner. The empty string is treated as this method, so an absent
	// "method" field (every pre-existing manifest) takes the ed25519 path.
	MethodEd25519 SigningMethod = "ed25519"
	// MethodCosign is a real sigstore bundle produced offline by the cosign CLI
	// (internal/attest.CosignSigner). For this method Signature carries the
	// sigstore bundle bytes and PublicKey carries the PEM cosign public key.
	MethodCosign SigningMethod = "cosign"
)

// SignedManifest is a manifest plus a signature over its canonical bytes and the
// public key needed to verify it offline (bundle export carries this whole thing).
type SignedManifest struct {
	Manifest  Manifest `json:"manifest"`
	Signature []byte   `json:"signature"`
	PublicKey []byte   `json:"public_key"`
	// Method names the signer that produced Signature so VerifyManifest can
	// dispatch. Empty means MethodEd25519 (the original self-signed profile);
	// with omitempty an ed25519 manifest stays byte-identical to legacy output.
	Method SigningMethod `json:"method,omitempty"`
}

// Signer produces and verifies manifest signatures. The degraded profile binds
// this to SelfSigner; a customer deployment binds it to cosign/KMS. Verify is a
// package function (below) because a verifier needs only the public key in the
// SignedManifest — offline, air-gapped verification (the bundle-only gate replay).
type Signer interface {
	Sign(m Manifest) (SignedManifest, error)
	PrincipalID() string
}

// SelfSigner is the degraded-profile signer: a locally generated ed25519 key,
// no CA, no transparency log. Sufficient to prove the pipeline end to end;
// explicitly NOT customer-grade, which every manifest it emits declares.
type SelfSigner struct {
	priv      ed25519.PrivateKey
	pub       ed25519.PublicKey
	principal string
}

// NewSelfSigner generates a fresh ed25519 keypair bound to principal id.
func NewSelfSigner(principal string) (*SelfSigner, error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, fmt.Errorf("record: generate signing key: %w", err)
	}
	return &SelfSigner{priv: priv, pub: pub, principal: principal}, nil
}

// PrincipalID returns the signer's verified principal id.
func (s *SelfSigner) PrincipalID() string { return s.principal }

// Sign stamps the manifest with this signer's principal and profile, then signs
// its canonical bytes. The returned SignedManifest verifies offline.
func (s *SelfSigner) Sign(m Manifest) (SignedManifest, error) {
	m.PrincipalID = s.principal
	if m.Profile == "" {
		m.Profile = ProfileDegraded
	}
	b, err := m.canonicalBytes()
	if err != nil {
		return SignedManifest{}, err
	}
	sig := ed25519.Sign(s.priv, b)
	return SignedManifest{Manifest: m, Signature: sig, PublicKey: s.pub}, nil
}

// VerifyManifest checks a signed manifest against its embedded public key — the
// offline verification a bundle importer runs before trusting any object. It
// does NOT establish that the key belongs to a trusted principal; on the
// customer profile that binding comes from cosign/Fulcio + the identity plane.
func VerifyManifest(sm SignedManifest) error {
	b, err := sm.Manifest.canonicalBytes()
	if err != nil {
		return err
	}
	switch sm.Method {
	case "", MethodEd25519:
		// The original degraded path, unchanged: verify the ed25519 signature
		// against the key the manifest carries. Empty method == ed25519 keeps
		// every pre-existing manifest verifying exactly as before.
		if len(sm.PublicKey) != ed25519.PublicKeySize {
			return fmt.Errorf("record: bad public key length %d", len(sm.PublicKey))
		}
		if !ed25519.Verify(ed25519.PublicKey(sm.PublicKey), b, sm.Signature) {
			return fmt.Errorf("record: manifest signature does not verify")
		}
		return nil
	case MethodCosign:
		return verifyCosign(b, sm.Signature, sm.PublicKey)
	default:
		return fmt.Errorf("record: unknown signing method %q", sm.Method)
	}
}

// verifyCosign checks a MethodCosign manifest offline by handing the canonical
// payload, the sigstore bundle (carried in Signature) and the cosign public key
// (carried in PublicKey) to `cosign verify-blob`, ignoring the transparency log
// exactly as the offline signer omitted it. It reimplements NO cryptography —
// cosign makes the verification decision — and needs only the cosign binary on
// PATH plus the bytes already inside the SignedManifest, so a bundle importer
// still verifies air-gapped. Kept here (rather than calling internal/attest) so
// the record package remains self-verifying and free of an import cycle.
func verifyCosign(payload, bundle, pubKey []byte) error {
	bin, err := exec.LookPath("cosign")
	if err != nil {
		return fmt.Errorf("record: cosign not available to verify a cosign-signed manifest: %w", err)
	}
	tmp, err := os.MkdirTemp("", "record-verify-")
	if err != nil {
		return fmt.Errorf("record: temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	payloadFile := filepath.Join(tmp, "payload")
	bundleFile := filepath.Join(tmp, "bundle")
	pubFile := filepath.Join(tmp, "cosign.pub")
	if err := os.WriteFile(payloadFile, payload, 0o600); err != nil {
		return fmt.Errorf("record: write payload: %w", err)
	}
	if err := os.WriteFile(bundleFile, bundle, 0o600); err != nil {
		return fmt.Errorf("record: write bundle: %w", err)
	}
	if err := os.WriteFile(pubFile, pubKey, 0o600); err != nil {
		return fmt.Errorf("record: write public key: %w", err)
	}

	cmd := exec.Command(bin, "verify-blob",
		"--key", pubFile,
		"--bundle", bundleFile,
		"--insecure-ignore-tlog=true",
		payloadFile,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("record: cosign verify-blob rejected the manifest: %w: %s", err, out)
	}
	return nil
}
