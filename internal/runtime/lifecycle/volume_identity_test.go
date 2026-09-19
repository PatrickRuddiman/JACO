package lifecycle_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	grpcsrv "github.com/PatrickRuddiman/jaco/internal/controlplane/grpc"
	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
	"github.com/PatrickRuddiman/jaco/internal/runtime/lifecycle"
	"github.com/PatrickRuddiman/jaco/internal/runtime/volumes"
)

type volumeDocker struct {
	*fakeDocker
	hostConfigs         map[string]*container.HostConfig
	volumes             map[string]volume.Volume
	contents            map[string]string
	inspectErrors       map[string]error
	volumeCreateErr     error
	containerInspectErr error
	createRace          *volume.Volume
}

func newVolumeDocker() *volumeDocker {
	return &volumeDocker{
		fakeDocker:  newFakeDocker(),
		hostConfigs: make(map[string]*container.HostConfig),
		volumes:     make(map[string]volume.Volume),
		contents:    make(map[string]string),
	}
}

func (d *volumeDocker) ContainerCreate(ctx context.Context, cfg *container.Config, host *container.HostConfig, net *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error) {
	resp, err := d.fakeDocker.ContainerCreate(ctx, cfg, host, net, platform, name)
	if err == nil {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.hostConfigs[resp.ID] = host
		for _, m := range host.Mounts {
			if m.Type == "volume" && m.Source != "" {
				if _, exists := d.volumes[m.Source]; !exists {
					d.volumes[m.Source] = volume.Volume{Name: m.Source}
					d.contents[m.Source] = ""
				}
			}
		}
	}
	return resp, err
}

func (d *volumeDocker) VolumeInspect(_ context.Context, name string) (volume.Volume, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.inspectErrors[name]; err != nil {
		return volume.Volume{}, err
	}
	v, ok := d.volumes[name]
	if !ok {
		return volume.Volume{}, errdefs.NotFound(fmt.Errorf("no such volume %s", name))
	}
	return v, nil
}

func (d *volumeDocker) VolumeCreate(_ context.Context, opts volume.CreateOptions) (volume.Volume, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.volumeCreateErr != nil {
		return volume.Volume{}, d.volumeCreateErr
	}
	if d.createRace != nil {
		d.volumes[opts.Name] = *d.createRace
	}
	if v, exists := d.volumes[opts.Name]; exists {
		return v, nil
	}
	v := volume.Volume{Name: opts.Name, Labels: maps.Clone(opts.Labels)}
	d.volumes[opts.Name] = v
	d.contents[opts.Name] = ""
	return v, nil
}

