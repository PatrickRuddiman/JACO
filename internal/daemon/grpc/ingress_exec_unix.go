//go:build unix

package grpc

import (
	"fmt"
	"os"
	"syscall"
)

func checkIngressAdminDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat caddy admin directory: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0o700 || !ok || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("caddy admin directory %s must be a real directory owned by uid %d with mode 0700", path, os.Geteuid())
	}
	return nil
}
