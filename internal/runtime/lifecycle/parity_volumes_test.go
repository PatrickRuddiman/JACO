package lifecycle

import (
	"testing"

	"github.com/docker/docker/api/types/mount"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
	"github.com/PatrickRuddiman/jaco/internal/runtime/volumes"
)

// Issue #91 PR1: buildConfig MUST emit a Mount{Type: Bind} pointing at
// the managed on-host path for every named-volume mount whose source
// appears in the deployment's top-level `volumes:` set — but ONLY when
// the daemon-side runtime.managed_volumes.enabled flag is on. Flag off
// → existing behaviour (Mount{Type: Volume, Source: <name>}) — old
// deployments must keep working byte-for-byte. Inline bind mounts
// (m.Type == "bind") never get rewritten regardless of the flag.

// TestBuildConfig_VolumesFlagOff_KeepsDockerNamedVolume — with the flag
// off, a per-service `pgdata:/var/lib/postgresql/data` lands on docker
// as Mount{Type: Volume, Source: "pgdata"} exactly as it did before
// this PR. NamedVolumes carries the set so the test also confirms the
// rewrite path doesn't trigger purely on the presence of the set.
func TestBuildConfig_VolumesFlagOff_KeepsDockerNamedVolume(t *testing.T) {
	spec := compose.ContainerSpec{
		Image:      "postgres:16",
		Deployment: "sample",
		Service:    "pg-primary",
		ReplicaID:  "sample-pg-primary-0",
		Mounts: []compose.Mount{
			{Type: "volume", Source: "pgdata", Target: "/var/lib/postgresql/data"},
		},
		NamedVolumes: map[string]struct{}{"pgdata": {}},
	}
	_, hc, _, err := buildConfig(spec, nil, managedVolumeOpts{ /* enabled: false */ })
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}
	if len(hc.Mounts) != 1 {
		t.Fatalf("Mounts len = %d, want 1", len(hc.Mounts))
	}
	got := hc.Mounts[0]
	if got.Type != mount.TypeVolume {
		t.Errorf("Mount[0].Type = %v, want %v (flag off must preserve docker named volume)", got.Type, mount.TypeVolume)
	}
	if got.Source != "pgdata" {
		t.Errorf("Mount[0].Source = %q, want %q (flag off must preserve compose volume name as source)", got.Source, "pgdata")
	}
	if got.Target != "/var/lib/postgresql/data" {
		t.Errorf("Mount[0].Target = %q, want %q", got.Target, "/var/lib/postgresql/data")
	}
}

// TestBuildConfig_VolumesFlagOn_RewritesToManagedBind — with the flag
// on and the volume name in the top-level set, the same compose entry
// rewrites to a Bind mount whose source is volumesRoot.PathFor(...).
// ReadOnly + Target carry through verbatim.
func TestBuildConfig_VolumesFlagOn_RewritesToManagedBind(t *testing.T) {
	root := volumes.NewRoot("/var/lib/jaco/volumes")
	spec := compose.ContainerSpec{
		Image:      "postgres:16",
		Deployment: "sample",
		Service:    "pg-primary",
		ReplicaID:  "sample-pg-primary-0",
		Mounts: []compose.Mount{
			{Type: "volume", Source: "pgdata", Target: "/var/lib/postgresql/data", ReadOnly: false},
		},
		NamedVolumes: map[string]struct{}{"pgdata": {}},
	}
	_, hc, _, err := buildConfig(spec, nil, managedVolumeOpts{enabled: true, root: root})
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}
	if len(hc.Mounts) != 1 {
		t.Fatalf("Mounts len = %d, want 1", len(hc.Mounts))
	}
	got := hc.Mounts[0]
	if got.Type != mount.TypeBind {
		t.Errorf("Mount[0].Type = %v, want %v (flag on must rewrite to bind)", got.Type, mount.TypeBind)
	}
	wantSource := root.PathFor("sample", "pg-primary", "pgdata")
	if got.Source != wantSource {
		t.Errorf("Mount[0].Source = %q, want %q", got.Source, wantSource)
	}
	if got.Target != "/var/lib/postgresql/data" {
		t.Errorf("Mount[0].Target = %q, want %q", got.Target, "/var/lib/postgresql/data")
	}
	if got.ReadOnly {
		t.Errorf("Mount[0].ReadOnly = true, want false (carry-through)")
	}
}

