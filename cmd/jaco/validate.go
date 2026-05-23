package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	grpcsrv "github.com/PatrickRuddiman/jaco/internal/controlplane/grpc"
	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

func init() {
	rootCmd.AddCommand(validateCmd())
}

func validateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "validate",
		Short: "Validate jaco and/or compose manifests locally (no cluster needed)",
		Long: `validate checks jaco.yaml and compose.yaml files for correctness without
contacting a cluster. Pass --jaco to lint a jaco manifest, --compose to lint a
compose file, or both to additionally cross-check that every jaco service
references a real compose service.`,
		Args: cobra.NoArgs,
	}
	var jacoPath, composePath string
	c.Flags().StringVar(&jacoPath, "jaco", "", "path to jaco YAML manifest")
	c.Flags().StringVar(&composePath, "compose", "", "path to compose YAML file")

	c.RunE = func(cmd *cobra.Command, _ []string) error {
		if jacoPath == "" && composePath == "" {
			return fmt.Errorf("at least one of --jaco or --compose is required")
		}
		return runValidate(cmd, jacoPath, composePath)
	}
	return c
}

// runValidate is the unit-testable body of `jaco validate`. It reads the
// specified files, runs each validator, and optionally cross-checks that
// every jaco service name matches a compose service key.
func runValidate(cmd *cobra.Command, jacoPath, composePath string) error {
	var jacoBytes, composeBytes []byte

	if jacoPath != "" {
		b, err := os.ReadFile(jacoPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", jacoPath, err)
		}
		jacoBytes = b
	}

	if composePath != "" {
		b, err := os.ReadFile(composePath)
		if err != nil {
			return fmt.Errorf("read %s: %w", composePath, err)
		}
		composeBytes = b
	}

	// Validate the jaco manifest if provided.
	if jacoPath != "" {
		if err := grpcsrv.ValidateJacoYAMLBytes(jacoBytes); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Error: validation_failed: %s\n", err.Error())
			return err
		}
	}

	// Validate the compose file if provided.
	if composePath != "" {
		if err := compose.Validate(composeBytes); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Error: validation_failed: %s\n", err.Error())
			return err
		}
	}

	// Cross-check: every jaco service must reference a real compose service.
	if jacoPath != "" && composePath != "" {
		if err := crossValidate(jacoBytes, composeBytes); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Error: validation_failed: %s\n", err.Error())
			return err
		}
	}

	return nil
}

// crossValidate asserts that every service declared in the jaco manifest has a
// matching key in the compose file. Each JacoServiceDecl uses compose_service
// (defaulting to name) as the compose-side key.
func crossValidate(jacoBytes, composeBytes []byte) error {
	jacoSpec, err := grpcsrv.ParseJacoYAML(jacoBytes)
	if err != nil {
		return fmt.Errorf("parse jaco yaml: %w", err)
	}
	project, err := compose.LoadBytes(composeBytes, "compose.yaml")
	if err != nil {
		return fmt.Errorf("parse compose yaml: %w", err)
	}

	composeServices := make(map[string]bool, len(project.Services))
	for name := range project.Services {
		composeServices[name] = true
	}

	for _, svc := range jacoSpec.Services {
		composeName := svc.ComposeService
		if composeName == "" {
			composeName = svc.Name
		}
		if !composeServices[composeName] {
			return fmt.Errorf("jaco service %q references compose service %q which is not defined in the compose file", svc.Name, composeName)
		}
	}
	return nil
}
