// Package migration creates new encrypted state files without rewriting or
// deleting source history, including logically deleted Bolt free pages.
package migration

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	hraft "github.com/hashicorp/raft"
	boltdb "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
)

type Options struct {
	SourceDir            string
	TargetDir            string
	SourceKeys           *seal.Keyring
	TargetKeys           *seal.Keyring
	AllowLegacyPlaintext bool
}

type Report struct {
	Logs          int `json:"logs"`
	Snapshots     int `json:"snapshots"`
	CacheFiles    int `json:"cache_files"`
	IdentityFiles int `json:"identity_files"`
}

func Copy(opts Options) (Report, error) {
	var report Report
	if opts.SourceKeys == nil || opts.TargetKeys == nil {
		return report, errors.New("migration: source and destination keyrings are required")
	}
	if !filepath.IsAbs(opts.SourceDir) || !filepath.IsAbs(opts.TargetDir) {
		return report, errors.New("migration: absolute source and destination paths are required")
	}
	source, err := seal.ResolvePath(opts.SourceDir)
	if err != nil {
		return report, err
	}
	target, err := seal.ResolvePath(opts.TargetDir)
	if err != nil {
		return report, err
	}
	for _, pair := range [][2]string{{source, target}, {target, source}} {
		relative, err := filepath.Rel(pair[0], pair[1])
		if err != nil {
			return report, err
		}
		if relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return report, errors.New("migration: source and destination must not overlap")
		}
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return report, errors.New("migration: destination must not exist")
	}
	if err := seal.CheckCopyComplete(source); err != nil {
		return report, err
	}
	files, err := inventory(source)
	if err != nil {
		return report, err
	}
	sourcePath := filepath.Join(source, "raft", "log.db")
	sourceStore, err := seal.OpenReadOnlyStore(sourcePath)
	if err != nil {
		return report, err
	}
	defer sourceStore.Close()
	legacy := false
	if err := opts.SourceKeys.CheckStore(sourceStore); err != nil {
		if !opts.AllowLegacyPlaintext || !errors.Is(err, seal.ErrLegacyStore) {
			return report, err
		}
		legacy = true
	}
	indices, stable, err := databaseInventory(sourcePath)
	if err != nil {
		return report, err
	}
	// Authenticate/validate the complete source before creating any output.
	for _, index := range indices {
		var record hraft.Log
		if err := sourceStore.GetLog(index, &record); err != nil {
			return report, err
		}
		if record.Index != index {
			return report, errors.New("migration: source log index mismatch")
		}
		if _, err := convertLog(opts, legacy, &record); err != nil {
			return report, fmt.Errorf("migration: validate log %d: %w", index, err)
		}
	}
	if err := seal.WalkSnapshots(filepath.Join(source, "raft"), func(snapshot seal.SnapshotFile) error {
		_, err := convertSnapshot(opts, legacy, snapshot)
		return err
	}); err != nil {
		return report, err
	}
	for _, file := range files {
		if _, err := convertFile(source, file, opts, legacy); err != nil {
			return report, err
		}
	}
	if err := seal.BeginCopy(target, true); err != nil {
		return report, err
	}
	targetStore, err := boltdb.NewBoltStore(filepath.Join(target, "raft", "log.db"))
	if err != nil {
		return report, err
	}
	closed := false
	defer func() {
		if !closed {
			targetStore.Close()
		}
	}()
	for key, value := range stable {
		if err := targetStore.Set([]byte(key), value); err != nil {
			return report, err
		}
	}
	for _, index := range indices {
		var record hraft.Log
		if err := sourceStore.GetLog(index, &record); err != nil {
			return report, err
		}
		converted, err := convertLog(opts, legacy, &record)
		if err != nil {
			return report, err
		}
		if err := targetStore.StoreLog(converted); err != nil {
			return report, err
		}
		report.Logs++
	}
	if err := seal.WalkSnapshots(filepath.Join(source, "raft"), func(snapshot seal.SnapshotFile) error {
		converted, err := convertSnapshot(opts, legacy, snapshot)
		if err != nil {
			return err
		}
		if err := seal.WriteSnapshotFile(filepath.Join(target, "raft"), converted); err != nil {
			return err
		}
		report.Snapshots++
		return nil
	}); err != nil {
		return report, err
	}
	for _, file := range files {
		data, err := convertFile(source, file, opts, legacy)
		if err != nil {
			return report, err
		}
		path := filepath.Join(target, file.path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			clear(data)
			return report, err
		}
		err = seal.WriteFreshFile(path, data)
		clear(data)
		if err != nil {
			return report, err
		}
		if file.cache {
			report.CacheFiles++
		} else {
			report.IdentityFiles++
		}
	}
	if err := opts.TargetKeys.MarkFreshStore(targetStore); err != nil {
		return report, err
	}
	if _, err := seal.WrapLogs(targetStore, opts.TargetKeys); err != nil {
		return report, err
	}
	if err := errors.Join(
		seal.ValidateSnapshots(filepath.Join(target, "raft"), opts.TargetKeys),
		seal.ValidateCache(filepath.Join(target, "ingress", "cache"), opts.TargetKeys),
	); err != nil {
		return report, err
	}
	err = targetStore.Close()
	closed = true
	if err != nil {
		return report, err
	}
	if err := seal.FinishCopy(target); err != nil {
		return report, err
	}
	return report, nil
}

func databaseInventory(path string) (indices []uint64, stable map[string][]byte, resultErr error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		return nil, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, db.Close()) }()
	stable = make(map[string][]byte)
	err = db.View(func(tx *bbolt.Tx) error {
		if err := tx.Bucket([]byte("logs")).ForEach(func(key, value []byte) error {
			if len(key) != 8 || value == nil || binary.BigEndian.Uint64(key) == 0 {
				return errors.New("migration: malformed source log bucket")
			}
			indices = append(indices, binary.BigEndian.Uint64(key))
			return nil
		}); err != nil {
			return err
		}
		return tx.Bucket([]byte("conf")).ForEach(func(key, value []byte) error {
			switch string(key) {
			case seal.FormatMarkerKey:
				return nil
			case "CurrentTerm", "LastVoteTerm":
				if len(value) != 8 {
					return errors.New("migration: malformed stable Raft term")
				}
			case "LastVoteCand":
				if value == nil {
					return errors.New("migration: malformed stable Raft vote")
				}
			default:
				return fmt.Errorf("migration: unrecognized stable-store key %q", key)
			}
			stable[string(key)] = bytes.Clone(value)
			return nil
		})
	})
	return indices, stable, err
}
