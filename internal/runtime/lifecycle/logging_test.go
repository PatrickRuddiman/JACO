package lifecycle

import (
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

// TestBuildConfig_LogConfig — a spec's resolved LogConfig projects onto
// docker's HostConfig.LogConfig (whose driver field is named Type), carrying
// the options map (tag, max-size, max-file, etc.) through verbatim.
func TestBuildConfig_LogConfig(t *testing.T) {
	spec := compose.ContainerSpec{
		Image: "nginx:1.27",
		LogConfig: &compose.LogConfig{
			Driver: "json-file",
			Options: map[string]string{
				"tag":      "{{.Name}}",
				"max-size": "10m",
				"max-file": "3",
			},
		},
	}
	_, hc, _ := buildConfig(spec)
	if hc.LogConfig.Type != "json-file" {
		t.Errorf("LogConfig.Type = %q, want json-file", hc.LogConfig.Type)
	}
	if got := hc.LogConfig.Config["tag"]; got != "{{.Name}}" {
		t.Errorf("LogConfig.Config[tag] = %q, want {{.Name}}", got)
	}
	if got := hc.LogConfig.Config["max-size"]; got != "10m" {
		t.Errorf("LogConfig.Config[max-size] = %q, want 10m", got)
	}
}

// TestBuildConfig_NoLogConfig — a nil spec LogConfig yields the zero
// container.LogConfig (empty Type), which docker treats as "use the daemon's
// default log driver".
func TestBuildConfig_NoLogConfig(t *testing.T) {
	spec := compose.ContainerSpec{Image: "nginx:1.27"}
	_, hc, _ := buildConfig(spec)
	if hc.LogConfig.Type != "" {
		t.Errorf("LogConfig.Type = %q, want empty (no spec LogConfig)", hc.LogConfig.Type)
	}
	if len(hc.LogConfig.Config) != 0 {
		t.Errorf("LogConfig.Config = %v, want empty", hc.LogConfig.Config)
	}
}
