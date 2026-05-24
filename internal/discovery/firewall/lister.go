package firewall

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// emptyNftDoc is the JSON shape NftList returns when the `inet jaco` table
// doesn't exist yet. SelfTestFromJSON treats it as "everything missing",
// which causes Reconciler.Tick to render+apply on its first pass and
// create the table from scratch.
var emptyNftDoc = []byte(`{"nftables":[]}`)

// NftList shells out to `nft -j list table inet jaco` and returns the raw
// JSON. On a cold-boot host where the `inet jaco` table has not been
// created yet, nft exits non-zero with "No such file or directory" — that
// case is translated to an empty document with nil error so the
// Reconciler can drive Apply and bring the table into existence. Other
// failures surface with stderr included to aid debugging.
func NftList(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "nft", "-j", "list", "table", "inet", "jaco")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	if isNftTableNotFound(stderr.Bytes()) {
		return emptyNftDoc, nil
	}
	return nil, fmt.Errorf("nft -j list: %w: %s", err, strings.TrimSpace(stderr.String()))
}

// isNftTableNotFound reports whether nft's stderr indicates the queried
// table does not exist yet. nft prints "Error: No such file or directory"
// (sometimes followed by a hint line) when the table is absent.
func isNftTableNotFound(stderr []byte) bool {
	return bytes.Contains(stderr, []byte("No such file or directory"))
}
