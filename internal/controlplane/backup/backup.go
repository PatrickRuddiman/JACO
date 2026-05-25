// Package backup implements Export (write a raft snapshot + metadata tarball)
// and Import (untar + seed a fresh raft data dir) so a JACO cluster can be
// reproduced on a new node from a single tar.gz file.
//
// Export is called against a live raft node. Import operates on disk: it
// preps a data dir that `jaco serve` can boot via hashicorp/raft's normal
// snapshot-restore path; restore.txt is the marker the daemon entry (task 17)
// reads to emit RESTORE_COMPLETED on first boot.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	hraft "github.com/hashicorp/raft"
	boltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/logging"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// schemaVersion identifies the on-disk backup format. Bump on any breaking
// change to meta.json shape or snapshot encoding.
const schemaVersion = 1

// Meta is the JSON written as meta.json inside the tarball.
type Meta struct {
	SchemaVersion    int    `json:"schema_version"`
	ClusterID        string `json:"cluster_id"`
	SnapshotIndex    uint64 `json:"snapshot_index"`
	SnapshotTerm     uint64 `json:"snapshot_term"`
	JacoVersion      string `json:"jaco_version"`
	TakenAt          string `json:"taken_at"`
	LeaderAtSnapshot string `json:"leader_at_snapshot"`
}

// ExportOptions are the inputs to Export.
type ExportOptions struct {
	Raft        *raftnode.Node
	ClusterID   string
	JacoVersion string
	Identity    string // for the BACKUP_TAKEN audit event
	Writer      io.Writer
	// Logger logs export start/finish at INFO. nil → discard.
	Logger *slog.Logger
}

// Export triggers a raft snapshot, then writes a tar.gz containing
// `meta.json` and `snapshot.bin` to opts.Writer. Raft-Applies an audit event
// of type BACKUP_TAKEN once the snapshot is materialized.
func Export(opts ExportOptions) error {
	if opts.Raft == nil {
		return fmt.Errorf("Raft is required")
	}
	if opts.ClusterID == "" {
		return fmt.Errorf("ClusterID is required")
	}
	if opts.Writer == nil {
		return fmt.Errorf("Writer is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = logging.Discard()
	}
	logger.Info("backup export started", "cluster_id", opts.ClusterID)

	snapF := opts.Raft.Raft.Snapshot()
	if err := snapF.Error(); err != nil {
		return fmt.Errorf("trigger snapshot: %w", err)
	}
	snapMeta, rc, err := snapF.Open()
	if err != nil {
		return fmt.Errorf("open snapshot: %w", err)
	}
	defer rc.Close()
	snapshotBytes, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("read snapshot: %w", err)
	}

	meta := Meta{
		SchemaVersion:    schemaVersion,
		ClusterID:        opts.ClusterID,
		SnapshotIndex:    snapMeta.Index,
		SnapshotTerm:     snapMeta.Term,
		JacoVersion:      opts.JacoVersion,
		TakenAt:          time.Now().UTC().Format(time.RFC3339),
		LeaderAtSnapshot: string(opts.Raft.Leader()),
	}
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}

	gz := gzip.NewWriter(opts.Writer)
	tw := tar.NewWriter(gz)
	if err := writeTarFile(tw, "meta.json", metaBytes); err != nil {
		_ = tw.Close()
		_ = gz.Close()
		return err
	}
	if err := writeTarFile(tw, "snapshot.bin", snapshotBytes); err != nil {
		_ = tw.Close()
		_ = gz.Close()
		return err
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("close gzip: %w", err)
	}

	// BACKUP_TAKEN audit event — best-effort. Failure to write the audit
	// shouldn't fail the backup itself.
	if opts.Raft.IsLeader() {
		audit := &pb.Command{
			Identity: opts.Identity,
			Ts:       timestamppb.Now(),
			Payload: &pb.Command_AuditAppend{AuditAppend: &pb.AuditAppend{
				Event: &pb.AuditEvent{
					Type: pb.AuditEventType_AUDIT_EVENT_TYPE_BACKUP_TAKEN,
					Payload: map[string]string{
						"snapshot_index": strconv.FormatUint(snapMeta.Index, 10),
					},
				},
			}},
		}
		if data, err := proto.Marshal(audit); err == nil {
			_, _ = opts.Raft.Apply(data, 5*time.Second)
		}
	}

	logger.Info("backup export finished",
		"cluster_id", opts.ClusterID, "snapshot_index", snapMeta.Index, "bytes", len(snapshotBytes))
	return nil
}

