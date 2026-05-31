package volumes

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Root is the on-host root of JACO's managed volume tree. Every managed
// volume materializes under
//
//	<base>/<deployment>/<service>/<volume>/
//
// and is bind-mounted into the container instead of using a docker named
// volume. Owning the path on the host is what makes the v1 ship-volume
// design possible — the daemon needs a file-tree to snapshot, manifest,
// stage, and atomically swap during a planned move (ADR 0001).
//
// Root is a value type; the base path is immutable after construction.
// All mutating operations are file-system calls — no shell-out, no
// network, no docker dependency — so this package unit-tests against a
// t.TempDir() base with zero plumbing.
type Root struct{ base string }

// NewRoot returns a Root rooted at base. base is taken verbatim — the
// caller (daemon constructor) decides the on-disk location, typically
// "<data_dir>/volumes". Empty base is allowed for tests that override
// every path explicitly; production callers pass a non-empty path.
func NewRoot(base string) *Root { return &Root{base: base} }

// Base returns the configured root path.
func (r *Root) Base() string { return r.base }

// errInvalidIdent is returned by PathFor (and EnsureLive) when a path
// component carries `..`, a path separator, an empty string, or a NUL.
// These segments would let a malicious or buggy deployment escape the
// managed root via `os.MkdirAll` symlink + ".." traversal. Reject up
// front so escape is structurally impossible.
var errInvalidIdent = errors.New("volumes: invalid identifier (path traversal rejected)")

// safeIdent rejects identifiers that could escape the managed root or
// the per-(deployment, service, volume) directory. The compose validator
// already enforces the deployment / service / volume names against a
// stricter character class — this layer is the last line of defence.
func safeIdent(s string) error {
	if s == "" {
		return errInvalidIdent
	}
	if s == "." || s == ".." {
		return errInvalidIdent
	}
	if strings.ContainsAny(s, "/\\\x00") {
		return errInvalidIdent
	}
	if strings.Contains(s, "..") {
		// Reject "..foo", "foo..", "foo..bar": every traversal token
		// uses these two bytes. The compose layer's allowed character
		// class doesn't include "..", so any input that reaches here
		// with one is malformed.
		return errInvalidIdent
	}
	return nil
}

// validateTriple rejects the (deployment, service, volume) triple if any
// segment would let the caller escape the per-volume directory. Returns
// nil when every segment is safe.
func validateTriple(deployment, service, volumeName string) error {
	if err := safeIdent(deployment); err != nil {
		return fmt.Errorf("deployment %q: %w", deployment, err)
	}
	if err := safeIdent(service); err != nil {
		return fmt.Errorf("service %q: %w", service, err)
	}
	if err := safeIdent(volumeName); err != nil {
		return fmt.Errorf("volume %q: %w", volumeName, err)
	}
	return nil
}

// PathFor returns the deterministic live path for (deployment, service,
// volumeName). Panics on a triple that would escape the root — callers
// MUST sanitise input upstream; reaching here with `..` is a bug, not a
// runtime condition. The returned path always has the managed base as a
// prefix and never contains a `..` segment.
func (r *Root) PathFor(deployment, service, volumeName string) string {
	if err := validateTriple(deployment, service, volumeName); err != nil {
		panic(err)
	}
	return filepath.Join(r.base, deployment, service, volumeName)
}

// EnsureLive creates the live volume directory (mode 0750) and every
// parent. Idempotent: a pre-existing tree at the same path with any
// mode is left untouched and returns ok. Returns the resolved live path
// on success.
func (r *Root) EnsureLive(deployment, service, volumeName string) (string, error) {
	if err := validateTriple(deployment, service, volumeName); err != nil {
		return "", err
	}
	live := filepath.Join(r.base, deployment, service, volumeName)
	// MkdirAll creates parents with 0755-ish mode; we then chmod the
	// leaf to 0750 so the per-volume directory matches the documented
	// permission. Pre-existing directories keep their mode (idempotent).
	if err := os.MkdirAll(live, 0o750); err != nil {
		return "", fmt.Errorf("EnsureLive %s: %w", live, err)
	}
	if err := os.Chmod(live, 0o750); err != nil {
		return "", fmt.Errorf("EnsureLive chmod %s: %w", live, err)
	}
	return live, nil
}

