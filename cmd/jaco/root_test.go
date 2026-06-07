package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runRootWith invokes rootCmd with the supplied args and returns the error
// from Execute plus whatever the command wrote to its captured out/err.
// flagOutput is restored after the call so tests don't leak the persistent
// flag value into one another.
func runRootWith(t *testing.T, args ...string) (error, string) {
	t.Helper()
	prev := flagOutput
	t.Cleanup(func() {
		flagOutput = prev
		_ = rootCmd.PersistentFlags().Set("output", prev)
	})
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs(args)
	err := rootCmd.Execute()
	return err, buf.String()
}

// TestRoot_OutputFlag_RejectsUnsupported pins the quick-fix from #156:
// commands that do not opt into --output via annotationHonorsOutput must
// error loudly on -o json / -o yaml so CI pipelines piping through jq fail
// at the source instead of silently receiving table output.
func TestRoot_OutputFlag_RejectsUnsupported(t *testing.T) {
	for _, fmt := range []string{"json", "yaml"} {
		t.Run(fmt, func(t *testing.T) {
			err, _ := runRootWith(t, "status", "-o", fmt)
			if err == nil {
				t.Fatalf("expected error for -o %s, got nil", fmt)
			}
			want := "output format \"" + fmt + "\" not implemented"
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not mention %q", err.Error(), want)
			}
		})
	}
}

// TestRoot_OutputFlag_TableAllowed pins the default path: -o table (and the
// implicit default) must pass the PersistentPreRunE guard. We exercise the
// guard directly with a fresh ephemeral command tree so we don't depend on
// status' RunE (which would try to dial a daemon).
func TestRoot_OutputFlag_TableAllowed(t *testing.T) {
	for _, v := range []string{"", "table"} {
		t.Run("out="+v, func(t *testing.T) {
			cmd := &cobra.Command{Use: "stub"}
			cmd.Flags().String("output", v, "")
			if err := rootCmd.PersistentPreRunE(cmd, nil); err != nil {
				t.Fatalf("PersistentPreRunE rejected output=%q: %v", v, err)
			}
		})
	}
}

// TestRoot_OutputFlag_HonorsAnnotation pins the carve-out: commands tagged
// with annotationHonorsOutput (today: `audit`) keep handling -o json / -o
// yaml in their own RunE, so the root guard must let them through.
func TestRoot_OutputFlag_HonorsAnnotation(t *testing.T) {
	cmd := &cobra.Command{
		Use:         "stub",
		Annotations: map[string]string{annotationHonorsOutput: "true"},
	}
	cmd.Flags().String("output", "json", "")
	if err := rootCmd.PersistentPreRunE(cmd, nil); err != nil {
		t.Fatalf("PersistentPreRunE rejected annotated command: %v", err)
	}
}
