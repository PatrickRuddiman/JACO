package compose_test

import (
	"bytes"
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

func TestValidate_BuildFieldAcceptedAndIgnored(t *testing.T) {
	body := []byte(`services:
  web:
    image: registry.example.com/web:1.0
    build: ./web
`)
	if err := compose.Validate(body); err != nil {
		t.Fatalf("Validate should accept build:; got %v", err)
	}

	project, err := compose.LoadBytes(body, "memory.yml")
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	svc, ok := lookupService(project, "web")
	if !ok {
		t.Fatalf("web service missing")
	}
	spec := compose.ToContainerSpec(svc, compose.SpecOptions{
		Deployment: "sample", Service: "web", ReplicaID: "sample-web-0",
	})
	if spec.Image != "registry.example.com/web:1.0" {
		t.Errorf("Image = %q, want registry.example.com/web:1.0", spec.Image)
	}
	// The ContainerSpec surface has no Build field — projecting drops it. Sanity-
	// check the runtime view stayed image-only by confirming nothing leaked into
	// Labels under a build-ish key.
	for k := range spec.Labels {
		if strings.Contains(strings.ToLower(k), "build") {
			t.Errorf("Labels carries build remnant %q = %q", k, spec.Labels[k])
		}
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
	if ve.Details["field"] != "mac_address" {
		t.Errorf("details.field = %q, want mac_address", ve.Details["field"])
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

// TestValidate_ResourcesFixturePasses — the resources fixture exercises both
// the modern deploy.resources block (with ignored replicas/placement/
// restart_policy subkeys) and the legacy top-level keys. Validate must accept
// all of it (issue #49).
func TestValidate_ResourcesFixturePasses(t *testing.T) {
	body := loadFixture(t, "resources.yml")
	if err := compose.Validate(body); err != nil {
		t.Fatalf("Validate(resources): %v", err)
	}
}

// TestToContainerSpec_ModernDeployResources — deploy.resources.limits projects
// cpus→NanoCPUs (cores × 1e9), memory→bytes, pids→*PidsLimit, and
// reservations.memory→MemoryReservationBytes.
func TestToContainerSpec_ModernDeployResources(t *testing.T) {
	project, _, err := compose.Load(filepath.Join("testdata", "resources.yml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s, ok := lookupService(project, "modern")
	if !ok {
		t.Fatal("modern service missing")
	}
	spec := compose.ToContainerSpec(s, compose.SpecOptions{Deployment: "sample", Service: "modern"})

	if want := int64(1.5 * 1e9); spec.NanoCPUs != want {
		t.Errorf("NanoCPUs = %d, want %d", spec.NanoCPUs, want)
	}
	if want := int64(256 * 1024 * 1024); spec.MemoryBytes != want {
		t.Errorf("MemoryBytes = %d, want %d", spec.MemoryBytes, want)
	}
	if want := int64(128 * 1024 * 1024); spec.MemoryReservationBytes != want {
		t.Errorf("MemoryReservationBytes = %d, want %d", spec.MemoryReservationBytes, want)
	}
	if spec.PidsLimit == nil || *spec.PidsLimit != 100 {
		t.Errorf("PidsLimit = %v, want 100", spec.PidsLimit)
	}
	// No legacy cpu_shares/cpuset on this service.
	if spec.CPUShares != 0 {
		t.Errorf("CPUShares = %d, want 0", spec.CPUShares)
	}
	if spec.CpusetCpus != "" {
		t.Errorf("CpusetCpus = %q, want empty", spec.CpusetCpus)
	}
}

// TestToContainerSpec_LegacyTopLevelResources — the legacy keys project
// directly: cpus→NanoCPUs, mem_limit→MemoryBytes, mem_reservation→
// MemoryReservationBytes, pids_limit→*PidsLimit, cpu_shares→CPUShares,
// cpuset→CpusetCpus.
func TestToContainerSpec_LegacyTopLevelResources(t *testing.T) {
	project, _, err := compose.Load(filepath.Join("testdata", "resources.yml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s, ok := lookupService(project, "legacy")
	if !ok {
		t.Fatal("legacy service missing")
	}
	spec := compose.ToContainerSpec(s, compose.SpecOptions{Deployment: "sample", Service: "legacy"})

	if want := int64(0.5 * 1e9); spec.NanoCPUs != want {
		t.Errorf("NanoCPUs = %d, want %d", spec.NanoCPUs, want)
	}
	if want := int64(128 * 1024 * 1024); spec.MemoryBytes != want {
		t.Errorf("MemoryBytes = %d, want %d", spec.MemoryBytes, want)
	}
	if want := int64(64 * 1024 * 1024); spec.MemoryReservationBytes != want {
		t.Errorf("MemoryReservationBytes = %d, want %d", spec.MemoryReservationBytes, want)
	}
	if spec.PidsLimit == nil || *spec.PidsLimit != 50 {
		t.Errorf("PidsLimit = %v, want 50", spec.PidsLimit)
	}
	if spec.CPUShares != 512 {
		t.Errorf("CPUShares = %d, want 512", spec.CPUShares)
	}
	if spec.CpusetCpus != "0,1" {
		t.Errorf("CpusetCpus = %q, want 0,1", spec.CpusetCpus)
	}
}

// TestToContainerSpec_ModernWinsOverLegacy — when a service declares BOTH a
// deploy.resources block and legacy top-level keys, the modern block decides
// cpus/memory/pids outright (no field-by-field merge). cpu_shares/cpuset have
// no modern equivalent, so they still carry through from the legacy keys.
func TestToContainerSpec_ModernWinsOverLegacy(t *testing.T) {
	project, _, err := compose.Load(filepath.Join("testdata", "resources.yml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s, ok := lookupService(project, "both")
	if !ok {
		t.Fatal("both service missing")
	}
	spec := compose.ToContainerSpec(s, compose.SpecOptions{Deployment: "sample", Service: "both"})

	// Modern wins: 2.0 cores, 512Mi, 200 pids — NOT the legacy 9.0/999M/999.
	if want := int64(2.0 * 1e9); spec.NanoCPUs != want {
		t.Errorf("NanoCPUs = %d, want %d (modern should win)", spec.NanoCPUs, want)
	}
	if want := int64(512 * 1024 * 1024); spec.MemoryBytes != want {
		t.Errorf("MemoryBytes = %d, want %d (modern should win)", spec.MemoryBytes, want)
	}
	if spec.PidsLimit == nil || *spec.PidsLimit != 200 {
		t.Errorf("PidsLimit = %v, want 200 (modern should win)", spec.PidsLimit)
	}
	// Modern block has no reservations → MemoryReservationBytes stays 0 (the
	// legacy mem_reservation is NOT merged in).
	if spec.MemoryReservationBytes != 0 {
		t.Errorf("MemoryReservationBytes = %d, want 0 (modern path, no reservations)", spec.MemoryReservationBytes)
	}
	// cpu_shares/cpuset have no modern equivalent → carried from legacy keys.
	if spec.CPUShares != 256 {
		t.Errorf("CPUShares = %d, want 256", spec.CPUShares)
	}
	if spec.CpusetCpus != "2,3" {
		t.Errorf("CpusetCpus = %q, want 2,3", spec.CpusetCpus)
	}
}

// TestToContainerSpec_NoResourcesLeavesZero — a service with no resource
// fields must leave every resource field at its zero value (PidsLimit nil) so
// docker applies its defaults.
func TestToContainerSpec_NoResourcesLeavesZero(t *testing.T) {
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
	spec := compose.ToContainerSpec(s, compose.SpecOptions{Deployment: "sample", Service: "bare"})
	if spec.NanoCPUs != 0 || spec.MemoryBytes != 0 || spec.MemoryReservationBytes != 0 ||
		spec.CPUShares != 0 || spec.CpusetCpus != "" {
		t.Errorf("expected zero resources, got %+v", spec)
	}
	if spec.PidsLimit != nil {
		t.Errorf("PidsLimit = %v, want nil", spec.PidsLimit)
	}
}

func TestContainerName_UsesReplicaID(t *testing.T) {
	got := compose.ContainerName(compose.SpecOptions{ReplicaID: "sample-web-3"})
	if got != "jaco_sample-web-3" {
		t.Errorf("ContainerName = %q, want jaco_sample-web-3", got)
	}
}

// envFileFixtureDir is the per-fixture root for env_file resolution tests.
// Co-locating the compose file and its .env companions in a subdirectory
// keeps testdata/ scannable.
const envFileFixtureDir = "testdata/env_file"

// TestResolveEnvFiles_MergesAndStripsEnvFile is the happy-path golden:
// fixture has two env_files plus an explicit environment block; the resolved
// YAML must (a) drop env_file:, (b) carry every merged key, (c) respect both
// later-file-wins and environment-wins precedence rules.
func TestResolveEnvFiles_MergesAndStripsEnvFile(t *testing.T) {
	body := loadFixture(t, "env_file/compose.yml")
	out, err := compose.ResolveEnvFiles(body, envFileFixtureDir)
	if err != nil {
		t.Fatalf("ResolveEnvFiles: %v", err)
	}
	if strings.Contains(string(out), "env_file") {
		t.Fatalf("resolved YAML still mentions env_file:\n%s", out)
	}

	project, err := compose.LoadBytes(out, "resolved.yml")
	if err != nil {
		t.Fatalf("LoadBytes resolved: %v", err)
	}
	web, ok := lookupService(project, "web")
	if !ok {
		t.Fatalf("web service missing after resolve")
	}
	want := map[string]string{
		"FROM_FILE_ONE": "alpha",       // only in .env.web
		"FROM_FILE_TWO": "beta",        // only in .env.web2
		"SHARED_KEY":    "from_second", // later env_file wins over earlier
		"WIN_OVER_FILE": "from_env",    // explicit environment wins over env_file
		"EXISTING":      "stays",       // explicit environment passes through
	}
	got := envToMap(t, web.Environment)
	if len(got) != len(want) {
		t.Errorf("environment cardinality = %d, want %d (%v)", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, got[k], v)
		}
	}
}

// TestResolveEnvFiles_RoundTripsToContainerSpec is the wire-shape check:
// after ResolveEnvFiles → LoadBytes → ToContainerSpec, the merged variables
// land in ContainerSpec.Env as deterministic, sorted KEY=value strings.
func TestResolveEnvFiles_RoundTripsToContainerSpec(t *testing.T) {
	body := loadFixture(t, "env_file/compose.yml")
	out, err := compose.ResolveEnvFiles(body, envFileFixtureDir)
	if err != nil {
		t.Fatalf("ResolveEnvFiles: %v", err)
	}
	project, err := compose.LoadBytes(out, "resolved.yml")
	if err != nil {
		t.Fatalf("LoadBytes resolved: %v", err)
	}
	web, ok := lookupService(project, "web")
	if !ok {
		t.Fatalf("web service missing after resolve")
	}
	spec := compose.ToContainerSpec(web, compose.SpecOptions{
		Deployment: "sample", Service: "web", ReplicaID: "sample-web-0",
	})
	wantEnv := []string{
		"EXISTING=stays",
		"FROM_FILE_ONE=alpha",
		"FROM_FILE_TWO=beta",
		"SHARED_KEY=from_second",
		"WIN_OVER_FILE=from_env",
	}
	if !equalStrings(spec.Env, wantEnv) {
		t.Errorf("ContainerSpec.Env = %v, want %v", spec.Env, wantEnv)
	}
}

// TestResolveEnvFiles_NoEnvFileIsByteIdentical asserts the fast path: when
// no service declares env_file, ResolveEnvFiles returns the input bytes
// unchanged so the daemon receives exactly what the operator wrote (relevant
// for any future content-hash audit of the YAML envelope).
func TestResolveEnvFiles_NoEnvFileIsByteIdentical(t *testing.T) {
	body := []byte("services:\n  web:\n    image: nginx:1.27\n")
	out, err := compose.ResolveEnvFiles(body, t.TempDir())
	if err != nil {
		t.Fatalf("ResolveEnvFiles: %v", err)
	}
	if !bytes.Equal(out, body) {
		t.Errorf("expected byte-identical passthrough; got\n%s", out)
	}
}

// TestResolveEnvFiles_MissingFileSurfacesError — a required env_file that
// doesn't exist must turn into an error at resolve time (CLI). The exact
// message is compose-go's; we only assert the wrapper prefix that pins this
// failure to the resolution step rather than to a general compose load.
func TestResolveEnvFiles_MissingFileSurfacesError(t *testing.T) {
	body := []byte(`services:
  web:
    image: nginx:1.27
    env_file:
      - .nonexistent.env
`)
	_, err := compose.ResolveEnvFiles(body, t.TempDir())
	if err == nil {
		t.Fatalf("expected error for missing env_file; got nil")
	}
	if !strings.Contains(err.Error(), "resolve env_file") {
		t.Errorf("err = %v, want wrap prefix 'resolve env_file'", err)
	}
}

// TestLoadBytes_RejectsUnresolvedEnvFile is the daemon-side guard: bytes
// that still carry env_file: must fail with a stable, typed error code
// (env_file_unresolved). Old client → new daemon must be loud, not silent.
func TestLoadBytes_RejectsUnresolvedEnvFile(t *testing.T) {
	body := []byte(`services:
  web:
    image: nginx:1.27
    env_file:
      - .env.web
`)
	_, err := compose.LoadBytes(body, "deploy-compose.yml")
	if err == nil {
		t.Fatalf("LoadBytes accepted unresolved env_file")
	}
	var ve *compose.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v (%T), want *compose.ValidationError", err, err)
	}
	if ve.Code != "env_file_unresolved" {
		t.Errorf("Code = %q, want env_file_unresolved", ve.Code)
	}
	if ve.Details["service"] != "web" {
		t.Errorf("Details[service] = %q, want web", ve.Details["service"])
	}
	if !strings.Contains(ve.Message, "client-side before apply") {
		t.Errorf("Message = %q, want hint about client-side resolution", ve.Message)
	}
}

// TestLoadBytes_AcceptsResolvedYAML proves the daemon-side check is gated on
// env_file:, not on the env contents themselves — a YAML that carried env_file
// originally is accepted as soon as ResolveEnvFiles has stripped it.
func TestLoadBytes_AcceptsResolvedYAML(t *testing.T) {
	body := loadFixture(t, "env_file/compose.yml")
	out, err := compose.ResolveEnvFiles(body, envFileFixtureDir)
	if err != nil {
		t.Fatalf("ResolveEnvFiles: %v", err)
	}
	if _, err := compose.LoadBytes(out, "resolved.yml"); err != nil {
		t.Errorf("LoadBytes(resolved) = %v, want nil", err)
	}
}

// envToMap flattens a compose-go env mapping (values may be *string) into a
// plain map for assertion. nil values render as "" — matching what the
// runtime would set when an env entry has no value.
func envToMap(t *testing.T, env types.MappingWithEquals) map[string]string {
	t.Helper()
	out := make(map[string]string, len(env))
	for k, v := range env {
		if v == nil {
			out[k] = ""
		} else {
			out[k] = *v
		}
	}
	return out
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

// TestLoadBytes_PreservesShellEscapes_NoInterpolation pins the daemon-side
// loader's SkipInterpolation invariant (issue #149). After PR #145, CLI-side
// SubstituteEnvVars resolves every ${VAR} and collapses $$VAR → $VAR per
// compose-spec. The bytes the daemon parses MUST be treated as authoritative:
// re-running compose-go's interpolator against an always-empty Environment
// map would (a) emit "variable not set" warnings on every reconcile tick,
// and (b) corrupt the post-CLI $VAR shell-escape to "".
//
// Asserts the healthcheck and command preserve a bare $VAR scalar through
// the daemon's LoadBytes — which only holds when SkipInterpolation is set.
func TestLoadBytes_PreservesShellEscapes_NoInterpolation(t *testing.T) {
	body := []byte(`services:
  db:
    image: mysql:8
    healthcheck:
      test: ["CMD-SHELL", "mysqladmin -p$MYSQL_ROOT_PASSWORD ping"]
    command: ["sh", "-c", "echo $SOME_RUNTIME_VAR"]
`)
	p, err := compose.LoadBytes(body, "test.yml")
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	svc, ok := lookupService(p, "db")
	if !ok {
		t.Fatalf("service db missing")
	}
	if svc.HealthCheck == nil || len(svc.HealthCheck.Test) < 2 {
		t.Fatalf("HealthCheck.Test missing or malformed: %#v", svc.HealthCheck)
	}
	if got := svc.HealthCheck.Test[1]; got != "mysqladmin -p$MYSQL_ROOT_PASSWORD ping" {
		t.Errorf("healthcheck shell escape corrupted: got %q, want literal $VAR preserved", got)
	}
	if len(svc.Command) < 3 || svc.Command[2] != "echo $SOME_RUNTIME_VAR" {
		t.Errorf("command shell escape corrupted: got %#v, want literal $VAR preserved", svc.Command)
	}
}
