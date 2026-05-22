package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/packaging"
)

// inMemoryFetcher returns canned bytes per URL.
type inMemoryFetcher struct {
	files map[string][]byte
}

func (f *inMemoryFetcher) fetch(_ context.Context, url, dst string) error {
	body, ok := f.files[url]
	if !ok {
		return errors.New("not found: " + url)
	}
	return os.WriteFile(dst, body, 0o600)
}

// buildTarball builds a deterministic tarball with one regular file
// `dir/jaco` containing payload. Returns the gzipped bytes.
func buildTarball(t *testing.T, dirName string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// Directory entry.
	if err := tw.WriteHeader(&tar.Header{Name: dirName + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	// jaco binary.
	if err := tw.WriteHeader(&tar.Header{
		Name: dirName + "/jaco", Typeflag: tar.TypeReg,
		Mode: 0o755, Size: int64(len(payload)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestRunSelfUpgrade_CorruptedTarballDoesNotModifyBinary(t *testing.T) {
	// The AC: a corrupted tarball aborts cleanly without touching the
	// destination binary.
	dir := t.TempDir()
	binPath := filepath.Join(dir, "jaco")
	originalBody := []byte("ORIGINAL-BINARY")
	if err := os.WriteFile(binPath, originalBody, 0o755); err != nil {
		t.Fatal(err)
	}
	originalStat, _ := os.Stat(binPath)

	tarballURL := "https://example.com/jaco-vNEW-linux-amd64.tar.gz"
	checksumsURL := "https://example.com/SHA256SUMS"
	sigURL := "https://example.com/SHA256SUMS.minisig"

	// Build the *real* tarball with new content.
	realTarball := buildTarball(t, "jaco-vNEW-linux-amd64", []byte("NEW-BINARY"))
	// Compute checksum of REAL tarball but serve a CORRUPTED tarball.
	checksums := []byte(sha256Hex(realTarball) + "  jaco-vNEW-linux-amd64.tar.gz\n")
	corruptedTarball := append([]byte(nil), realTarball...)
	corruptedTarball[len(corruptedTarball)-1] ^= 0xFF // flip a bit

	fetcher := &inMemoryFetcher{files: map[string][]byte{
		tarballURL:   corruptedTarball,
		checksumsURL: checksums,
		sigURL:       []byte("untrusted comment: x\nbogus\n"),
	}}
	err := runSelfUpgrade(context.Background(), tarballURL, binPath, "", fetcher)
	if err == nil {
		t.Fatalf("expected verification failure on corrupted tarball")
	}
	if !packaging.IsVerificationFailed(err) {
		t.Errorf("err = %v; want upgrade_verification_failed", err)
	}

	// Confirm the binary is untouched.
	post, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	if !bytes.Equal(post, originalBody) {
		t.Errorf("binary was modified after failed verify: got %q want %q", post, originalBody)
	}
	postStat, _ := os.Stat(binPath)
	if !postStat.ModTime().Equal(originalStat.ModTime()) {
		t.Errorf("mtime changed: original=%v post=%v", originalStat.ModTime(), postStat.ModTime())
	}
}

func TestRunSelfUpgrade_MissingURLRejected(t *testing.T) {
	err := runSelfUpgrade(context.Background(), "", "/tmp/jaco", "", &inMemoryFetcher{})
	if err == nil || !strings.Contains(err.Error(), "--url is required") {
		t.Errorf("err = %v; want missing-url error", err)
	}
}

func TestRunSelfUpgrade_FetchErrorAbortsBeforeTouchingBinary(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "jaco")
	original := []byte("ORIG")
	if err := os.WriteFile(binPath, original, 0o755); err != nil {
		t.Fatal(err)
	}
	// Fetcher returns nothing — every URL is not-found.
	err := runSelfUpgrade(context.Background(), "https://example.com/jaco.tar.gz", binPath, "", &inMemoryFetcher{})
	if err == nil {
		t.Fatalf("expected fetch error")
	}
	post, _ := os.ReadFile(binPath)
	if !bytes.Equal(post, original) {
		t.Errorf("binary modified after fetch error")
	}
}

func TestSiblingURL_ReplacesLastPathSegment(t *testing.T) {
	cases := []struct {
		in, name, want string
	}{
		{"https://example.com/dir/tarball.tar.gz", "SHA256SUMS", "https://example.com/dir/SHA256SUMS"},
		{"https://example.com/tarball.tar.gz", "SHA256SUMS", "https://example.com/SHA256SUMS"},
		{"tarball.tar.gz", "x", "x"},
	}
	for _, c := range cases {
		if got := siblingURL(c.in, c.name); got != c.want {
			t.Errorf("siblingURL(%q,%q) = %q, want %q", c.in, c.name, got, c.want)
		}
	}
}

func TestExtractJacoBinary_FindsJacoEntry(t *testing.T) {
	dir := t.TempDir()
	tarballPath := filepath.Join(dir, "t.tar.gz")
	payload := []byte("hello")
	if err := os.WriteFile(tarballPath, buildTarball(t, "jaco-vX-linux-amd64", payload), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "jaco")
	if err := extractJacoBinary(tarballPath, dst); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("extracted payload mismatch: got %q want %q", got, payload)
	}
}

// silence unused
var _ = time.Now
