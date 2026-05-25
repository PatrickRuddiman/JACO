package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/daemon/config"
)

func TestLoad_MissingFileReturnsDefaults(t *testing.T) {
	got, err := config.Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	want := config.Defaults()
	if got != want {
		t.Errorf("missing file → %+v, want defaults %+v", got, want)
	}
}

func TestLoad_FullFileOverridesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jacod.yaml")
	body := `data_dir: /opt/jaco
listen_addr: 10.0.0.1:7000
cluster_addr: 10.0.0.1:7001
unix_socket: /run/jaco.sock
wg_port: 41641
acme_email: ops@example.com
log_level: debug
ipam_pool: 10.42.0.0/16
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.DataDir != "/opt/jaco" || got.ListenAddr != "10.0.0.1:7000" || got.UnixSocket != "/run/jaco.sock" {
		t.Errorf("paths wrong: %+v", got)
	}
	if got.WGPort != 41641 || got.ACMEEmail != "ops@example.com" || got.LogLevel != "debug" || got.IPAMPool != "10.42.0.0/16" {
		t.Errorf("scalars wrong: %+v", got)
	}
}

func TestLoad_PartialFileFillsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jacod.yaml")
	if err := os.WriteFile(path, []byte("acme_email: ops@example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.ACMEEmail != "ops@example.com" {
		t.Errorf("ACMEEmail = %q", got.ACMEEmail)
	}
	d := config.Defaults()
	if got.DataDir != d.DataDir || got.ListenAddr != d.ListenAddr || got.WGPort != d.WGPort {
		t.Errorf("partial file didn't fill defaults: %+v", got)
	}
}

func TestLoad_UnknownFieldRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jacod.yaml")
	if err := os.WriteFile(path, []byte("foo_bar: 42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load(path)
	if err == nil {
		t.Fatalf("expected unknown-field error")
	}
	if !strings.Contains(err.Error(), "foo_bar") && !strings.Contains(err.Error(), "field foo_bar") {
		t.Errorf("err = %v; should mention foo_bar", err)
	}
}

func TestValidate_RejectsBadValues(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*config.Config)
		match string
	}{
		{"empty data_dir", func(c *config.Config) { c.DataDir = "" }, "data_dir"},
		{"empty listen_addr", func(c *config.Config) { c.ListenAddr = "" }, "listen_addr"},
		{"bad listen_addr", func(c *config.Config) { c.ListenAddr = "notavalidaddr" }, "listen_addr"},
		{"empty cluster_addr", func(c *config.Config) { c.ClusterAddr = "" }, "cluster_addr"},
		{"bad cluster_addr", func(c *config.Config) { c.ClusterAddr = "notavalidaddr" }, "cluster_addr"},
		{"matching listen+cluster", func(c *config.Config) { c.ClusterAddr = c.ListenAddr }, "must differ"},
		{"empty unix_socket", func(c *config.Config) { c.UnixSocket = "" }, "unix_socket"},
		{"wg_port=0", func(c *config.Config) { c.WGPort = 0 }, "wg_port"},
		{"wg_port=99999", func(c *config.Config) { c.WGPort = 99999 }, "wg_port"},
		{"bad log_level", func(c *config.Config) { c.LogLevel = "trace" }, "log_level"},
		{"empty ipam_pool", func(c *config.Config) { c.IPAMPool = "" }, "ipam_pool"},
		{"non-CIDR ipam_pool", func(c *config.Config) { c.IPAMPool = "not-a-cidr" }, "ipam_pool"},
		{"wrong mask ipam_pool", func(c *config.Config) { c.IPAMPool = "10.0.0.0/8" }, "/16"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.Defaults()
			c.mut(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), c.match) {
				t.Errorf("err %q should contain %q", err.Error(), c.match)
			}
		})
	}
}

func TestLoadBytes_GarbageYAML(t *testing.T) {
	_, err := config.LoadBytes([]byte("this is: not: valid: yaml: ["))
	if err == nil {
		t.Errorf("expected parse error on garbage")
	}
}

func TestDefaults_PassValidation(t *testing.T) {
	if err := config.Defaults().Validate(); err != nil {
		t.Errorf("defaults failed validation: %v", err)
	}
}

func TestACMEDefaults_WhenKeysAbsent(t *testing.T) {
	cfg, err := config.LoadBytes([]byte("acme_email: ops@example.com\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ACMEEnabledOrDefault() {
		t.Errorf("acme_enabled default = false; want true when absent")
	}
	if cfg.ACMECAOrDefault() != config.DefaultACMECA {
		t.Errorf("acme_ca default = %q; want %q", cfg.ACMECAOrDefault(), config.DefaultACMECA)
	}
	if cfg.ACMESkipStaging {
		t.Errorf("acme_skip_staging default = true; want false")
	}
}

func TestACMEEnabledFalse_Parsed(t *testing.T) {
	cfg, err := config.LoadBytes([]byte("acme_enabled: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ACMEEnabled == nil {
		t.Fatalf("acme_enabled: false left ACMEEnabled nil (can't distinguish from absent)")
	}
	if cfg.ACMEEnabledOrDefault() {
		t.Errorf("acme_enabled: false → ACMEEnabledOrDefault() true")
	}
}

func TestACMECA_PinStaging(t *testing.T) {
	cfg, err := config.LoadBytes([]byte("acme_ca: " + config.ACMEStagingCA + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ACMECAOrDefault() != config.ACMEStagingCA {
		t.Errorf("acme_ca = %q; want staging", cfg.ACMECAOrDefault())
	}
}

func TestACMESkipStaging_Parsed(t *testing.T) {
	cfg, err := config.LoadBytes([]byte("acme_skip_staging: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ACMESkipStaging {
		t.Errorf("acme_skip_staging: true not parsed")
	}
}

func TestValidate_RejectsBadACMECA(t *testing.T) {
	cfg := config.Defaults()
	cfg.ACMECA = "ftp://nope"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "acme_ca") {
		t.Errorf("bad acme_ca err = %v; want mention of acme_ca", err)
	}
	cfg.ACMECA = "not a url at all ::::"
	if err := cfg.Validate(); err == nil {
		t.Errorf("malformed acme_ca accepted")
	}
}

func TestACMEEnabled_UnknownFieldStillRejected(t *testing.T) {
	// Closed-schema invariant must survive the new keys.
	_, err := config.LoadBytes([]byte("acme_enabledd: false\n"))
	if err == nil {
		t.Fatalf("typo acme_enabledd accepted; schema not closed")
	}
}
