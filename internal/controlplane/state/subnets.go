package state

import (
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// Subnets are keyed by (deployment, network, host). The separator "\x00" is
// reserved — none of the user-facing identifiers contain it.
const subnetKeySep = "\x00"

func newSubnets(b *watch.Broker[*pb.Subnet]) *Store[*pb.Subnet] {
	return NewStore(b, func(s *pb.Subnet) string {
		return SubnetKey(s.GetDeployment(), s.GetNetwork(), s.GetHost())
	})
}

// SubnetKey is the canonical composite-key helper used by callers (IPAM,
// discovery) so they don't need to know the separator.
func SubnetKey(deployment, network, host string) string {
	return deployment + subnetKeySep + network + subnetKeySep + host
}
