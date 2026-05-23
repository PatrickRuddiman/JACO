package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newTestValidateCmd returns a fresh cobra.Command with captured stdout/stderr
// buffers. The command is wired identically to the production validateCmd but
// with SilenceErrors/SilenceUsage set so test output is clean.
func newTestValidateCmd(t *testing.T) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	c := validateCmd()
	var out, errBuf bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&errBuf)
	c.SilenceUsage = true
	c.SilenceErrors = true
	return c, &out, &errBuf
}

// TestValidateCmd_GoodJacoAndCompose — both files pass, exit 0, no stderr.
func TestValidateCmd_GoodJacoAndCompose(t *testing.T) {
	c, _, errBuf := newTestValidateCmd(t)
	c.SetArgs([]string{
		"--jaco", "testdata/validate-good.jaco.yaml",
		"--compose", "testdata/validate-good.compose.yaml",
	})
	if err := c.Execute(); err != nil {
		t.Fatalf("expected success, got error: %v\nstderr: %s", err, errBuf.String())
	}
	if errBuf.Len() > 0 {
		t.Errorf("unexpected stderr output: %s", errBuf.String())
	}
}

// TestValidateCmd_BadJaco — invalid jaco file triggers a validation error.
func TestValidateCmd_BadJaco(t *testing.T) {
	c, _, errBuf := newTestValidateCmd(t)
	c.SetArgs([]string{
		"--jaco", "testdata/validate-bad.jaco.yaml",
	})
	err := c.Execute()
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	// Error should reference the unknown service.
	combined := errBuf.String() + err.Error()
	if !strings.Contains(combined, "unknown service") && !strings.Contains(combined, "validation_failed") {
		t.Errorf("expected validation error message referencing unknown service, got stderr=%q err=%v", errBuf.String(), err)
	}
}

// TestValidateCmd_ComposeOnly — compose-only path succeeds on a valid file.
func TestValidateCmd_ComposeOnly(t *testing.T) {
	c, _, errBuf := newTestValidateCmd(t)
	c.SetArgs([]string{
		"--compose", "testdata/validate-good.compose.yaml",
	})
	if err := c.Execute(); err != nil {
		t.Fatalf("expected success, got error: %v\nstderr: %s", err, errBuf.String())
	}
	if errBuf.Len() > 0 {
		t.Errorf("unexpected stderr output: %s", errBuf.String())
	}
}

// TestValidateCmd_CrossMismatch — jaco service not present in compose triggers
// a cross-validation error.
func TestValidateCmd_CrossMismatch(t *testing.T) {
	// Write a compose file with a different service name into a temp file.
	dir := t.TempDir()
	composePath := dir + "/mismatch.compose.yaml"
	if err := writeFile(composePath, "services:\n  other:\n    image: nginx:1.27\n"); err != nil {
		t.Fatal(err)
	}

	c, _, errBuf := newTestValidateCmd(t)
	c.SetArgs([]string{
		"--jaco", "testdata/validate-good.jaco.yaml",
		"--compose", composePath,
	})
	err := c.Execute()
	if err == nil {
		t.Fatal("expected cross-validation error, got nil")
	}
	combined := errBuf.String() + err.Error()
	if !strings.Contains(combined, "compose") {
		t.Errorf("expected error mentioning compose service, got err=%v stderr=%s", err, errBuf.String())
	}
}

// TestValidateCmd_NoFlags — omitting both flags returns an error.
func TestValidateCmd_NoFlags(t *testing.T) {
	c, _, _ := newTestValidateCmd(t)
	c.SetArgs([]string{})
	if err := c.Execute(); err == nil {
		t.Fatal("expected error when no flags passed")
	}
}

// TestRunValidate_DirectCall exercises runValidate directly with the good
// fixture pair to confirm the pure function succeeds without cobra overhead.
func TestRunValidate_DirectCall(t *testing.T) {
	cmd := validateCmd()
	var errBuf bytes.Buffer
	cmd.SetErr(&errBuf)
	if err := runValidate(cmd, "testdata/validate-good.jaco.yaml", "testdata/validate-good.compose.yaml"); err != nil {
		t.Fatalf("runValidate: %v\nstderr: %s", err, errBuf.String())
	}
}
