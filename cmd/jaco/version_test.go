package main

import (
	"bytes"
	"strings"
	"testing"
)

// withVersion swaps the package-level `version` and rootCmd.Version for the
// duration of a test, restoring both on teardown. Cobra reads
// rootCmd.Version (not the package var) when rendering `--version`, so both
// must be set.
func withVersion(t *testing.T, v string) {
	t.Helper()
	origVar := version
	origRoot := rootCmd.Version
	version = v
	rootCmd.Version = v
	t.Cleanup(func() {
		version = origVar
		rootCmd.Version = origRoot
	})
}

// runRootWithArgs executes rootCmd with the given args, capturing the
// command's stdout/stderr buffers. Returns the merged output for assertion.
func runRootWithArgs(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errBuf)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	err := rootCmd.Execute()
	return out.String() + errBuf.String(), err
}

// TestRoot_VersionFlag — `jaco --version` prints the linked version string.
func TestRoot_VersionFlag(t *testing.T) {
	withVersion(t, "test-version-flag")
	got, err := runRootWithArgs(t, "--version")
	if err != nil {
		t.Fatalf("--version returned error: %v\noutput: %s", err, got)
	}
	if !strings.Contains(got, "test-version-flag") {
		t.Fatalf("--version output missing version string; got %q", got)
	}
}

// TestRoot_VersionSubcommand — `jaco version` prints the same string. Cobra
// does not auto-register a `version` subcommand even when Version is set;
// this pins that the explicit registration in version.go is present.
func TestRoot_VersionSubcommand(t *testing.T) {
	withVersion(t, "test-version-sub")
	got, err := runRootWithArgs(t, "version")
	if err != nil {
		t.Fatalf("version subcommand returned error: %v\noutput: %s", err, got)
	}
	if !strings.Contains(got, "test-version-sub") {
		t.Fatalf("version subcommand output missing version string; got %q", got)
	}
}