func (d *volumeDocker) ContainerInspect(ctx context.Context, id string) (types.ContainerJSON, error) {
	info, err := d.fakeDocker.ContainerInspect(ctx, id)
	if err != nil {
		return info, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.containerInspectErr != nil {
		return types.ContainerJSON{}, d.containerInspectErr
	}
	for _, m := range d.hostConfigs[id].Mounts {
		info.Mounts = append(info.Mounts, types.MountPoint{
			Type: m.Type, Name: m.Source, Source: m.Source,
			Destination: m.Target, RW: !m.ReadOnly,
		})
	}
	return info, nil
}

func defaultVolumeSpec(t *testing.T, deployment, key string) compose.ContainerSpec {
	t.Helper()
	body := []byte(fmt.Sprintf(`services:
  app:
    image: alpine:3.20
    pull_policy: never
    network_mode: none
    volumes:
      - %s:/data
volumes:
  %s: {}
`, key, key))
	return declaredVolumeSpec(t, deployment, body)
}

func declaredVolumeSpec(t *testing.T, deployment string, body []byte) compose.ContainerSpec {
	t.Helper()
	if err := grpcsrv.ValidateJacoYAMLBytes([]byte("deployment: " + deployment + "\n")); err != nil {
		t.Fatalf("deployment %q rejected: %v", deployment, err)
	}
	if err := compose.Validate(body); err != nil {
		t.Fatalf("compose validation: %v", err)
	}
	project, err := compose.LoadBytes(body, "volumes.yml")
	if err != nil {
		t.Fatal(err)
	}
	overrides, err := compose.TopLevelVolumeNames(body)
	if err != nil {
		t.Fatal(err)
	}
	return compose.ToContainerSpec(project.Services["app"], compose.SpecOptions{
		ClusterID: "cluster-x", Deployment: deployment, Service: "app",
		ReplicaID: deployment + "-app-0", RaftIndex: 1,
		VolumeNameOverrides: overrides,
		VolumeDefinitions:   project.Volumes,
	})
}

func TestStart_ExternalVolumeMustExist(t *testing.T) {
	spec := declaredVolumeSpec(t, "orders", []byte(`services:
  app:
    image: alpine:3.20
    pull_policy: never
    network_mode: none
    volumes: [prod_data:/data]
volumes:
  prod_data:
    name: jaco_orders_prod_data
    external: true
`))
	d := newVolumeDocker()
	_, err := lifecycle.Start(context.Background(), d, spec)
	var ve *volumes.Error
	if !errors.As(err, &ve) || ve.Code != "external_volume_missing" {
		t.Fatalf("Start error = %v, want external_volume_missing", err)
	}
	if len(d.volumes) != 0 || len(d.containers) != 0 {
		t.Fatal("created empty replacement for an external volume")
	}
}

func TestStart_DefaultVolumeIdentitySeparatesCollidingTuples(t *testing.T) {
	d := newVolumeDocker()
	sources := make(map[string]string)
	for _, tuple := range []struct{ deployment, key string }{
		{"orders", "prod_data"},
		{"orders_prod", "data"},
	} {
		spec := defaultVolumeSpec(t, tuple.deployment, tuple.key)
		id, err := lifecycle.Start(context.Background(), d, spec)
		if err != nil {
			t.Fatalf("Start %s: %v", tuple.deployment, err)
		}
		host := d.hostConfigs[id]
		if host.NetworkMode != "none" || len(d.attached[id]) != 0 {
			t.Fatalf("container %s unexpectedly attached to a deployment network", id)
		}
		if len(host.Mounts) != 1 {
			t.Fatalf("container %s mounts = %v, want one volume", id, host.Mounts)
		}
		source := host.Mounts[0].Source
		if owner, exists := sources[source]; exists {
			t.Fatalf("%s/%s and %s share Docker volume %q on the same engine",
				tuple.deployment, tuple.key, owner, source)
		}
		sources[source] = tuple.deployment + "/" + tuple.key
	}
}

func TestStart_ExplicitNamesPreserveSharing(t *testing.T) {
	for _, tc := range []struct {
		name, definition, source string
		external                 bool
	}{
		{"name", "name: shared-data", "shared-data", false},
		{"external_name", "name: shared-data\n    external: true", "shared-data", true},
		{"external_key", "external: true", "shared", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newVolumeDocker()
			if tc.external {
				d.volumes[tc.source] = volume.Volume{
					Name: tc.source, Driver: "operator-driver",
					Labels: map[string]string{"operator": "keep"},
				}
			}
			for i, deployment := range []string{"orders", "orders_prod"} {
				spec := declaredVolumeSpec(t, deployment, []byte(fmt.Sprintf(`services:
  app:
    image: alpine:3.20
    pull_policy: never
    network_mode: none
    volumes: [shared:/data]
volumes:
  shared:
    %s
`, tc.definition)))
				id, err := lifecycle.Start(context.Background(), d, spec)
				if err != nil {
					t.Fatal(err)
				}
				source := d.hostConfigs[id].Mounts[0].Source
				if source != tc.source {
					t.Fatalf("explicit volume renamed to %q, want %q", source, tc.source)
				}
				if i == 0 {
					d.contents[source] = "intentionally shared data"
				} else if d.contents[source] != "intentionally shared data" {
					t.Fatal("explicit sharing lost the existing data")
				}
			}
			if len(d.volumes) != 1 || d.volumes[tc.source].Labels["jaco.volume_identity"] != "" {
				t.Fatal("explicit storage was replaced or claimed by JACO")
			}
			if tc.external && (d.volumes[tc.source].Driver != "operator-driver" || d.volumes[tc.source].Labels["operator"] != "keep") {
				t.Fatal("external volume configuration was changed")
			}
		})
	}
}

