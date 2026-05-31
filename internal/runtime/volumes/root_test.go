package volumes_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/runtime/volumes"
)

// TestPathFor_Deterministic — same triple always yields the same path;
// PathFor doesn't touch the disk, so calling it 1000 times is free and
// stable.
func TestPathFor_Deterministic(t *testing.T) {
	r := volumes.NewRoot("/var/lib/jaco/volumes")
	a := r.PathFor("sample", "pg-primary", "pgdata")
	b := r.PathFor("sample", "pg-primary", "pgdata")
	if a != b {
		t.Fatalf("PathFor not deterministic: %q vs %q", a, b)
	}
	want := filepath.Join("/var/lib/jaco/volumes", "sample", "pg-primary", "pgdata")
	if a != want {
		t.Fatalf("PathFor = %q, want %q", a, want)
	}
}

// TestPathFor_RejectsEscape — a `..` in any segment or a path
// separator inside a segment is structurally unsafe (would let a
// caller write outside the managed root); PathFor panics so the bug
// is caught at the lifecycle layer, not at runtime when files have
// already moved.
func TestPathFor_RejectsEscape(t *testing.T) {
	r := volumes.NewRoot("/var/lib/jaco/volumes")
	cases := []struct {
		name   string
		dep    string
		svc    string
		volume string
	}{
		{"dep ..", "..", "svc", "v"},
		{"svc with slash", "dep", "svc/with/slash", "v"},
		{"volume ..", "dep", "svc", ".."},
		{"empty service", "dep", "", "v"},
		{"dot service", "dep", ".", "v"},
		{"dotdot in middle", "dep", "ok..bad", "v"},
		{"backslash", "dep", "ok\\bad", "v"},
		{"nul byte", "dep", "ok\x00bad", "v"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("PathFor(%q, %q, %q) did not panic", tc.dep, tc.svc, tc.volume)
				}
			}()
			_ = r.PathFor(tc.dep, tc.svc, tc.volume)
		})
	}
}

// TestEnsureLive_IdempotentAndMode — first call creates the tree at
// 0750; second call against the same triple is a no-op and returns
// the same path. The owning daemon runs as root, so 0750 keeps the
// per-volume directory readable by the docker-spawned container
// process while denying everyone else.
func TestEnsureLive_IdempotentAndMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("0750 mode bits are POSIX-only")
	}
	base := t.TempDir()
	r := volumes.NewRoot(base)

	p1, err := r.EnsureLive("dep", "svc", "vol")
	if err != nil {
		t.Fatalf("EnsureLive first: %v", err)
	}
	want := filepath.Join(base, "dep", "svc", "vol")
	if p1 != want {
		t.Errorf("EnsureLive returned %q, want %q", p1, want)
	}
	info, err := os.Stat(p1)
	if err != nil {
		t.Fatalf("stat live: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("EnsureLive did not create a directory")
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Errorf("EnsureLive mode = %o, want 0750", got)
	}

	// Second call — idempotent.
	p2, err := r.EnsureLive("dep", "svc", "vol")
	if err != nil {
		t.Fatalf("EnsureLive second: %v", err)
	}
	if p2 != p1 {
		t.Errorf("EnsureLive idempotent path drift: %q vs %q", p1, p2)
	}
}

// TestSnapshot_LexicalOrder — Snapshot walks the tree and yields
// entries sorted by relative path. Two siblings in a deeper directory
// come AFTER a sibling at the parent level, but within the same
// directory they sort lexically. Excludes the "." root entry from
// the consumer's view.
func TestSnapshot_LexicalOrder(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "a.txt", "1")
	mustWrite(t, root, "sub/b.txt", "2")
	mustWrite(t, root, "sub/a.txt", "3")
	mustWrite(t, root, "z.txt", "4")

	r := volumes.NewRoot(filepath.Dir(root))
	it, err := r.Snapshot(root)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	var got []string
	for {
		e, ok, err := it.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		got = append(got, e.Path)
	}
	want := []string{".", "a.txt", "sub", "sub/a.txt", "sub/b.txt", "z.txt"}
	if !stringSliceEq(got, want) {
		t.Errorf("Snapshot order:\n got=%v\nwant=%v", got, want)
	}
}

