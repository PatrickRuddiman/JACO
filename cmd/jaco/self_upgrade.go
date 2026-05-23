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
		Short: "Verify + atomically swap the local jaco + jacod binaries",
	}
	var url, prefix string
	c.Flags().StringVar(&url, "url", "", "tarball URL (https://.../jaco-vX-os-arch.tar.gz)")
	c.Flags().StringVar(&prefix, "prefix", "/usr/local/bin", "directory holding jaco + jacod")
	_ = c.MarkFlagRequired("url")

	c.RunE = func(_ *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		return runSelfUpgrade(ctx, url, prefix, packaging.EmbeddedPubKey, httpFetcher{})
	}
	return c
}

// runSelfUpgrade is the unit-testable body. fetcher abstracts HTTP so tests
// inject a fake serving canned tarball / checksum / signature bytes. prefix
// is the directory holding both jaco + jacod (default /usr/local/bin); the
// swap covers both atomically — fail before either binary is touched.
func runSelfUpgrade(ctx context.Context, url, prefix string, pubKey string, fetcher fetcher) error {
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

	// 3. Extract BOTH binaries from the tarball.
	jacoExtracted := filepath.Join(tmp, "jaco-new")
	jacodExtracted := filepath.Join(tmp, "jacod-new")
	if err := extractTarballEntry(tarballPath, "jaco", jacoExtracted); err != nil {
		return fmt.Errorf("extract jaco: %w", err)
	}
	if err := extractTarballEntry(tarballPath, "jacod", jacodExtracted); err != nil {
		return fmt.Errorf("extract jacod: %w", err)
	}

	jacoBin := filepath.Join(prefix, "jaco")
	jacodBin := filepath.Join(prefix, "jacod")

	// 4. Save existing binaries as <bin>.prev.
	for _, bin := range []string{jacoBin, jacodBin} {
		if _, err := os.Stat(bin); err == nil {
			if err := copyFile(bin, bin+".prev"); err != nil {
				return fmt.Errorf("save previous %s: %w", filepath.Base(bin), err)
			}
		}
	}

	// 5. Stage both new binaries.
	for src, dst := range map[string]string{
		jacoExtracted:  jacoBin + ".upgrading",
		jacodExtracted: jacodBin + ".upgrading",
	} {
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("stage %s: %w", filepath.Base(dst), err)
		}
		if err := os.Chmod(dst, 0o755); err != nil {
			return fmt.Errorf("chmod %s: %w", filepath.Base(dst), err)
		}
	}

	// 6. Atomic rename — both binaries, back-to-back. After the first
	// succeeds the operator is briefly in a mixed state; the rename pair
	// is the closest we can get to "both at once" without a transactional
	// filesystem.
	if err := os.Rename(jacoBin+".upgrading", jacoBin); err != nil {
		return fmt.Errorf("rename jaco: %w", err)
	}
	if err := os.Rename(jacodBin+".upgrading", jacodBin); err != nil {
		// Best-effort rollback: restore jaco from .prev if we have one.
		if _, statErr := os.Stat(jacoBin + ".prev"); statErr == nil {
			_ = os.Rename(jacoBin+".prev", jacoBin)
		}
		return fmt.Errorf("rename jacod (jaco was restored): %w", err)
	}

	// 7. systemctl restart + health poll — left to the operator. Full
	// restart-and-poll orchestration lands alongside the daemon-entry
	// scheduler/runtime/ingress wiring (later iters of task 38).
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

// extractTarballEntry scans tarballPath for an entry whose basename equals
// name (e.g. "jaco" or "jacod") and writes its body to dst.
func extractTarballEntry(tarballPath, name, dst string) error {
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
			return fmt.Errorf("tarball does not contain a %s binary", name)
		}
		if err != nil {
			return err
		}
		if filepath.Base(h.Name) != name {
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