func TestStart_ExplicitLegacyAdoptionKeepsContainerAndData(t *testing.T) {
	d := newVolumeDocker()
	legacy := defaultVolumeSpec(t, "orders", "prod_data")
	legacy.Mounts = []compose.Mount{{Type: "volume", Source: "jaco_orders_prod_data", Target: "/data"}}
	ctx := context.Background()
	original, err := lifecycle.Start(ctx, d, legacy)
	if err != nil {
		t.Fatal(err)
	}
	d.contents["jaco_orders_prod_data"] = "operator-verified data"
	adopted := declaredVolumeSpec(t, "orders", []byte(`services:
  app:
    image: alpine:3.20
    pull_policy: never
    network_mode: none
    volumes: [prod_data:/data]
volumes:
  prod_data:
    name: jaco_orders_prod_data
    external: true
`))
	got, err := lifecycle.Start(ctx, d, adopted)
	if err != nil || got != original {
		t.Fatalf("same-source adoption replaced the running container: %q, %v", got, err)
	}
	source := d.hostConfigs[got].Mounts[0].Source
	if len(d.volumes) != 1 || d.contents[source] != "operator-verified data" ||
		d.volumes[source].Labels != nil {
		t.Fatal("explicit legacy adoption changed data or claimed ownership")
	}
}

func TestStart_DefaultVolumeCreatesOwnedStorage(t *testing.T) {
	d := newVolumeDocker()
	spec := defaultVolumeSpec(t, "orders", "prod_data")
	if _, err := lifecycle.Start(context.Background(), d, spec); err != nil {
		t.Fatal(err)
	}
	got := d.volumes[spec.Mounts[0].Source]
	want := map[string]string{
		"jaco.volume_identity": "2",
		"jaco.cluster_id":      "cluster-x",
		"jaco.deployment":      "orders",
		"jaco.volume_key":      "prod_data",
	}
	if !maps.Equal(got.Labels, want) {
		t.Fatalf("volume ownership labels = %v, want %v", got.Labels, want)
	}
}

func TestStart_ReusesOwnedDefaultVolume(t *testing.T) {
	d := newVolumeDocker()
	spec := defaultVolumeSpec(t, "orders", "prod_data")
	ctx := context.Background()
	first, err := lifecycle.Start(ctx, d, spec)
	if err != nil {
		t.Fatal(err)
	}
	source := d.hostConfigs[first].Mounts[0].Source
	d.contents[source] = "persistent application data"
	d.volumes[source].Labels["operator.annotation"] = "keep"

	secondSpec := spec
	secondSpec.ReplicaID = "orders-app-1"
	secondSpec.Labels = maps.Clone(spec.Labels)
	secondSpec.Labels["jaco.replica_id"] = secondSpec.ReplicaID
	second, err := lifecycle.Start(ctx, d, secondSpec)
	if err != nil {
		t.Fatal(err)
	}
	if d.hostConfigs[second].Mounts[0].Source != source {
		t.Fatal("replicas of the same deployment no longer share their declared volume")
	}

	spec.RaftIndex = 2
	spec.Labels["jaco.raft_index"] = "2"
	replaced, err := lifecycle.Start(ctx, d, spec)
	if err != nil || replaced == first {
		t.Fatalf("redeploy did not replace the container: %q, %v", replaced, err)
	}
	if err := lifecycle.Stop(ctx, d, spec.ReplicaID, 1); err != nil {
		t.Fatal(err)
	}
	restarted, err := lifecycle.Start(ctx, d, spec)
	if err != nil || restarted != replaced {
		t.Fatalf("restart did not reuse the container: %q, %v", restarted, err)
	}
	mounted := d.hostConfigs[restarted].Mounts[0].Source
	if mounted != source || d.contents[mounted] != "persistent application data" ||
		d.volumes[source].Labels["operator.annotation"] != "keep" || len(d.volumes) != 1 {
		t.Fatal("existing managed storage or its data was replaced")
	}
}