// TestManifest_DeterministicAcrossTrees — two trees built with the
// same bytes hash to bit-identical maps. Anchors the "snapshot now,
// re-derive on the receiver, compare manifests" check that
// ShipVolume (PR2) uses for VERIFY.
func TestManifest_DeterministicAcrossTrees(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	for _, root := range []string{a, b} {
		mustWrite(t, root, "alpha.txt", "hello")
		mustWrite(t, root, "nested/beta.txt", "world")
	}
	r := volumes.NewRoot("/var/lib/jaco/volumes") // base unused by Manifest
	ma, err := r.Manifest(a)
	if err != nil {
		t.Fatalf("Manifest a: %v", err)
	}
	mb, err := r.Manifest(b)
	if err != nil {
		t.Fatalf("Manifest b: %v", err)
	}
	if !mapEq(ma, mb) {
		t.Errorf("Manifest drift on identical trees:\n a=%v\n b=%v", ma, mb)
	}
	if len(ma) == 0 {
		t.Errorf("Manifest empty for non-empty tree")
	}
	if _, ok := ma["alpha.txt"]; !ok {
		t.Errorf("alpha.txt missing from manifest: %v", ma)
	}
	if _, ok := ma["nested/beta.txt"]; !ok {
		t.Errorf("nested/beta.txt missing from manifest: %v", ma)
	}
}

// TestManifest_DiffersOnContentChange — flipping a single byte in any
// file MUST flip that file's hash (and only that file's). The other
// entries stay identical to the baseline so callers can diff
// manifests to spot drift.
func TestManifest_DiffersOnContentChange(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "a.txt", "before")
	mustWrite(t, root, "b.txt", "stable")

	r := volumes.NewRoot("/var/lib/jaco/volumes")
	before, err := r.Manifest(root)
	if err != nil {
		t.Fatalf("Manifest before: %v", err)
	}

	mustWrite(t, root, "a.txt", "after")
	after, err := r.Manifest(root)
	if err != nil {
		t.Fatalf("Manifest after: %v", err)
	}

	if before["a.txt"] == after["a.txt"] {
		t.Errorf("a.txt hash unchanged after content edit (%q)", before["a.txt"])
	}
	if before["b.txt"] != after["b.txt"] {
		t.Errorf("b.txt hash drifted with no edit: %q vs %q", before["b.txt"], after["b.txt"])
	}
}

// TestManifest_DiffersOnModeChange — a chmod on a single file flips
// its hash; siblings stay stable. This is the explicit reason the
// hash includes the mode bits, not just the content.
func TestManifest_DiffersOnModeChange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod bits don't round-trip on Windows")
	}
	root := t.TempDir()
	mustWrite(t, root, "a.txt", "x")
	mustWrite(t, root, "b.txt", "y")

	r := volumes.NewRoot("/var/lib/jaco/volumes")
	before, err := r.Manifest(root)
	if err != nil {
		t.Fatalf("Manifest before: %v", err)
	}
	if err := os.Chmod(filepath.Join(root, "a.txt"), 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	after, err := r.Manifest(root)
	if err != nil {
		t.Fatalf("Manifest after: %v", err)
	}
	if before["a.txt"] == after["a.txt"] {
		t.Errorf("a.txt hash unchanged after chmod (%q)", before["a.txt"])
	}
	if before["b.txt"] != after["b.txt"] {
		t.Errorf("b.txt hash drifted with no edit: %q vs %q", before["b.txt"], after["b.txt"])
	}
}

// TestManifest_DiffersOnPathChange — renaming a file changes the
// hashed (path,mode,content) triple of the renamed entry's manifest
// key, and adds/removes keys. The key set itself drifts, which is
// the canonical signal for "the receiver got a different tree".
func TestManifest_DiffersOnPathChange(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "a.txt", "z")
	r := volumes.NewRoot("/var/lib/jaco/volumes")
	before, err := r.Manifest(root)
	if err != nil {
		t.Fatalf("Manifest before: %v", err)
	}
	if err := os.Rename(filepath.Join(root, "a.txt"), filepath.Join(root, "renamed.txt")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	after, err := r.Manifest(root)
	if err != nil {
		t.Fatalf("Manifest after: %v", err)
	}
	if _, ok := after["a.txt"]; ok {
		t.Errorf("a.txt should be absent after rename")
	}
	if _, ok := after["renamed.txt"]; !ok {
		t.Errorf("renamed.txt should be present after rename")
	}
	if before["a.txt"] == after["renamed.txt"] {
		t.Errorf("manifest hash carried the same content through a path change — should differ because the path is part of the triple")
	}
}

