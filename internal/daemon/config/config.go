// Package config defines and loads the jacod.yaml daemon configuration.
// Schema is closed — additional keys produce an unknown-field error so
// operators can't silently set typos that the daemon ignores.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults match slices/daemon.md §4.
//
// listen_addr / cluster_addr default to an unspecified host (0.0.0.0) so
// the bind FOLLOWS the auto-detected advertise face rather than listening
// on every interface. At startup cmd/jacod resolves a private-LAN-first IP
// via internal/daemon/netdetect and binds the gRPC (7000) + raft (7001)
// planes to THAT face — the control/data plane is not world-reachable on a
// public NIC by default (issue #44), so JACO does not depend on an external
// firewall for that boundary. netdetect never auto-picks a public address;
// a host with only a public face fails fast with guidance to pin one.
//
// Operators pin a specific routable host:port to override — required for
// overlay-only clusters whose nodes share no private LAN, and useful on
// multi-NIC hosts. A pinned value is honored verbatim for both bind and
// advertise.
const (
	DefaultDataDir     = "/var/lib/jaco"
	DefaultListenAddr  = "0.0.0.0:7000"
	DefaultClusterAddr = "0.0.0.0:7001"
	DefaultUnixSocket  = "/var/run/jaco/jaco.sock"
	DefaultWGPort      = 51820
	DefaultLogLevel    = "info"
	DefaultIPAMPool    = "10.244.0.0/16"

	// DefaultACMECA is the Let's Encrypt production ACME directory — the CA
	// JACO issues against unless the operator pins acme_ca to staging (or any
	// other ACME provider). Stage-first issuance (issue #41) runs against
	// ACMEStagingCA before flipping to this URL.
	DefaultACMECA = "https://acme-v02.api.letsencrypt.org/directory"
	// ACMEStagingCA is the Let's Encrypt staging directory used for the
	// stage-first dry run. Staging has far looser rate limits, so a
	// DNS/firewall misconfig burns a cheap staging failure instead of a prod
	// rate-limit hit.
	ACMEStagingCA = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

// Config is the typed view of jacod.yaml.
type Config struct {
	// DataDir holds raft store, snapshots, node certs, wg keys.
	DataDir string `yaml:"data_dir"`
	// ListenAddr is the cluster gRPC TLS endpoint (peers + remote CLI).
	ListenAddr string `yaml:"listen_addr"`
	// ClusterAddr is the raft TCP transport listen address (peer-to-peer
	// replication). Must be a different port from ListenAddr.
	ClusterAddr string `yaml:"cluster_addr"`
	// UnixSocket is the local-control socket path (CLI ↔ daemon).
	UnixSocket string `yaml:"unix_socket"`
	// WGPort is the WireGuard UDP listen port.
	WGPort int `yaml:"wg_port"`
	// ACMEEmail is the contact address registered with ACME. Empty disables.
	ACMEEmail string `yaml:"acme_email"`
	// ACMECA is the ACME directory URL the issuer targets. Empty → LE prod
	// (DefaultACMECA). Operators pin staging here for a dev/test cluster.
	ACMECA string `yaml:"acme_ca"`
	// ACMEEnabled is the cluster-wide ACME switch. When false the daemon does
	// not register the ACME issuer and the rendered Caddy config carries no
	// tls.automation block — verifiable without any outbound ACME call. nil
	// (key absent) defaults to true; an explicit `acme_enabled: false` opts
	// the whole cluster out so an operator can plug in their own cert pipeline.
	ACMEEnabled *bool `yaml:"acme_enabled"`
	// ACMESkipStaging opts out of the stage-first dry run (issue #41). When
	// false (default) new domains issue against LE staging first, then flip to
	// prod on success. Automatically skipped when ACMECA is already non-prod.
	ACMESkipStaging bool `yaml:"acme_skip_staging"`
	// LogLevel is one of debug | info | warn | error.
	LogLevel string `yaml:"log_level"`
	// IPAMPool is the per-deployment subnet allocator CIDR (must be /16).
	IPAMPool string `yaml:"ipam_pool"`
	// Scheduler holds the scheduler subsystem's optional knobs. The only
	// member today is the pressure-based rebalancer (issue #92, ADR
	// 0002). The block is OPTIONAL: omitting it leaves the rebalancer
	// unconfigured (no loop spawned). Setting `scheduler.rebalance:`
	// at all — even `enabled: false` — counts as "configured" and
	// starts the loop in dry-run mode so operators can evaluate the
	// policy from the audit log before turning it on.
	Scheduler *SchedulerConfig `yaml:"scheduler"`
}


// SchedulerConfig groups scheduler subsystem knobs that are off by
// default and surface only when the operator opts in. Today: just the
// rebalancer. Future scheduler tunables (e.g. reconcile cadence,
// rollout pacing) live here too.
type SchedulerConfig struct {
	Rebalance *RebalanceConfig `yaml:"rebalance"`
}

// RebalanceConfig is the yaml view of the pressure-rebalancer's knobs.
// Field documentation lives in internal/scheduler/rebalance/config.go;
// the daemon just transports values. Pointer-typed numeric fields are
// avoided — the rebalancer fills missing values from its own
// DefaultConfig() at construction time, so "zero" means "use default".
type RebalanceConfig struct {
	Enabled           bool          `yaml:"enabled"`
	TriggerThreshold  float64       `yaml:"trigger_threshold"`
	ImbalanceGap      float64       `yaml:"imbalance_gap"`
	ReliefFloor       float64       `yaml:"relief_floor"`
	DstCap            float64       `yaml:"dst_cap"`
	CooldownReplica   time.Duration `yaml:"cooldown_replica"`
	CooldownNode      time.Duration `yaml:"cooldown_node"`
	CycleInterval     time.Duration `yaml:"cycle_interval"`
	ConsecutiveCycles int           `yaml:"consecutive_cycles"`
	ReplicaSoftCap    int           `yaml:"replica_soft_cap"`
}

// ACMEEnabledOrDefault returns the effective cluster-wide ACME switch. The
// key is a *bool so "absent" (default true) is distinguishable from an
// explicit "acme_enabled: false".
func (c Config) ACMEEnabledOrDefault() bool {
	if c.ACMEEnabled == nil {
		return true
	}
	return *c.ACMEEnabled
}

// ACMECAOrDefault returns the configured ACME directory URL, or LE prod when
// the operator left acme_ca unset.
func (c Config) ACMECAOrDefault() string {
	if c.ACMECA == "" {
		return DefaultACMECA
	}
	return c.ACMECA
}

// Defaults returns a Config populated with the documented defaults.
func Defaults() Config {
	return Config{
		DataDir:     DefaultDataDir,
		ListenAddr:  DefaultListenAddr,
		ClusterAddr: DefaultClusterAddr,
		UnixSocket:  DefaultUnixSocket,
		WGPort:      DefaultWGPort,
		LogLevel:    DefaultLogLevel,
		IPAMPool:    DefaultIPAMPool,
	}
}

// Load reads jacod.yaml at path and returns a fully-populated Config with
// defaults filled in. Returns (Defaults(), nil) when path doesn't exist —
// the daemon should accept "no config file" as "all defaults".
func Load(path string) (Config, error) {
	cfg := Defaults()

	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	return LoadBytes(body)
}

// LoadBytes parses jacod.yaml bytes (for testing). Unknown keys fail; the
// schema is closed.
func LoadBytes(body []byte) (Config, error) {
	cfg := Defaults()
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse jacod.yaml: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks a Config for shape errors. Called by Load; safe to call
// again after manual mutation.
func (c Config) Validate() error {
	if c.DataDir == "" {
		return fmt.Errorf("data_dir is required")
	}
	if c.ListenAddr == "" {
		return fmt.Errorf("listen_addr is required")
	}
	if _, _, err := net.SplitHostPort(c.ListenAddr); err != nil {
		return fmt.Errorf("listen_addr %q is not host:port: %w", c.ListenAddr, err)
	}
	if c.ClusterAddr == "" {
		return fmt.Errorf("cluster_addr is required")
	}
	if _, _, err := net.SplitHostPort(c.ClusterAddr); err != nil {
		return fmt.Errorf("cluster_addr %q is not host:port: %w", c.ClusterAddr, err)
	}
	if c.ClusterAddr == c.ListenAddr {
		return fmt.Errorf("cluster_addr and listen_addr must differ")
	}
	if c.UnixSocket == "" {
		return fmt.Errorf("unix_socket is required")
	}
	if c.WGPort <= 0 || c.WGPort > 65535 {
		return fmt.Errorf("wg_port %d is out of range", c.WGPort)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level %q must be debug|info|warn|error", c.LogLevel)
	}
	if c.ACMECA != "" {
		u, err := url.Parse(c.ACMECA)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("acme_ca %q must be an https URL", c.ACMECA)
		}
	}
	if c.IPAMPool == "" {
		return fmt.Errorf("ipam_pool is required")
	}
	if _, ipNet, err := net.ParseCIDR(c.IPAMPool); err != nil {
		return fmt.Errorf("ipam_pool %q is not a CIDR: %w", c.IPAMPool, err)
	} else if ones, _ := ipNet.Mask.Size(); ones != 16 {
		return fmt.Errorf("ipam_pool %q must be a /16 (got /%d)", c.IPAMPool, ones)
	}
	return nil
}
