package packaging

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jedisct1/go-minisign"
)

// VerifyError is the typed error VerifyTarball returns. Step is "signature"
// or "checksum" so the operator can tell which side failed.
type VerifyError struct {
	Code    string
	Step    string
	Message string
}

// Error implements the error interface.
func (e *VerifyError) Error() string {
	if e.Step != "" {
		return fmt.Sprintf("%s (step=%s): %s", e.Code, e.Step, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// IsVerificationFailed reports whether err is an upgrade_verification_failed.
func IsVerificationFailed(err error) bool {
	var ve *VerifyError
	if errors.As(err, &ve) {
		return ve.Code == "upgrade_verification_failed"
	}
	return false
}

// VerifyTarball validates a downloaded release tarball before
// `jaco self-upgrade` swaps it in. Two-step:
//
//  1. Minisign-verify checksumsPath against signaturePath using pubKey.
//  2. SHA-256 the tarball and assert it matches the entry in checksumsPath
//     for its basename.
//
// Returns a *VerifyError on either failure.
func VerifyTarball(tarballPath, checksumsPath, signaturePath, pubKey string) error {
	if pubKey == "" {
		return &VerifyError{Code: "upgrade_verification_failed", Step: "pubkey", Message: "release pubkey is empty"}
	}
	checksums, err := os.ReadFile(checksumsPath)
	if err != nil {
		return &VerifyError{Code: "upgrade_verification_failed", Step: "checksums_read", Message: err.Error()}
	}

	if err := verifySignature(checksums, signaturePath, pubKey); err != nil {
		return err
	}
	if err := verifyChecksum(tarballPath, checksums); err != nil {
		return err
	}
	return nil
}

func verifySignature(checksumsBody []byte, signaturePath, pubKey string) error {
	pk, err := minisign.NewPublicKey(strings.TrimSpace(extractKeyLine(pubKey)))
	if err != nil {
		return &VerifyError{Code: "upgrade_verification_failed", Step: "signature", Message: "parse pubkey: " + err.Error()}
	}
	sigBody, err := os.ReadFile(signaturePath)
	if err != nil {
		return &VerifyError{Code: "upgrade_verification_failed", Step: "signature", Message: "read signature: " + err.Error()}
	}
	sig, err := minisign.DecodeSignature(string(sigBody))
	if err != nil {
		return &VerifyError{Code: "upgrade_verification_failed", Step: "signature", Message: "decode signature: " + err.Error()}
	}
	ok, err := pk.Verify(checksumsBody, sig)
	if err != nil {
		return &VerifyError{Code: "upgrade_verification_failed", Step: "signature", Message: err.Error()}
	}
	if !ok {
		return &VerifyError{Code: "upgrade_verification_failed", Step: "signature", Message: "signature does not match checksums"}
	}
	return nil
}

func verifyChecksum(tarballPath string, checksums []byte) error {
	want, err := findChecksum(filepath.Base(tarballPath), checksums)
	if err != nil {
		return &VerifyError{Code: "upgrade_verification_failed", Step: "checksum", Message: err.Error()}
	}
	got, err := sha256File(tarballPath)
	if err != nil {
		return &VerifyError{Code: "upgrade_verification_failed", Step: "checksum", Message: err.Error()}
	}
	if got != want {
		return &VerifyError{
			Code: "upgrade_verification_failed", Step: "checksum",
			Message: fmt.Sprintf("sha256 mismatch: got %s want %s", got, want),
		}
	}
	return nil
}

// findChecksum extracts the `<hex>  <basename>` line for the named file from
// a SHA256SUMS body. Returns the hex string (no whitespace) or an error
// when the file isn't listed.
func findChecksum(basename string, checksums []byte) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(checksums)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Each line: "<hex>  <name>" (two spaces between sha256sum).
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[len(fields)-1] == basename {
			return fields[0], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("checksums file does not list %s", basename)
}

// sha256File returns the lowercase hex SHA-256 of the file at path.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractKeyLine pulls the base64 key body out of a 2-line minisign pubkey
// (skips the `untrusted comment:` line).
func extractKeyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "untrusted comment") {
			continue
		}
		return line
	}
	return s
}
