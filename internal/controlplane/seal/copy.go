package seal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
)

// BeginCopy reserves an empty destination. The guard remains on any failure,
// so an interrupted copy cannot be mistaken for a complete Raft state.
func BeginCopy(dir string, requireNew bool) error {
	entries, err := os.ReadDir(dir)
	if err == nil && (requireNew || len(entries) != 0) {
		return errors.New("seal: destination must be fresh; refusing to overwrite existing artifacts")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := WriteFreshFile(filepath.Join(dir, IncompleteMarker), []byte("Retain the source. This destination is not yet complete.\n")); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(dir, "raft"), 0o700); err != nil {
		return err
	}
	return WriteFreshFile(filepath.Join(dir, "raft", "log.db"), nil)
}

// FinishCopy durably publishes completion only after every file and database
// has been closed. POSIX is the supported production key-file platform.
func FinishCopy(dir string) error {
	if runtime.GOOS != "windows" {
		var directories []string
		err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				directories = append(directories, path)
			}
			return err
		})
		if err != nil {
			return err
		}
		slices.Reverse(directories)
		for _, path := range directories {
			if err := syncDirectory(path); err != nil {
				return err
			}
		}
	}
	if err := os.Remove(filepath.Join(dir, IncompleteMarker)); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return syncDirectory(dir)
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
		return fmt.Errorf("seal: sync directory %s: %w", path, err)
	}
	return nil
}
