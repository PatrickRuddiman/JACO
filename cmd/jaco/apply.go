package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/PatrickRuddiman/jaco/internal/cliclient"
	grpcsrv "github.com/PatrickRuddiman/jaco/internal/controlplane/grpc"
	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
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
		server, opToken, caCertPath, socket string
		composePath                         string
		dryRun                              bool
	)
	c.Flags().StringVar(&server, "server", "", "leader address (host:port); off-node only — omit to use the local socket")
	c.Flags().StringVar(&opToken, "token", "", "operator bearer token (or JACO_TOKEN); required with --server")
	c.Flags().StringVar(&caCertPath, "ca-cert", defaultCACertPath(), "path to cluster CA cert PEM")
	c.Flags().StringVar(&socket, "socket", socketDefault(), "local jacod unix socket (used when --server is omitted)")
	c.Flags().StringVar(&composePath, "compose", "", "compose file path; defaults to compose.yml next to jaco.yaml")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print the diff and exit without applying")

	c.RunE = func(_ *cobra.Command, args []string) error {
		jacoPath := args[0]
		jacoBytes, composeBytes, resolvedComposePath, err := readManifestPair(jacoPath, composePath)
		if err != nil {
			return err
		}
		// Top-level jaco.yaml `environment:` (optional) loads an env-style
		// file CLIENT-SIDE and feeds its KEY=value entries into compose-spec
		// `${VAR}` interpolation across the WHOLE compose document. Runs
		// BEFORE service-level env_file resolution so any `${VAR}` in
		// env_file: paths or in service.environment entries is substituted
		// once, consistently, against the same source.
		stackEnv, err := loadStackEnv(jacoPath, jacoBytes)
		if err != nil {
			return err
		}
		composeBytes, err = compose.SubstituteEnvVars(composeBytes, stackEnv)
		if err != nil {
			return fmt.Errorf("interpolate %s: %w", resolvedComposePath, err)
		}
		// env_file is resolved CLIENT-SIDE — the daemon does not have the
		// operator's .env files on disk. resolvedComposePath is always set
		// here (the CLI's only source of compose bytes today is a file);
		// when a stdin source is added it must pass "" and this guard will
		// refuse env_file outright.
		composeBytes, err = resolveComposeEnvFiles(composeBytes, resolvedComposePath)
		if err != nil {
			return err
		}

		conn, withAuth, err := dialOperator(operatorAuth{server: server, token: opToken, caCert: caCertPath, socket: socket})
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		ctx = withAuth(ctx)

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
		return cliclient.FormatError(err)
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
// `<dir of jaco.yaml>/compose.yml` (falling back to `.yaml`). The resolved
// compose path is returned so the caller can use its directory as the
// env_file base dir.
func readManifestPair(jacoPath, composeOverride string) ([]byte, []byte, string, error) {
	jacoBytes, err := os.ReadFile(jacoPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("read %s: %w", jacoPath, err)
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
			return nil, nil, "", fmt.Errorf("no compose file found next to %s; pass --compose explicitly", jacoPath)
		}
	}
	composeBytes, err := os.ReadFile(composePath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("read %s: %w", composePath, err)
	}
	return jacoBytes, composeBytes, composePath, nil
}

// resolveComposeEnvFiles merges every service's env_file into its environment
// map before the bytes leave the CLI (issue #103). The daemon node does not
// have the operator's .env files, so resolution at apply time on the daemon
// is impossible — the daemon-side LoadBytes will fail loud with
// env_file_unresolved if the CLI ever skips this step.
//
// composePath is the file the bytes came from; its directory is the base for
// relative env_file paths. An empty composePath means the compose document
// arrived from a non-file source (e.g. stdin in a future iteration); in that
// case env_file is rejected outright because there is no defensible base
// directory to resolve against.
func resolveComposeEnvFiles(composeBytes []byte, composePath string) ([]byte, error) {
	if composePath == "" {
		// Probe rather than hand the bytes to ResolveEnvFiles with an empty
		// baseDir — we want a clear "use --compose <path>" message, not a
		// file-not-found error against ".".
		if name, ok := composeHasEnvFile(composeBytes); ok {
			return nil, fmt.Errorf(
				"service %q uses env_file but the compose document has no path on disk; "+
					"env_file paths can only be resolved relative to a real file — pass --compose <path>",
				name)
		}
		return composeBytes, nil
	}
	out, err := compose.ResolveEnvFiles(composeBytes, filepath.Dir(composePath))
	if err != nil {
		return nil, fmt.Errorf("resolve env_file in %s: %w", composePath, err)
	}
	return out, nil
}

// composeHasEnvFile reports the first service that declares env_file:, mostly
// so the stdin-mode rejection message can name the offending service. A YAML
// parse failure here is swallowed — ResolveEnvFiles would have surfaced it
// in the with-path branch, and the stdin branch never reaches the loader.
func composeHasEnvFile(body []byte) (string, bool) {
	var probe struct {
		Services map[string]struct {
			EnvFile []any `yaml:"env_file"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(body, &probe); err != nil {
		return "", false
	}
	// Deterministic pick of the first offender for the error message.
	names := make([]string, 0, len(probe.Services))
	for name, svc := range probe.Services {
		if len(svc.EnvFile) > 0 {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "", false
	}
	sort.Strings(names)
	return names[0], true
}

// loadStackEnv resolves the optional top-level `environment:` field of
// jaco.yaml into a KEY=value map suitable for compose-spec `${VAR}`
// interpolation. Returns (nil, nil) when the field is absent — the apply
// path treats nil identically to "no interpolation source", so the compose
// document passes through SubstituteEnvVars on the fast path.
//
// The env file path is interpreted relative to the jaco.yaml file's
// directory (matching the compose service-level `env_file:` convention).
// A missing or unreadable file is a loud CLI error — the operator's
// responsibility to have it present at apply time.
//
// jacoBytes is the raw manifest the caller already read for the apply RPC;
// re-parsing it here keeps the helper self-contained and unit-testable
// without needing the deploy client.
func loadStackEnv(jacoPath string, jacoBytes []byte) (map[string]string, error) {
	spec, err := grpcsrv.ParseJacoYAML(jacoBytes)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", jacoPath, err)
	}
	if spec.Environment == "" {
		return nil, nil
	}
	envPath := spec.Environment
	if !filepath.IsAbs(envPath) {
		envPath = filepath.Join(filepath.Dir(jacoPath), envPath)
	}
	// nil currentEnv: no process-env passthrough into the interpolation map
	// (matches the "manifests are explicit and reproducible" posture). The
	// dotenv loader still threads earlier keys forward so back-refs like
	// FOO=${BAR} inside the same file resolve against prior lines.
	env, err := dotenv.GetEnvFromFile(nil, []string{envPath})
	if err != nil {
		return nil, fmt.Errorf("load environment file %s: %w", envPath, err)
	}
	return env, nil
}
