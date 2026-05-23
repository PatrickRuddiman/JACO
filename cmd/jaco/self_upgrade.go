package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
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
		return runSelfUpgrade(ctx, url, prefix, packaging.EmbeddedPubKey, httpFetch)
	}
	return c
}

// runSelfUpgrade is the unit-testable body. fetch abstracts HTTP so tests
// inject a fake serving canned tarball / checksum / signature bytes. prefix
// is the directory holding both jaco + jacod (default /usr/local/bin); the
// swap covers both atomically — fail before either binary is touched.
func runSelfUpgrade(ctx context.Context, url, prefix string, pubKey string, fetch func(ctx context.Context, url, dst string) error) error {
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
	if err := fetch(ctx, url, tarballPath); err != nil {
		return fmt.Errorf("fetch tarball: %w", err)
	}
	checksumsURL := siblingURL(url, "SHA256SUMS")
	if err := fetch(ctx, checksumsURL, checksumsPath); err != nil {
		return fmt.Errorf("fetch checksums: %w", err)
	}
	sigURL := siblingURL(url, "SHA256SUMS.minisig")
	if err := fetch(ctx, sigURL, signaturePath); err != nil {
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

	// 7. systemctl restart jacod + health poll. On poll failure roll
	// back both binaries from .prev and surface the failure so the
	// operator knows the upgrade was aborted (task 37 deferral).
	if err := postUpgradeRestart(ctx, jacoBin, jacodBin); err != nil {
		// Try to roll back to the previous binaries.
		if _, statErr := os.Stat(jacoBin + ".prev"); statErr == nil {
			_ = os.Rename(jacoBin+".prev", jacoBin)
		}
		if _, statErr := os.Stat(jacodBin + ".prev"); statErr == nil {
			_ = os.Rename(jacodBin+".prev", jacodBin)
		}
		// Attempt one more restart with the rolled-back binaries.
		_ = exec.CommandContext(ctx, "systemctl", "restart", "jacod").Run()
		return fmt.Errorf("post-upgrade health check failed; rolled back: %w", err)
	}
	return nil
}

// postUpgradeRestart restarts jacod via systemctl + polls
// `<bin>/jacod --version` for up-to-3 seconds as a liveness check.
// Returns the first error encountered. On hosts without systemctl
// (developer machines, container CI) the restart step is a soft skip
// — the binaries swap landed but the restart is the operator's job.
func postUpgradeRestart(ctx context.Context, jacoBin, jacodBin string) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil
	}
	cmd := exec.CommandContext(ctx, "systemctl", "restart", "jacod")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl restart jacod: %w: %s", err, string(out))
	}
	// Poll `<bin> --version` — jacod prints the embedded version and
	// exits 0 when the new binary is healthy enough to start.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if out, err := exec.CommandContext(ctx, jacodBin, "--version").Output(); err == nil && len(out) > 0 {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("jacod did not report a version within 3s post-restart")
}

// httpFetch is the production fetcher passed to runSelfUpgrade. Tests
// inject an in-memory fake serving canned bytes.
func httpFetch(ctx context.Context, url, dst string) error {
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
