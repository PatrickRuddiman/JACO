package packaging

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFindChecksum_HappyPath — the SHA256SUMS-style file has one line
// per artifact; findChecksum returns the leftmost field for the
// matching basename.
func TestFindChecksum_HappyPath(t *testing.T) {
	body := strings.Join([]string{
		"# comment line ignored",
		"",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  foo.tar.gz",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb  bar.tar.gz",
	}, "\n")
	got, err := findChecksum("bar.tar.gz", []byte(body))
	if err != nil {
		t.Fatalf("findChecksum: %v", err)
	}
	if got != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("got = %q", got)
	}
}

// TestFindChecksum_MissingReturnsError — basename not listed; error is
// surfaced verbatim so the operator can spot a malformed checksums
// file.
func TestFindChecksum_MissingReturnsError(t *testing.T) {
	_, err := findChecksum("ghost", []byte("aaa  foo\n"))
	if err == nil {
		t.Errorf("findChecksum(ghost) returned nil err")
	}
}

// TestFindChecksum_SkipsCommentsAndShortLines — blank lines, comments
// (prefixed with #), and lines with fewer than 2 fields are skipped.
func TestFindChecksum_SkipsCommentsAndShortLines(t *testing.T) {
	body := strings.Join([]string{
		"# this is a comment",
		"",
		"singletoken",
		"aaa  foo",
		"  # indented comment is NOT skipped — TrimSpace + HasPrefix check",
		"bbb  bar",
	}, "\n")
	got, err := findChecksum("foo", []byte(body))
	if err != nil {
		t.Fatalf("findChecksum: %v", err)
	}
	if got != "aaa" {
		t.Errorf("got = %q, want aaa", got)
	}
	got, err = findChecksum("bar", []byte(body))
	if err != nil {
		t.Fatalf("findChecksum bar: %v", err)
	}
	if got != "bbb" {
		t.Errorf("got = %q, want bbb", got)
	}
}

// TestSha256File_ReturnsKnownDigest — pre-computed digest of "hello"
// is well-known: 2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e7304331...
func TestSha256File_ReturnsKnownDigest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "in.txt")
	body := []byte("hello")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := sha256File(path)
	if err != nil {
		t.Fatalf("sha256File: %v", err)
	}
	want := sha256.Sum256(body)
	if got != hex.EncodeToString(want[:]) {
		t.Errorf("got = %s, want %s", got, hex.EncodeToString(want[:]))
	}
}

// TestSha256File_MissingFileSurfacesError — open error is returned
// directly; callers re-wrap as VerifyError.
func TestSha256File_MissingFileSurfacesError(t *testing.T) {
	if _, err := sha256File(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Errorf("sha256File on missing file returned nil err")
	}
}

// TestVerifyChecksum_Mismatch — happy path of the helper: we feed a
// tarball whose sha256 doesn't match the entry in checksums; expect a
// typed VerifyError with step=checksum.
func TestVerifyChecksum_Mismatch(t *testing.T) {
	dir := t.TempDir()
	tarball := filepath.Join(dir, "t.tar.gz")
	if err := os.WriteFile(tarball, []byte("real-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Lie about the checksum.
	body := []byte("0000000000000000000000000000000000000000000000000000000000000000  t.tar.gz\n")
	err := verifyChecksum(tarball, body)
	if err == nil {
		t.Fatalf("verifyChecksum mismatch returned nil err")
	}
	ve, ok := err.(*VerifyError)
	if !ok {
		t.Fatalf("err is not *VerifyError: %T %v", err, err)
	}
	if ve.Step != "checksum" {
		t.Errorf("Step = %q, want checksum", ve.Step)
	}
	if !strings.Contains(ve.Message, "sha256 mismatch") {
		t.Errorf("Message = %q, want sha256 mismatch substring", ve.Message)
	}
}

// TestVerifyChecksum_Match — feed the real sha256 of the tarball;
// verifyChecksum returns nil.
func TestVerifyChecksum_Match(t *testing.T) {
	dir := t.TempDir()
	tarball := filepath.Join(dir, "t.tar.gz")
	body := []byte("the-real-bytes")
	if err := os.WriteFile(tarball, body, 0o600); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(body)
	checksums := []byte(hex.EncodeToString(h[:]) + "  t.tar.gz\n")
	if err := verifyChecksum(tarball, checksums); err != nil {
		t.Errorf("verifyChecksum match returned err = %v", err)
	}
}

// TestVerifyChecksum_TarballMissing — when the tarball can't be read,
// the helper surfaces the sha256File error as a step=checksum
// VerifyError.
func TestVerifyChecksum_TarballMissing(t *testing.T) {
	err := verifyChecksum(filepath.Join(t.TempDir(), "missing"), []byte("aaa  missing\n"))
	ve, ok := err.(*VerifyError)
	if !ok {
		t.Fatalf("err is not *VerifyError: %v", err)
	}
	if ve.Step != "checksum" {
		t.Errorf("Step = %q, want checksum", ve.Step)
	}
}

// TestVerifyChecksum_BasenameNotListed — checksums file is well-formed
// but doesn't mention the tarball at all.
func TestVerifyChecksum_BasenameNotListed(t *testing.T) {
	dir := t.TempDir()
	tarball := filepath.Join(dir, "t.tar.gz")
	if err := os.WriteFile(tarball, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := verifyChecksum(tarball, []byte("aaa  other.tar.gz\n"))
	ve, ok := err.(*VerifyError)
	if !ok {
		t.Fatalf("err is not *VerifyError: %v", err)
	}
	if ve.Step != "checksum" {
		t.Errorf("Step = %q, want checksum", ve.Step)
	}
}

// TestExtractKeyLine_StripsHeaderAndBlanks — the embedded pubkey file
// has a `untrusted comment: ...` header on line 1 and the actual key
// on line 2; extractKeyLine returns the body.
func TestExtractKeyLine_StripsHeaderAndBlanks(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"untrusted comment: foo\nKEY-BODY\n", "KEY-BODY"},
		{"\n\nuntrusted comment: x\n\nBODY\n", "BODY"},
		{"BODY\n", "BODY"},
		// All-blank / all-header degenerates to returning the original
		// input — current behaviour per the function's docstring.
		{"", ""},
	}
	for _, c := range cases {
		if got := extractKeyLine(c.in); got != c.want {
			t.Errorf("extractKeyLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestVerifyError_ErrorFormat — covers both branches of the
// Error() method (with and without Step).
func TestVerifyError_ErrorFormat(t *testing.T) {
	withStep := (&VerifyError{Code: "code-a", Step: "sig", Message: "msg"}).Error()
	if !strings.Contains(withStep, "step=sig") {
		t.Errorf("with-step format = %q, want step=sig substring", withStep)
	}
	noStep := (&VerifyError{Code: "code-b", Message: "msg"}).Error()
	if strings.Contains(noStep, "step=") {
		t.Errorf("no-step format = %q, contains step=", noStep)
	}
}