func TestStart_DefaultVolumeRejectsUnownedStorage(t *testing.T) {
	spec := defaultVolumeSpec(t, "orders", "prod_data")
	source := spec.Mounts[0].Source
	for _, field := range []string{
		"unlabelled", "jaco.cluster_id", "jaco.deployment",
		"jaco.volume_key", "jaco.volume_identity", "name",
	} {
		t.Run(field, func(t *testing.T) {
			d := newVolumeDocker()
			stored := volume.Volume{Name: source, Labels: map[string]string{
				"jaco.volume_identity": "2",
				"jaco.cluster_id":      "cluster-x",
				"jaco.deployment":      "orders",
				"jaco.volume_key":      "prod_data",
			}}
			switch field {
			case "unlabelled":
				stored.Labels = nil
			case "name":
				stored.Name = "different-volume"
			default:
				stored.Labels[field] = "different-owner"
			}
			d.volumes[source] = stored
			before := maps.Clone(stored.Labels)
			_, err := lifecycle.Start(context.Background(), d, spec)
			var ve *volumes.Error
			if !errors.As(err, &ve) || ve.Code != "volume_identity_conflict" {
				t.Fatalf("Start error = %v, want volume_identity_conflict", err)
			}
			if len(d.containers) != 0 {
				t.Fatal("created a container against unowned storage")
			}
			if !maps.Equal(d.volumes[source].Labels, before) {
				t.Fatal("changed existing volume ownership labels")
			}
		})
	}
}

func TestStart_ConcurrentReplicasReuseOwnedVolume(t *testing.T) {
	d := newVolumeDocker()
	base := defaultVolumeSpec(t, "orders", "prod_data")
	type result struct {
		id  string
		err error
	}
	results := make(chan result, 8)
	for i := 0; i < cap(results); i++ {
		spec := base
		spec.ReplicaID = fmt.Sprintf("orders-app-%d", i)
		spec.Labels = maps.Clone(base.Labels)
		spec.Labels["jaco.replica_id"] = spec.ReplicaID
		go func() {
			id, err := lifecycle.Start(context.Background(), d, spec)
			results <- result{id, err}
		}()
	}
	var ids []string
	for i := 0; i < cap(results); i++ {
		got := <-results
		if got.err != nil {
			t.Errorf("concurrent Start: %v", got.err)
		}
		ids = append(ids, got.id)
	}
	if t.Failed() {
		return
	}
	if len(d.volumes) != 1 {
		t.Fatalf("created %d volumes for one owner", len(d.volumes))
	}
	for _, id := range ids {
		if d.hostConfigs[id].Mounts[0].Source != base.Mounts[0].Source {
			t.Fatalf("replica %s did not reuse the owner's volume", id)
		}
	}
}

func TestStart_DefaultVolumeRejectsConcurrentForeignCreation(t *testing.T) {
	d := newVolumeDocker()
	spec := defaultVolumeSpec(t, "orders", "prod_data")
	source := spec.Mounts[0].Source
	d.createRace = &volume.Volume{Name: source}
	d.contents[source] = "concurrent owner's data"
	_, err := lifecycle.Start(context.Background(), d, spec)
	var ve *volumes.Error
	if !errors.As(err, &ve) || ve.Code != "volume_identity_conflict" {
		t.Fatalf("Start error = %v, want volume_identity_conflict", err)
	}
	if len(d.containers) != 0 || d.volumes[source].Labels != nil ||
		d.contents[source] != "concurrent owner's data" {
		t.Fatal("claimed or mounted a volume created concurrently by another owner")
	}
}

