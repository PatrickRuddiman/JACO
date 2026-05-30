package lifecycle

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

// TestBuildConfig_ForwardsDevicesAndGPUs — issues #115 and #116: every
// ContainerSpec.Devices entry maps onto HostConfig.Resources.Devices, and
// every GPURequest expands into HostConfig.Resources.DeviceRequests with
// the OR-of-AND capability shape docker expects.
func TestBuildConfig_ForwardsDevicesAndGPUs(t *testing.T) {
	spec := compose.ContainerSpec{
		Image: "nvidia/cuda:12",
		Devices: []compose.DeviceMapping{
			{Source: "/dev/fuse", Target: "/dev/fuse", Permissions: "rwm"},
			{Source: "/dev/snd", Target: "/dev/snd"},
		},
		GPURequests: []compose.GPURequest{
			{
				Driver:       "nvidia",
				Count:        -1, // gpus: all
				Capabilities: []string{"gpu", "compute"},
				DeviceIDs:    []string{"GPU-uuid"},
				Options:      map[string]string{"runtime": "nvidia"},
			},
		},
	}
	_, hc, _ := buildConfig(spec)

	// Devices forwarded with permissions preserved.
	if got, want := len(hc.Devices), 2; got != want {
		t.Fatalf("len(Devices) = %d, want %d", got, want)
	}
	if hc.Devices[0] != (container.DeviceMapping{
		PathOnHost:        "/dev/fuse",
		PathInContainer:   "/dev/fuse",
		CgroupPermissions: "rwm",
	}) {
		t.Errorf("Devices[0] = %+v", hc.Devices[0])
	}
	if hc.Devices[1].CgroupPermissions != "" {
		t.Errorf("Devices[1].CgroupPermissions = %q, want empty (docker default)", hc.Devices[1].CgroupPermissions)
	}

	// GPU request shape (capabilities wrapped).
	if got, want := len(hc.DeviceRequests), 1; got != want {
		t.Fatalf("len(DeviceRequests) = %d, want %d", got, want)
	}
	dr := hc.DeviceRequests[0]
	if dr.Driver != "nvidia" {
		t.Errorf("Driver = %q, want nvidia", dr.Driver)
	}
	if dr.Count != -1 {
		t.Errorf("Count = %d, want -1 (gpus: all)", dr.Count)
	}
	if len(dr.Capabilities) != 1 {
		t.Fatalf("Capabilities outer = %v, want one OR group", dr.Capabilities)
	}
	if got := strings.Join(dr.Capabilities[0], ","); got != "gpu,compute" {
		t.Errorf("Capabilities[0] = %v, want [gpu compute]", dr.Capabilities[0])
	}
	if len(dr.DeviceIDs) != 1 || dr.DeviceIDs[0] != "GPU-uuid" {
		t.Errorf("DeviceIDs = %v", dr.DeviceIDs)
	}
	if dr.Options["runtime"] != "nvidia" {
		t.Errorf("Options[runtime] = %q", dr.Options["runtime"])
	}
}

// TestBuildConfig_Batch2ZeroValues — empty Devices/GPURequests carry nil
// into HostConfig (no override → docker default applies).
func TestBuildConfig_Batch2ZeroValues(t *testing.T) {
	_, hc, _ := buildConfig(compose.ContainerSpec{Image: "nginx:1.27"})
	if hc.Devices != nil {
		t.Errorf("Devices = %v, want nil", hc.Devices)
	}
	if hc.DeviceRequests != nil {
		t.Errorf("DeviceRequests = %v, want nil", hc.DeviceRequests)
	}
}

// fakeDocker is the minimum dockerx.Docker surface the pull_policy test
// needs. ImagePull records that it was called; every other method either
// returns a fixed "no such image" / "no container matched" response or
// is unimplemented (the test never calls them).
type fakeDocker struct {
	pullCalled bool
}

func (f *fakeDocker) ContainerCreate(context.Context, *container.Config, *container.HostConfig, *network.NetworkingConfig, *ocispec.Platform, string) (container.CreateResponse, error) {
	return container.CreateResponse{}, errors.New("no such image")
}
func (f *fakeDocker) ContainerStart(context.Context, string, container.StartOptions) error {
	return nil
}
func (f *fakeDocker) ContainerStop(context.Context, string, container.StopOptions) error {
	return nil
}
func (f *fakeDocker) ContainerRemove(context.Context, string, container.RemoveOptions) error {
	return nil
}
func (f *fakeDocker) ContainerInspect(context.Context, string) (types.ContainerJSON, error) {
	return types.ContainerJSON{}, nil
}
func (f *fakeDocker) ContainerList(context.Context, container.ListOptions) ([]types.Container, error) {
	return nil, nil
}
func (f *fakeDocker) ContainerLogs(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
	return nil, nil
}
func (f *fakeDocker) ImagePull(context.Context, string, image.PullOptions) (io.ReadCloser, error) {
	f.pullCalled = true
	return io.NopCloser(strings.NewReader("")), nil
}
func (f *fakeDocker) VolumeCreate(context.Context, volume.CreateOptions) (volume.Volume, error) {
	return volume.Volume{}, nil
}
func (f *fakeDocker) NetworkConnect(context.Context, string, string, *network.EndpointSettings) error {
	return nil
}
func (f *fakeDocker) NetworkCreate(context.Context, string, network.CreateOptions) (network.CreateResponse, error) {
	return network.CreateResponse{}, nil
}
func (f *fakeDocker) NetworkRemove(context.Context, string) error { return nil }
func (f *fakeDocker) NetworkList(context.Context, network.ListOptions) ([]network.Summary, error) {
	return nil, nil
}
func (f *fakeDocker) NetworkInspect(context.Context, string, network.InspectOptions) (network.Inspect, error) {
	return network.Inspect{}, nil
}

// TestStart_PullPolicyNeverSkipsImagePull — issue #120: when the spec
// declares pull_policy: never the lifecycle path must skip the pull
// entirely. ContainerCreate then surfaces docker's "no such image"
// error untouched, which is the documented signal for air-gapped
// operators that the side-loaded image is missing.
func TestStart_PullPolicyNeverSkipsImagePull(t *testing.T) {
	d := &fakeDocker{}
	_, err := Start(context.Background(), d, compose.ContainerSpec{
		ReplicaID:  "r-1",
		Image:      "nginx:1.27",
		PullPolicy: "never",
	})
	if err == nil {
		t.Fatalf("Start: expected ContainerCreate to error (no such image), got nil")
	}
	if !strings.Contains(err.Error(), "no such image") {
		t.Errorf("error = %q, want to wrap docker's no-such-image", err.Error())
	}
	if d.pullCalled {
		t.Errorf("ImagePull was called even though pull_policy: never")
	}
}

// TestStart_PullPolicyMissingPulls — for every other policy (always,
// missing, build, unset) the lifecycle path must still call ImagePull so
// fresh hosts cache the layers before ContainerCreate runs.
func TestStart_PullPolicyMissingPulls(t *testing.T) {
	for _, p := range []string{"always", "missing", "build", ""} {
		t.Run("p="+p, func(t *testing.T) {
			d := &fakeDocker{}
			_, _ = Start(context.Background(), d, compose.ContainerSpec{
				ReplicaID:  "r-1",
				Image:      "nginx:1.27",
				PullPolicy: p,
			})
			if !d.pullCalled {
				t.Errorf("pull_policy=%q: ImagePull was NOT called", p)
			}
		})
	}
}