// StagingPath returns the deterministic staging path used by the
// ShipVolume receiver (PR2). One staging directory per in-flight move
// keyed by moveID; the receiver writes incoming chunks here, then
// hands the path to Swap on a successful manifest match.
//
// The leading "." in ".staging-<moveID>" keeps staging directories out
// of casual `ls <volume>/` listings inside the live tree. Note that the
// staging path lives ALONGSIDE the live directory (under
// <deployment>/<service>/), not inside it — Swap renames atomically
// without recursing through the live tree first.
func (r *Root) StagingPath(deployment, service, volumeName, moveID string) string {
	if err := validateTriple(deployment, service, volumeName); err != nil {
		panic(err)
	}
	if err := safeIdent(moveID); err != nil {
		panic(fmt.Errorf("moveID %q: %w", moveID, err))
	}
	return filepath.Join(r.base, deployment, service, ".staging-"+volumeName+"-"+moveID)
}

// Entry is one file emitted by a snapshot Iterator. Mode carries the
// permission bits and file-type (regular / symlink / directory) so the
// receiver can reproduce the source tree faithfully. Open is a lazy
// reader so the iterator never holds more than one file descriptor at
// a time even for million-file trees.
type Entry struct {
	// Path is the relative path from the volume root, using '/' as the
	// separator regardless of host OS. Always cleaned (no `..`, no
	// leading `/`, no trailing `/`).
	Path string
	// Mode is the os.FileInfo Mode() of the source file, including the
	// type bits. Receivers reconstruct directory / symlink / regular
	// file decisions from Mode.IsDir() etc.
	Mode fs.FileMode
	// Open returns a fresh reader on the file content. Nil for
	// directories and symlinks (their bytes are zero).
	Open func() (io.ReadCloser, error)
}

// Iterator yields entries one at a time in lexical Path order. Done
// returns false when the walk is exhausted; the last call may also
// return a non-nil error. Iterators are not safe for concurrent use.
type Iterator interface {
	Next() (entry Entry, ok bool, err error)
}

// sliceIterator is the implementation backing Snapshot. It pre-walks
// the tree (necessary to sort deterministically) and replays entries
// on Next.
type sliceIterator struct {
	entries []Entry
	i       int
}

// Next implements Iterator. The lazy Open closure each entry carries
// keeps the iterator's footprint at one (open file) max regardless of
// tree size.
func (it *sliceIterator) Next() (Entry, bool, error) {
	if it.i >= len(it.entries) {
		return Entry{}, false, nil
	}
	e := it.entries[it.i]
	it.i++
	return e, true, nil
}

// Snapshot walks volPath and returns an Iterator over every entry in
// lexical path order. The walk includes the root entry itself as
// path "." — receivers can ignore it (everything else has a non-"."
// path). volPath must be a directory on disk; a missing path returns
// the OS error verbatim so the caller can distinguish "no such volume"
// from a partial-walk failure.
func (r *Root) Snapshot(volPath string) (Iterator, error) {
	info, err := os.Lstat(volPath)
	if err != nil {
		return nil, fmt.Errorf("Snapshot stat %s: %w", volPath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("Snapshot %s: not a directory", volPath)
	}
	entries, err := walkSorted(volPath)
	if err != nil {
		return nil, err
	}
	return &sliceIterator{entries: entries}, nil
}

// walkSorted does the lexical-order walk Snapshot returns and Manifest
// hashes. Sort is on the relative path to keep two identical trees
// hashing to the same manifest regardless of the operating system's
// readdir ordering.
func walkSorted(root string) ([]Entry, error) {
	// Two passes: collect everything (sorted), then materialise Entry
	// values with lazy Open closures. Collecting first is necessary
	// because filepath.WalkDir's lexical guarantee is per-directory,
	// not globally lexical — a deeper file can sort before a sibling
	// directory's content otherwise.
	type raw struct {
		rel  string
		mode fs.FileMode
		abs  string
	}
	var rs []raw
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		rs = append(rs, raw{rel: rel, mode: info.Mode(), abs: path})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].rel < rs[j].rel })

	out := make([]Entry, 0, len(rs))
	for _, r := range rs {
		r := r // capture for closure
		e := Entry{Path: r.rel, Mode: r.mode}
		// Lazy open: directories and non-regular files don't have
		// content readers. Symlinks intentionally don't expose Open
		// either — the receiver reconstructs them from Mode + target,
		// which lands in the ship-volume payload (PR2). For now,
		// non-regular files contribute (path, mode, "") to the hash.
		if r.mode.IsRegular() {
			path := r.abs
			e.Open = func() (io.ReadCloser, error) { return os.Open(path) }
		}
		out = append(out, e)
	}
	return out, nil
}

