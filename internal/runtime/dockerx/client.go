package dockerx

import (
	"fmt"

	"github.com/docker/docker/client"
)

// DefaultSocket is the Linux docker engine socket path.
const DefaultSocket = "unix:///var/run/docker.sock"

// Client wraps *client.Client. Use the embedded *client.Client to access the
// full docker API surface; JACO code generally takes the narrower Docker
// interface and accepts this concrete type via implicit satisfaction.
type Client struct {
	*client.Client
}

// New constructs a docker client against socket. Empty socket defaults to
// DefaultSocket. API-version negotiation is enabled so the client adapts to
// whichever engine version is on the host.
func New(socket string) (*Client, error) {
	if socket == "" {
		socket = DefaultSocket
	}
	c, err := client.NewClientWithOpts(
		client.WithHost(socket),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Client{Client: c}, nil
}
