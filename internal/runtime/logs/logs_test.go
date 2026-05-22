package logs_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
	"github.com/PatrickRuddiman/jaco/internal/runtime/logs"
)

// fakeDocker only implements ContainerLogs. The other methods on
// dockerx.Docker panic via the embedded interface — anything else is a
// wiring bug, not a silent pass.
type fakeDocker struct {
	dockerx.Docker
	frames []byte
	err    error
}

func (f *fakeDocker) ContainerLogs(_ context.Context, _ string, _ container.LogsOptions) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(bytes.NewReader(f.frames)), nil
}

// silence unused-import checks for the docker types we hold on hand
var (
	_ = types.ContainerJSON{}
	_ = image.PullOptions{}
	_ = network.NetworkingConfig{}
	_ = volume.Volume{}
	_ = ocispec.Platform{}
)

// dockerFrame builds one wire-frame in the multiplexed stdout/stderr format
// docker's ContainerLogs returns. STREAM is 1 (stdout) or 2 (stderr).
func dockerFrame(stream byte, payload string) []byte {
	buf := make([]byte, 8+len(payload))
	buf[0] = stream
	binary.BigEndian.PutUint32(buf[4:8], uint32(len(payload)))
	copy(buf[8:], payload)
	return buf
}

func TestStream_DemuxesStdoutAndStderr(t *testing.T) {
	// Two stdout lines and one stderr line, with docker-style timestamp
	// prefixes (Timestamps=true).
	ts := "2026-04-01T12:00:00.123456789Z"
	frames := bytes.Join([][]byte{
		dockerFrame(1, ts+" hello-1\n"),
		dockerFrame(2, ts+" warn-1\n"),
		dockerFrame(1, ts+" hello-2\n"),
	}, nil)
	d := &fakeDocker{frames: frames}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ch, err := logs.Stream(ctx, d, "sample-web-0", "c-1", "node-a", logs.Options{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var got []string
	var streams []string
	for ll := range ch {
		got = append(got, ll.GetLine())
		streams = append(streams, ll.GetStream())
	}

	want := []string{"hello-1", "warn-1", "hello-2"}
	if !equalStrings(got, want) {
		t.Errorf("lines = %v, want %v", got, want)
	}
	wantStreams := []string{"stdout", "stderr", "stdout"}
	if !equalStrings(streams, wantStreams) {
		t.Errorf("streams = %v, want %v", streams, wantStreams)
	}
}

func TestStream_StampsReplicaIDAndHost(t *testing.T) {
	frames := dockerFrame(1, "ignore-me\n")
	d := &fakeDocker{frames: frames}

	ch, err := logs.Stream(context.Background(), d, "sample-web-0", "c-1", "node-a", logs.Options{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := <-ch
	if got.GetReplicaId() != "sample-web-0" {
		t.Errorf("replica_id = %q", got.GetReplicaId())
	}
	if got.GetHost() != "node-a" {
		t.Errorf("host = %q", got.GetHost())
	}
}

func TestStream_HandlesPartialLineThenFlush(t *testing.T) {
	// A stream that ends without a trailing newline must still emit the
	// partial line on flush.
	d := &fakeDocker{frames: dockerFrame(1, "no-newline-trailer")}
	ch, err := logs.Stream(context.Background(), d, "r", "c", "h", logs.Options{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var lines []string
	for ll := range ch {
		lines = append(lines, ll.GetLine())
	}
	if len(lines) != 1 || lines[0] != "no-newline-trailer" {
		t.Errorf("lines = %v, want [no-newline-trailer]", lines)
	}
}

func TestStream_ContextCancellationClosesChannel(t *testing.T) {
	// Many frames; cancel mid-stream and assert the channel closes.
	var buf bytes.Buffer
	for i := 0; i < 100; i++ {
		buf.Write(dockerFrame(1, "line\n"))
	}
	d := &fakeDocker{frames: buf.Bytes()}

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := logs.Stream(ctx, d, "r", "c", "h", logs.Options{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// Read a few then cancel.
	for i := 0; i < 3; i++ {
		<-ch
	}
	cancel()
	// Drain until close. The sink emit() observes ctx.Done() and the
	// goroutine closes the channel.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, ok := <-ch
		if !ok {
			return
		}
	}
	t.Fatalf("channel did not close after cancel")
}

func TestStream_PropagatesDockerError(t *testing.T) {
	d := &fakeDocker{err: io.ErrUnexpectedEOF}
	_, err := logs.Stream(context.Background(), d, "r", "c", "h", logs.Options{})
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestStream_SinceTranslatesToDockerSinceOption(t *testing.T) {
	captured := container.LogsOptions{}
	d := &fakeDockerCapture{captured: &captured}
	_, _ = logs.Stream(context.Background(), d, "r", "c", "h", logs.Options{Since: 30 * time.Second})
	if captured.Since == "" {
		t.Fatalf("Since not set")
	}
	if _, err := time.Parse(time.RFC3339Nano, captured.Since); err != nil {
		t.Errorf("Since = %q is not RFC3339: %v", captured.Since, err)
	}
}

// fakeDockerCapture records the LogsOptions but returns empty content.
type fakeDockerCapture struct {
	dockerx.Docker
	captured *container.LogsOptions
}

func (f *fakeDockerCapture) ContainerLogs(_ context.Context, _ string, opts container.LogsOptions) (io.ReadCloser, error) {
	*f.captured = opts
	return io.NopCloser(strings.NewReader("")), nil
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
