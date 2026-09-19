//go:build unix

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func TestBackupOwnerOnlyPermissions(t *testing.T) {
	if os.Getenv("JACO_BACKUP_UMASK_TEST") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestBackupOwnerOnlyPermissions$")
		cmd.Env = append(os.Environ(), "JACO_BACKUP_UMASK_TEST=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("permissive-umask subprocess: %v\n%s", err, out)
		}
		return
	}
	previous := syscall.Umask(0)
	defer syscall.Umask(previous)

	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			outputPath := filepath.Join(dir, "cluster.tar.gz")
			linkedPath := filepath.Join(dir, "previous-link")
			if existing {
				if err := os.WriteFile(outputPath, []byte("previous"), 0o666); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(outputPath, linkedPath); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			client := &fakeBackupClient{recv: func() (*pb.BackupChunk, error) {
				entries, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if existing && (entry.Name() == "cluster.tar.gz" || entry.Name() == "previous-link") {
						continue
					}
					info, err := entry.Info()
					if err != nil {
						t.Fatal(err)
					}
					if got := info.Mode().Perm(); got != 0o600 {
						t.Fatalf("in-progress backup mode = %04o, want 0600", got)
					}
				}
				calls++
				if calls == 1 {
					return &pb.BackupChunk{Data: []byte("snapshot")}, nil
				}
				return nil, io.EOF
			}}
			var out bytes.Buffer
			if err := runBackup(context.Background(), client, outputPath, &out); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0o600 {
				t.Fatalf("finished backup mode = %04o, want 0600", got)
			}
			if data, err := os.ReadFile(outputPath); err != nil || string(data) != "snapshot" {
				t.Fatalf("backup = %q, %v; want snapshot", data, err)
			}
			if got, want := out.String(), fmt.Sprintf("Wrote 8 bytes to %s\n", outputPath); got != want {
				t.Fatalf("output = %q, want %q", got, want)
			}
			if existing {
				linkedInfo, err := os.Stat(linkedPath)
				if err != nil {
					t.Fatal(err)
				}
				if os.SameFile(info, linkedInfo) || linkedInfo.Mode().Perm() != 0o666 {
					t.Fatal("replacement changed the old inode or its permissions")
				}
				if data, err := os.ReadFile(linkedPath); err != nil || string(data) != "previous" {
					t.Fatalf("hard-linked backup changed: %q, %v", data, err)
				}
			}
		})
	}
}

func TestBackupDirectoryReplacementCannotRedirectPublication(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "output")
	moved := filepath.Join(base, "moved")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(dir, "cluster.tar.gz")
	if err := os.WriteFile(outputPath, []byte("previous good backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	var decoy string
	err := writeBackupFile(context.Background(), outputPath, func(f *os.File) error {
		if _, err := f.WriteString("snapshot"); err != nil {
			return err
		}
		if err := os.Rename(dir, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		decoy = filepath.Join(dir, filepath.Base(f.Name()))
		if err := os.WriteFile(decoy, []byte("decoy"), 0o644); err != nil {
			t.Fatal(err)
		}
		return nil
	})
	if err == nil {
		t.Fatal("backup reported success after its directory was replaced")
	}
	if _, err := os.Lstat(outputPath); !os.IsNotExist(err) {
		t.Fatalf("published into replacement directory: %v", err)
	}
	if data, err := os.ReadFile(decoy); err != nil || string(data) != "decoy" {
		t.Fatalf("cleanup touched replacement directory: %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(moved, "cluster.tar.gz")); err != nil || string(data) != "previous good backup" {
		t.Fatalf("previous backup changed: %q, %v", data, err)
	}
	if entries, err := os.ReadDir(moved); err != nil || len(entries) != 1 {
		t.Fatalf("incomplete backup left in original directory: %v, %v", entries, err)
	}
}

func TestBackupRejectsUnsafeDirectory(t *testing.T) {
	for _, mode := range []os.FileMode{0o770, 0o777} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatal(err)
			}
			received := false
			client := &fakeBackupClient{recv: func() (*pb.BackupChunk, error) {
				received = true
				return nil, io.EOF
			}}
			var out bytes.Buffer
			err := runBackup(context.Background(), client, filepath.Join(dir, "cluster.tar.gz"), &out)
			if err == nil {
				t.Fatal("backup accepted a directory writable by other users")
			}
			if received || out.Len() != 0 {
				t.Fatalf("unsafe directory consumed stream or reported success: received=%v, output=%q", received, out.String())
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Fatalf("backup artifacts in unsafe directory: %v, %v", entries, err)
			}
		})
	}
}

