package seal

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type KeyFileOptions struct {
	Path       string
	DataDir    string
	KeyID      string
	RetainFile string
}

// GenerateFile never prints key material or overwrites a recovery key file.
// Rotation creates a new version and optionally retains every historical key.
func GenerateFile(opts KeyFileOptions) (resultErr error) {
	if err := validateKeyPath(opts.Path, opts.DataDir); err != nil {
		return err
	}
	if !keyID.MatchString(opts.KeyID) {
		return errors.New("seal: a valid new key ID is required")
	}
	cfg := &keyFile{Version: 1, Keys: make(map[string][]byte)}
	if opts.RetainFile != "" {
		var err error
		cfg, err = loadKeyFile(opts.RetainFile, opts.DataDir)
		if err != nil {
			return err
		}
	}
	defer cfg.clear()
	if _, exists := cfg.Keys[opts.KeyID]; exists {
		return errors.New("seal: rotation must use a new key ID; refusing to replace a recovery key")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	cfg.Keys[opts.KeyID] = key
	cfg.Active = opts.KeyID
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	defer clear(data)
	if len(data) > 64*1024 {
		return errors.New("seal: generated key file would exceed 64 KiB")
	}
	file, err := os.OpenFile(opts.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("seal: create new external key file: %w", err)
	}
	defer func() {
		if file != nil {
			resultErr = errors.Join(resultErr, file.Close())
		}
		if resultErr != nil {
			resultErr = errors.Join(resultErr, os.Remove(opts.Path))
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("seal: filesystem cannot enforce owner-only key-file permissions")
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	err = file.Close()
	file = nil
	if err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(opts.Path))
}
