//go:build docker

package logs_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
	"github.com/PatrickRuddiman/jaco/internal/runtime/logs"
)

// TestIntegration_StreamCapturesRealLines starts a busybox container
// that prints to stdout/stderr, calls logs.Stream against the real
// docker daemon, asserts both streams flow through.
// Skipped when JACO_INTEGRATION_DOCKER is unset.
func TestIntegration_StreamCapturesRealLines(t *testing.T) {
	if os.Getenv("JACO_INTEGRATION_DOCKER") == "" {
		t.Skip("set JACO_INTEGRATION_DOCKER=1 to enable")
	}
	d, err := dockerx.New("")
	if err != nil {
		t.Skipf("docker unreachable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := d.ContainerCreate(ctx,
		&container.Config{
			Image: "busybox",
			Cmd:   []string{"sh", "-c", `printf 'out-line-1\n' && printf 'err-line-1\n' >&2 && sleep 0.2`},
			Labels: map[string]string{"jaco.replica_id": "logs-int-1"},
		},
		&container.HostConfig{},
		&network.NetworkingConfig{},
		&ocispec.Platform{},
		"jaco-logs-int")
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	t.Cleanup(func() {
		_ = d.ContainerRemove(context.Background(), resp.ID, container.RemoveOptions{Force: true})
	})

	if err := d.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}

	// Stream logs. Container exits quickly so Follow=false captures
	// both lines.
	ch, err := logs.Stream(ctx, d, "logs-int-1", resp.ID, "test-host", logs.Options{Follow: false})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var bodies []string
	for ll := range ch {
		bodies = append(bodies, ll.GetStream()+":"+ll.GetLine())
	}

	joined := strings.Join(bodies, "\n")
	if !strings.Contains(joined, "out-line-1") || !strings.Contains(joined, "err-line-1") {
		t.Errorf("missing expected lines; got:\n%s", joined)
	}
}
