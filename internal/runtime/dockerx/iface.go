// Package dockerx wraps the moby docker client and exports the narrow Docker
// interface JACO's runtime / pull / volumes / lifecycle code depends on.
// Defining the interface in this package lets every consumer mock it without
// importing the docker module directly.
package dockerx

import (
	"context"
	"io"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Docker is the subset of *github.com/docker/docker/client.Client JACO uses.
// The real client implements every method below; tests provide partial fakes.
type Docker interface {
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, opts container.StartOptions) error
	ContainerStop(ctx context.Context, containerID string, opts container.StopOptions) error
	ContainerRemove(ctx context.Context, containerID string, opts container.RemoveOptions) error
	ContainerInspect(ctx context.Context, containerID string) (types.ContainerJSON, error)
	ContainerList(ctx context.Context, opts container.ListOptions) ([]types.Container, error)
	ContainerLogs(ctx context.Context, containerID string, opts container.LogsOptions) (io.ReadCloser, error)
	ImagePull(ctx context.Context, ref string, opts image.PullOptions) (io.ReadCloser, error)
	VolumeCreate(ctx context.Context, opts volume.CreateOptions) (volume.Volume, error)
	NetworkConnect(ctx context.Context, networkID, containerID string, config *network.EndpointSettings) error
	NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error)
	NetworkRemove(ctx context.Context, networkID string) error
	NetworkList(ctx context.Context, options network.ListOptions) ([]network.Summary, error)
}
