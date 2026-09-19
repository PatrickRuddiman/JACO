//go:build unix

package main

import (
	"errors"
	"os"
	"syscall"
)

func checkBackupDirectory(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot determine backup output directory owner")
	}
	if stat.Uid != 0 && int(stat.Uid) != os.Geteuid() {
		return errors.New("backup output directory must be owned by the current user or root")
	}
	if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
		return errors.New("backup output directory is writable by other users; choose a private directory (0700)")
	}
	return nil
}
