package compose_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

// TestSmokeFixtures_DocumentedInvariants pins the per-deployment volume
// scoping to the same on-disk fixtures that drive the Azure live smoke
// test (tests/samples/jaco/smoke-volumes/). If the smoke fixtures
// change shape — different volume key, opt-out literal, or compose
// service name — this test catches it locally so the live run on the
// testbed doesn't fail on a renamed fixture rather than a real
// regression.
func TestSmokeFixtures_DocumentedInvariants(t *testing.T) {
	root := filepath.Join("..", "..", "..", "tests", "samples", "jaco", "smoke-volumes")

	// Default-scoped pair: front and back must produce two distinct
	// jaco_<deployment>_<key> sources for the same compose service.
	front := smokeMount(t, filepath.Join(root, "front.compose.yml"), "vol-front")
	back := smokeMount(t, filepath.Join(root, "back.compose.yml"), "vol-back")
	if front.Source == back.Source {
		t.Fatalf("smoke fixtures collide: front=%q back=%q (same docker volume)", front.Source, back.Source)
	}
	if got, want := front.Source, "jaco_vol-front_data"; got != want {
		t.Errorf("front Source = %q, want %q", got, want)
	}
	if got, want := back.Source, "jaco_vol-back_data"; got != want {
		t.Errorf("back Source = %q, want %q", got, want)
	}

	// Opt-out: shared.compose.yml's `volumes.data.name: smoke-shared-data`
	// must reach the spec unprefixed regardless of which deployment
	// mounts it.
	shared := smokeMount(t, filepath.Join(root, "shared.compose.yml"), "vol-front")
	if got, want := shared.Source, "smoke-shared-data"; got != want {
		t.Errorf("shared Source = %q, want %q (unprefixed escape hatch)", got, want)
	}
}

// smokeMount loads the smoke fixture at path, projects its single
// `redis` service under the given deployment, and returns the only
// expected Mount. Fails the test on any structural surprise so a
// fixture rename surfaces immediately.
func smokeMount(t *testing.T, path, deployment string) compose.Mount {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	project, err := compose.LoadBytes(body, filepath.Base(path))
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	svc, ok := lookupService(project, "redis")
	if !ok {
		t.Fatalf("%s: service redis missing", path)
	}
	overrides, err := compose.TopLevelVolumeNames(body)
	if err != nil {
		t.Fatalf("overrides %s: %v", path, err)
	}
	spec := compose.ToContainerSpec(svc, compose.SpecOptions{
		Deployment:          deployment,
		Service:             "redis",
		VolumeNameOverrides: overrides,
	})
	if len(spec.Mounts) != 1 {
		t.Fatalf("%s: Mounts len = %d, want 1; got %+v", path, len(spec.Mounts), spec.Mounts)
	}
	return spec.Mounts[0]
}
