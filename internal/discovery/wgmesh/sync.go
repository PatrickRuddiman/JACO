package wgmesh

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os/exec"
	"sort"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// DefaultSyncInterval is how often Syncer.Run reconciles the live wg
// interface against state.Nodes. Short enough that membership changes
// propagate to the mesh within ~half a minute.
const DefaultSyncInterval = 30 * time.Second

// DefaultInterface is the wg device JACO owns by default.
const DefaultInterface = "jaco0"

// EnsureInterface brings up name as a wireguard device if it doesn't
// already exist. Idempotent — returns nil when the device is present
// already. On hosts without CAP_NET_ADMIN the `ip link add` shells out
// and fails; the daemon logs the error and proceeds without mesh
// (Syncer.tick will then log its own ConfigureDevice failure once).
func EnsureInterface(name string) error {
	c, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("EnsureInterface: open wgctrl: %w", err)
	}
	defer c.Close()
	if _, err := c.Device(name); err == nil {
		return nil // already exists
	}
	cmd := exec.Command("ip", "link", "add", name, "type", "wireguard")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("EnsureInterface: ip link add %s: %w: %s", name, err, string(out))
	}
	// Bring the link up.
	up := exec.Command("ip", "link", "set", name, "up")
	if out, err := up.CombinedOutput(); err != nil {
		return fmt.Errorf("EnsureInterface: ip link set %s up: %w: %s", name, err, string(out))
	}
	return nil
}

// IsKernelAvailable returns nil when the kernel WireGuard module is
// reachable via wgctrl. Returns the underlying wgctrl.New error otherwise
// — typical causes are missing CONFIG_WIREGUARD, missing CAP_NET_ADMIN,
// or running inside an unprivileged container.
func IsKernelAvailable() error {
	c, err := wgctrl.New()
	if err != nil {
		return err
	}
	_ = c.Close()
	return nil
}

// Syncer reconciles the local wg interface with the desired peer set
// derived from state.Nodes on a 30s cadence.
type Syncer struct {
	Iface        string
	Interval     time.Duration
	State        *state.State
	SelfHostname string
	PrivateKey   wgtypes.Key
	Logger       *log.Logger

	// loggedConfigError suppresses repeat ConfigureDevice warnings — the
	// most common reason it fails (no such device / permission denied) is
	// the same every tick, so the operator only needs to hear about it
	// once per daemon lifetime.
	loggedConfigError bool

	// loggedRouteError does the same for the route-reconcile step (jaco0
	// absent / lacking CAP_NET_ADMIN repeats every tick).
	loggedRouteError bool
}

// Run blocks until ctx is cancelled. Each tick computes the desired
// wgtypes.Config from state.Nodes and ConfigureDevices it onto the kernel
// wg interface. Returns nil + logs a one-line warning when wgctrl.New
// fails (kernel WG unavailable) — the daemon should already have skipped
// spawning this goroutine via IsKernelAvailable, but stay defensive.
func (s *Syncer) Run(ctx context.Context) error {
	if s.Logger == nil {
		s.Logger = log.Default()
	}
	if s.Iface == "" {
		s.Iface = DefaultInterface
	}
	if s.Interval == 0 {
		s.Interval = DefaultSyncInterval
	}
	if s.SelfHostname == "" {
		return errors.New("wgmesh.Syncer: SelfHostname is required")
	}

	client, err := wgctrl.New()
	if err != nil {
		s.Logger.Printf("wgmesh.Syncer.Run: wgctrl unavailable (%v), exiting cleanly", err)
		return nil
	}
	defer client.Close()

	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()

	// Initial reconcile.
	s.tick(client)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.tick(client)
		}
	}
}

func (s *Syncer) tick(client *wgctrl.Client) {
	cfg, err := BuildConfig(s.State, s.SelfHostname, s.PrivateKey)
	if err != nil {
		s.Logger.Printf("wgmesh build config: %v", err)
		return
	}
	if err := client.ConfigureDevice(s.Iface, cfg); err != nil {
		// Most common failure is "operation not permitted" (daemon
		// lacks CAP_NET_ADMIN) or "no such device" (operator hasn't
		// created jaco0 yet) — both repeat every tick. Log once.
		if !s.loggedConfigError {
			s.Logger.Printf("wgmesh ConfigureDevice %s: %v (mesh disabled; only logged once)", s.Iface, err)
			s.loggedConfigError = true
		}
	}
	s.reconcileRoutes()
}

