// Package backup implements Export (write a raft snapshot + metadata tarball)
// and Import (untar + seed a fresh raft data dir) so a JACO cluster can be
// reproduced from an encrypted tar.gz and an independently held keyring.
//
// Export is called against a live raft node. Import operates on disk: it
// preps a data dir that jacod can boot via hashicorp/raft's normal
// snapshot-restore path.
package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	hraft "github.com/hashicorp/raft"
	boltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/logging"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// schemaVersion identifies the on-disk backup format. Bump on any breaking
// change to meta.json shape or snapshot encoding.
const schemaVersion = 2

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
	plain, err := opts.Raft.StateKeys().Open(seal.SnapshotPurpose, snapshotBytes)
	if err != nil {
		return fmt.Errorf("authenticate exported snapshot: %w", err)
	}
	defer clear(plain)
	if err := writeArchive(opts.Writer, meta, plain, opts.Raft.StateKeys()); err != nil {
		return err
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
	Keys        *seal.Keyring
	// Logger logs restore start/finish at INFO (with bytes-written). nil →
	// discard.
	Logger *slog.Logger
}

// Import untars opts.Reader, validates meta.json's schema_version, primes a
// fresh encrypted Raft store at ${DataDir}/raft/ with RecoverCluster, and
// writes ${DataDir}/restore.txt as local restoration metadata.
func Import(opts ImportOptions) (resultErr error) {
	if opts.DataDir == "" {
		return fmt.Errorf("DataDir is required")
	}
	if opts.Reader == nil {
		return fmt.Errorf("Reader is required")
	}
	if opts.LocalID == "" {
		return fmt.Errorf("LocalID is required")
	}
	if opts.Keys == nil {
		return fmt.Errorf("external state encryption Keys are required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = logging.Discard()
	}
	logger.Info("backup restore started", "data_dir", opts.DataDir, logging.KeyNode, opts.LocalID)

	meta, plain, err := readArchive(opts.Reader, opts.Keys, false)
	if err != nil {
		return err
	}
	defer clear(plain)
	if !majorVersionsCompatible(meta.JacoVersion, opts.JacoVersion) {
		return fmt.Errorf("backup jaco_version %q is incompatible with running %q", meta.JacoVersion, opts.JacoVersion)
	}

	snapshotBytes, err := opts.Keys.Seal(seal.SnapshotPurpose, plain)
	if err != nil {
		return err
	}
	if err := seal.BeginCopy(opts.DataDir, false); err != nil {
		return err
	}
	raftDir := filepath.Join(opts.DataDir, "raft")
	logStore, err := boltdb.NewBoltStore(filepath.Join(raftDir, "log.db"))
	if err != nil {
		return fmt.Errorf("bolt store: %w", err)
	}
	defer func() {
		if logStore != nil {
			resultErr = errors.Join(resultErr, logStore.Close())
		}
	}()
	logs, err := seal.WrapLogs(logStore, opts.Keys)
	if err != nil {
		return err
	}
	// Recover in memory, then publish one disk snapshot. Creating both an
	// input and a recovered file snapshot can collide on Raft's millisecond ID.
	snapStore, err := seal.WrapSnapshots(hraft.NewInmemSnapshotStore(), opts.Keys)
	if err != nil {
		return err
	}

	// In-memory transport is sufficient for recovery; jacod binds the network
	// transport only when it starts against the completed state.
	_, transport := hraft.NewInmemTransport(hraft.ServerAddress(opts.LocalID))
	defer transport.Close()

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
	// reloads the snapshot through FSM.Restore when jacod starts.
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	recoveryFSM, err := seal.WrapFSM(fsm.New(st, brokers), opts.Keys)
	if err != nil {
		return err
	}

	raftCfg := hraft.DefaultConfig()
	raftCfg.LocalID = hraft.ServerID(opts.LocalID)
	raftCfg.LogOutput = io.Discard

	if err := hraft.RecoverCluster(raftCfg, recoveryFSM, logs, logStore, snapStore, transport, configuration); err != nil {
		return fmt.Errorf("RecoverCluster: %w", err)
	}

	recovered, err := snapStore.List()
	if err != nil {
		return err
	}
	if len(recovered) != 1 {
		return errors.New("recovery did not produce exactly one snapshot")
	}
	recoveredMeta, reader, err := snapStore.Open(recovered[0].ID)
	if err != nil {
		return err
	}
	defer reader.Close()
	rawDisk, err := hraft.NewFileSnapshotStore(raftDir, 3, io.Discard)
	if err != nil {
		return err
	}
	diskSnapshots, err := seal.WrapSnapshots(rawDisk, opts.Keys)
	if err != nil {
		return err
	}
	diskSink, err := diskSnapshots.Create(recoveredMeta.Version, recoveredMeta.Index, recoveredMeta.Term,
		recoveredMeta.Configuration, recoveredMeta.ConfigurationIndex, transport)
	if err != nil {
		return err
	}
	if _, err := io.Copy(diskSink, reader); err != nil {
		return errors.Join(err, diskSink.Cancel())
	}
	if err := diskSink.Close(); err != nil {
		return err
	}
	if err := logStore.SetUint64([]byte("CurrentTerm"), recoveredMeta.Term); err != nil {
		return err
	}
	if err := opts.Keys.MarkFreshStore(logStore); err != nil {
		return err
	}
	if err := seal.ValidateSnapshots(raftDir, opts.Keys); err != nil {
		return err
	}
	markerPath := filepath.Join(opts.DataDir, "restore.txt")
	markerContents := fmt.Sprintf("cluster_id=%s\nsnapshot_index=%d\ntaken_at=%s\nimported_at=%s\n",
		meta.ClusterID, meta.SnapshotIndex, meta.TakenAt, time.Now().UTC().Format(time.RFC3339))
	if err := seal.WriteFreshFile(markerPath, []byte(markerContents)); err != nil {
		return fmt.Errorf("write restore marker: %w", err)
	}
	err = logStore.Close()
	logStore = nil
	if err != nil {
		return err
	}
	if err := seal.FinishCopy(opts.DataDir); err != nil {
		return err
	}
	logger.Info("backup restore finished",
		"cluster_id", meta.ClusterID, "snapshot_index", meta.SnapshotIndex, "bytes", len(snapshotBytes))
	return nil
}

// ReadMeta untars opts.Reader and returns just the meta.json content,
// without committing anything to disk. Useful for the CLI's `--dry-run` style
// inspection (not exercised in v1 but cheap to expose).
func ReadMeta(r io.Reader, keys *seal.Keyring) (Meta, error) {
	meta, plain, err := readArchive(r, keys, false)
	clear(plain)
	return meta, err
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
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return nil, nil, fmt.Errorf("backup entry %s must be a regular file", hdr.Name)
		}
		if (hdr.Name == "meta.json" && metaBytes != nil) || (hdr.Name == "snapshot.bin" && snapshotBytes != nil) {
			return nil, nil, fmt.Errorf("duplicate backup entry: %s", hdr.Name)
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
	trailing, err := io.ReadAll(gz)
	if err != nil {
		return nil, nil, fmt.Errorf("backup gzip checksum/trailer: %w", err)
	}
	if len(bytes.Trim(trailing, "\x00")) != 0 {
		return nil, nil, errors.New("backup has unexpected trailing data")
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