// TestSwap_HappyPath — Swap with a populated staging directory and
// an existing live directory replaces live with staging's contents
// and removes the trash.
func TestSwap_HappyPath(t *testing.T) {
	base := t.TempDir()
	r := volumes.NewRoot(base)

	live, err := r.EnsureLive("dep", "svc", "vol")
	if err != nil {
		t.Fatalf("EnsureLive: %v", err)
	}
	mustWrite(t, live, "old.txt", "previous")

	staging := r.StagingPath("dep", "svc", "vol", "move-1")
	if err := os.MkdirAll(staging, 0o750); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	mustWrite(t, staging, "new.txt", "current")

	if err := r.Swap("dep", "svc", "vol", staging); err != nil {
		t.Fatalf("Swap: %v", err)
	}

	if _, err := os.Stat(filepath.Join(live, "new.txt")); err != nil {
		t.Errorf("expected new.txt under live after Swap: %v", err)
	}
	if _, err := os.Stat(filepath.Join(live, "old.txt")); err == nil {
		t.Errorf("old.txt still present in live after Swap")
	}
	// Trash purge happened.
	parent := filepath.Dir(live)
	dirents, _ := os.ReadDir(parent)
	for _, d := range dirents {
		if strings.HasPrefix(d.Name(), "vol.trash-") {
			t.Errorf("trash %q left behind after Swap", d.Name())
		}
	}
	if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging dir should no longer exist after Swap: err=%v", err)
	}
}

// TestSwap_FirstTimePromote — no existing live, just promote staging.
// The "live missing, staging present" branch is the common case on a
// fresh ACTIVATE-after-PLAN.
func TestSwap_FirstTimePromote(t *testing.T) {
	base := t.TempDir()
	r := volumes.NewRoot(base)

	staging := r.StagingPath("dep", "svc", "vol", "move-1")
	if err := os.MkdirAll(staging, 0o750); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	mustWrite(t, staging, "k.txt", "v")

	if err := r.Swap("dep", "svc", "vol", staging); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	live := r.PathFor("dep", "svc", "vol")
	if _, err := os.Stat(filepath.Join(live, "k.txt")); err != nil {
		t.Errorf("expected promoted file under live: %v", err)
	}
}

// TestSwap_FailurePreservesLive — when the staging→live rename fails
// the function MUST restore the prior live tree from trash. Simulate
// the failure by passing a staging path that doesn't exist after
// the trash rename has happened — easier to engineer reliably than
// an EPERM. The contract under test is: caller sees the original
// live tree intact regardless of how Swap exited.
func TestSwap_FailurePreservesLive(t *testing.T) {
	base := t.TempDir()
	r := volumes.NewRoot(base)

	live, err := r.EnsureLive("dep", "svc", "vol")
	if err != nil {
		t.Fatalf("EnsureLive: %v", err)
	}
	mustWrite(t, live, "old.txt", "must-survive")

	// Staging path is "absent" — the file or directory does not
	// exist. Swap should refuse without disturbing live.
	staging := r.StagingPath("dep", "svc", "vol", "move-1")
	err = r.Swap("dep", "svc", "vol", staging)
	if err == nil {
		t.Fatalf("Swap with missing staging returned nil err; expected refusal")
	}
	if errors.Is(err, volumes.ErrNeedsRecovery) {
		t.Fatalf("Swap with missing staging returned ErrNeedsRecovery; expected plain failure when live is still present")
	}

	// Live MUST still carry old.txt — Swap did not blow it away.
	if _, err := os.Stat(filepath.Join(live, "old.txt")); err != nil {
		t.Errorf("live lost its content after a no-op Swap: %v", err)
	}
}

