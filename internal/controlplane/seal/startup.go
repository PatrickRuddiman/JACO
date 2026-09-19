package seal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	boltdb "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"
)

func CheckCopyComplete(dataDir string) error {
	_, err := os.Lstat(filepath.Join(dataDir, IncompleteMarker))
	if err == nil {
		return errors.New("seal: incomplete state copy; retain the original and recover into a new destination")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// OpenReadOnlyStore retains a shared file lock against daemon writers. Check
// bucket structure before using raft-boltdb, whose getters assume it exists.
func OpenReadOnlyStore(path string) (*boltdb.BoltStore, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("seal: database must be a non-symlink regular file")
	}
	opts := &bbolt.Options{ReadOnly: true, Timeout: time.Second}
	db, err := bbolt.Open(path, 0o600, opts)
	if err != nil {
		return nil, fmt.Errorf("seal: lock source database read-only (stop the daemon first): %w", err)
	}
	err = db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket([]byte("logs")) == nil || tx.Bucket([]byte("conf")) == nil {
			return errors.New("seal: database is missing Raft buckets")
		}
		return tx.ForEach(func(name []byte, _ *bbolt.Bucket) error {
			if string(name) != "logs" && string(name) != "conf" {
				return errors.New("seal: unrecognized database bucket")
			}
			return nil
		})
	})
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	store, err := boltdb.New(boltdb.Options{Path: path, BoltOptions: opts})
	closeErr := db.Close()
	if err != nil {
		return nil, errors.Join(err, closeErr)
	}
	if closeErr != nil {
		return nil, errors.Join(closeErr, store.Close())
	}
	return store, nil
}

// ValidateRaftDataDir runs before listeners are opened. A missing log.db in
// a populated Raft directory is not proof that historical data was removed.
func ValidateRaftDataDir(dataDir string, keys *Keyring) error {
	if dataDir == "" || keys == nil {
		return errors.New("seal: data directory and state keyring are required")
	}
	if err := CheckCopyComplete(dataDir); err != nil {
		return err
	}
	dir := filepath.Join(dataDir, "raft")
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("seal: Raft directory must not be a symlink or regular file")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	foundLog := false
	for _, entry := range entries {
		switch entry.Name() {
		case "log.db":
			foundLog = true
		case "snapshots":
		default:
			return fmt.Errorf("seal: unrecognized Raft artifact %q; inspect before migration", entry.Name())
		}
	}
	if !foundLog {
		return errors.New("seal: historical Raft directory has no database; explicit recovery/migration is required")
	}
	store, err := OpenReadOnlyStore(filepath.Join(dir, "log.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	if err := keys.CheckStore(store); err != nil {
		return err
	}
	if _, err := WrapLogs(store, keys); err != nil {
		return err
	}
	return ValidateSnapshots(dir, keys)
}
