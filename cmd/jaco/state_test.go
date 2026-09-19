package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/backup"
	"github.com/PatrickRuddiman/jaco/internal/testutil"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func TestStateMigrationRequiresExplicitLegacyPolicy(t *testing.T) {
	command := stateCmd()
	migrate, _, err := command.Find([]string{"migrate"})
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"source-dir", "target-dir", "key-file", "new-key-file", "allow-legacy-plaintext"} {
		if migrate.Flags().Lookup(flag) == nil {
			t.Errorf("missing explicit migration option --%s", flag)
		}
	}
	legacy, err := migrate.Flags().GetBool("allow-legacy-plaintext")
	if err != nil || legacy {
		t.Fatal("plaintext migration must be an explicit opt-in")
	}
}

func TestStateKeygenRefusesCoLocatedKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unsafe.json")
	command := stateCmd()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"keygen", "--file", path, "--key-id", "one", "--data-dir", dir})
	if err := command.Execute(); err == nil {
		t.Fatal("keygen accepted a wrapping key inside application state")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("failed keygen wrote a key")
	}
	if restoreCmd().Flags().Lookup("key-file") == nil {
		t.Fatal("restore has no explicit external-key provisioning option")
	}
}

type failedStateReport struct{}

func (failedStateReport) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestStateBackupThroughRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("production key files require POSIX permissions")
	}
	oldState, _, err := rootCmd.Find([]string{"state"})
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr, oldFormat := rootCmd.OutOrStdout(), rootCmd.ErrOrStderr(), flagOutput
	rootCmd.RemoveCommand(oldState)
	fresh := stateCmd()
	rootCmd.AddCommand(fresh)
	t.Cleanup(func() {
		rootCmd.RemoveCommand(fresh)
		rootCmd.AddCommand(oldState)
		rootCmd.SetOut(oldOut)
		rootCmd.SetErr(oldErr)
		rootCmd.SetArgs(nil)
		flagOutput = oldFormat
	})
	source := filepath.Join(t.TempDir(), "legacy.tar.gz")
	var original bytes.Buffer
	gz := gzip.NewWriter(&original)
	tw := tar.NewWriter(gz)
	meta, err := json.Marshal(backup.Meta{SchemaVersion: 1, ClusterID: "test", SnapshotIndex: 2, SnapshotTerm: 1})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := proto.Marshal(&pb.FSMSnapshot{Cluster: &pb.ClusterMeta{
		ClusterId: "test", CaKey: []byte("synthetic-cli-private-key"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"meta.json": meta, "snapshot.bin": snapshot} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(data)), Mode: 0o600}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(tw.Close(), gz.Close()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, original.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "converted.tar.gz")
	args := []string{"--output", "table", "state", "reencrypt-backup",
		"--input", source, "--file", output, "--key-file", testutil.StateKeyFile(t), "--data-dir", t.TempDir()}
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs(args)
	if err := rootCmd.Execute(); err == nil {
		t.Fatal("root command accepted plaintext without explicit opt-in")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("rejected conversion published a partial archive")
	}
	args = append(args, "--allow-legacy-plaintext")
	rootCmd.SetArgs(args)
	rootCmd.SetOut(failedStateReport{})
	if err := rootCmd.Execute(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected only reporting failure, got %v", err)
	}
	converted, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("reporting failure destroyed successfully published backup: %v", err)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("converted backup is not private: %v", err)
	}
	if _, err := backup.ReadMeta(bytes.NewReader(converted), testutil.StateKeys(t)); err != nil {
		t.Fatal(err)
	}
	rootCmd.SetOut(io.Discard)
	rootCmd.SetArgs(args)
	if err := rootCmd.Execute(); err == nil {
		t.Fatal("conversion overwrote an existing archive")
	}
	after, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(converted, after) {
		t.Fatal("exclusive output changed an existing archive")
	}
	sourceAfter, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(sourceAfter, original.Bytes()) {
		t.Fatal("conversion altered its source")
	}
}

func TestStateKeygenThroughRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("production key files require POSIX permissions")
	}
	state, _, err := rootCmd.Find([]string{"state"})
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := rootCmd.OutOrStdout(), rootCmd.ErrOrStderr()
	rootCmd.RemoveCommand(state)
	fresh := stateCmd()
	rootCmd.AddCommand(fresh)
	t.Cleanup(func() {
		rootCmd.RemoveCommand(fresh)
		rootCmd.AddCommand(state)
		rootCmd.SetOut(oldOut)
		rootCmd.SetErr(oldErr)
		rootCmd.SetArgs(nil)
	})
	var output bytes.Buffer
	rootCmd.SetOut(&output)
	rootCmd.SetErr(&output)
	path := filepath.Join(t.TempDir(), "external.json")
	args := []string{"state", "keygen", "--file", path, "--key-id", "fixture", "--data-dir", t.TempDir()}
	rootCmd.SetArgs(args)
	if err := rootCmd.Execute(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file is not owner-only: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), `"keys"`) || bytes.Contains(output.Bytes(), before) {
		t.Fatal("keygen printed secret-bearing key file data")
	}
	rootCmd.SetArgs(args)
	if err := rootCmd.Execute(); err == nil {
		t.Fatal("root keygen overwrote an existing recovery key")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("exclusive key creation changed the original")
	}
}