// reconcileRoutes installs a kernel route over the wg interface for every
// OTHER host's container /24 and removes routes for subnets that no longer
// exist. The WG mesh then carries the cross-host packets (issue #28). JACO is
// the only thing that adds routes to this interface, so current-minus-desired
// is exactly the set of orphaned JACO routes.
func (s *Syncer) reconcileRoutes() {
	current, err := listRoutes(s.Iface)
	if err != nil {
		if !s.loggedRouteError {
			s.Logger.Printf("wgmesh route list %s: %v (route reconcile disabled; only logged once)", s.Iface, err)
			s.loggedRouteError = true
		}
		return
	}
	desired := s.desiredRoutes()
	_, del := routeDiff(desired, current)
	// Source-address hint: host-originated traffic to a peer's container /24
	// (the embedded ingress proxying to a replica on another host) must carry
	// a pool source so the destination host's pool→pool firewall exemption
	// admits it — jaco0 itself has no address, so without this the kernel
	// picks a non-pool source and the far side drops the packet (issue #28).
	// `src` only affects locally-originated packets; forwarded container↔
	// container traffic is untouched. Empty until a local bridge exists; a
	// later tick adds it once one does. `ip route replace` re-asserts every
	// tick so a route installed before a bridge existed gains the src.
	gw := s.localPoolGateway()
	for _, cidr := range desired {
		args := []string{"route", "replace", cidr, "dev", s.Iface}
		if gw != "" {
			args = append(args, "src", gw)
		}
		if out, rerr := exec.Command("ip", args...).CombinedOutput(); rerr != nil {
			s.Logger.Printf("wgmesh route replace %s dev %s src %q: %v: %s", cidr, s.Iface, gw, rerr, string(out))
		}
	}
	for _, cidr := range del {
		if out, rerr := exec.Command("ip", "route", "del", cidr, "dev", s.Iface).CombinedOutput(); rerr != nil {
			s.Logger.Printf("wgmesh route del %s dev %s: %v: %s", cidr, s.Iface, rerr, string(out))
		}
	}
}

// localPoolGateway returns a pool IP assigned to this node — the .1 gateway of
// one of its local container /24s — for use as the `src` on peer routes so
// host-originated overlay traffic (e.g. the ingress proxying cross-host) has a
// pool source address. Returns "" when the node has no local subnet yet (the
// route is then installed without a src and gains one on a later tick once a
// bridge exists). Deterministic: the lexicographically-first local CIDR.
func (s *Syncer) localPoolGateway() string {
	var cidrs []string
	for _, sn := range s.State.Subnets.List() {
		if sn.GetHost() == s.SelfHostname {
			cidrs = append(cidrs, sn.GetCidr())
		}
	}
	if len(cidrs) == 0 {
		return ""
	}
	sort.Strings(cidrs)
	_, ipnet, err := net.ParseCIDR(cidrs[0])
	if err != nil {
		return ""
	}
	gw := make(net.IP, len(ipnet.IP))
	copy(gw, ipnet.IP)
	gw[len(gw)-1] |= 1 // .1 of the /24 — Docker's bridge gateway address
	return gw.String()
}

// desiredRoutes is every container /24 owned by a host other than self.
func (s *Syncer) desiredRoutes() []string {
	var out []string
	for _, sn := range s.State.Subnets.List() {
		if sn.GetHost() == "" || sn.GetHost() == s.SelfHostname {
			continue
		}
		out = append(out, sn.GetCidr())
	}
	return out
}

// listRoutes parses `ip route show dev <iface>` plain-text output (no
// -j/JSON, for busybox compatibility) into the set of CIDRs routed over the
// interface.
func listRoutes(iface string) ([]string, error) {
	out, err := exec.Command("ip", "route", "show", "dev", iface).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ip route show dev %s: %w: %s", iface, err, string(out))
	}
	return parseRouteCIDRs(string(out)), nil
}

