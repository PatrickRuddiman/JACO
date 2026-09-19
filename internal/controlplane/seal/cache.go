package seal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var cacheName = regexp.MustCompile(`^([0-9a-f]{64})(?:\.tmp(?:-[0-9]+)?)?$`)

// CachePurpose also recognizes atomic-write remnants for offline migration.
func CachePurpose(name string) (string, error) {
	match := cacheName.FindStringSubmatch(name)
	if match == nil {
		return "", fmt.Errorf("seal: unrecognized cache artifact %q; inspect before migration", name)
	}
	return CachePurposePrefix + match[1], nil
}

// ValidateCache inventories physical artifacts, not just currently used keys.
// Legacy and damaged files are retained; startup never silently deletes them.
func ValidateCache(dir string, keys *Keyring) error {
	if keys == nil {
		return errors.New("seal: cache keyring is required")
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("seal: inventory cache: %w", err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("seal: cache artifact %q is not a regular file", entry.Name())
		}
		purpose, err := CachePurpose(entry.Name())
		if err != nil {
			return err
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
		plain, err := keys.Open(purpose, data)
		clear(plain)
		if err != nil {
			return fmt.Errorf("seal: authenticate cache %q: %w", entry.Name(), err)
		}
	}
	return nil
}
