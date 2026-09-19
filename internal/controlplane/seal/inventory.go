package seal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc64"
	"io"
	"os"
	"path/filepath"
	"strings"

	hraft "github.com/hashicorp/raft"
)

const IncompleteMarker = "state-copy.incomplete"

type SnapshotFile struct {
	Directory string
	Meta      hraft.SnapshotMeta
	Data      []byte
}

type diskSnapshotMeta struct {
	hraft.SnapshotMeta
	CRC []byte
}

// WalkSnapshots reads every physical snapshot, including complete .tmp
// remnants and entries beyond Raft's retention-limited List. Source files
// are never opened for writing. Incomplete/unknown artifacts require review.
func WalkSnapshots(raftDir string, visit func(SnapshotFile) error) error {
	root := filepath.Join(raftDir, "snapshots")
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("seal: snapshot root is not a non-symlink directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			return fmt.Errorf("seal: unexpected snapshot artifact %q", entry.Name())
		}
		dir := filepath.Join(root, entry.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		if len(files) != 2 {
			return fmt.Errorf("seal: incomplete or unrecognized snapshot %q; retain and inspect it", entry.Name())
		}
		for _, file := range files {
			info, err := file.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || (file.Name() != "meta.json" && file.Name() != "state.bin") {
				return fmt.Errorf("seal: unexpected artifact in snapshot %q", entry.Name())
			}
		}
		rawMeta, err := os.ReadFile(filepath.Join(dir, "meta.json"))
		if err != nil {
			return err
		}
		var meta diskSnapshotMeta
		decoder := json.NewDecoder(bytes.NewReader(rawMeta))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&meta); err != nil {
			return fmt.Errorf("seal: snapshot %q metadata: %w", entry.Name(), err)
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return fmt.Errorf("seal: snapshot %q has trailing metadata", entry.Name())
		}
		if meta.Version != hraft.SnapshotVersionMax || meta.ID != strings.TrimSuffix(entry.Name(), ".tmp") {
			return fmt.Errorf("seal: snapshot %q has unsupported or mismatched metadata", entry.Name())
		}
		data, err := os.ReadFile(filepath.Join(dir, "state.bin"))
		if err != nil {
			return err
		}
		hash := crc64.New(crc64.MakeTable(crc64.ECMA))
		hash.Write(data)
		if meta.Size != int64(len(data)) || !bytes.Equal(meta.CRC, hash.Sum(nil)) {
			return fmt.Errorf("seal: snapshot %q has a size/checksum mismatch", entry.Name())
		}
		if err := visit(SnapshotFile{Directory: entry.Name(), Meta: meta.SnapshotMeta, Data: data}); err != nil {
			return fmt.Errorf("seal: snapshot %q: %w", entry.Name(), err)
		}
	}
	return nil
}

func ValidateSnapshots(raftDir string, keys *Keyring) error {
	return WalkSnapshots(raftDir, func(snapshot SnapshotFile) error {
		_, err := keys.OpenSnapshotFile(&snapshot.Meta, snapshot.Data)
		return err
	})
}

// WriteSnapshotFile only writes a new directory in a fresh-copy destination.
// It preserves the source ID and metadata without timestamp collisions or
// pruning older snapshots during migration.
func WriteSnapshotFile(raftDir string, snapshot SnapshotFile) error {
	if snapshot.Directory == "." || snapshot.Directory == ".." ||
		filepath.Base(snapshot.Directory) != snapshot.Directory ||
		snapshot.Meta.ID != strings.TrimSuffix(snapshot.Directory, ".tmp") {
		return errors.New("seal: invalid snapshot directory")
	}
	root := filepath.Join(raftDir, "snapshots")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	dir := filepath.Join(root, snapshot.Directory)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	meta := diskSnapshotMeta{SnapshotMeta: snapshot.Meta}
	meta.Size = int64(len(snapshot.Data))
	hash := crc64.New(crc64.MakeTable(crc64.ECMA))
	hash.Write(snapshot.Data)
	meta.CRC = hash.Sum(nil)
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := WriteFreshFile(filepath.Join(dir, "state.bin"), snapshot.Data); err != nil {
		return err
	}
	return WriteFreshFile(filepath.Join(dir, "meta.json"), data)
}

func WriteFreshFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	return errors.Join(writeErr, syncErr, f.Close())
}
