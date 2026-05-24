package backup_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/backup"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/bootstrap"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
)

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// pollDeadline clamps a requested timeout to t.Deadline() when the latter is
// closer, so a stress-loaded test process can't poll past the harness deadline
// before observing the state it's waiting on.
func pollDeadline(t *testing.T, timeout time.Duration) time.Time {
	t.Helper()
	d := time.Now().Add(timeout)
	if td, ok := t.Deadline(); ok && td.Before(d) {
		return td
	}
	return d
}

func waitForLeader(t *testing.T, n *raftnode.Node, timeout time.Duration) {
	t.Helper()
	// LeaderCh fires on leadership transitions; combine it with State() so we
	// also catch the case where the node is already leader by the time we
	// subscribe (LeaderCh is transition-only and may have already fired).
	deadline := pollDeadline(t, timeout)
	leaderCh := n.Raft.LeaderCh()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if n.Raft.State() == hraft.Leader {
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("never became leader; state=%v", n.Raft.State())
		}
		select {
		case <-leaderCh:
		case <-ticker.C:
		case <-time.After(remaining):
		}
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := pollDeadline(t, timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if cond() {
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("waitFor(%s) timed out", what)
		}
		select {
		case <-ticker.C:
		case <-time.After(remaining):
		}
	}
}