// parseRouteCIDRs extracts the destination prefix (first token) of each
// non-empty line, keeping only CIDR-form entries (a.b.c.d/n). JACO only
// installs /24s, so default/scope/link lines without a prefix are skipped.
func parseRouteCIDRs(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		dst := fields[0]
		if !strings.Contains(dst, "/") {
			continue
		}
		if _, _, err := net.ParseCIDR(dst); err != nil {
			continue
		}
		out = append(out, dst)
	}
	return out
}

// routeDiff returns the CIDRs to add and remove so the live route set
// (current) matches desired. Output is sorted for deterministic application.
func routeDiff(desired, current []string) (add, del []string) {
	want := make(map[string]bool, len(desired))
	for _, c := range desired {
		want[c] = true
	}
	have := make(map[string]bool, len(current))
	for _, c := range current {
		have[c] = true
	}
	for c := range want {
		if !have[c] {
			add = append(add, c)
		}
	}
	for c := range have {
		if !want[c] {
			del = append(del, c)
		}
	}
	sort.Strings(add)
	sort.Strings(del)
	return add, del
}

// BuildConfig is the typed counterpart to RenderConfig. Returns a
// wgtypes.Config ready for wgctrl.ConfigureDevice. ReplacePeers is true
// so each reconcile fully overwrites the peer list (membership changes
// drop removed peers as well as add new ones).
func BuildConfig(st *state.State, selfHostname string, selfPrivate wgtypes.Key) (wgtypes.Config, error) {
	if selfHostname == "" {
		return wgtypes.Config{}, errors.New("BuildConfig: selfHostname is required")
	}
	port := DefaultListenPort
	cfg := wgtypes.Config{
		PrivateKey:   &selfPrivate,
		ListenPort:   &port,
		ReplacePeers: true,
	}
	for _, n := range st.Nodes.List() {
		if n.GetHostname() == selfHostname {
			continue
		}
		if len(n.GetWireguardPubkey()) == 0 {
			continue
		}
		pubKey, err := keyFromBytes(n.GetWireguardPubkey())
		if err != nil {
			return wgtypes.Config{}, fmt.Errorf("peer %s pubkey: %w", n.GetHostname(), err)
		}
		ip, _, err := net.ParseCIDR(AllowedIP(n.GetHostname()))
		if err != nil {
			return wgtypes.Config{}, fmt.Errorf("peer %s allowed ip: %w", n.GetHostname(), err)
		}
		host := stripPort(n.GetAddress())
		endpointIP := net.ParseIP(host)
		if endpointIP == nil {
			// Best-effort: skip peers whose address didn't parse rather
			// than fail the whole reconcile.
			continue
		}
		// AllowedIPs is WireGuard's crypto-routing table: the peer's /32 mesh
		// address PLUS every container /24 that peer owns (issue #28). Without
		// the /24s, packets a kernel route sends to jaco0 for a peer's
		// containers would be dropped as having no matching peer.
		allowed := []net.IPNet{{IP: ip, Mask: net.CIDRMask(32, 32)}}
		for _, sn := range st.Subnets.List() {
			if sn.GetHost() != n.GetHostname() {
				continue
			}
			if _, ipnet, perr := net.ParseCIDR(sn.GetCidr()); perr == nil && ipnet != nil {
				allowed = append(allowed, *ipnet)
			}
		}
		keepalive := 25 * time.Second
		cfg.Peers = append(cfg.Peers, wgtypes.PeerConfig{
			PublicKey:                   pubKey,
			Endpoint:                    &net.UDPAddr{IP: endpointIP, Port: DefaultListenPort},
			AllowedIPs:                  allowed,
			PersistentKeepaliveInterval: &keepalive,
			ReplaceAllowedIPs:           true,
		})
	}
	// silence unused if any peer slipped through with no pb.Node fields.
	_ = pb.Node{}
	return cfg, nil
}