// TestBuildConfig_VolumesFlagOn_ReadOnlyCarriesThrough — explicit
// guard that `read_only` on a managed volume stays attached to the
// rewritten bind. A compose `pgdata:/var/lib/pg:ro` MUST surface
// as ReadOnly: true on the docker bind so the container fails to
// open the data for write at boot, exactly as the operator intended.
func TestBuildConfig_VolumesFlagOn_ReadOnlyCarriesThrough(t *testing.T) {
	root := volumes.NewRoot("/data/jaco/volumes")
	spec := compose.ContainerSpec{
		Image:      "alpine",
		Deployment: "dep",
		Service:    "svc",
		ReplicaID:  "dep-svc-0",
		Mounts: []compose.Mount{
			{Type: "volume", Source: "cfg", Target: "/etc/cfg", ReadOnly: true},
		},
		NamedVolumes: map[string]struct{}{"cfg": {}},
	}
	_, hc, _, err := buildConfig(spec, nil, managedVolumeOpts{enabled: true, root: root})
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}
	if !hc.Mounts[0].ReadOnly {
		t.Errorf("ReadOnly = false on rewritten managed bind; want true")
	}
	if hc.Mounts[0].Type != mount.TypeBind {
		t.Errorf("Type = %v, want bind", hc.Mounts[0].Type)
	}
}

// TestBuildConfig_VolumesFlagOn_InlineBindUnchanged — an inline
// `/host/path:/in/container` bind mount (m.Type == "bind") MUST flow
// straight through to docker with Source preserved, regardless of
// the flag. The managed-volume rewrite is keyed strictly on
// (Type==volume AND name in top-level set); bind mounts are operator-
// supplied host paths and JACO never touches them.
func TestBuildConfig_VolumesFlagOn_InlineBindUnchanged(t *testing.T) {
	root := volumes.NewRoot("/var/lib/jaco/volumes")
	spec := compose.ContainerSpec{
		Image:      "alpine",
		Deployment: "dep",
		Service:    "svc",
		ReplicaID:  "dep-svc-0",
		Mounts: []compose.Mount{
			{Type: "bind", Source: "/host/data", Target: "/in/container"},
		},
		NamedVolumes: map[string]struct{}{
			// Even with a top-level volume sharing a unrelated name,
			// inline binds stay binds.
			"some-other-volume": {},
		},
	}
	_, hc, _, err := buildConfig(spec, nil, managedVolumeOpts{enabled: true, root: root})
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}
	if len(hc.Mounts) != 1 {
		t.Fatalf("Mounts len = %d, want 1", len(hc.Mounts))
	}
	if hc.Mounts[0].Type != mount.TypeBind {
		t.Errorf("Type = %v, want bind", hc.Mounts[0].Type)
	}
	if hc.Mounts[0].Source != "/host/data" {
		t.Errorf("Source = %q, want %q (inline bind must keep operator host path)", hc.Mounts[0].Source, "/host/data")
	}
}

// TestBuildConfig_VolumesFlagOn_UnknownNameStaysDockerVolume — a
// `Type: volume` mount whose Source is NOT in the deployment's
// top-level `volumes:` set falls through the unmanaged path even
// when the flag is on. That's the path that handles external /
// driver-backed volumes the operator manages outside JACO.
func TestBuildConfig_VolumesFlagOn_UnknownNameStaysDockerVolume(t *testing.T) {
	root := volumes.NewRoot("/var/lib/jaco/volumes")
	spec := compose.ContainerSpec{
		Image:      "alpine",
		Deployment: "dep",
		Service:    "svc",
		ReplicaID:  "dep-svc-0",
		Mounts: []compose.Mount{
			{Type: "volume", Source: "external", Target: "/mnt"},
		},
		NamedVolumes: map[string]struct{}{
			// "external" deliberately missing — declared with
			// `external: true` in compose, not in the deployment's
			// top-level managed set.
		},
	}
	_, hc, _, err := buildConfig(spec, nil, managedVolumeOpts{enabled: true, root: root})
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}
	if hc.Mounts[0].Type != mount.TypeVolume {
		t.Errorf("Type = %v, want volume (unmanaged name must stay docker volume)", hc.Mounts[0].Type)
	}
	if hc.Mounts[0].Source != "external" {
		t.Errorf("Source = %q, want %q", hc.Mounts[0].Source, "external")
	}
}

// TestBuildConfig_VolumesFlagOn_AnonymousVolumeStaysDocker — a
// compose entry that declares a target with no source (anonymous
// volume) MUST flow as docker named volume with empty Source so
// docker assigns its own; rewriting would invent a path that doesn't
// belong to any deployment-level managed key.
func TestBuildConfig_VolumesFlagOn_AnonymousVolumeStaysDocker(t *testing.T) {
	root := volumes.NewRoot("/var/lib/jaco/volumes")
	spec := compose.ContainerSpec{
		Image:      "alpine",
		Deployment: "dep",
		Service:    "svc",
		ReplicaID:  "dep-svc-0",
		Mounts: []compose.Mount{
			{Type: "volume", Source: "", Target: "/scratch"},
		},
		NamedVolumes: map[string]struct{}{"scratch": {}},
	}
	_, hc, _, err := buildConfig(spec, nil, managedVolumeOpts{enabled: true, root: root})
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}
	if hc.Mounts[0].Type != mount.TypeVolume {
		t.Errorf("anonymous volume Type = %v, want volume", hc.Mounts[0].Type)
	}
	if hc.Mounts[0].Source != "" {
		t.Errorf("anonymous volume Source = %q, want empty", hc.Mounts[0].Source)
	}
}
