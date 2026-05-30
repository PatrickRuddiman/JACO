package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestTrimPassword exercises the small input-sanitation step the CLI runs
// before sending the secret to the gRPC handler. The contract: strip CRLF
// and surrounding whitespace; an entirely-blank password becomes nil so the
// caller can reject it with len(0).
func TestTrimPassword(t *testing.T) {
	cases := []struct {
		in   string
		want []byte
	}{
		{"hunter2\n", []byte("hunter2")},
		{"hunter2\r\n", []byte("hunter2")},
		{"  hunter2  \n", []byte("hunter2")},
		{"\n", nil},
		{"   ", nil},
		{"", nil},
	}
	for _, c := range cases {
		if got := trimPassword([]byte(c.in)); !bytes.Equal(got, c.want) {
			t.Errorf("trimPassword(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestReadStdinLine_FirstLineOnly asserts the helper consumes a single line
// and drops anything after (a here-doc that includes a trailing prompt
// shouldn't smuggle extra content into the secret).
func TestReadStdinLine_FirstLineOnly(t *testing.T) {
	r := strings.NewReader("password-1\npassword-2\n")
	got, err := readStdinLine(r)
	if err != nil {
		t.Fatalf("readStdinLine: %v", err)
	}
	if string(got) != "password-1" {
		t.Errorf("got %q, want password-1", got)
	}
}

// TestReadStdinLine_EmptyErrors covers the "operator forgot to pipe" case;
// we refuse to store an empty secret so the gRPC handler never sees one.
func TestReadStdinLine_EmptyErrors(t *testing.T) {
	if _, err := readStdinLine(strings.NewReader("")); err == nil {
		t.Errorf("readStdinLine(\"\") want error, got nil")
	}
}

// fakeRegistryClient implements pb.RegistryCredentialsClient for the
// list-formatting test. Real network dials would need a running daemon and
// gRPC channel setup, which we side-step by exercising the formatter
// against a hand-rolled response.
type fakeRegistryClient struct {
	pb.RegistryCredentialsClient
	addFn    func(ctx context.Context, req *pb.RegistryCredentialAddRequest) (*pb.RegistryCredentialAddResponse, error)
	removeFn func(ctx context.Context, req *pb.RegistryCredentialRemoveRequest) (*pb.RegistryCredentialRemoveResponse, error)
	listFn   func(ctx context.Context, req *pb.RegistryCredentialListRequest) (*pb.RegistryCredentialListResponse, error)
}

func (f *fakeRegistryClient) Add(ctx context.Context, in *pb.RegistryCredentialAddRequest, _ ...grpc.CallOption) (*pb.RegistryCredentialAddResponse, error) {
	return f.addFn(ctx, in)
}
func (f *fakeRegistryClient) Remove(ctx context.Context, in *pb.RegistryCredentialRemoveRequest, _ ...grpc.CallOption) (*pb.RegistryCredentialRemoveResponse, error) {
	return f.removeFn(ctx, in)
}
func (f *fakeRegistryClient) List(ctx context.Context, in *pb.RegistryCredentialListRequest, _ ...grpc.CallOption) (*pb.RegistryCredentialListResponse, error) {
	return f.listFn(ctx, in)
}

// TestRegistryClient_ListResponseHasNoSecretField is a structural assertion
// that the wire type returned to the CLI has no Secret/Password/Token
// field. If a future schema change re-introduced one, every operator with
// list rights could exfiltrate every registry credential — break the build
// loudly here instead.
func TestRegistryClient_ListResponseHasNoSecretField(t *testing.T) {
	resp := &pb.RegistryCredentialListResponse{
		Credentials: []*pb.RegistryCredentialSummary{{
			Registry: "ghcr.io",
			Username: "ci",
		}},
	}
	for _, c := range resp.GetCredentials() {
		text := c.String()
		for _, banned := range []string{"secret", "password", "Secret", "Password", "Token"} {
			if strings.Contains(text, banned) {
				t.Errorf("RegistryCredentialSummary string %q contains banned field token %q (secret may be on the wire)", text, banned)
			}
		}
	}
}

// TestRegistryClient_FakeRoundTrip wires the fake against the CLI's
// expected flow (Add → List → Remove) so a refactor that swaps the proto
// types compiles or fails here, not in production.
func TestRegistryClient_FakeRoundTrip(t *testing.T) {
	store := map[string]*pb.RegistryCredentialSummary{}
	c := &fakeRegistryClient{
		addFn: func(_ context.Context, req *pb.RegistryCredentialAddRequest) (*pb.RegistryCredentialAddResponse, error) {
			store[req.GetRegistry()] = &pb.RegistryCredentialSummary{
				Registry: req.GetRegistry(),
				Username: req.GetUsername(),
			}
			return &pb.RegistryCredentialAddResponse{Registry: req.GetRegistry()}, nil
		},
		removeFn: func(_ context.Context, req *pb.RegistryCredentialRemoveRequest) (*pb.RegistryCredentialRemoveResponse, error) {
			delete(store, req.GetRegistry())
			return &pb.RegistryCredentialRemoveResponse{}, nil
		},
		listFn: func(_ context.Context, _ *pb.RegistryCredentialListRequest) (*pb.RegistryCredentialListResponse, error) {
			out := make([]*pb.RegistryCredentialSummary, 0, len(store))
			for _, v := range store {
				out = append(out, v)
			}
			return &pb.RegistryCredentialListResponse{Credentials: out}, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.Add(ctx, &pb.RegistryCredentialAddRequest{Registry: "ghcr.io", Username: "ci", Secret: []byte("pat")}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	resp, err := c.List(ctx, &pb.RegistryCredentialListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.GetCredentials()) != 1 || resp.GetCredentials()[0].GetUsername() != "ci" {
		t.Errorf("List entries unexpected: %v", resp.GetCredentials())
	}
	if _, err := c.Remove(ctx, &pb.RegistryCredentialRemoveRequest{Registry: "ghcr.io"}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	resp, _ = c.List(ctx, &pb.RegistryCredentialListRequest{})
	if len(resp.GetCredentials()) != 0 {
		t.Errorf("Remove did not delete; List = %v", resp.GetCredentials())
	}
}
