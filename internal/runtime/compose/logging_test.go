package compose_test

import (
	"path/filepath"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

// TestToContainerSpec_ModernLogging — a service's modern `logging:` block
// (driver + options incl. tag) projects onto ContainerSpec.LogConfig.
func TestToContainerSpec_ModernLogging(t *testing.T) {
	project, err := compose.LoadBytes([]byte(`services:
  web:
    image: nginx
    logging:
      driver: json-file
      options:
        tag: "{{.Name}}"
        max-size: "10m"
        max-file: "3"
`), "memory.yml")
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	s, ok := lookupService(project, "web")
	if !ok {
		t.Fatal("web service missing")
	}
	spec := compose.ToContainerSpec(s, compose.SpecOptions{Deployment: "sample", Service: "web"})
	if spec.LogConfig == nil {
		t.Fatal("LogConfig nil; want json-file driver populated")
	}
	if spec.LogConfig.Driver != "json-file" {
		t.Errorf("Driver = %q, want json-file", spec.LogConfig.Driver)
	}
	if got := spec.LogConfig.Options["tag"]; got != "{{.Name}}" {
		t.Errorf("Options[tag] = %q, want {{.Name}}", got)
	}
	if got := spec.LogConfig.Options["max-size"]; got != "10m" {
		t.Errorf("Options[max-size] = %q, want 10m", got)
	}
	if got := spec.LogConfig.Options["max-file"]; got != "3" {
		t.Errorf("Options[max-file] = %q, want 3", got)
	}
}

// TestToContainerSpec_LegacyLogging — the legacy top-level log_driver/log_opt
// keys project onto LogConfig when no modern block is present (fixture service
// "legacy").
func TestToContainerSpec_LegacyLogging(t *testing.T) {
	project, _, err := compose.Load(filepath.Join("testdata", "logging.yml"))
	if err != nil {
		t.Fatalf("Load logging.yml: %v", err)
	}
	s, ok := lookupService(project, "legacy")
	if !ok {
		t.Fatal("legacy service missing")
	}
	spec := compose.ToContainerSpec(s, compose.SpecOptions{Deployment: "sample", Service: "legacy"})
	if spec.LogConfig == nil {
		t.Fatal("LogConfig nil; want syslog from legacy keys")
	}
	if spec.LogConfig.Driver != "syslog" {
		t.Errorf("Driver = %q, want syslog", spec.LogConfig.Driver)
	}
	if got := spec.LogConfig.Options["syslog-address"]; got != "tcp://127.0.0.1:514" {
		t.Errorf("Options[syslog-address] = %q, want tcp://127.0.0.1:514", got)
	}
}

// TestToContainerSpec_ModernLoggingWinsOverLegacy — when a service declares
// BOTH a modern `logging:` block and legacy log_driver/log_opt, the modern
// block wins outright (fixture service "both": modern json-file vs legacy
// syslog). Mirrors the resolveResources modern-wins policy.
func TestToContainerSpec_ModernLoggingWinsOverLegacy(t *testing.T) {
	project, _, err := compose.Load(filepath.Join("testdata", "logging.yml"))
	if err != nil {
		t.Fatalf("Load logging.yml: %v", err)
	}
	s, ok := lookupService(project, "both")
	if !ok {
		t.Fatal("both service missing")
	}
	spec := compose.ToContainerSpec(s, compose.SpecOptions{Deployment: "sample", Service: "both"})
	if spec.LogConfig == nil {
		t.Fatal("LogConfig nil; want modern json-file")
	}
	if spec.LogConfig.Driver != "json-file" {
		t.Errorf("Driver = %q, want json-file (modern wins, not syslog)", spec.LogConfig.Driver)
	}
	if got := spec.LogConfig.Options["max-size"]; got != "5m" {
		t.Errorf("Options[max-size] = %q, want 5m (modern options)", got)
	}
	if _, leaked := spec.LogConfig.Options["syslog-address"]; leaked {
		t.Errorf("legacy syslog-address leaked into modern options: %v", spec.LogConfig.Options)
	}
}

// TestToContainerSpec_NoLogging — a service with no logging config leaves
// LogConfig nil so docker's default driver applies (fixture service "none").
func TestToContainerSpec_NoLogging(t *testing.T) {
	project, _, err := compose.Load(filepath.Join("testdata", "logging.yml"))
	if err != nil {
		t.Fatalf("Load logging.yml: %v", err)
	}
	s, ok := lookupService(project, "none")
	if !ok {
		t.Fatal("none service missing")
	}
	spec := compose.ToContainerSpec(s, compose.SpecOptions{Deployment: "sample", Service: "none"})
	if spec.LogConfig != nil {
		t.Errorf("LogConfig = %+v, want nil for a service with no logging config", spec.LogConfig)
	}
}

// TestValidate_LoggingAccepted — the logging:/log_driver/log_opt keys are in
// the allowed closed field set, so a fixture using all three validates clean
// (previously they were silently rejected as unknown fields).
func TestValidate_LoggingAccepted(t *testing.T) {
	body := loadFixture(t, "logging.yml")
	if err := compose.Validate(body); err != nil {
		t.Fatalf("Validate(logging.yml) should accept logging/log_driver/log_opt; got %v", err)
	}
}
