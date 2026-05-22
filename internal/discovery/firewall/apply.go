package firewall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// Applier shells out to the `nft` binary; the production daemon uses
// `defaultApplier`; tests can inject a stub.
type Applier interface {
	Apply(ctx context.Context, ruleset string) error
}

// DefaultApplier returns the production applier — writes the ruleset to a
// 0600 temp file and invokes `nft -f <file>`.
func DefaultApplier() Applier { return &defaultApplier{} }

type defaultApplier struct{}

// Apply writes ruleset to a 0600 temp file and runs `nft -f` against it.
// The temp file is unlinked on success and on failure.
func (d *defaultApplier) Apply(ctx context.Context, ruleset string) error {
	f, err := os.CreateTemp("", "jaco-nft-*.nft")
	if err != nil {
		return fmt.Errorf("CreateTemp: %w", err)
	}
	path := f.Name()
	defer os.Remove(path)
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if _, err := f.WriteString(ruleset); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	cmd := exec.CommandContext(ctx, "nft", "-f", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nft -f %s: %w (output: %s)", path, err, string(out))
	}
	return nil
}

// ErrNftNotFound is returned by IsAvailable when the `nft` binary isn't on
// PATH — useful for the daemon to skip the firewall path in dev / docker
// environments without nftables.
var ErrNftNotFound = errors.New("nft binary not found on PATH")

// IsAvailable reports whether the `nft` binary is available. The daemon
// uses this on boot to decide whether to apply the ruleset or skip
// (degraded mode).
func IsAvailable() error {
	if _, err := exec.LookPath("nft"); err != nil {
		return ErrNftNotFound
	}
	return nil
}