func TestBackupRejectsDirectoryOwnedByAnotherUser(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Change the stat snapshot only; no real directory ownership is changed.
	owner := uint32(1)
	if os.Geteuid() == 1 {
		owner = 2
	}
	info.Sys().(*syscall.Stat_t).Uid = owner
	if err := checkBackupDirectory(info); err == nil {
		t.Fatal("backup accepted a directory controlled by another user")
	}
}

type backupContextServer struct {
	pb.UnimplementedClusterServer
}

func (*backupContextServer) Backup(*pb.BackupRequest, pb.Cluster_BackupServer) error {
	return status.Error(codes.FailedPrecondition, "command context was not canceled")
}

func TestBackupCommandUsesContext(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "jaco.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterClusterServer(server, &backupContextServer{})
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		if err := <-done; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("backup server: %v", err)
		}
	})

	outputPath := filepath.Join(dir, "cluster.tar.gz")
	cmd := backupCmd()
	cmd.SetArgs([]string{"--socket", socket, "--output", outputPath})
	cmd.SetErr(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = cmd.ExecuteContext(ctx)
	if !errors.Is(err, context.Canceled) && status.Code(err) != codes.Canceled {
		t.Fatalf("command error = %v, want cancellation", err)
	}
	if _, err := os.Stat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled command created output: %v", err)
	}
}

func TestBackupRejectsSpecialFiles(t *testing.T) {
	for _, kind := range []string{"directory", "dangling symlink", "FIFO", "socket"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			outputPath := filepath.Join(dir, "cluster.tar.gz")
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(outputPath, 0o700)
			case "dangling symlink":
				err = os.Symlink(filepath.Join(dir, "missing"), outputPath)
			case "FIFO":
				err = syscall.Mkfifo(outputPath, 0o600)
			case "socket":
				var listener net.Listener
				listener, err = net.Listen("unix", outputPath)
				if err == nil {
					t.Cleanup(func() { _ = listener.Close() })
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			received := false
			client := &fakeBackupClient{recv: func() (*pb.BackupChunk, error) {
				received = true
				return nil, io.EOF
			}}
			var out bytes.Buffer
			if err := runBackup(context.Background(), client, outputPath, &out); err == nil {
				t.Fatal("backup accepted a special file destination")
			}
			if received || out.Len() != 0 {
				t.Fatalf("special file consumed stream or reported success: received=%v, output=%q", received, out.String())
			}
			after, err := os.Lstat(outputPath)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("special file replaced: %v", err)
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
				t.Fatalf("incomplete backup remains: %v, %v", entries, err)
			}
		})
	}
}

func TestBackupAllowsStickySharedDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(dir, "cluster.tar.gz")
	if err := writeBackupFile(context.Background(), outputPath, func(f *os.File) error {
		_, err := f.WriteString("snapshot")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(outputPath); err != nil || string(data) != "snapshot" {
		t.Fatalf("backup = %q, %v; want snapshot", data, err)
	}
}

func TestBackupWriteFailurePreservesPreviousFile(t *testing.T) {
	if os.Getenv("JACO_BACKUP_WRITE_FAILURE_TEST") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestBackupWriteFailurePreservesPreviousFile$")
		cmd.Env = append(os.Environ(), "JACO_BACKUP_WRITE_FAILURE_TEST=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("file-size-limit subprocess: %v\n%s", err, out)
		}
		return
	}
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "cluster.tar.gz")
	if err := os.WriteFile(outputPath, []byte("previous good backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	t.Cleanup(func() {
		_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit)
		signal.Reset(syscall.SIGXFSZ)
	})
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 4, Max: limit.Max}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := &fakeBackupClient{recv: func() (*pb.BackupChunk, error) {
		calls++
		if calls == 1 {
			return &pb.BackupChunk{Data: []byte("snapshot")}, nil
		}
		return nil, io.EOF
	}}
	var out bytes.Buffer
	err := runBackup(context.Background(), client, outputPath, &out)
	if !errors.Is(err, syscall.EFBIG) {
		t.Fatalf("write error = %v, want EFBIG", err)
	}
	if out.Len() != 0 {
		t.Fatalf("reported success after a partial write: %q", out.String())
	}
	if data, err := os.ReadFile(outputPath); err != nil || string(data) != "previous good backup" {
		t.Fatalf("previous backup changed: %q, %v", data, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Fatalf("incomplete backup remains: %v, %v", entries, err)
	}
}
