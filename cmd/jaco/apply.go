package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/metadata"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func init() {
	rootCmd.AddCommand(applyCmd())
}

func applyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "apply <jaco.yaml>",
		Short: "Apply a JACO deployment manifest",
		Args:  cobra.ExactArgs(1),
	}
	var (
		server, opToken, caCertPath string
		composePath                 string
		dryRun                      bool
	)
	c.Flags().StringVar(&server, "server", "", "leader address (host:port); required")
	c.Flags().StringVar(&opToken, "token", "", "operator bearer token (or JACO_TOKEN)")
	c.Flags().StringVar(&caCertPath, "ca-cert", "", "path to cluster CA cert PEM; required")
	c.Flags().StringVar(&composePath, "compose", "", "compose file path; defaults to compose.yml next to jaco.yaml")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print the diff and exit without applying")
	_ = c.MarkFlagRequired("server")
	_ = c.MarkFlagRequired("ca-cert")

	c.RunE = func(_ *cobra.Command, args []string) error {
		if opToken == "" {
			opToken = os.Getenv("JACO_TOKEN")
		}
		if opToken == "" {
			return fmt.Errorf("--token or JACO_TOKEN env is required")
		}
		jacoPath := args[0]
		jacoBytes, composeBytes, err := readManifestPair(jacoPath, composePath)
		if err != nil {
			return err
		}

		conn, err := dialServer(server, mustReadFile(caCertPath))
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+opToken)

		client := pb.NewDeployClient(conn)
		return runApply(ctx, client, jacoBytes, composeBytes, dryRun, os.Stdout)
	}
	return c
}

// runApply is the unit-testable body of `jaco apply`. Takes a pb.DeployClient
// so tests can inject a fake without spinning up a server.
func runApply(ctx context.Context, client pb.DeployClient, jacoBytes, composeBytes []byte, dryRun bool, out io.Writer) error {
	resp, err := client.Apply(ctx, &pb.ApplyRequest{
		JacoYaml:    jacoBytes,
		ComposeYaml: composeBytes,
		DryRun:      dryRun,
	})
	if err != nil {
		return err
	}
	if dryRun {
		renderDiff(out, resp.GetDiff())
		return nil
	}
	fmt.Fprintf(out, "Applied revision: %d\n", resp.GetAppliedRevision())
	return nil
}

// renderDiff prints the apply diff. Empty diff → "No changes". Otherwise
// dumps adds / updates / removes line per entity (replicas / routes /
// subnets — though v1 returns an empty Diff per task 14's stub).
func renderDiff(out io.Writer, diff *pb.Diff) {
	if diff == nil || (len(diff.GetAdds()) == 0 && len(diff.GetUpdates()) == 0 && len(diff.GetRemoves()) == 0) {
		fmt.Fprintln(out, "No changes")
		return
	}
	for _, a := range diff.GetAdds() {
		fmt.Fprintf(out, "+ %s\n", a)
	}
	for _, u := range diff.GetUpdates() {
		fmt.Fprintf(out, "~ %s\n", u)
	}
	for _, r := range diff.GetRemoves() {
		fmt.Fprintf(out, "- %s\n", r)
	}
}

// readManifestPair loads the jaco.yaml + compose file from disk. composeOverride
// is honored when set; otherwise the compose file is resolved as
// `<dir of jaco.yaml>/compose.yml` (falling back to `.yaml`).
func readManifestPair(jacoPath, composeOverride string) ([]byte, []byte, error) {
	jacoBytes, err := os.ReadFile(jacoPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", jacoPath, err)
	}
	composePath := composeOverride
	if composePath == "" {
		dir := filepath.Dir(jacoPath)
		for _, name := range []string{"compose.yml", "compose.yaml"} {
			cand := filepath.Join(dir, name)
			if _, err := os.Stat(cand); err == nil {
				composePath = cand
				break
			}
		}
		if composePath == "" {
			return nil, nil, fmt.Errorf("no compose file found next to %s; pass --compose explicitly", jacoPath)
		}
	}
	composeBytes, err := os.ReadFile(composePath)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", composePath, err)
	}
	return jacoBytes, composeBytes, nil
}
