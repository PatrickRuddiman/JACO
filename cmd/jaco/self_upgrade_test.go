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

// buildTarball builds a deterministic tarball with dir/jaco and
// dir/jacod each carrying the given payload. Returns the gzipped bytes.
func buildTarball(t *testing.T, dirName string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: dirName + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"jaco", "jacod"} {
		if err := tw.WriteHeader(&tar.Header{
			Name: dirName + "/" + name, Typeflag: tar.TypeReg,
			Mode: 0o755, Size: int64(len(payload)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(payload); err != nil {
			t.Fatal(err)
		}
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

func TestRunSelfUpgrade_CorruptedTarballDoesNotModifyBinaries(t *testing.T) {
	// The AC: a corrupted tarball aborts cleanly without touching either
	// destination binary in the prefix dir.
	prefix := t.TempDir()
	jacoPath := filepath.Join(prefix, "jaco")
	jacodPath := filepath.Join(prefix, "jacod")
	originalBody := []byte("ORIGINAL-BINARY")
	if err := os.WriteFile(jacoPath, originalBody, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jacodPath, originalBody, 0o755); err != nil {
		t.Fatal(err)
	}
	jacoStat, _ := os.Stat(jacoPath)
	jacodStat, _ := os.Stat(jacodPath)

	tarballURL := "https://example.com/jaco-vNEW-linux-amd64.tar.gz"
	checksumsURL := "https://example.com/SHA256SUMS"
	sigURL := "https://example.com/SHA256SUMS.minisig"

	realTarball := buildTarball(t, "jaco-vNEW-linux-amd64", []byte("NEW-BINARY"))
	checksums := []byte(sha256Hex(realTarball) + "  jaco-vNEW-linux-amd64.tar.gz\n")
	corruptedTarball := append([]byte(nil), realTarball...)
	corruptedTarball[len(corruptedTarball)-1] ^= 0xFF

	fetcher := &inMemoryFetcher{files: map[string][]byte{
		tarballURL:   corruptedTarball,
		checksumsURL: checksums,
		sigURL:       []byte("untrusted comment: x\nbogus\n"),
	}}
	err := runSelfUpgrade(context.Background(), tarballURL, prefix, "", fetcher)
	if err == nil {
		t.Fatalf("expected verification failure on corrupted tarball")
	}
	if !packaging.IsVerificationFailed(err) {
		t.Errorf("err = %v; want upgrade_verification_failed", err)
	}

	for path, originalStat := range map[string]os.FileInfo{
		jacoPath:  jacoStat,
		jacodPath: jacodStat,
	} {
		post, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !bytes.Equal(post, originalBody) {
			t.Errorf("%s was modified after failed verify", path)
		}
		postStat, _ := os.Stat(path)
		if !postStat.ModTime().Equal(originalStat.ModTime()) {
			t.Errorf("%s mtime changed", path)
		}
	}
}

func TestRunSelfUpgrade_MissingURLRejected(t *testing.T) {
	err := runSelfUpgrade(context.Background(), "", "/tmp/jaco", "", &inMemoryFetcher{})
	if err == nil || !strings.Contains(err.Error(), "--url is required") {
		t.Errorf("err = %v; want missing-url error", err)
	}
}

func TestRunSelfUpgrade_FetchErrorAbortsBeforeTouchingBinaries(t *testing.T) {
	prefix := t.TempDir()
	jacoPath := filepath.Join(prefix, "jaco")
	original := []byte("ORIG")
	if err := os.WriteFile(jacoPath, original, 0o755); err != nil {
		t.Fatal(err)
	}
	err := runSelfUpgrade(context.Background(), "https://example.com/jaco.tar.gz", prefix, "", &inMemoryFetcher{})
	if err == nil {
		t.Fatalf("expected fetch error")
	}
	post, _ := os.ReadFile(jacoPath)
	if !bytes.Equal(post, original) {
		t.Errorf("jaco modified after fetch error")
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

func TestExtractTarballEntry_FindsBothBinaries(t *testing.T) {
	dir := t.TempDir()
	tarballPath := filepath.Join(dir, "t.tar.gz")
	payload := []byte("hello")
	if err := os.WriteFile(tarballPath, buildTarball(t, "jaco-vX-linux-amd64", payload), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"jaco", "jacod"} {
		dst := filepath.Join(dir, name)
		if err := extractTarballEntry(tarballPath, name, dst); err != nil {
			t.Fatalf("extract %s: %v", name, err)
		}
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("%s payload mismatch: got %q want %q", name, got, payload)
		}
	}
}

func TestExtractTarballEntry_MissingEntryErrors(t *testing.T) {
	dir := t.TempDir()
	tarballPath := filepath.Join(dir, "t.tar.gz")
	if err := os.WriteFile(tarballPath, buildTarball(t, "jaco-vX-linux-amd64", []byte("x")), 0o600); err != nil {
		t.Fatal(err)
	}
	err := extractTarballEntry(tarballPath, "ghost", filepath.Join(dir, "out"))
	if err == nil {
		t.Errorf("expected error for missing entry")
	}
}

// silence unused
var _ = time.Now
