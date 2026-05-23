// Package config defines and loads the jacod.yaml daemon configuration.
// Schema is closed — additional keys produce an unknown-field error so
// operators can't silently set typos that the daemon ignores.
package config

import (
	"fmt"
	"net"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Defaults match slices/daemon.md §4.
const (
	DefaultDataDir     = "/var/lib/jaco"
	DefaultListenAddr  = "0.0.0.0:7000"
	DefaultClusterAddr = "0.0.0.0:7001"
	DefaultUnixSocket  = "/var/run/jaco/jaco.sock"
	DefaultWGPort      = 51820
	DefaultLogLevel    = "info"
	DefaultIPAMPool    = "10.244.0.0/16"
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
	// LogLevel is one of debug | info | warn | error.
	LogLevel string `yaml:"log_level"`
	// IPAMPool is the per-deployment subnet allocator CIDR (must be /16).
	IPAMPool string `yaml:"ipam_pool"`
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
