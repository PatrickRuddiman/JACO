package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"google.golang.org/grpc"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

type fakeBackupClient struct {
	pb.ClusterClient
	recv func() (*pb.BackupChunk, error)
}

func (c *fakeBackupClient) Backup(context.Context, *pb.BackupRequest, ...grpc.CallOption) (pb.Cluster_BackupClient, error) {
	return &fakeBackupStream{recv: c.recv}, nil
}

type fakeBackupStream struct {
	grpc.ClientStream
	recv func() (*pb.BackupChunk, error)
}

func (s *fakeBackupStream) Recv() (*pb.BackupChunk, error) {
	return s.recv()
}

func TestBackupCmdAllowsOutputFilename(t *testing.T) {
	cmd := backupCmd()
	if err := cmd.Flags().Set("output", "cluster.tar.gz"); err != nil {
		t.Fatal(err)
	}
	if err := rootCmd.PersistentPreRunE(cmd, nil); err != nil {
		t.Fatalf("backup filename rejected: %v", err)
	}
}

func TestBackupKeepsPreviousFileOnStreamError(t *testing.T) {
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "cluster.tar.gz")
	if err := os.WriteFile(outputPath, []byte("previous good backup"), 0o644); err != nil {
		t.Fatal(err)
	}
	streamErr := errors.New("transfer interrupted")
	calls := 0
	client := &fakeBackupClient{recv: func() (*pb.BackupChunk, error) {
		calls++
		if calls == 1 {
			return &pb.BackupChunk{Data: []byte("partial replacement")}, nil
		}
		return nil, streamErr
	}}
	var out bytes.Buffer
	if err := runBackup(context.Background(), client, outputPath, &out); !errors.Is(err, streamErr) {
		t.Fatalf("error = %v, want transfer interruption", err)
	}
	if out.Len() != 0 {
		t.Fatalf("reported success after failed transfer: %q", out.String())
	}
	data, err := os.ReadFile(outputPath)
	if err != nil || string(data) != "previous good backup" {
		t.Fatalf("previous backup changed: %q, %v", data, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "cluster.tar.gz" {
		t.Fatalf("unexpected files after failed transfer: %v, %v", entries, err)
	}
}

func TestBackupRejectsSymlinkDestination(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("do not change"), 0o644); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(dir, "cluster.tar.gz")
	if err := os.Symlink(target, outputPath); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("Windows symlink privileges unavailable: %v", err)
		}
		t.Fatal(err)
	}
	received := false
	client := &fakeBackupClient{recv: func() (*pb.BackupChunk, error) {
		received = true
		return nil, io.EOF
	}}
	var out bytes.Buffer
	if err := runBackup(context.Background(), client, outputPath, &out); err == nil {
		t.Fatal("backup accepted a symlink destination")
	}
	if received || out.Len() != 0 {
		t.Fatalf("invalid destination consumed stream or reported success: received=%v, output=%q", received, out.String())
	}
	if got, err := os.Readlink(outputPath); err != nil || got != target {
		t.Fatalf("destination link changed: %q, %v", got, err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "do not change" {
		t.Fatalf("symlink target changed: %q, %v", data, err)
	}
}

func TestBackupDoesNotPublishCanceledTransfer(t *testing.T) {
	for _, when := range []string{"before receiving", "between chunks", "at EOF"} {
		t.Run(when, func(t *testing.T) {
			dir := t.TempDir()
			outputPath := filepath.Join(dir, "cluster.tar.gz")
			if err := os.WriteFile(outputPath, []byte("previous good backup"), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if when == "before receiving" {
				cancel()
			}
			calls := 0
			client := &fakeBackupClient{recv: func() (*pb.BackupChunk, error) {
				calls++
				if calls == 1 {
					if when == "between chunks" {
						cancel()
					}
					return &pb.BackupChunk{Data: []byte("replacement")}, nil
				}
				cancel()
				return nil, io.EOF
			}}
			var out bytes.Buffer
			if err := runBackup(ctx, client, outputPath, &out); !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want cancellation", err)
			}
			if out.Len() != 0 {
				t.Fatalf("reported success after cancellation: %q", out.String())
			}
			data, err := os.ReadFile(outputPath)
			if err != nil || string(data) != "previous good backup" {
				t.Fatalf("previous backup changed: %q, %v", data, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("incomplete backup remains: %v, %v", entries, err)
			}
		})
	}
}

func TestBackupRejectsDestinationSymlinkCreatedDuringTransfer(t *testing.T) {
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "cluster.tar.gz")
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("do not change"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := writeBackupFile(context.Background(), outputPath, func(f *os.File) error {
		if _, err := f.WriteString("snapshot"); err != nil {
			return err
		}
		if err := os.Symlink(target, outputPath); err != nil {
			if runtime.GOOS == "windows" {
				t.Skipf("Windows symlink privileges unavailable: %v", err)
			}
			t.Fatal(err)
		}
		return nil
	})
	if err == nil {
		t.Fatal("backup accepted a destination changed to a symlink during transfer")
	}
	if got, err := os.Readlink(outputPath); err != nil || got != target {
		t.Fatalf("destination link changed: %q, %v", got, err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "do not change" {
		t.Fatalf("symlink target changed: %q, %v", data, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 2 {
		t.Fatalf("incomplete backup remains: %v, %v", entries, err)
	}
}

func TestBackupRequiresFilePath(t *testing.T) {
	for _, suffix := range []string{"", string(os.PathSeparator), string(os.PathSeparator) + ".", string(os.PathSeparator) + ".."} {
		t.Run("directory"+suffix, func(t *testing.T) {
			dir := t.TempDir()
			received := false
			client := &fakeBackupClient{recv: func() (*pb.BackupChunk, error) {
				received = true
				return nil, io.EOF
			}}
			var out bytes.Buffer
			if err := runBackup(context.Background(), client, dir+suffix, &out); err == nil {
				t.Fatal("backup accepted a directory path")
			}
			if received || out.Len() != 0 {
				t.Fatalf("invalid file path consumed stream or reported success: received=%v, output=%q", received, out.String())
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Fatalf("backup artifacts in directory: %v, %v", entries, err)
			}
		})
	}
}

func TestBackupPublishesCompleteStream(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			dir := t.TempDir()
			outputPath := filepath.Join(dir, "cluster.tar.gz")
			if existing {
				if err := os.WriteFile(outputPath, []byte("previous"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			chunks := []string{"snap", "", "shot"}
			client := &fakeBackupClient{recv: func() (*pb.BackupChunk, error) {
				data, err := os.ReadFile(outputPath)
				if existing {
					if err != nil || string(data) != "previous" {
						t.Fatalf("previous backup changed before EOF: %q, %v", data, err)
					}
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("incomplete backup published before EOF: %q, %v", data, err)
				}
				if out.Len() != 0 {
					t.Fatalf("reported success before EOF: %q", out.String())
				}
				if len(chunks) == 0 {
					return nil, io.EOF
				}
				data = []byte(chunks[0])
				chunks = chunks[1:]
				return &pb.BackupChunk{Data: data}, nil
			}}
			if err := runBackup(context.Background(), client, outputPath, &out); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(outputPath); err != nil || string(data) != "snapshot" {
				t.Fatalf("backup = %q, %v; want snapshot", data, err)
			}
			if got, want := out.String(), fmt.Sprintf("Wrote 8 bytes to %s\n", outputPath); got != want {
				t.Fatalf("output = %q, want %q", got, want)
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
				t.Fatalf("unexpected backup artifacts: %v, %v", entries, err)
			}
		})
	}
}

func TestBackupFileFailurePreservesOutput(t *testing.T) {
	writeErr := errors.New("disk write failed")
	for _, tc := range []struct {
		name   string
		fail   func(*os.File) error
		want   error
		prefix string
	}{
		{"write", func(*os.File) error { return writeErr }, writeErr, "disk write failed"},
		{"sync", func(f *os.File) error { return f.Close() }, os.ErrClosed, "sync output:"},
		{"publish", func(f *os.File) error { return os.Remove(f.Name()) }, os.ErrNotExist, "publish output:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "publish" && runtime.GOOS == "windows" {
				t.Skip("unlinking an open staging file requires Unix semantics")
			}
			for _, existing := range []bool{false, true} {
				t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
					dir := t.TempDir()
					outputPath := filepath.Join(dir, "cluster.tar.gz")
					if existing {
						if err := os.WriteFile(outputPath, []byte("previous"), 0o644); err != nil {
							t.Fatal(err)
						}
					}
					err := writeBackupFile(context.Background(), outputPath, func(f *os.File) error {
						if _, err := f.WriteString("partial replacement"); err != nil {
							return err
						}
						return tc.fail(f)
					})
					if !errors.Is(err, tc.want) || !strings.HasPrefix(err.Error(), tc.prefix) {
						t.Fatalf("error = %v, want %s (%v)", err, tc.prefix, tc.want)
					}
					data, err := os.ReadFile(outputPath)
					wantFiles := 0
					if existing {
						wantFiles = 1
						if err != nil || string(data) != "previous" {
							t.Fatalf("previous backup changed: %q, %v", data, err)
						}
					} else if !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("failed backup published: %q, %v", data, err)
					}
					if entries, err := os.ReadDir(dir); err != nil || len(entries) != wantFiles {
						t.Fatalf("incomplete backup remains: %v, %v", entries, err)
					}
				})
			}
		})
	}
}