func TestExportImport_RoundTripPreservesBootstrapToken(t *testing.T) {
	// 1. Bootstrap a single-node cluster.
	aDir := t.TempDir()
	aRaftAddr := freePort(t)
	_, err := bootstrap.Run(bootstrap.Options{
		DataDir:  aDir,
		Name:     "node-a",
		BindAddr: aRaftAddr,
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// 2. Re-open A's raft + wait for replay so state.Cluster.Get() returns the
	//    cluster_id.
	brokersA := watch.NewRegistry()
	stA := state.New(brokersA)
	fsmA := fsm.New(stA, brokersA)
	rA, err := raftnode.New(raftnode.Config{
		DataDir: aDir, BindAddr: aRaftAddr, LocalID: "node-a",
		Bootstrap: false, FSM: fsmA, LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatalf("reopen A: %v", err)
	}
	waitForLeader(t, rA, 10*time.Second)
	// Wait on the bootstrap token rather than ClusterMeta: both are written by
	// the same ClusterInit log entry, but Cluster.Set + Tokens.Apply each
	// release their own mutex independently, so a Cluster!=nil observation can
	// occur before Tokens.Apply runs. The token write is the last state mutation
	// in applyPayload(ClusterInit), so its visibility is the deterministic
	// "ClusterInit fully applied" signal.
	waitFor(t, 5*time.Second, "ClusterInit applied", func() bool {
		if stA.Cluster.Get() == nil {
			return false
		}
		_, ok := stA.Tokens.Get("bootstrap")
		return ok
	})

	clusterID := stA.Cluster.Get().GetClusterId()
	if clusterID == "" {
		t.Fatalf("empty cluster_id post-replay")
	}

	// 3. Export to a buffer.
	var buf bytes.Buffer
	if err := backup.Export(backup.ExportOptions{
		Raft:        rA,
		ClusterID:   clusterID,
		JacoVersion: "0.0.1-dev",
		Identity:    "operator",
		Writer:      &buf,
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	tarBytes := buf.Bytes()
	if len(tarBytes) == 0 {
		t.Fatalf("Export produced 0 bytes")
	}

	// Shutdown A so its raft addr can be reused by the restored node (we use
	// a different addr below, but we want a clean shutdown for the bolt
	// store).
	_ = rA.Shutdown()

	// 4. Verify the tarball contents are exactly { meta.json, snapshot.bin }.
	entries := listTarEntries(t, bytes.NewReader(tarBytes))
	sort.Strings(entries)
	if want := []string{"meta.json", "snapshot.bin"}; !equalStrings(entries, want) {
		t.Errorf("tar entries = %v, want %v", entries, want)
	}

	// 5. Inspect meta via the public reader.
	meta, err := backup.ReadMeta(bytes.NewReader(tarBytes))
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if meta.ClusterID != clusterID {
		t.Errorf("meta.cluster_id = %q, want %q", meta.ClusterID, clusterID)
	}
	if meta.SnapshotIndex == 0 {
		t.Errorf("meta.snapshot_index = 0; expected non-zero post-bootstrap")
	}
	if meta.SchemaVersion != 1 {
		t.Errorf("meta.schema_version = %d, want 1", meta.SchemaVersion)
	}

	// 6. Import into a fresh data dir.
	bDir := t.TempDir()
	if err := backup.Import(backup.ImportOptions{
		DataDir:     bDir,
		Reader:      bytes.NewReader(tarBytes),
		LocalID:     "node-a",
		JacoVersion: "0.0.1-dev",
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}

	// The restore.txt marker must be present so the daemon can emit
	// RESTORE_COMPLETED on first boot (task 17).
	markerPath := filepath.Join(bDir, "restore.txt")
	if _, err := os.Stat(markerPath); err != nil {
		t.Errorf("restore.txt marker missing: %v", err)
	}

	// 7. Re-open raft on the restored dir; FSM.Restore rehydrates state.
	brokersB := watch.NewRegistry()
	stB := state.New(brokersB)
	fsmB := fsm.New(stB, brokersB)
	bRaftAddr := freePort(t)
	rB, err := raftnode.New(raftnode.Config{
		DataDir: bDir, BindAddr: bRaftAddr, LocalID: "node-a",
		Bootstrap: false, FSM: fsmB, LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatalf("reopen restored: %v", err)
	}
	t.Cleanup(func() { _ = rB.Shutdown() })
	waitForLeader(t, rB, 10*time.Second)

	// 8. AC: the bootstrap token is present in the restored state.
	waitFor(t, 5*time.Second, "bootstrap token present after restore", func() bool {
		_, ok := stB.Tokens.Get("bootstrap")
		return ok
	})

	// Cluster meta should also have survived.
	if got := stB.Cluster.Get().GetClusterId(); got != clusterID {
		t.Errorf("restored cluster_id = %q, want %q", got, clusterID)
	}
}

func TestImport_RefusesExistingState(t *testing.T) {
	dir := t.TempDir()
	raftDir := filepath.Join(dir, "raft")
	if err := os.MkdirAll(raftDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(raftDir, "log.db"), []byte("pretend"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := backup.Import(backup.ImportOptions{
		DataDir: dir,
		Reader:  fakeTarball(t),
		LocalID: "node-x",
	})
	if err == nil {
		t.Fatalf("expected error when raft state pre-exists")
	}
}

func TestImport_RejectsSchemaMismatch(t *testing.T) {
	// Construct a tarball with schema_version = 99.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	meta := []byte(`{"schema_version":99,"cluster_id":"x","snapshot_index":1,"snapshot_term":1,"jaco_version":"0.0.1-dev","taken_at":"","leader_at_snapshot":""}`)
	if err := tw.WriteHeader(&tar.Header{Name: "meta.json", Size: int64(len(meta))}); err != nil {
		t.Fatal(err)
	}
	tw.Write(meta)
	dummy := []byte("snap")
	if err := tw.WriteHeader(&tar.Header{Name: "snapshot.bin", Size: int64(len(dummy))}); err != nil {
		t.Fatal(err)
	}
	tw.Write(dummy)
	tw.Close()
	gz.Close()

	err := backup.Import(backup.ImportOptions{
		DataDir: t.TempDir(),
		Reader:  &buf,
		LocalID: "node-x",
	})
	if err == nil {
		t.Fatalf("expected schema mismatch error")
	}
}

func TestImport_RejectsMissingEntries(t *testing.T) {
	// Tarball with only meta.json.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	meta := []byte(`{"schema_version":1}`)
	if err := tw.WriteHeader(&tar.Header{Name: "meta.json", Size: int64(len(meta))}); err != nil {
		t.Fatal(err)
	}
	tw.Write(meta)
	tw.Close()
	gz.Close()

	err := backup.Import(backup.ImportOptions{
		DataDir: t.TempDir(),
		Reader:  &buf,
		LocalID: "node-x",
	})
	if err == nil {
		t.Fatalf("expected missing-snapshot error")
	}
}

// --- helpers -----------------------------------------------------------------

func listTarEntries(t *testing.T, r io.Reader) []string {
	t.Helper()
	gz, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		names = append(names, hdr.Name)
	}
	return names
}

func equalStrings(a, b []string) bool {
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

func fakeTarball(t *testing.T) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	meta := []byte(`{"schema_version":1,"cluster_id":"x","snapshot_index":1,"snapshot_term":1,"jaco_version":"","taken_at":"","leader_at_snapshot":""}`)
	if err := tw.WriteHeader(&tar.Header{Name: "meta.json", Size: int64(len(meta))}); err != nil {
		t.Fatal(err)
	}
	tw.Write(meta)
	snap := []byte{}
	if err := tw.WriteHeader(&tar.Header{Name: "snapshot.bin", Size: int64(len(snap))}); err != nil {
		t.Fatal(err)
	}
	tw.Write(snap)
	tw.Close()
	gz.Close()
	return &buf
}
