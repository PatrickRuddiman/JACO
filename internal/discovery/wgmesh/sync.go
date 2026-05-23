package wgmesh

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
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
		s.Logger.Printf("wgmesh ConfigureDevice %s: %v", s.Iface, err)
	}
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
		keepalive := 25 * time.Second
		cfg.Peers = append(cfg.Peers, wgtypes.PeerConfig{
			PublicKey:                   pubKey,
			Endpoint:                    &net.UDPAddr{IP: endpointIP, Port: DefaultListenPort},
			AllowedIPs:                  []net.IPNet{{IP: ip, Mask: net.CIDRMask(32, 32)}},
			PersistentKeepaliveInterval: &keepalive,
			ReplaceAllowedIPs:           true,
		})
	}
	// silence unused if any peer slipped through with no pb.Node fields.
	_ = pb.Node{}
	return cfg, nil
}
