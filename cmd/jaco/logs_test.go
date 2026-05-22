package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// fakeLogsStream implements grpc.ClientStream + pb.Deploy_LogsClient for the
// unit test, replaying canned LogLine values then returning io.EOF.
type fakeLogsStream struct {
	grpc.ClientStream
	lines []*pb.LogLine
	idx   int
}

func (s *fakeLogsStream) Recv() (*pb.LogLine, error) {
	if s.idx >= len(s.lines) {
		return nil, io.EOF
	}
	ll := s.lines[s.idx]
	s.idx++
	return ll, nil
}

// The remaining ClientStream methods aren't exercised; ClientStream
// satisfies them via the embedded nil.
func (s *fakeLogsStream) Header() (metadata.MD, error) { return nil, nil }
func (s *fakeLogsStream) Trailer() metadata.MD         { return nil }
func (s *fakeLogsStream) CloseSend() error             { return nil }
func (s *fakeLogsStream) Context() context.Context     { return context.Background() }
func (s *fakeLogsStream) SendMsg(any) error            { return nil }
func (s *fakeLogsStream) RecvMsg(any) error            { return nil }

// fakeDeployLogsClient just exposes the Logs method; the rest of
// pb.DeployClient is inherited from the embedded interface (nil).
type fakeDeployLogsClient struct {
	pb.DeployClient
	logsFn func(ctx context.Context, req *pb.LogsRequest) (pb.Deploy_LogsClient, error)
}

func (f *fakeDeployLogsClient) Logs(ctx context.Context, req *pb.LogsRequest, _ ...grpc.CallOption) (pb.Deploy_LogsClient, error) {
	return f.logsFn(ctx, req)
}

func TestRunLogs_FormatsLineAsReplicaAtHost(t *testing.T) {
	stream := &fakeLogsStream{lines: []*pb.LogLine{
		{ReplicaId: "sample-web-0", Host: "node-a", Line: "hello-1"},
		{ReplicaId: "sample-web-1", Host: "node-b", Line: "hello-2"},
	}}
	var captured *pb.LogsRequest
	client := &fakeDeployLogsClient{
		logsFn: func(_ context.Context, req *pb.LogsRequest) (pb.Deploy_LogsClient, error) {
			captured = req
			return stream, nil
		},
	}

	var out bytes.Buffer
	err := runLogs(context.Background(), client, "sample", "web", true, 60, &out)
	if err != nil {
		t.Fatalf("runLogs: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"[sample-web-0@node-a] hello-1",
		"[sample-web-1@node-b] hello-2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	// Request shape passed through to the server.
	if captured.GetDeployment() != "sample" || captured.GetService() != "web" {
		t.Errorf("request shape = %+v", captured)
	}
	if !captured.GetFollow() {
		t.Errorf("Follow not propagated")
	}
	if captured.GetSinceSeconds() != 60 {
		t.Errorf("SinceSeconds = %d, want 60", captured.GetSinceSeconds())
	}
}

func TestRunLogs_PropagatesServerError(t *testing.T) {
	client := &fakeDeployLogsClient{
		logsFn: func(_ context.Context, _ *pb.LogsRequest) (pb.Deploy_LogsClient, error) {
			return nil, errors.New("transport failed")
		},
	}
	err := runLogs(context.Background(), client, "sample", "web", false, 60, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "transport failed") {
		t.Errorf("err = %v", err)
	}
}

func TestSplitDeploymentService(t *testing.T) {
	cases := []struct {
		in       string
		dep, svc string
		err      bool
	}{
		{"sample/web", "sample", "web", false},
		{"sample", "", "", true},
		{"/web", "", "", true},
		{"sample/", "", "", true},
	}
	for _, c := range cases {
		dep, svc, err := splitDeploymentService(c.in)
		if c.err {
			if err == nil {
				t.Errorf("splitDeploymentService(%q) = (%q, %q, nil); want error", c.in, dep, svc)
			}
			continue
		}
		if err != nil || dep != c.dep || svc != c.svc {
			t.Errorf("splitDeploymentService(%q) = (%q, %q, %v); want (%q, %q, nil)",
				c.in, dep, svc, err, c.dep, c.svc)
		}
	}
}

func TestParseSinceSeconds(t *testing.T) {
	got, err := parseSinceSeconds("1h")
	if err != nil || got != 3600 {
		t.Errorf("1h → %d, want 3600 (err=%v)", got, err)
	}
	got, err = parseSinceSeconds("30m")
	if err != nil || got != 1800 {
		t.Errorf("30m → %d, want 1800", got)
	}
	if _, err := parseSinceSeconds("not-a-duration"); err == nil {
		t.Errorf("expected error on garbage input")
	}
}
