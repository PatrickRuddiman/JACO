package firewall

import (
	"context"
	"fmt"
	"os/exec"
)

// NftList shells out to `nft -j list table inet jaco` and returns the raw
// JSON. The Reconciler wires this to Lister; tests substitute a fake.
func NftList(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "nft", "-j", "list", "table", "inet", "jaco")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nft -j list: %w", err)
	}
	return out, nil
}
