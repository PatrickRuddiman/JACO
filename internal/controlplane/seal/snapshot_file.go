package seal

import (
	"encoding/json"
	"errors"

	hraft "github.com/hashicorp/raft"
)

// The outer file envelope binds replay metadata. The inner envelope remains
// portable between replica-local snapshot IDs and stays sealed on the wire.
func snapshotFilePurpose(meta *hraft.SnapshotMeta) (string, error) {
	if meta == nil || meta.Version != hraft.SnapshotVersionMax || meta.ID == "" {
		return "", errors.New("seal: unsupported snapshot metadata")
	}
	bound := struct {
		Version            hraft.SnapshotVersion
		ID                 string
		Index              uint64
		Term               uint64
		Configuration      hraft.Configuration
		ConfigurationIndex uint64
	}{meta.Version, meta.ID, meta.Index, meta.Term, meta.Configuration, meta.ConfigurationIndex}
	data, err := json.Marshal(bound)
	if err != nil {
		return "", err
	}
	return "raft-snapshot-file:" + string(data), nil
}

func (r *Keyring) SealSnapshotFile(meta *hraft.SnapshotMeta, snapshot []byte) ([]byte, error) {
	purpose, err := snapshotFilePurpose(meta)
	if err != nil {
		return nil, err
	}
	plain, err := r.Open(SnapshotPurpose, snapshot)
	clear(plain)
	if err != nil {
		return nil, err
	}
	return r.Seal(purpose, snapshot)
}

func (r *Keyring) OpenSnapshotFile(meta *hraft.SnapshotMeta, data []byte) ([]byte, error) {
	purpose, err := snapshotFilePurpose(meta)
	if err != nil {
		return nil, err
	}
	snapshot, err := r.Open(purpose, data)
	if err != nil {
		return nil, err
	}
	plain, err := r.Open(SnapshotPurpose, snapshot)
	clear(plain)
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}
