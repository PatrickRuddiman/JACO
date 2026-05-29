// Package ipam owns deterministic /24 allocation for the JACO IPAM pool. The
// pool defaults to 10.244.0.0/16 (per the discovery slice §3) — 256 /24
// subnets keyed by (deployment, network). Allocations live in raft as
// Subnet entities so the assignment survives crashes and leader failover.
//
// Single-writer: only the scheduler (raft leader) calls Allocate / Free.
// Reads are safe from any node because state.Subnets is a watched store.
package ipam

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/logging"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// DefaultPoolCIDR is the JACO default IPAM pool. 256 /24 subnets available.
const DefaultPoolCIDR = "10.244.0.0/16"

// Applier wraps raft.Apply.
type Applier func(cmd []byte) error

// IPAMError is the typed error Allocate returns when the pool is exhausted
// or the requested CIDR shape is invalid.
type IPAMError struct {
	Code    string
	Message string
}

// Error implements the error interface.
func (e *IPAMError) Error() string { return e.Message }

// IsExhausted reports whether err is the pool-exhausted error.
func IsExhausted(err error) bool {
	var e *IPAMError
	if errors.As(err, &e) {
		return e.Code == "ipam_pool_exhausted"
	}
	return false
}

// IPAM allocates /24 subnets out of the configured pool.
type IPAM struct {
	state *state.State
	apply Applier
	pool  *net.IPNet

	// mu serializes the read-nextFree-apply sequence so concurrent
	// EnsureSubnet calls on the leader can't pick the same /24.
	mu sync.Mutex

	// Logger logs allocate/release at DEBUG and pool exhaustion at ERROR.
	// nil → discard. Set by the daemon after construction.
	Logger *slog.Logger
}

func (i *IPAM) log() *slog.Logger {
	if i.Logger == nil {
		return logging.Discard()
	}
	return i.Logger
}

// New constructs an IPAM allocator. poolCIDR must be a /16; pass
// DefaultPoolCIDR for the JACO default.
func New(s *state.State, apply Applier, poolCIDR string) (*IPAM, error) {
	if poolCIDR == "" {
		poolCIDR = DefaultPoolCIDR
	}
	_, pool, err := net.ParseCIDR(poolCIDR)
	if err != nil {
		return nil, fmt.Errorf("ipam: invalid pool %q: %w", poolCIDR, err)
	}
	ones, bits := pool.Mask.Size()
	if bits != 32 || ones != 16 {
		return nil, fmt.Errorf("ipam: pool %q must be IPv4 /16 (got /%d)", poolCIDR, ones)
	}
	return &IPAM{state: s, apply: apply, pool: pool}, nil
}

// Allocate idempotently assigns a /24 to (deployment, network, host).
// Returns the pre-existing Subnet when one's already on file; otherwise picks
// the next free /24 inside the pool and raft-Applies SubnetAllocate. The
// read-nextFree-apply sequence is serialized by i.mu so concurrent callers on
// the leader never pick the same /24.
func (i *IPAM) Allocate(deployment, network, host string) (*pb.Subnet, error) {
	if deployment == "" || network == "" || host == "" {
		return nil, fmt.Errorf("Allocate: deployment + network + host are required")
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	if existing, ok := i.state.Subnets.Get(state.SubnetKey(deployment, network, host)); ok {
		return existing, nil
	}

	cidr, err := i.nextFree()
	if err != nil {
		if IsExhausted(err) {
			i.log().Error("ipam pool exhausted",
				logging.KeyDeployment, deployment, "network", network, "host", host)
		}
		return nil, err
	}

	cmd := &pb.Command{
		Identity: "scheduler",
		Ts:       timestamppb.Now(),
		Payload: &pb.Command_SubnetAllocate{SubnetAllocate: &pb.SubnetAllocate{
			Deployment: deployment,
			Network:    network,
			Cidr:       cidr,
			Host:       host,
		}},
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("marshal SubnetAllocate: %w", err)
	}
	if err := i.apply(data); err != nil {
		return nil, fmt.Errorf("raft apply: %w", err)
	}

	allocated, _ := i.state.Subnets.Get(state.SubnetKey(deployment, network, host))
	i.log().Debug("ipam allocated subnet",
		logging.KeyDeployment, deployment, "network", network, "host", host, "cidr", cidr)
	return allocated, nil
}

// Free releases the /24 owned by (deployment, network, host). No-op on missing.
func (i *IPAM) Free(deployment, network, host string) error {
	if deployment == "" || network == "" || host == "" {
		return fmt.Errorf("Free: deployment + network + host are required")
	}
	if _, ok := i.state.Subnets.Get(state.SubnetKey(deployment, network, host)); !ok {
		return nil
	}
	cmd := &pb.Command{
		Identity: "scheduler",
		Ts:       timestamppb.Now(),
		Payload: &pb.Command_SubnetFree{SubnetFree: &pb.SubnetFree{
			Deployment: deployment,
			Network:    network,
			Host:       host,
		}},
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}
	if err := i.apply(data); err != nil {
		return err
	}
	i.log().Debug("ipam released subnet",
		logging.KeyDeployment, deployment, "network", network, "host", host)
	return nil
}

// nextFree returns the next unused /24 inside the pool. Walks the existing
// Subnets to find which third-octet values are taken, then picks the
// lowest-numbered free one. Returns ipam_pool_exhausted when all 256 slots
// are taken.
func (i *IPAM) nextFree() (string, error) {
	taken := make(map[uint8]bool, 256)
	for _, s := range i.state.Subnets.List() {
		o, ok := thirdOctet(s.GetCidr())
		if !ok {
			continue
		}
		taken[o] = true
	}

	for n := 0; n < 256; n++ {
		o := uint8(n)
		if taken[o] {
			continue
		}
		// Build the /24 inside the pool.
		ip := append([]byte(nil), i.pool.IP.To4()...)
		ip[2] = o
		ip[3] = 0
		return fmt.Sprintf("%s/24", net.IP(ip).String()), nil
	}
	return "", &IPAMError{
		Code:    "ipam_pool_exhausted",
		Message: fmt.Sprintf("ipam pool %s is fully allocated (256 / 256 /24s)", i.pool.String()),
	}
}

// thirdOctet extracts the third octet from `a.b.c.d/24` strings.
func thirdOctet(cidr string) (uint8, bool) {
	slash := strings.IndexByte(cidr, '/')
	if slash < 0 {
		return 0, false
	}
	ip := net.ParseIP(cidr[:slash])
	if ip == nil {
		return 0, false
	}
	ip = ip.To4()
	if ip == nil {
		return 0, false
	}
	return ip[2], true
}

// allocatedCIDRs returns the existing Subnet CIDRs sorted for deterministic
// test output.
func allocatedCIDRs(s *state.State) []string {
	out := make([]string, 0, s.Subnets.Len())
	for _, sub := range s.Subnets.List() {
		out = append(out, sub.GetCidr())
	}
	sort.Strings(out)
	return out
}