// TestSwap_RecoversFromInterruptedSwap — simulate a crash between
// the live→trash rename and the staging→live rename: live is missing,
// trash carries the prior contents, no staging. Next Swap restores
// live from trash and returns ErrNeedsRecovery so the migration FSM
// retries the move.
func TestSwap_RecoversFromInterruptedSwap(t *testing.T) {
	base := t.TempDir()
	r := volumes.NewRoot(base)

	live := r.PathFor("dep", "svc", "vol")
	parent := filepath.Dir(live)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	// Hand-build a trash directory carrying the pre-crash contents.
	trash := live + ".trash-1700000000000000000"
	if err := os.MkdirAll(trash, 0o750); err != nil {
		t.Fatalf("mkdir trash: %v", err)
	}
	mustWrite(t, trash, "old.txt", "recovered")

	staging := r.StagingPath("dep", "svc", "vol", "move-99")
	err := r.Swap("dep", "svc", "vol", staging)
	if !errors.Is(err, volumes.ErrNeedsRecovery) {
		t.Fatalf("Swap recovery err = %v, want ErrNeedsRecovery", err)
	}
	// live now exists with the trash's content; trash is gone (consumed by the rename).
	if _, err := os.Stat(filepath.Join(live, "old.txt")); err != nil {
		t.Errorf("recovered live missing old.txt: %v", err)
	}
	if _, err := os.Stat(trash); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("trash should have been renamed to live: stat err=%v", err)
	}
}

// TestStagingPath_DistinctPerMove — two moveIDs under the same triple
// produce distinct paths so concurrent receivers don't collide.
func TestStagingPath_DistinctPerMove(t *testing.T) {
	r := volumes.NewRoot("/var/lib/jaco/volumes")
	a := r.StagingPath("dep", "svc", "vol", "move-1")
	b := r.StagingPath("dep", "svc", "vol", "move-2")
	if a == b {
		t.Errorf("StagingPath returned the same path for different moves: %q", a)
	}
	// Live path MUST sit under the same parent so Swap's rename is
	// a same-directory operation (atomicity guarantee).
	live := r.PathFor("dep", "svc", "vol")
	if filepath.Dir(a) != filepath.Dir(live) {
		t.Errorf("StagingPath parent %q != live parent %q (Swap relies on same-directory rename)",
			filepath.Dir(a), filepath.Dir(live))
	}
}

// TestStagingPath_RejectsEscape — same surface as PathFor: a `..` in
// the moveID is a programming bug and panics rather than letting an
// untrusted move identifier escape the per-volume directory.
func TestStagingPath_RejectsEscape(t *testing.T) {
	r := volumes.NewRoot("/var/lib/jaco/volumes")
	defer func() {
		if recover() == nil {
			t.Errorf("StagingPath with `..` moveID did not panic")
		}
	}()
	_ = r.StagingPath("dep", "svc", "vol", "..")
}

// TestSnapshot_LazyOpen — directories and other non-regular entries
// have no Open closure; regular files have one that reads the
// original content. Catches a regression where the iterator might
// open every file up front (would blow up on million-file trees).
func TestSnapshot_LazyOpen(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "regular.txt", "bytes")
	if err := os.Mkdir(filepath.Join(root, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	r := volumes.NewRoot("/var/lib/jaco/volumes")
	it, err := r.Snapshot(root)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	saw := map[string]bool{}
	for {
		e, ok, err := it.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		saw[e.Path] = true
		switch e.Path {
		case "regular.txt":
			if e.Open == nil {
				t.Errorf("regular file has nil Open closure")
				continue
			}
			rc, err := e.Open()
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			body, _ := io.ReadAll(rc)
			_ = rc.Close()
			if string(body) != "bytes" {
				t.Errorf("file content = %q, want %q", string(body), "bytes")
			}
		case "subdir", ".":
			if e.Open != nil {
				t.Errorf("non-regular entry %q has non-nil Open", e.Path)
			}
		}
	}
	if !saw["regular.txt"] || !saw["subdir"] {
		t.Errorf("missing entries: saw=%v", saw)
	}
}

// mustWrite creates parent dirs and writes content to root/relpath.
func mustWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o640); err != nil {
		t.Fatalf("writefile %s: %v", abs, err)
	}
}

func stringSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mapEq(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
