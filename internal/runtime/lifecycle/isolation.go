package lifecycle

import (
	"errors"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// ErrIsolationUnavailable is the typed error CheckIsolationAvailable returns
// when the local Node entity reports status=isolation_unavailable. The
// daemon entry calls this before lifecycle.Start so containers aren't
// created on a node whose nftables ruleset has drifted out of sync.
var ErrIsolationUnavailable = errors.New("isolation_unavailable")

// CheckIsolationAvailable returns ErrIsolationUnavailable when state shows
// the local node's status is NODE_STATUS_ISOLATION_UNAVAILABLE. Returns nil
// otherwise (READY, JOINING, or even an unknown status all pass — the gate
// is specifically about isolation failures, not other status states).
func CheckIsolationAvailable(st *state.State, selfHostname string) error {
	if st == nil || selfHostname == "" {
		return nil
	}
	n, ok := st.Nodes.Get(selfHostname)
	if !ok {
		return nil
	}
	if n.GetStatus() == pb.NodeStatus_NODE_STATUS_ISOLATION_UNAVAILABLE {
		return ErrIsolationUnavailable
	}
	return nil
}
