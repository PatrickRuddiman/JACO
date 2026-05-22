package compose_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return body
}

func TestLoad_ValidFixtureParses(t *testing.T) {
	path := filepath.Join("testdata", "valid.yml")
	project, raw, err := compose.Load(path)
	if err != nil {
		t.Fatalf("Load valid: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("Load returned 0 raw bytes")
	}
	if got := len(project.Services); got != 2 {
		t.Errorf("services count = %d, want 2", got)
	}
	if _, ok := project.Networks["frontend"]; !ok {
		t.Errorf("frontend network missing in parsed project")
	}
}

func TestValidate_ValidFixturePasses(t *testing.T) {
	body := loadFixture(t, "valid.yml")
	if err := compose.Validate(body); err != nil {
		t.Fatalf("Validate(valid): %v", err)
	}
}

func TestValidate_UnknownFieldRejected(t *testing.T) {
	body := loadFixture(t, "unknown-field.yml")
	err := compose.Validate(body)
	if err == nil {
		t.Fatalf("expected ValidationError, got nil")
	}
	var ve *compose.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err is not ValidationError: %T %v", err, err)
	}
	if ve.Code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", ve.Code)
	}
	if ve.Details["field"] != "deploy" {
		t.Errorf("details.field = %q, want deploy", ve.Details["field"])
	}
	if ve.Details["service"] != "web" {
		t.Errorf("details.service = %q, want web", ve.Details["service"])
	}
}

func TestValidate_UnknownNetworkRejected(t *testing.T) {
	body := loadFixture(t, "unknown-network.yml")
	err := compose.Validate(body)
	if err == nil {
		t.Fatalf("expected ValidationError, got nil")
	}
	var ve *compose.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err is not ValidationError: %T %v", err, err)
	}
	if ve.Code != "unknown_network" {
		t.Errorf("code = %q, want unknown_network", ve.Code)
	}
	if !strings.Contains(ve.Message, "missing") {
		t.Errorf("message should mention the bad network name; got %q", ve.Message)
	}
}

func TestValidate_DefaultNetworkAlwaysDeclared(t *testing.T) {
	body := []byte(`services:
  web:
    image: nginx
    networks:
      - _default
`)
	if err := compose.Validate(body); err != nil {
		t.Errorf("_default should always be considered declared; got %v", err)
	}
}

func TestToContainerSpec_StampsAllSixJACOLabels(t *testing.T) {
	project, _, err := compose.Load(filepath.Join("testdata", "valid.yml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	svc, ok := lookupService(project, "web")
	if !ok {
		t.Fatalf("web service missing")
	}
	spec := compose.ToContainerSpec(svc, compose.SpecOptions{
		ClusterID:    "cluster-x",
		Deployment:   "sample",
		Service:      "web",
		ReplicaID:    "sample-web-0",
		ReplicaIndex: 0,
		RaftIndex:    42,
	})

	wantLabels := map[string]string{
		"jaco.cluster_id":    "cluster-x",
		"jaco.deployment":    "sample",
		"jaco.service":       "web",
		"jaco.replica_id":    "sample-web-0",
		"jaco.replica_index": "0",
		"jaco.raft_index":    "42",
	}
	for k, want := range wantLabels {
		if got := spec.Labels[k]; got != want {
			t.Errorf("Labels[%q] = %q, want %q", k, got, want)
		}
	}
	// User labels survive too.
	if got := spec.Labels["app"]; got != "web" {
		t.Errorf("user label app = %q, want web", got)
	}
}

func TestToContainerSpec_MapsCoreFields(t *testing.T) {
	project, _, _ := compose.Load(filepath.Join("testdata", "valid.yml"))
	s, ok := lookupService(project, "web")
	if !ok {
		t.Fatal("web service missing")
	}
	spec := compose.ToContainerSpec(s, compose.SpecOptions{
		Deployment: "sample", Service: "web",
		ReplicaID: "sample-web-0",
	})
	{
		if spec.Image != "nginx:1.27" {
			t.Errorf("Image = %q", spec.Image)
		}
		if want := []string{"nginx", "-g", "daemon off;"}; !equalStrings(spec.Command, want) {
			t.Errorf("Command = %v, want %v", spec.Command, want)
		}
		if spec.User != "1000:1000" {
			t.Errorf("User = %q", spec.User)
		}
		if !spec.ReadOnly {
			t.Errorf("ReadOnly = false, want true")
		}
		if got := spec.Healthcheck; got == nil || len(got.Test) == 0 {
			t.Errorf("Healthcheck not populated: %+v", got)
		}
		// Env should be alphabetically sorted.
		if len(spec.Env) < 2 || spec.Env[0] >= spec.Env[1] {
			t.Errorf("Env not sorted: %v", spec.Env)
		}
		if len(spec.Mounts) < 1 {
			t.Errorf("Mounts empty")
		}
		if got := spec.CapAdd; len(got) != 1 || got[0] != "NET_BIND_SERVICE" {
			t.Errorf("CapAdd = %v, want [NET_BIND_SERVICE]", got)
		}
		if got := spec.Tmpfs; len(got) != 2 {
			t.Errorf("Tmpfs len = %d, want 2", len(got))
		}
		if got := spec.Ulimits["nofile"]; got.Soft != 1024 || got.Hard != 2048 {
			t.Errorf("ulimits nofile = %+v", got)
		}
	}
}

func TestToContainerSpec_NetworksDefaultsToServiceDefault(t *testing.T) {
	project, err := compose.LoadBytes([]byte(`services:
  bare:
    image: nginx
`), "memory.yml")
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	s, ok := lookupService(project, "bare")
	if !ok {
		t.Fatal("bare service missing")
	}
	spec := compose.ToContainerSpec(s, compose.SpecOptions{
		Deployment: "sample", Service: "bare",
	})
	if got, want := spec.Networks, []string{"jaco_sample__default"}; !equalStrings(got, want) {
		t.Errorf("Networks = %v, want %v", got, want)
	}
}

func TestToContainerSpec_NetworksUseDeploymentPrefix(t *testing.T) {
	project, _, _ := compose.Load(filepath.Join("testdata", "valid.yml"))
	s, ok := lookupService(project, "api")
	if !ok {
		t.Fatal("api service missing")
	}
	spec := compose.ToContainerSpec(s, compose.SpecOptions{
		Deployment: "sample", Service: "api",
	})
	found := map[string]bool{}
	for _, n := range spec.Networks {
		found[n] = true
	}
	for _, want := range []string{"jaco_sample_frontend", "jaco_sample_backend"} {
		if !found[want] {
			t.Errorf("Networks missing %q; got %v", want, spec.Networks)
		}
	}
}

func TestContainerName_UsesReplicaID(t *testing.T) {
	got := compose.ContainerName(compose.SpecOptions{ReplicaID: "sample-web-3"})
	if got != "jaco_sample-web-3" {
		t.Errorf("ContainerName = %q, want jaco_sample-web-3", got)
	}
}

// --- helpers -----------------------------------------------------------------

// lookupService finds a service by name in the parsed Project. compose-go v2
// keeps Services as a slice keyed by ServiceConfig.Name (not a map).
func lookupService(p *types.Project, name string) (types.ServiceConfig, bool) {
	for _, s := range p.Services {
		if s.Name == name {
			return s, true
		}
	}
	return types.ServiceConfig{}, false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// silence unused
var _ = strconv.Itoa
