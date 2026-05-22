package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/PatrickRuddiman/jaco/internal/packaging"
)

func init() {
	rootCmd.AddCommand(selfUpgradeCmd())
}

func selfUpgradeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "self-upgrade --url <https://.../jaco-vX-os-arch.tar.gz>",
		Short: "Verify + atomically swap the local jaco binary",
	}
	var url, binPath string
	c.Flags().StringVar(&url, "url", "", "tarball URL (https://.../jaco-vX-os-arch.tar.gz)")
	c.Flags().StringVar(&binPath, "bin", "/usr/local/bin/jaco", "binary install path")
	_ = c.MarkFlagRequired("url")

	c.RunE = func(_ *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		return runSelfUpgrade(ctx, url, binPath, packaging.EmbeddedPubKey, httpFetcher{})
	}
	return c
}

// runSelfUpgrade is the unit-testable body. fetcher abstracts HTTP so tests
// inject a fake serving canned tarball / checksum / signature bytes.
func runSelfUpgrade(ctx context.Context, url, binPath, pubKey string, fetcher fetcher) error {
	if url == "" {
		return fmt.Errorf("self-upgrade: --url is required")
	}
	tmp, err := os.MkdirTemp("", "jaco-upgrade-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	// 1. Fetch tarball + sidecar files.
	tarballPath := filepath.Join(tmp, filepath.Base(url))
	checksumsPath := filepath.Join(tmp, "SHA256SUMS")
	signaturePath := filepath.Join(tmp, "SHA256SUMS.minisig")
	if err := fetcher.fetch(ctx, url, tarballPath); err != nil {
		return fmt.Errorf("fetch tarball: %w", err)
	}
	checksumsURL := siblingURL(url, "SHA256SUMS")
	if err := fetcher.fetch(ctx, checksumsURL, checksumsPath); err != nil {
		return fmt.Errorf("fetch checksums: %w", err)
	}
	sigURL := siblingURL(url, "SHA256SUMS.minisig")
	if err := fetcher.fetch(ctx, sigURL, signaturePath); err != nil {
		return fmt.Errorf("fetch signature: %w", err)
	}

	// 2. Verify (signature + checksum) BEFORE touching anything.
	if err := packaging.VerifyTarball(tarballPath, checksumsPath, signaturePath, pubKey); err != nil {
		return err
	}

	// 3. Extract the jaco binary from the tarball.
	extracted := filepath.Join(tmp, "jaco-new")
	if err := extractJacoBinary(tarballPath, extracted); err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	// 4. Save the existing binary as <binPath>.prev.
	prevPath := binPath + ".prev"
	if _, err := os.Stat(binPath); err == nil {
		if err := copyFile(binPath, prevPath); err != nil {
			return fmt.Errorf("save previous binary: %w", err)
		}
	}

	// 5. Atomic rename — must be on the same filesystem as binPath.
	stagedPath := binPath + ".upgrading"
	if err := copyFile(extracted, stagedPath); err != nil {
		return fmt.Errorf("stage new binary: %w", err)
	}
	if err := os.Chmod(stagedPath, 0o755); err != nil {
		return fmt.Errorf("chmod staged: %w", err)
	}
	if err := os.Rename(stagedPath, binPath); err != nil {
		return fmt.Errorf("atomic rename: %w", err)
	}

	// 6 + 7. systemctl restart + health poll — left to the caller in v1.
	// `jaco upgrade` returns after the swap; the operator restarts the
	// service and runs `jaco status` to verify. Full restart-and-poll
	// orchestration lands with the daemon entry.
	return nil
}

// fetcher abstracts HTTP for testability.
type fetcher interface {
	fetch(ctx context.Context, url, dst string) error
}

type httpFetcher struct{}

func (httpFetcher) fetch(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("fetch %s: status %d", url, resp.StatusCode)
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

// siblingURL replaces the last path segment of url with name.
func siblingURL(url, name string) string {
	idx := strings.LastIndex(url, "/")
	if idx < 0 {
		return name
	}
	return url[:idx+1] + name
}

// extractJacoBinary scans tarballPath for a `*/jaco` entry (the rendered
// directory name from the release pipeline) and writes its body to dst.
func extractJacoBinary(tarballPath, dst string) error {
	f, err := os.Open(tarballPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gunzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("tarball does not contain a jaco binary")
		}
		if err != nil {
			return err
		}
		if filepath.Base(h.Name) != "jaco" {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		out, err := os.Create(dst)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if info, err := os.Stat(src); err == nil {
		_ = os.Chmod(dst, info.Mode())
	}
	return nil
}