// ImportOptions are the inputs to Import.
type ImportOptions struct {
	DataDir     string
	Reader      io.Reader
	LocalID     string // hostname / raft local-id for the restoring node
	JacoVersion string // running binary version for compatibility check
	// Logger logs restore start/finish at INFO (with bytes-written). nil →
	// discard.
	Logger *slog.Logger
}

// Import untars opts.Reader, validates meta.json's schema_version, primes a
// fresh raft store at ${DataDir}/raft/ that will boot via RecoverCluster on
// next `jaco serve`, and writes ${DataDir}/restore.txt as a marker for the
// daemon to emit RESTORE_COMPLETED on first FSM apply.
func Import(opts ImportOptions) error {
	if opts.DataDir == "" {
		return fmt.Errorf("DataDir is required")
	}
	if opts.Reader == nil {
		return fmt.Errorf("Reader is required")
	}
	if opts.LocalID == "" {
		return fmt.Errorf("LocalID is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = logging.Discard()
	}
	logger.Info("backup restore started", "data_dir", opts.DataDir, logging.KeyNode, opts.LocalID)

	metaBytes, snapshotBytes, err := untar(opts.Reader)
	if err != nil {
		return err
	}

	var meta Meta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return fmt.Errorf("parse meta.json: %w", err)
	}
	if meta.SchemaVersion != schemaVersion {
		return fmt.Errorf("backup schema_version %d != running %d", meta.SchemaVersion, schemaVersion)
	}
	if !majorVersionsCompatible(meta.JacoVersion, opts.JacoVersion) {
		return fmt.Errorf("backup jaco_version %q is incompatible with running %q", meta.JacoVersion, opts.JacoVersion)
	}

	raftDir := filepath.Join(opts.DataDir, "raft")
	if err := os.MkdirAll(raftDir, 0o700); err != nil {
		return fmt.Errorf("mkdir raft dir: %w", err)
	}

	// Refuse to overwrite an existing log store — the operator must point at
	// a fresh data dir for restore.
	if _, err := os.Stat(filepath.Join(raftDir, "log.db")); err == nil {
		return fmt.Errorf("raft state already exists at %s; refusing to overwrite", raftDir)
	}

	logStore, err := boltdb.NewBoltStore(filepath.Join(raftDir, "log.db"))
	if err != nil {
		return fmt.Errorf("bolt store: %w", err)
	}
	defer logStore.Close()

	snapStore, err := hraft.NewFileSnapshotStore(raftDir, 3, io.Discard)
	if err != nil {
		return fmt.Errorf("file snapshot store: %w", err)
	}

	// In-memory transport is sufficient for RecoverCluster — no network needed
	// during recovery; the real TCP transport binds when `jaco serve` runs.
	_, transport := hraft.NewInmemTransport(hraft.ServerAddress(opts.LocalID))

	configuration := hraft.Configuration{
		Servers: []hraft.Server{{
			Suffrage: hraft.Voter,
			ID:       hraft.ServerID(opts.LocalID),
			Address:  hraft.ServerAddress(opts.LocalID),
		}},
	}

	sink, err := snapStore.Create(hraft.SnapshotVersionMax, meta.SnapshotIndex, meta.SnapshotTerm,
		configuration, meta.SnapshotIndex, transport)
	if err != nil {
		return fmt.Errorf("create snapshot sink: %w", err)
	}
	if _, err := sink.Write(snapshotBytes); err != nil {
		_ = sink.Cancel()
		return fmt.Errorf("write snapshot bytes: %w", err)
	}
	if err := sink.Close(); err != nil {
		return fmt.Errorf("close snapshot sink: %w", err)
	}

	// Spin up a throwaway FSM for RecoverCluster; the daemon's real FSM
	// reloads the snapshot through FSM.Restore when `jaco serve` starts.
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	recoveryFSM := fsm.New(st, brokers)

	raftCfg := hraft.DefaultConfig()
	raftCfg.LocalID = hraft.ServerID(opts.LocalID)
	raftCfg.LogOutput = io.Discard

	if err := hraft.RecoverCluster(raftCfg, recoveryFSM, logStore, logStore, snapStore, transport, configuration); err != nil {
		return fmt.Errorf("RecoverCluster: %w", err)
	}

	// Marker for the daemon's first-boot audit emission.
	markerPath := filepath.Join(opts.DataDir, "restore.txt")
	markerContents := fmt.Sprintf("cluster_id=%s\nsnapshot_index=%d\ntaken_at=%s\nimported_at=%s\n",
		meta.ClusterID, meta.SnapshotIndex, meta.TakenAt, time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(markerPath, []byte(markerContents), 0o600); err != nil {
		return fmt.Errorf("write restore marker: %w", err)
	}
	logger.Info("backup restore finished",
		"cluster_id", meta.ClusterID, "snapshot_index", meta.SnapshotIndex, "bytes", len(snapshotBytes))
	return nil
}

// ReadMeta untars opts.Reader and returns just the meta.json content,
// without committing anything to disk. Useful for the CLI's `--dry-run` style
// inspection (not exercised in v1 but cheap to expose).
func ReadMeta(r io.Reader) (Meta, error) {
	metaBytes, _, err := untar(r)
	if err != nil {
		return Meta{}, err
	}
	var meta Meta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return Meta{}, fmt.Errorf("parse meta.json: %w", err)
	}
	return meta, nil
}

