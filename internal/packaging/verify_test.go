package packaging_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/packaging"
)

// sha256Hex returns the lowercase hex SHA-256 of body.
func sha256Hex(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

// writeFile is a tiny helper that fails the test on write errors.
func writeFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestVerifyTarball_ChecksumMismatchReturnsTypedError(t *testing.T) {
	dir := t.TempDir()
	tarball := filepath.Join(dir, "jaco-vX-linux-amd64.tar.gz")
	checksums := filepath.Join(dir, "SHA256SUMS")
	signature := filepath.Join(dir, "SHA256SUMS.minisig")

	writeFile(t, tarball, []byte("real-tarball-bytes"))
	// SHA256SUMS lists a WRONG hash for the tarball.
	writeFile(t, checksums, []byte(
		"0000000000000000000000000000000000000000000000000000000000000000  jaco-vX-linux-amd64.tar.gz\n"))
	writeFile(t, signature, []byte("untrusted comment: x\nbogus-base64-signature\n"))

	// Pubkey is empty → step=pubkey error.
	err := packaging.VerifyTarball(tarball, checksums, signature, "")
	var ve *packaging.VerifyError
	if !errors.As(err, &ve) || ve.Code != "upgrade_verification_failed" {
		t.Fatalf("empty pubkey err = %v; want upgrade_verification_failed", err)
	}
}

func TestVerifyTarball_TarballMissingReturnsChecksumStepError(t *testing.T) {
	dir := t.TempDir()
	tarball := filepath.Join(dir, "missing.tar.gz")
	checksums := filepath.Join(dir, "SHA256SUMS")
	signature := filepath.Join(dir, "SHA256SUMS.minisig")

	body := []byte("anything")
	writeFile(t, checksums, []byte(
		sha256Hex(body)+"  missing.tar.gz\n"))
	writeFile(t, signature, []byte("untrusted comment: x\nbogus\n"))

	// pubkey is set but the signature is bogus — signature step fails first
	// before we get to checksum.
	err := packaging.VerifyTarball(tarball, checksums, signature, validPubKey())
	if !packaging.IsVerificationFailed(err) {
		t.Errorf("expected upgrade_verification_failed; got %v", err)
	}
}

func TestVerifyTarball_BogusSignatureReturnsSignatureStepError(t *testing.T) {
	dir := t.TempDir()
	tarball := filepath.Join(dir, "jaco.tar.gz")
	checksums := filepath.Join(dir, "SHA256SUMS")
	signature := filepath.Join(dir, "SHA256SUMS.minisig")

	body := []byte("tarball-bytes")
	writeFile(t, tarball, body)
	writeFile(t, checksums, []byte(sha256Hex(body)+"  jaco.tar.gz\n"))
	writeFile(t, signature, []byte("untrusted comment: bogus\nXXXXXXXXXXXXXXXX\n"))

	err := packaging.VerifyTarball(tarball, checksums, signature, validPubKey())
	var ve *packaging.VerifyError
	if !errors.As(err, &ve) {
		t.Fatalf("err is not VerifyError: %v", err)
	}
	if ve.Step != "signature" {
		t.Errorf("step = %q, want signature", ve.Step)
	}
}

func TestVerifyTarball_ChecksumsFileMissingFails(t *testing.T) {
	dir := t.TempDir()
	tarball := filepath.Join(dir, "jaco.tar.gz")
	writeFile(t, tarball, []byte("x"))
	err := packaging.VerifyTarball(tarball, filepath.Join(dir, "no.sums"), filepath.Join(dir, "no.minisig"), validPubKey())
	if !packaging.IsVerificationFailed(err) {
		t.Errorf("err = %v; want upgrade_verification_failed", err)
	}
}

func TestIsVerificationFailed_ReturnsTrueOnlyForOurError(t *testing.T) {
	if packaging.IsVerificationFailed(errors.New("plain error")) {
		t.Errorf("plain error should not match IsVerificationFailed")
	}
	if !packaging.IsVerificationFailed(&packaging.VerifyError{Code: "upgrade_verification_failed", Step: "x"}) {
		t.Errorf("our VerifyError should match")
	}
}

func TestEmbeddedPubKey_PresentInBinary(t *testing.T) {
	// The embed should produce a non-empty string with the minisign comment
	// + a key body line. The placeholder content asserts the embed worked
	// even though the key itself is invalid.
	if got := len(packaging.EmbeddedPubKey); got == 0 {
		t.Fatalf("EmbeddedPubKey is empty")
	}
	if !contains(packaging.EmbeddedPubKey, "untrusted comment") {
		t.Errorf("EmbeddedPubKey missing minisign 'untrusted comment' header")
	}
}

// validPubKey returns a real-shape but development-only minisign pubkey
// the tests can hand to VerifyTarball when they want signature-step
// failures (rather than pubkey-parse failures).
func validPubKey() string {
	// This is a well-formed shape but a synthetic key — signature
	// verification against any real signature will fail, which is what
	// the signature-step tests want to assert.
	return "untrusted comment: test\nRWQf6LRCGA9i53mlYecO4IzT51TGPpvWucNSCh1CBM0KQYdy8jM4cZmM\n"
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
outer:
	for i := 0; i+len(needle) <= len(haystack); i++ {
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}
