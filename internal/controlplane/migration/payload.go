package migration

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	hraft "github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func openPayload(keys *seal.Keyring, purpose string, data []byte, legacy bool) ([]byte, error) {
	plain, err := keys.Open(purpose, data)
	if errors.Is(err, seal.ErrUnsealed) && legacy {
		return bytes.Clone(data), nil
	}
	return plain, err
}

func convertLog(opts Options, legacy bool, record *hraft.Log) (*hraft.Log, error) {
	opened, err := opts.SourceKeys.OpenLogFile(record)
	if err != nil {
		if !legacy || !errors.Is(err, seal.ErrUnsealed) {
			return nil, err
		}
		copied := *record
		opened = &copied
	}
	if opened.Type > hraft.LogConfiguration {
		return nil, errors.New("migration: unsupported Raft log type")
	}
	if opened.Type == hraft.LogCommand {
		plain, err := openPayload(opts.SourceKeys, seal.CommandPurpose, opened.Data, legacy)
		if err != nil {
			return nil, err
		}
		defer clear(plain)
		var command pb.Command
		if err := proto.Unmarshal(plain, &command); err != nil || command.GetPayload() == nil {
			return nil, errors.New("migration: invalid application command")
		}
		opened.Data, err = opts.TargetKeys.Seal(seal.CommandPurpose, plain)
		if err != nil {
			return nil, err
		}
	}
	return opts.TargetKeys.SealLogFile(opened)
}

func convertSnapshot(opts Options, legacy bool, snapshot seal.SnapshotFile) (seal.SnapshotFile, error) {
	var plain []byte
	inner, err := opts.SourceKeys.OpenSnapshotFile(&snapshot.Meta, snapshot.Data)
	if errors.Is(err, seal.ErrUnsealed) && legacy {
		plain = bytes.Clone(snapshot.Data)
	} else if err != nil {
		return snapshot, err
	} else {
		plain, err = opts.SourceKeys.Open(seal.SnapshotPurpose, inner)
		if err != nil {
			return snapshot, err
		}
	}
	defer clear(plain)
	var decoded pb.FSMSnapshot
	if err := proto.Unmarshal(plain, &decoded); err != nil || len(decoded.ProtoReflect().GetUnknown()) != 0 {
		return snapshot, errors.New("migration: invalid or unsupported snapshot payload")
	}
	inner, err = opts.TargetKeys.Seal(seal.SnapshotPurpose, plain)
	if err != nil {
		return snapshot, err
	}
	snapshot.Data, err = opts.TargetKeys.SealSnapshotFile(&snapshot.Meta, inner)
	return snapshot, err
}

func convertFile(source string, file artifact, opts Options, legacy bool) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(source, file.path))
	if err != nil {
		return nil, err
	}
	if !file.cache {
		return data, nil
	}
	defer clear(data)
	purpose, err := seal.CachePurpose(filepath.Base(file.path))
	if err != nil {
		return nil, err
	}
	plain, err := openPayload(opts.SourceKeys, purpose, data, legacy)
	if err != nil {
		return nil, fmt.Errorf("migration: cache %s: %w", file.path, err)
	}
	defer clear(plain)
	return opts.TargetKeys.Seal(purpose, plain)
}