func writeTarFile(tw *tar.Writer, name string, body []byte) error {
	if err := tw.WriteHeader(&tar.Header{
		Name:    name,
		Mode:    0o600,
		Size:    int64(len(body)),
		ModTime: time.Now(),
	}); err != nil {
		return fmt.Errorf("tar header %s: %w", name, err)
	}
	if _, err := tw.Write(body); err != nil {
		return fmt.Errorf("tar body %s: %w", name, err)
	}
	return nil
}

func untar(r io.Reader) (metaBytes, snapshotBytes []byte, err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, nil, fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("tar next: %w", err)
		}
		buf, err := io.ReadAll(tr)
		if err != nil {
			return nil, nil, fmt.Errorf("tar read %s: %w", hdr.Name, err)
		}
		switch hdr.Name {
		case "meta.json":
			metaBytes = buf
		case "snapshot.bin":
			snapshotBytes = buf
		default:
			return nil, nil, fmt.Errorf("unexpected tar entry: %s", hdr.Name)
		}
	}
	if metaBytes == nil {
		return nil, nil, fmt.Errorf("backup is missing meta.json")
	}
	if snapshotBytes == nil {
		return nil, nil, fmt.Errorf("backup is missing snapshot.bin")
	}
	return metaBytes, snapshotBytes, nil
}

// majorVersionsCompatible accepts versions of the shape "X.Y.Z[-...]" and
// returns true when X matches. Empty strings match anything (older backups or
// dev builds before -ldflags wiring lands).
func majorVersionsCompatible(a, b string) bool {
	if a == "" || b == "" {
		return true
	}
	return major(a) == major(b)
}

func major(v string) string {
	if i := strings.IndexByte(v, '.'); i >= 0 {
		return v[:i]
	}
	return v
}
