package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/metadata"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func init() {
	token := &cobra.Command{Use: "token", Short: "Manage operator tokens"}
	token.AddCommand(tokenIssueCmd())
	token.AddCommand(tokenRevokeCmd())
	token.AddCommand(tokenListCmd())
	rootCmd.AddCommand(token)
}

func tokenIssueCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "issue",
		Short: "Issue a new operator token (printed to stdout once)",
	}
	var server, opToken, caCertPath, name string
	c.Flags().StringVar(&server, "server", "", "leader address (host:port); required")
	c.Flags().StringVar(&opToken, "token", "", "operator bearer token (or JACO_TOKEN)")
	c.Flags().StringVar(&caCertPath, "ca-cert", defaultCACertPath(), "path to cluster CA cert PEM")
	c.Flags().StringVar(&name, "name", "", "identity for the new token; required")
	_ = c.MarkFlagRequired("server")
	_ = c.MarkFlagRequired("name")

	c.RunE = func(_ *cobra.Command, _ []string) error {
		if opToken == "" {
			opToken = os.Getenv("JACO_TOKEN")
		}
		if opToken == "" {
			return fmt.Errorf("--token or JACO_TOKEN env is required")
		}
		caCertPEM, err := readCACert(caCertPath)
		if err != nil {
			return err
		}
		conn, err := dialServer(server, caCertPEM)
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+opToken)
		resp, err := pb.NewTokensClient(conn).Issue(ctx, &pb.TokenIssueRequest{Identity: name})
		if err != nil {
			return err
		}
		fmt.Printf("Token for %s (save this; not recoverable): %s\n", resp.GetIdentity(), resp.GetToken())
		return nil
	}
	return c
}

func tokenRevokeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "revoke <identity>",
		Short: "Revoke an operator token",
		Args:  cobra.ExactArgs(1),
	}
	var server, opToken, caCertPath string
	c.Flags().StringVar(&server, "server", "", "leader address (host:port); required")
	c.Flags().StringVar(&opToken, "token", "", "operator bearer token (or JACO_TOKEN)")
	c.Flags().StringVar(&caCertPath, "ca-cert", defaultCACertPath(), "path to cluster CA cert PEM")
	_ = c.MarkFlagRequired("server")

	c.RunE = func(_ *cobra.Command, args []string) error {
		if opToken == "" {
			opToken = os.Getenv("JACO_TOKEN")
		}
		if opToken == "" {
			return fmt.Errorf("--token or JACO_TOKEN env is required")
		}
		caCertPEM, err := readCACert(caCertPath)
		if err != nil {
			return err
		}
		conn, err := dialServer(server, caCertPEM)
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+opToken)
		if _, err := pb.NewTokensClient(conn).Revoke(ctx, &pb.TokenRevokeRequest{Identity: args[0]}); err != nil {
			return err
		}
		fmt.Printf("Revoked token for %s\n", args[0])
		return nil
	}
	return c
}

func tokenListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List all known operator tokens (identity + timestamps only)",
	}
	var server, opToken, caCertPath string
	c.Flags().StringVar(&server, "server", "", "leader address (host:port); required")
	c.Flags().StringVar(&opToken, "token", "", "operator bearer token (or JACO_TOKEN)")
	c.Flags().StringVar(&caCertPath, "ca-cert", defaultCACertPath(), "path to cluster CA cert PEM")
	_ = c.MarkFlagRequired("server")

	c.RunE = func(_ *cobra.Command, _ []string) error {
		if opToken == "" {
			opToken = os.Getenv("JACO_TOKEN")
		}
		caCertPEM, err := readCACert(caCertPath)
		if err != nil {
			return err
		}
		conn, err := dialServer(server, caCertPEM)
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+opToken)
		resp, err := pb.NewTokensClient(conn).List(ctx, &pb.TokenListRequest{})
		if err != nil {
			return err
		}
		fmt.Printf("%-30s %-20s %s\n", "IDENTITY", "ISSUED", "REVOKED")
		for _, t := range resp.GetTokens() {
			revoked := "-"
			if t.GetRevokedAt() != nil {
				revoked = t.GetRevokedAt().AsTime().Format(time.RFC3339)
			}
			issued := ""
			if t.GetIssuedAt() != nil {
				issued = t.GetIssuedAt().AsTime().Format(time.RFC3339)
			}
			fmt.Printf("%-30s %-20s %s\n", t.GetIdentity(), issued, revoked)
		}
		return nil
	}
	return c
}