func TestStart_VolumeDockerErrorsFailClosed(t *testing.T) {
	base := defaultVolumeSpec(t, "orders", "prod_data")
	cause := errors.New("Docker operation failed")
	for _, operation := range []string{"managed_inspect", "legacy_inspect", "external_inspect", "create", "container_inspect"} {
		t.Run(operation, func(t *testing.T) {
			d := newVolumeDocker()
			spec := base
			var existingID string
			wantVolumes, wantContainers := 0, 0
			switch operation {
			case "managed_inspect":
				d.inspectErrors = map[string]error{spec.Mounts[0].Source: cause}
			case "legacy_inspect":
				d.inspectErrors = map[string]error{"jaco_orders_prod_data": cause}
			case "external_inspect":
				spec.Mounts = []compose.Mount{{Type: "volume", Source: "external-data", Target: "/data", External: true}}
				d.inspectErrors = map[string]error{"external-data": cause}
			case "create":
				d.volumeCreateErr = cause
			case "container_inspect":
				var err error
				existingID, err = lifecycle.Start(context.Background(), d, spec)
				if err != nil {
					t.Fatal(err)
				}
				d.containerInspectErr = cause
				spec.RaftIndex++
				wantVolumes, wantContainers = 1, 1
			}
			id, err := lifecycle.Start(context.Background(), d, spec)
			if !errors.Is(err, cause) {
				t.Fatalf("Start error = %v, want wrapped Docker error", err)
			}
			if id != existingID || len(d.volumes) != wantVolumes || len(d.containers) != wantContainers {
				t.Fatal("created or replaced resources after failed Docker preflight")
			}
			if existingID != "" && d.containers[existingID].State != "running" {
				t.Fatal("stopped the existing container after inspection failed")
			}
		})
	}
}

func TestStart_DefaultVolumeRejectsIncompleteIdentity(t *testing.T) {
	base := defaultVolumeSpec(t, "orders", "prod_data")
	for _, field := range []string{"cluster", "deployment", "key", "source", "type"} {
		t.Run(field, func(t *testing.T) {
			spec := base
			spec.Mounts = append([]compose.Mount(nil), base.Mounts...)
			switch field {
			case "cluster":
				spec.ClusterID = ""
			case "deployment":
				spec.Deployment = ""
			case "key":
				spec.Mounts[0].DefaultVolumeKey = "other"
			case "source":
				spec.Mounts[0].Source = "other-volume"
			case "type":
				spec.Mounts[0].Type = "bind"
			}
			d := newVolumeDocker()
			_, err := lifecycle.Start(context.Background(), d, spec)
			var ve *volumes.Error
			if !errors.As(err, &ve) || ve.Code != "volume_identity_conflict" {
				t.Fatalf("Start error = %v, want volume_identity_conflict", err)
			}
			if len(d.volumes) != 0 || len(d.containers) != 0 {
				t.Fatal("created resources before validating volume identity")
			}
		})
	}
}

func TestStart_LegacyDefaultVolumeRequiresMigration(t *testing.T) {
	d := newVolumeDocker()
	const legacy = "jaco_orders_prod_data"
	d.volumes[legacy] = volume.Volume{Name: legacy}
	for _, tuple := range []struct{ deployment, key string }{
		{"orders", "prod_data"},
		{"orders_prod", "data"},
	} {
		spec := defaultVolumeSpec(t, tuple.deployment, tuple.key)
		_, err := lifecycle.Start(context.Background(), d, spec)
		var ve *volumes.Error
		if !errors.As(err, &ve) || ve.Code != "volume_migration_required" {
			t.Fatalf("Start %s/%s error = %v, want volume_migration_required", tuple.deployment, tuple.key, err)
		}
		if !strings.Contains(err.Error(), legacy) || !strings.Contains(err.Error(), "external: true") {
			t.Fatalf("migration error is not actionable: %v", err)
		}
		if len(d.containers) != 0 || len(d.volumes) != 1 {
			t.Fatal("created replacement storage or a container despite ambiguous legacy data")
		}
		if d.volumes[legacy].Labels != nil {
			t.Fatal("silently adopted an unlabelled legacy volume")
		}
	}
}

