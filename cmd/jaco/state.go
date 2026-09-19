package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/backup"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/migration"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
	"github.com/PatrickRuddiman/jaco/internal/daemon/config"
)

func init() { rootCmd.AddCommand(stateCmd()) }

func defaultStateDataDir() string {
	if dir := os.Getenv("JACO_DATA_DIR"); dir != "" {
		return dir
	}
	return config.DefaultDataDir
}

func stateCmd() *cobra.Command {
	command := &cobra.Command{
		Use: "state", Short: "Manage external encryption keys and offline state copies",
	}
	command.AddCommand(stateKeygenCmd(), stateMigrateCmd(), stateBackupCmd())
	return command
}

func stateKeygenCmd() *cobra.Command {
	var opts seal.KeyFileOptions
	command := &cobra.Command{
		Use: "keygen", Short: "Create a new external owner-only state keyring without printing keys",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := seal.GenerateFile(opts); err != nil {
				return err
			}
			_, err := fmt.Fprintf(command.OutOrStdout(), "Created %s. Provision this versioned keyring independently on every member; retain its recovery copy separately from state backups.\n", opts.Path)
			return err
		},
	}
	command.Flags().StringVar(&opts.Path, "file", "", "new absolute external key-file path; never overwritten")
	command.Flags().StringVar(&opts.KeyID, "key-id", "", "new non-secret key version identifier")
	command.Flags().StringVar(&opts.DataDir, "data-dir", defaultStateDataDir(), "application data directory the key file must be outside")
	command.Flags().StringVar(&opts.RetainFile, "retain-file", "", "existing external keyring whose recovery keys must be retained")
	_ = command.MarkFlagRequired("file")
	_ = command.MarkFlagRequired("key-id")
	return command
}

func stateMigrateCmd() *cobra.Command {
	var source, target, keyFile, newKeyFile string
	var legacy bool
	command := &cobra.Command{
		Use: "migrate", Short: "Copy stopped-node state to a fresh encrypted directory; retain originals",
		Long: "Stop every cluster member before a coordinated migration or rotation.\n" +
			"Copies all recognized log history, snapshots, caches and local identities without\n" +
			"rewriting the source. Unknown/incomplete artifacts require explicit inspection.\n" +
			"Original logs (including free pages), snapshots and backups remain sensitive;\n" +
			"quarantine/retire them separately after verified recovery.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			sourceKeys, err := seal.LoadFile(keyFile, source, target)
			if err != nil {
				return err
			}
			targetKeys := sourceKeys
			if newKeyFile != "" {
				targetKeys, err = seal.LoadFile(newKeyFile, target, source)
				if err != nil {
					return err
				}
			}
			report, err := migration.Copy(migration.Options{
				SourceDir: source, TargetDir: target, SourceKeys: sourceKeys,
				TargetKeys: targetKeys, AllowLegacyPlaintext: legacy,
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(command.OutOrStdout(),
				"Created %s: %d logs, %d snapshots, %d cache files, %d local identity/metadata files. Source %s is unchanged; retain it securely until recovery and credential rotation are complete.\n",
				target, report.Logs, report.Snapshots, report.CacheFiles, report.IdentityFiles, source)
			return err
		},
	}
	command.Flags().StringVar(&source, "source-dir", "", "absolute stopped-node data directory; remains unchanged")
	command.Flags().StringVar(&target, "target-dir", "", "absolute new destination directory; must not exist")
	command.Flags().StringVar(&keyFile, "key-file", "", "external source keyring; defaults to JACO_STATE_KEY_FILE/service credential")
	command.Flags().StringVar(&newKeyFile, "new-key-file", "", "external destination ring for rotation; defaults to source ring")
	command.Flags().BoolVar(&legacy, "allow-legacy-plaintext", false, "explicitly accept an unmarked legacy plaintext source")
	_ = command.MarkFlagRequired("source-dir")
	_ = command.MarkFlagRequired("target-dir")
	return command
}

func stateBackupCmd() *cobra.Command {
	var input, output, keyFile, newKeyFile, dataDir string
	var legacy bool
	command := &cobra.Command{
		Use: "reencrypt-backup", Short: "Create an authenticated encrypted backup copy without altering its source",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) (resultErr error) {
			sourceKeys, err := seal.LoadFile(keyFile, dataDir)
			if err != nil {
				return err
			}
			targetKeys := sourceKeys
			if newKeyFile != "" {
				targetKeys, err = seal.LoadFile(newKeyFile, dataDir)
				if err != nil {
					return err
				}
			}
			reader, err := os.Open(input)
			if err != nil {
				return err
			}
			defer reader.Close()
			writer, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return fmt.Errorf("create new backup copy (no overwrite): %w", err)
			}
			published := false
			defer func() {
				if writer != nil {
					resultErr = errors.Join(resultErr, writer.Close())
				}
				if resultErr != nil && !published {
					resultErr = errors.Join(resultErr, os.Remove(output))
				}
			}()
			if err := backup.Reencrypt(backup.ReencryptOptions{
				Reader: reader, Writer: writer, SourceKeys: sourceKeys,
				TargetKeys: targetKeys, AllowLegacyPlaintext: legacy,
			}); err != nil {
				return err
			}
			if err := writer.Sync(); err != nil {
				return err
			}
			err = writer.Close()
			writer = nil
			if err != nil {
				return err
			}
			parent, err := os.Open(filepath.Dir(output))
			if err != nil {
				return err
			}
			if err := errors.Join(parent.Sync(), parent.Close()); err != nil {
				return err
			}
			published = true
			_, err = fmt.Fprintf(command.OutOrStdout(), "Created %s. Original %s is unchanged; keep required recovery keys separate from both archives.\n", output, input)
			return err
		},
	}
	command.Flags().StringVar(&input, "input", "", "existing backup archive")
	command.Flags().StringVar(&output, "file", "", "new encrypted archive path; never overwritten")
	command.Flags().StringVar(&keyFile, "key-file", "", "external source keyring; defaults to JACO_STATE_KEY_FILE/service credential")
	command.Flags().StringVar(&newKeyFile, "new-key-file", "", "external destination ring; defaults to source ring")
	command.Flags().StringVar(&dataDir, "data-dir", defaultStateDataDir(), "application data directory the keys must be outside")
	command.Flags().BoolVar(&legacy, "allow-legacy-plaintext", false, "explicitly accept an unauthenticated schema-1 plaintext backup")
	_ = command.MarkFlagRequired("input")
	_ = command.MarkFlagRequired("file")
	return command
}