// Manifest walks volPath and returns map[relpath]hex(sha256) where the
// hash is over the triple (path, mode, content). Identical trees hash
// identically; a chmod, rename, or content edit on any file changes
// the corresponding entry's hash. The result map is independent of
// host filesystem ordering — Manifest hashes against the path+mode+
// content triple, not against any cross-file ordering — so two hosts
// that received the same payload via ShipVolume (PR2) produce
// bit-identical maps even when their underlying ext4 returns dirents
// in a different order.
//
// Symlinks contribute (path, mode, target-bytes); directories
// contribute (path, mode, ""). Hidden / dot files are included.
func (r *Root) Manifest(volPath string) (map[string]string, error) {
	entries, err := walkSorted(volPath)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.Path == "." {
			// Skip the root entry — its presence is implied by the
			// existence of the volume itself; including it would force
			// every receiver to know the host OS's mode for the
			// containing directory.
			continue
		}
		h := sha256.New()
		// Stable triple: path bytes, then ":", then mode (octal +
		// type bits via fs.FileMode.String), then ":", then content.
		// The two ":" separators keep "ab"+"c" distinct from "a"+"bc".
		h.Write([]byte(e.Path))
		h.Write([]byte{':'})
		h.Write([]byte(e.Mode.String()))
		h.Write([]byte{':'})
		switch {
		case e.Mode.IsRegular():
			rc, err := e.Open()
			if err != nil {
				return nil, fmt.Errorf("Manifest open %s: %w", e.Path, err)
			}
			if _, err := io.Copy(h, rc); err != nil {
				_ = rc.Close()
				return nil, fmt.Errorf("Manifest read %s: %w", e.Path, err)
			}
			if err := rc.Close(); err != nil {
				return nil, fmt.Errorf("Manifest close %s: %w", e.Path, err)
			}
		case e.Mode&fs.ModeSymlink != 0:
			target, err := os.Readlink(filepath.Join(volPath, filepath.FromSlash(e.Path)))
			if err != nil {
				return nil, fmt.Errorf("Manifest readlink %s: %w", e.Path, err)
			}
			h.Write([]byte(target))
		default:
			// Directory / device / named pipe — content portion is
			// empty by design. Mode already encoded the type.
		}
		out[e.Path] = hex.EncodeToString(h.Sum(nil))
	}
	return out, nil
}

