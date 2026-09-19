package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/PatrickRuddiman/jaco/internal/cliclient"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func init() {
	rootCmd.AddCommand(backupCmd())
}

func backupCmd() *cobra.Command {
	c := &cobra.Command{
		Use:         "backup",
		Short:       "Stream a cluster backup tarball to a local file",
		Annotations: map[string]string{annotationHonorsOutput: "true"},
	}
	var server, opToken, caCertPath, socket, outputPath string
	c.Flags().StringVar(&server, "server", "", "leader address (host:port); off-node only — omit to use the local socket")
	c.Flags().StringVar(&opToken, "token", "", "operator bearer token (or JACO_TOKEN); required with --server")
	c.Flags().StringVar(&caCertPath, "ca-cert", defaultCACertPath(), "path to cluster CA cert PEM")
	c.Flags().StringVar(&socket, "socket", socketDefault(), "local jacod unix socket (used when --server is omitted)")
	c.Flags().StringVar(&outputPath, "output", "", "destination file (e.g. cluster.tar.gz); required")
	_ = c.MarkFlagRequired("output")

	c.RunE = func(cmd *cobra.Command, _ []string) error {
		conn, withAuth, err := dialOperator(operatorAuth{server: server, token: opToken, caCert: caCertPath, socket: socket})
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
		defer cancel()
		ctx = withAuth(ctx)

		return runBackup(ctx, pb.NewClusterClient(conn), outputPath, os.Stdout)
	}
	return c
}

func runBackup(ctx context.Context, client pb.ClusterClient, outputPath string, out io.Writer) error {
	stream, err := client.Backup(ctx, &pb.BackupRequest{})
	if err != nil {
		return cliclient.FormatError(err)
	}

	var total int64
	err = writeBackupFile(ctx, outputPath, func(f *os.File) error {
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			chunk, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				// Preserve the operator-facing RPC error without a second prefix.
				return cliclient.FormatError(err)
			}
			n, err := f.Write(chunk.GetData())
			if err != nil {
				return fmt.Errorf("write: %w", err)
			}
			total += int64(n)
		}
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Wrote %d bytes to %s\n", total, outputPath)
	return nil
}

func writeBackupFile(ctx context.Context, outputPath string, write func(*os.File) error) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	outputName := filepath.Base(outputPath)
	// Root operations below use leaf names only, never trailing separators.
	if outputPath == "" || outputName == "." || outputName == ".." ||
		!filepath.IsLocal(outputName) || os.IsPathSeparator(outputPath[len(outputPath)-1]) {
		return errors.New("backup output must name a file, not a directory")
	}
	dirPath := filepath.Dir(outputPath)
	dir, err := os.OpenRoot(dirPath)
	if err != nil {
		return fmt.Errorf("open output directory: %w", err)
	}
	defer func() { _ = dir.Close() }()
	dirInfo, err := dir.Stat(".")
	if err != nil {
		return fmt.Errorf("inspect output directory: %w", err)
	}
	if err := checkBackupDirectory(dirInfo); err != nil {
		return err
	}
	if err := checkBackupDestination(dir, outputName); err != nil {
		return err
	}
	tempName := ".jaco-backup-" + rand.Text()
	f, err := dir.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	closed, published := false, false
	defer func() {
		if !closed {
			if err := f.Close(); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("close output: %w", err))
			}
		}
		if !published {
			if err := dir.Remove(tempName); err != nil && !errors.Is(err, os.ErrNotExist) {
				retErr = errors.Join(retErr, fmt.Errorf("remove incomplete backup: %w", err))
			}
		}
	}()

	if err := write(f); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync output: %w", err)
	}
	closed = true
	if err := f.Close(); err != nil {
		return fmt.Errorf("close output: %w", err)
	}
	currentDir, err := os.Stat(dirPath)
	if err != nil {
		return fmt.Errorf("inspect output directory: %w", err)
	}
	if !os.SameFile(dirInfo, currentDir) {
		return errors.New("backup output directory changed during transfer")
	}
	if err := checkBackupDestination(dir, outputName); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := dir.Rename(tempName, outputName); err != nil {
		return fmt.Errorf("publish output: %w", err)
	}
	published = true
	return nil
}

func checkBackupDestination(dir *os.Root, outputName string) error {
	info, err := dir.Lstat(outputName)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect output: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("backup output must be a regular file, not %s", info.Mode().Type())
	}
	return nil
}
