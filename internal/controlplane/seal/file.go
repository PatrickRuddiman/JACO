package seal

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type keyFile struct {
	Version int               `json:"version"`
	Active  string            `json:"active"`
	Keys    map[string][]byte `json:"keys"`
}

func (f *keyFile) clear() {
	for _, key := range f.Keys {
		clear(key)
	}
}

// LoadFile reads an owner-only regular key file outside the application data
// directory. File-based provisioning is supported on POSIX filesystems; a
// filesystem that cannot enforce these permission bits is rejected.
func LoadFile(path, dataDir string, otherDataDirs ...string) (*Keyring, error) {
	path = keyFilePath(path)
	for _, dir := range otherDataDirs {
		if err := validateKeyPath(path, dir); err != nil {
			return nil, err
		}
	}
	cfg, err := loadKeyFile(path, dataDir)
	if err != nil {
		return nil, err
	}
	defer cfg.clear()
	return New(cfg.Active, cfg.Keys)
}

func keyFilePath(path string) string {
	if path == "" {
		path = os.Getenv("JACO_STATE_KEY_FILE")
	}
	if path == "" {
		if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" {
			path = filepath.Join(dir, "jaco-state-keys")
		}
	}
	return path
}

func loadKeyFile(path, dataDir string) (_ *keyFile, resultErr error) {
	path = keyFilePath(path)
	if err := validateKeyPath(path, dataDir); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("seal: stat key file: %w", err)
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("seal: key file must be a regular non-symlink file with owner-only permissions (0400 or 0600)")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("seal: open key file: %w", err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("seal: stat opened key file: %w", err)
	}
	if !os.SameFile(before, opened) || opened.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("seal: key file changed while opening")
	}
	if opened.Size() > 64*1024 {
		return nil, errors.New("seal: key file exceeds 64 KiB")
	}
	var cfg keyFile
	defer func() {
		if resultErr != nil {
			cfg.clear()
		}
	}()
	dec := json.NewDecoder(io.LimitReader(f, 64*1024+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, errors.New("seal: invalid key file JSON (expected version, active and base64-encoded keys)")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("seal: unexpected trailing key file data")
	}
	if cfg.Version != 1 {
		return nil, errors.New("seal: unsupported key file version")
	}
	if _, err := New(cfg.Active, cfg.Keys); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func validateKeyPath(path, dataDir string) error {
	if !filepath.IsAbs(path) || dataDir == "" {
		return errors.New("seal: absolute external key file and data directory are required; set state_key_file, JACO_STATE_KEY_FILE or the jaco-state-keys service credential")
	}
	resolvedKey, err := ResolvePath(path)
	if err != nil {
		return fmt.Errorf("seal: resolve key file: %w", err)
	}
	resolvedData, err := ResolvePath(dataDir)
	if err != nil {
		return fmt.Errorf("seal: resolve data directory: %w", err)
	}
	rel, err := filepath.Rel(resolvedData, resolvedKey)
	if err != nil {
		return fmt.Errorf("seal: compare key and data paths: %w", err)
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("seal: wrapping key file must be outside the data directory")
	}
	return nil
}

// ResolvePath resolves existing symlink ancestors even for a not-yet-created
// data directory; a lexical prefix check alone misses aliased data paths.
func ResolvePath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolvedParent, err := ResolvePath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(path)), nil
}
