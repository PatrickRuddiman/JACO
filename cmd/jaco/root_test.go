package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestRoot_OutputFlag_RejectsUnsupported pins the interim guard from #156:
// commands that do not opt into --output via annotationHonorsOutput must
// error loudly on -o json / -o yaml so CI pipelines piping through jq fail
// at the source instead of silently receiving table output. We exercise the
// guard directly with an un-annotated stub so we don't depend on a command's
// RunE (which would try to dial a daemon).
func TestRoot_OutputFlag_RejectsUnsupported(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			cmd := &cobra.Command{Use: "stub"}
			cmd.Flags().String("output", format, "")
			err := rootCmd.PersistentPreRunE(cmd, nil)
			if err == nil {
				t.Fatalf("expected error for -o %s, got nil", format)
			}
			want := "output format \"" + format + "\" not implemented"
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not mention %q", err.Error(), want)
			}
		})
	}
}

// TestRoot_OutputFlag_StatusHonorsAnnotation pins that the read-only commands
// that now serialize json/yaml (status, cluster status, node list, audit) are
// tagged so the root guard lets non-table formats through to their RunE.
func TestRoot_OutputFlag_StatusHonorsAnnotation(t *testing.T) {
	for _, c := range []*cobra.Command{statusCmd(), clusterStatusCmd(), nodeListCmd()} {
		if c.Annotations[annotationHonorsOutput] != "true" {
			t.Errorf("command %q missing %s annotation", c.Name(), annotationHonorsOutput)
		}
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
