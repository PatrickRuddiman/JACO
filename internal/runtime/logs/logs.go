// Package logs streams demultiplexed stdout / stderr lines from a docker
// container. The runtime side (this package) supplies an iterator-style
// channel; control-plane gRPC handlers (Deploy.Logs, Internal.Logs) shape
// the wire surface around it.
package logs

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// Options carries the optional inputs of Stream.
type Options struct {
	// Since (when non-zero) maps to docker's `since` option — only lines
	// emitted after Now()-Since are streamed.
	Since time.Duration
	// Follow keeps the stream open and continues to deliver new lines.
	Follow bool
}

// Stream opens a container-logs stream and returns a channel of pb.LogLine.
// The channel closes when the docker stream ends, when ctx is cancelled, or
// on read error. The returned host string is stamped onto every LogLine; the
// caller knows which node ran the container.
func Stream(ctx context.Context, d dockerx.Docker, replicaID, containerID, host string, opts Options) (<-chan *pb.LogLine, error) {
	logOpts := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     opts.Follow,
		Timestamps: true,
	}
	if opts.Since > 0 {
		logOpts.Since = time.Now().Add(-opts.Since).Format(time.RFC3339Nano)
	}
	rc, err := d.ContainerLogs(ctx, containerID, logOpts)
	if err != nil {
		return nil, err
	}

	out := make(chan *pb.LogLine, 64)
	stdoutSink := newLineSink(ctx, replicaID, host, "stdout", out)
	stderrSink := newLineSink(ctx, replicaID, host, "stderr", out)

	go func() {
		defer close(out)
		defer rc.Close()
		_, _ = stdcopy.StdCopy(stdoutSink, stderrSink, rc)
		// Flush any trailing partial line that didn't end in '\n'.
		stdoutSink.flush()
		stderrSink.flush()
	}()

	return out, nil
}

// lineSink demuxes a stream of bytes (post-stdcopy) into newline-terminated
// LogLines. Docker's `Timestamps: true` prepends each line with an RFC3339
// timestamp + space; we strip that for the LogLine.Ts field.
type lineSink struct {
	ctx       context.Context
	replicaID string
	host      string
	stream    string
	out       chan<- *pb.LogLine

	mu  sync.Mutex
	buf strings.Builder
}

func newLineSink(ctx context.Context, replicaID, host, stream string, out chan<- *pb.LogLine) *lineSink {
	return &lineSink{ctx: ctx, replicaID: replicaID, host: host, stream: stream, out: out}
}

func (s *lineSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Append + scan for newlines.
	s.buf.Write(p)
	str := s.buf.String()
	for {
		idx := strings.IndexByte(str, '\n')
		if idx < 0 {
			break
		}
		line := str[:idx]
		str = str[idx+1:]
		if err := s.emit(line); err != nil {
			s.buf.Reset()
			s.buf.WriteString(str)
			return len(p), err
		}
	}
	s.buf.Reset()
	s.buf.WriteString(str)
	return len(p), nil
}

func (s *lineSink) flush() {
	s.mu.Lock()
	rem := s.buf.String()
	s.buf.Reset()
	s.mu.Unlock()
	if rem != "" {
		_ = s.emit(rem)
	}
}

func (s *lineSink) emit(line string) error {
	ts, body := splitDockerTimestamp(line)
	ll := &pb.LogLine{
		ReplicaId: s.replicaID,
		Host:      s.host,
		Stream:    s.stream,
		Line:      body,
	}
	if !ts.IsZero() {
		ll.Ts = timestamppb.New(ts)
	}
	select {
	case s.out <- ll:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

// splitDockerTimestamp parses the `RFC3339 ` prefix docker prepends when
// Timestamps=true. Returns a zero time + the original string if no timestamp
// is found (which can happen if the container produced an empty line).
func splitDockerTimestamp(line string) (time.Time, string) {
	if line == "" {
		return time.Time{}, ""
	}
	sp := strings.IndexByte(line, ' ')
	if sp < 0 {
		return time.Time{}, line
	}
	if ts, err := time.Parse(time.RFC3339Nano, line[:sp]); err == nil {
		return ts, line[sp+1:]
	}
	return time.Time{}, line
}

