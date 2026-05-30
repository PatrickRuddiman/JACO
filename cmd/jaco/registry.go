package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/PatrickRuddiman/jaco/internal/cliclient"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func init() {
	r := &cobra.Command{
		Use:   "registry",
		Short: "Manage replicated container-registry credentials",
		Long: "Container-registry credentials are stored in raft and consumed by " +
			"every node's image-pull path. The cluster authenticates pulls against " +
			"private registries (Docker Hub private repos, GHCR, ECR, self-hosted) " +
			"without per-node configuration. See `jaco registry --help` subcommands.",
	}
	r.AddCommand(registryLoginCmd())
	r.AddCommand(registryLogoutCmd())
	r.AddCommand(registryListCmd())
	rootCmd.AddCommand(r)
}

// registryFlags is the flag bag shared by login/logout/list. Mirrors token.go
// but adds --password-stdin for non-interactive pipelines (the password is
// NEVER taken as a command-line argument — that would leak via /proc/*/cmdline
// and shell history).
type registryFlags struct {
	server        string
	token         string
	caCert        string
	socket        string
	username      string
	passwordStdin bool
}

func addRegistryDialFlags(c *cobra.Command, f *registryFlags) {
	c.Flags().StringVar(&f.server, "server", "", "leader address (host:port); off-node only — omit to use the local socket")
	c.Flags().StringVar(&f.token, "token", "", "operator bearer token (or JACO_TOKEN); required with --server")
	c.Flags().StringVar(&f.caCert, "ca-cert", defaultCACertPath(), "path to cluster CA cert PEM")
	c.Flags().StringVar(&f.socket, "socket", socketDefault(), "local jacod unix socket (used when --server is omitted)")
}

func registryLoginCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "login <registry>",
		Short: "Add or replace a container-registry credential (rotates on repeat)",
		Long: "Reads the password from a terminal prompt by default, or from stdin " +
			"with --password-stdin (CI / piped). The password is NEVER taken as a " +
			"command-line argument — passing it that way would leak via /proc and " +
			"shell history. Docker Hub canonicalizes to 'docker.io'.",
		Args: cobra.ExactArgs(1),
	}
	f := &registryFlags{}
	addRegistryDialFlags(c, f)
	c.Flags().StringVarP(&f.username, "username", "u", "", "registry username; required")
	c.Flags().BoolVar(&f.passwordStdin, "password-stdin", false, "read the password from stdin instead of prompting")
	_ = c.MarkFlagRequired("username")

	c.RunE = func(_ *cobra.Command, args []string) error {
		if f.username == "" {
			return errors.New("--username is required")
		}
		secret, err := readRegistryPassword(f.passwordStdin)
		if err != nil {
			return err
		}
		if len(secret) == 0 {
			return errors.New("empty password — refusing to store")
		}
		conn, withAuth, err := dialOperator(operatorAuth{server: f.server, token: f.token, caCert: f.caCert, socket: f.socket})
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ctx = withAuth(ctx)
		resp, err := pb.NewRegistryCredentialsClient(conn).Add(ctx, &pb.RegistryCredentialAddRequest{
			Registry: args[0],
			Username: f.username,
			Secret:   secret,
		})
		if err != nil {
			return cliclient.FormatError(err)
		}
		fmt.Printf("Stored credential for %s (user %s)\n", resp.GetRegistry(), f.username)
		return nil
	}
	return c
}

func registryLogoutCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "logout <registry>",
		Short: "Remove a container-registry credential",
		Args:  cobra.ExactArgs(1),
	}
	f := &registryFlags{}
	addRegistryDialFlags(c, f)
	c.RunE = func(_ *cobra.Command, args []string) error {
		conn, withAuth, err := dialOperator(operatorAuth{server: f.server, token: f.token, caCert: f.caCert, socket: f.socket})
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ctx = withAuth(ctx)
		if _, err := pb.NewRegistryCredentialsClient(conn).Remove(ctx, &pb.RegistryCredentialRemoveRequest{Registry: args[0]}); err != nil {
			return cliclient.FormatError(err)
		}
		fmt.Printf("Removed credential for %s\n", args[0])
		return nil
	}
	return c
}

func registryListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List replicated registry credentials (host + username only; the secret is never printed)",
	}
	f := &registryFlags{}
	addRegistryDialFlags(c, f)
	c.RunE = func(_ *cobra.Command, _ []string) error {
		conn, withAuth, err := dialOperator(operatorAuth{server: f.server, token: f.token, caCert: f.caCert, socket: f.socket})
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ctx = withAuth(ctx)
		resp, err := pb.NewRegistryCredentialsClient(conn).List(ctx, &pb.RegistryCredentialListRequest{})
		if err != nil {
			return cliclient.FormatError(err)
		}
		fmt.Printf("%-40s %-30s %s\n", "REGISTRY", "USERNAME", "UPDATED")
		for _, s := range resp.GetCredentials() {
			updated := "-"
			if s.GetUpdatedAt() != nil {
				updated = s.GetUpdatedAt().AsTime().Format(time.RFC3339)
			}
			fmt.Printf("%-40s %-30s %s\n", s.GetRegistry(), s.GetUsername(), updated)
		}
		return nil
	}
	return c
}

// readRegistryPassword sources the registry password either from stdin
// (when --password-stdin is set, or stdin is not a TTY) or via an
// interactive prompt that suppresses echo. The result is the trimmed first
// line of input — registries reject trailing whitespace and a multi-line
// password is almost always a mistake from a here-doc.
func readRegistryPassword(passwordStdin bool) ([]byte, error) {
	if passwordStdin {
		return readStdinLine(os.Stdin)
	}
	// Interactive prompt: golang.org/x/term suppresses echo iff stdin is a
	// TTY. When it isn't (piped / redirected), fall through to a plain read
	// so CI-style invocations without --password-stdin still work — but log
	// to stderr so the operator notices they're not on a TTY.
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, "Password: ")
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return nil, fmt.Errorf("read password: %w", err)
		}
		return trimPassword(b), nil
	}
	fmt.Fprintln(os.Stderr, "warning: stdin is not a TTY; reading password without prompt — pass --password-stdin explicitly to suppress this warning")
	return readStdinLine(os.Stdin)
}

func readStdinLine(r io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		return nil, errors.New("read stdin: empty input")
	}
	return trimPassword([]byte(scanner.Text())), nil
}

// trimPassword strips a single trailing newline and any surrounding ASCII
// whitespace operators commonly paste by accident. Returns nil for an
// entirely-blank password so the caller's len-check rejects it.
func trimPassword(b []byte) []byte {
	b = bytes.TrimRight(b, "\r\n")
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) == 0 {
		return nil
	}
	return b
}