// Swap atomically promotes a fully-populated staging directory to the
// live position. Sequence:
//
//  1. rename live → <live>.trash-<unix>
//  2. rename staging → live
//  3. os.RemoveAll(trash)  [best-effort; logged on failure by caller]
//
// Interruption between (1) and (2) leaves no live directory and a
// trash directory carrying the prior contents — recoverable on the
// next Swap call: if `live` is missing AND staging is absent AND a
// `<live>.trash-*` exists, the trash is renamed back to live before
// the call returns ErrNeedsRecovery. The recovery path keeps the
// volume serviceable across an unclean crash and never silently
// promotes a partial staging tree.
//
// stagingDir must be the exact path StagingPath returned for the
// same triple + moveID; passing a different path is an error so the
// receiver can't accidentally promote some unrelated directory.
func (r *Root) Swap(deployment, service, volumeName, stagingDir string) error {
	if err := validateTriple(deployment, service, volumeName); err != nil {
		return err
	}
	live := filepath.Join(r.base, deployment, service, volumeName)
	parent := filepath.Dir(live)

	stagingExists := false
	if _, err := os.Lstat(stagingDir); err == nil {
		stagingExists = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("Swap stat staging %s: %w", stagingDir, err)
	}

	liveExists := false
	if _, err := os.Lstat(live); err == nil {
		liveExists = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("Swap stat live %s: %w", live, err)
	}

	if !stagingExists {
		// No staging directory to promote. The only sane action is
		// recovery: if there's a leftover trash from a previous
		// interrupted swap AND live is missing, rename it back.
		if liveExists {
			return fmt.Errorf("Swap %s: staging %s missing and live present (no-op)", live, stagingDir)
		}
		trash, err := findLatestTrash(parent, volumeName)
		if err != nil {
			return err
		}
		if trash == "" {
			return fmt.Errorf("Swap %s: staging %s missing and no trash to recover", live, stagingDir)
		}
		if err := os.Rename(trash, live); err != nil {
			return fmt.Errorf("Swap recover %s -> %s: %w", trash, live, err)
		}
		return ErrNeedsRecovery
	}

	// Happy path: live (if present) → trash, then staging → live.
	if liveExists {
		trash := fmt.Sprintf("%s.trash-%d", live, time.Now().UnixNano())
		if err := os.Rename(live, trash); err != nil {
			return fmt.Errorf("Swap live -> trash %s: %w", trash, err)
		}
		if err := os.Rename(stagingDir, live); err != nil {
			// Best effort: try to put live back so the caller is
			// not left without a volume on disk. If the second
			// rename also fails, surface both errors so the
			// operator sees the full picture.
			if rbErr := os.Rename(trash, live); rbErr != nil {
				return fmt.Errorf("Swap staging -> live %s: %w (rollback also failed: %v; trash at %s)", live, err, rbErr, trash)
			}
			return fmt.Errorf("Swap staging -> live %s: %w (rolled back; trash discarded)", live, err)
		}
		// Best-effort trash purge. A failure here leaves an
		// orphaned `<live>.trash-*` that the next Swap's recovery
		// path will ignore (recovery only triggers when live is
		// missing). The caller surfaces the warning; we don't
		// fail Swap on it because the live directory is already
		// correct.
		if err := os.RemoveAll(trash); err != nil {
			return fmt.Errorf("Swap trash purge %s: %w", trash, err)
		}
		return nil
	}

	// No live yet (first-time receive or post-crash). Just promote.
	if err := os.Rename(stagingDir, live); err != nil {
		return fmt.Errorf("Swap staging -> live %s: %w", live, err)
	}
	return nil
}

// ErrNeedsRecovery is returned by Swap when it recovered a prior
// interrupted swap by restoring `<live>.trash-*` back to `live`. The
// caller should treat the recovery as a no-op for the current move —
// the staging directory wasn't promoted; the volume's content is the
// pre-crash live tree. The migration FSM (PR3) translates this into
// "this move must be retried from SHIP" rather than ACTIVATE.
var ErrNeedsRecovery = errors.New("volumes: swap recovered prior interrupted swap; retry the move")

// findLatestTrash returns the most-recent `<volumeName>.trash-<unix>`
// path under parent, or "" when none exists. The unix-nano suffix
// makes ordering trivial; ties (within the same nanosecond) are
// broken by lexical order on the suffix which still yields a
// deterministic pick.
func findLatestTrash(parent, volumeName string) (string, error) {
	dirents, err := os.ReadDir(parent)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("findLatestTrash readdir %s: %w", parent, err)
	}
	prefix := volumeName + ".trash-"
	var (
		best   string
		bestTS int64
	)
	for _, d := range dirents {
		name := d.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(name, prefix)
		ts, err := strconv.ParseInt(suffix, 10, 64)
		if err != nil {
			continue
		}
		if best == "" || ts > bestTS {
			best = name
			bestTS = ts
		}
	}
	if best == "" {
		return "", nil
	}
	return filepath.Join(parent, best), nil
}
