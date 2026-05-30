package compose_test

import (
	"path/filepath"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

// TestToContainerSpec_ModernLogging — a service's modern `logging:` block
// (driver + options incl. tag) projects onto ContainerSpec.LogConfig (fixture
// service "modern").
func TestToContainerSpec_ModernLogging(t *testing.T) {
	project, _, err := compose.Load(filepath.Join("testdata", "logging.yml"))
	if err != nil {
		t.Fatalf("Load logging.yml: %v", err)
	}
	s, ok := lookupService(project, "modern")
	if !ok {
		t.Fatal("modern service missing")
	}
	spec := compose.ToContainerSpec(s, compose.SpecOptions{Deployment: "sample", Service: "modern"})
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

// TestToContainerSpec_InlineModernLogging — a driver-only logging block (no
// options) still projects, with a nil Options map.
func TestToContainerSpec_InlineModernLogging(t *testing.T) {
	project, err := compose.LoadBytes([]byte(`services:
  web:
    image: nginx
    logging:
      driver: journald
`), "memory.yml")
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	s, ok := lookupService(project, "web")
	if !ok {
		t.Fatal("web service missing")
	}
	spec := compose.ToContainerSpec(s, compose.SpecOptions{Deployment: "sample", Service: "web"})
	if spec.LogConfig == nil || spec.LogConfig.Driver != "journald" {
		t.Fatalf("LogConfig = %+v, want driver journald", spec.LogConfig)
	}
	if len(spec.LogConfig.Options) != 0 {
		t.Errorf("Options = %v, want empty for a driver-only block", spec.LogConfig.Options)
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

// TestValidate_LoggingAccepted — the modern `logging:` key is in the allowed
// closed field set, so a fixture using it validates clean (previously it was
// silently rejected as an unknown field).
func TestValidate_LoggingAccepted(t *testing.T) {
	body := loadFixture(t, "logging.yml")
	if err := compose.Validate(body); err != nil {
		t.Fatalf("Validate(logging.yml) should accept logging:; got %v", err)
	}
}