func TestStart_ValidatesAllVolumesBeforeCreatingAny(t *testing.T) {
	d := newVolumeDocker()
	spec := defaultVolumeSpec(t, "orders", "cache")
	spec.Mounts[0].Target = "/cache"
	data := defaultVolumeSpec(t, "orders", "prod_data")
	spec.Mounts = append(spec.Mounts, data.Mounts...)
	const legacy = "jaco_orders_prod_data"
	d.volumes[legacy] = volume.Volume{Name: legacy}
	_, err := lifecycle.Start(context.Background(), d, spec)
	var ve *volumes.Error
	if !errors.As(err, &ve) || ve.Code != "volume_migration_required" {
		t.Fatalf("Start error = %v, want volume_migration_required", err)
	}
	if len(d.volumes) != 1 || len(d.containers) != 0 {
		t.Fatal("created storage before finishing legacy preflight for all mounts")
	}
}

func TestStart_DoesNotRecreateMissingPreviouslyMountedVolume(t *testing.T) {
	for _, target := range []string{"/data", "/moved"} {
		t.Run(target, func(t *testing.T) {
			d := newVolumeDocker()
			spec := defaultVolumeSpec(t, "orders", "prod_data")
			ctx := context.Background()
			original, err := lifecycle.Start(ctx, d, spec)
			if err != nil {
				t.Fatal(err)
			}
			source := spec.Mounts[0].Source
			d.contents[source] = "data awaiting recovery"
			delete(d.volumes, source)
			spec.RaftIndex++
			spec.Mounts[0].Target = target
			id, err := lifecycle.Start(ctx, d, spec)
			var ve *volumes.Error
			if !errors.As(err, &ve) || ve.Code != "volume_migration_required" {
				t.Fatalf("Start error = %v, want volume_migration_required", err)
			}
			if id != original || len(d.volumes) != 0 || d.containers[original].State != "running" ||
				d.contents[source] != "data awaiting recovery" {
				t.Fatal("recreated empty storage for an existing container's missing volume")
			}
		})
	}
}

func TestStart_RetainsLegacyContainerEvenWhenManagedVolumeExists(t *testing.T) {
	base := defaultVolumeSpec(t, "orders", "prod_data")
	for _, state := range []string{"running", "exited"} {
		for _, raftIndex := range []uint64{1, 2} {
			t.Run(fmt.Sprintf("%s/raft-%d", state, raftIndex), func(t *testing.T) {
				d := newVolumeDocker()
				legacySpec := base
				legacySpec.Mounts = []compose.Mount{{Type: "volume", Source: "jaco_orders_prod_data", Target: "/data"}}
				id, err := lifecycle.Start(context.Background(), d, legacySpec)
				if err != nil {
					t.Fatal(err)
				}
				d.containers[id].State = state
				d.volumes[base.Mounts[0].Source] = volume.Volume{Name: base.Mounts[0].Source, Labels: map[string]string{
					"jaco.volume_identity": "2",
					"jaco.cluster_id":      "cluster-x",
					"jaco.deployment":      "orders",
					"jaco.volume_key":      "prod_data",
				}}
				spec := base
				spec.RaftIndex = raftIndex
				gotID, err := lifecycle.Start(context.Background(), d, spec)
				var ve *volumes.Error
				if !errors.As(err, &ve) || ve.Code != "volume_migration_required" {
					t.Fatalf("Start error = %v, want volume_migration_required", err)
				}
				if gotID != id || len(d.containers) != 1 || d.containers[id].State != state {
					t.Fatal("stopped, restarted or replaced an existing legacy container")
				}
				if len(d.volumes) != 2 || d.volumes["jaco_orders_prod_data"].Labels != nil {
					t.Fatal("mutated legacy storage")
				}
			})
		}
	}
}
