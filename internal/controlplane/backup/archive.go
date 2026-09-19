package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"

	"google.golang.org/protobuf/proto"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

type ReencryptOptions struct {
	Reader               io.Reader
	Writer               io.Writer
	SourceKeys           *seal.Keyring
	TargetKeys           *seal.Keyring
	AllowLegacyPlaintext bool
}

// Reencrypt converts an explicitly accepted v1 backup, or rotates a v2 one,
// into a new authenticated archive. Neither the source nor keyrings change.
func Reencrypt(opts ReencryptOptions) error {
	if opts.Reader == nil || opts.Writer == nil || opts.SourceKeys == nil || opts.TargetKeys == nil {
		return errors.New("backup: reader, writer and external source/destination keyrings are required")
	}
	meta, plain, err := readArchive(opts.Reader, opts.SourceKeys, opts.AllowLegacyPlaintext)
	if err != nil {
		return err
	}
	defer clear(plain)
	meta.SchemaVersion = schemaVersion
	return writeArchive(opts.Writer, meta, plain, opts.TargetKeys)
}

func validateSnapshot(meta Meta, plain []byte) error {
	if meta.ClusterID == "" || meta.SnapshotIndex == 0 || meta.SnapshotTerm == 0 ||
		meta.SnapshotIndex == math.MaxUint64 || meta.SnapshotTerm == math.MaxUint64 {
		return errors.New("backup: missing or invalid cluster/replay metadata")
	}
	var snapshot pb.FSMSnapshot
	if err := proto.Unmarshal(plain, &snapshot); err != nil || len(snapshot.ProtoReflect().GetUnknown()) != 0 {
		return errors.New("backup: invalid or unsupported snapshot payload")
	}
	if snapshot.GetCluster().GetClusterId() != meta.ClusterID {
		return errors.New("backup: metadata cluster does not match the snapshot")
	}
	return nil
}

func readArchive(reader io.Reader, keys *seal.Keyring, allowLegacy bool) (Meta, []byte, error) {
	var meta Meta
	if reader == nil || keys == nil {
		return meta, nil, errors.New("backup: reader and external keyring are required")
	}
	metaBytes, data, err := untar(reader)
	if err != nil {
		return meta, nil, err
	}
	defer clear(data)
	decoder := json.NewDecoder(bytes.NewReader(metaBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&meta); err != nil {
		return Meta{}, nil, fmt.Errorf("parse meta.json: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Meta{}, nil, errors.New("backup: trailing metadata")
	}
	var plain []byte
	switch meta.SchemaVersion {
	case schemaVersion:
		inner, err := keys.Open("backup-snapshot:"+string(metaBytes), data)
		if err != nil {
			return Meta{}, nil, fmt.Errorf("backup authentication: %w", err)
		}
		plain, err = keys.Open(seal.SnapshotPurpose, inner)
		if err != nil {
			return Meta{}, nil, err
		}
	case 1:
		if !allowLegacy {
			return Meta{}, nil, errors.New("backup: plaintext schema 1 requires explicit offline re-encryption before restore")
		}
		plain = bytes.Clone(data)
	default:
		return Meta{}, nil, fmt.Errorf("backup: unsupported schema_version %d", meta.SchemaVersion)
	}
	if err := validateSnapshot(meta, plain); err != nil {
		clear(plain)
		return Meta{}, nil, err
	}
	return meta, plain, nil
}

func writeArchive(writer io.Writer, meta Meta, plain []byte, keys *seal.Keyring) error {
	if writer == nil || keys == nil {
		return errors.New("backup: writer and external keyring are required")
	}
	if err := validateSnapshot(meta, plain); err != nil {
		return err
	}
	meta.SchemaVersion = schemaVersion
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	inner, err := keys.Seal(seal.SnapshotPurpose, plain)
	if err != nil {
		return err
	}
	data, err := keys.Seal("backup-snapshot:"+string(metaBytes), inner)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(writer)
	tw := tar.NewWriter(gz)
	err = writeTarFile(tw, "meta.json", metaBytes)
	if err == nil {
		err = writeTarFile(tw, "snapshot.bin", data)
	}
	return errors.Join(err, tw.Close(), gz.Close())
}
