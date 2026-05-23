package firewall

import (
	"context"
	"fmt"
	"os/exec"
)

// nftLister shells out to `nft -j list table inet jaco`. Production wiring.
type nftLister struct{}

func (nftLister) List(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "nft", "-j", "list", "table", "inet", "jaco")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nft -j list: %w", err)
	}
	return out, nil
}

// DefaultLister returns the production Lister that shells out to `nft`. nil
// out the table at boot if the daemon doesn't own one yet — Reconciler.Tick
// will then render+apply on its first pass.
func DefaultLister() Lister { return nftLister{} }
