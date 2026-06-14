package grpcsrv_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	grpcsrv "github.com/PatrickRuddiman/jaco/internal/controlplane/grpc"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestRegistryCredentials_ListRedactsSecret seeds two credentials directly
// into state and asserts the List handler returns RegistryCredentialSummary
// values that carry registry+username+updated_at, with the secret stripped.
// This is the contract that keeps secrets off the wire — break it and any
// operator with List access can exfiltrate every cluster's registry
// password.
func TestRegistryCredentials_ListRedactsSecret(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	now := timestamppb.Now()
	secret := []byte("plaintext-secret-must-not-leak")
	st.RegistryCredentials.Apply(&pb.RegistryCredential{
		Registry:  "docker.io",
		Username:  "alice",
		Secret:    secret,
		UpdatedAt: now,
	}, 1)
	st.RegistryCredentials.Apply(&pb.RegistryCredential{
		Registry:  "ghcr.io",
		Username:  "ci",
		Secret:    []byte("also-secret"),
		UpdatedAt: now,
	}, 2)

	srv := grpcsrv.NewRegistryCredentialsServer(st, nil)
	resp, err := srv.List(context.Background(), &pb.RegistryCredentialListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.GetCredentials()) != 2 {
		t.Fatalf("List returned %d entries, want 2", len(resp.GetCredentials()))
	}

	gotRegistries := map[string]string{}
	for _, c := range resp.GetCredentials() {
		gotRegistries[c.GetRegistry()] = c.GetUsername()
	}
	if !reflect.DeepEqual(gotRegistries, map[string]string{"docker.io": "alice", "ghcr.io": "ci"}) {
		t.Errorf("List entries = %v, want docker.io/alice + ghcr.io/ci", gotRegistries)
	}

	// The wire type is RegistryCredentialSummary which has no Secret field;
	// type-system guarantees redaction. We additionally string-scan the
	// marshaled response to catch any future schema regression that would
	// add Secret back into the summary.
	for _, c := range resp.GetCredentials() {
		s := c.String()
		if strings.Contains(s, string(secret)) || strings.Contains(s, "also-secret") {
			t.Errorf("List response leaked secret in %q", s)
		}
	}
}

// TestRegistryCredentials_ListEnumeratesNamespaceScopedKeys asserts the List
// handler returns one row per distinct canonical key — including multiple
// namespace-scoped credentials under the same host alongside a bare-host
// fallback. This is the surface issue #101's per-namespace support fixes:
// before, every "ghcr.io/<org>" collapsed to a single "ghcr.io" row.
func TestRegistryCredentials_ListEnumeratesNamespaceScopedKeys(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	now := timestamppb.Now()
	for _, c := range []struct{ key, user string }{
		{"ghcr.io", "fallback"},
		{"ghcr.io/personal", "alice"},
		{"ghcr.io/company", "ci-bot"},
	} {
		st.RegistryCredentials.Apply(&pb.RegistryCredential{
			Registry:  c.key,
			Username:  c.user,
			Secret:    []byte("secret"),
			UpdatedAt: now,
		}, 1)
	}

	srv := grpcsrv.NewRegistryCredentialsServer(st, nil)
	resp, err := srv.List(context.Background(), &pb.RegistryCredentialListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := map[string]string{}
	for _, c := range resp.GetCredentials() {
		got[c.GetRegistry()] = c.GetUsername()
	}
	want := map[string]string{
		"ghcr.io":          "fallback",
		"ghcr.io/personal": "alice",
		"ghcr.io/company":  "ci-bot",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List entries = %v, want %v", got, want)
	}
}

// TestRegistryCredentials_AddRequiresLeader covers the leader gate: with a
// nil raft handle, Add returns Unavailable rather than silently swallowing
// the write. (Same posture as Tokens.Issue / Cluster.IssueJoinToken.)
func TestRegistryCredentials_AddRequiresLeader(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	srv := grpcsrv.NewRegistryCredentialsServer(st, nil)
	_, err := srv.Add(context.Background(), &pb.RegistryCredentialAddRequest{
		Registry: "ghcr.io",
		Username: "ci",
		Secret:   []byte("pat"),
	})
	if err == nil {
		t.Fatalf("Add with nil raft: want error, got nil")
	}
	if !strings.Contains(err.Error(), "raft_unavailable") {
		t.Errorf("Add error = %v, want raft_unavailable", err)
	}
}

// TestRegistryCredentials_AddValidatesInputs documents the three required
// fields. With raft=nil the handler reaches the validation step only when we
// pass the leader gate, which we can't from a unit test — but the leader
// gate is the FIRST check so empty inputs surface the no-leader error
// instead. We exercise the validation path through the FSM round-trip in
// fsm tests; here we just assert the gate ordering doesn't silently 200.
func TestRegistryCredentials_AddMissingRegistryReturnsError(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	srv := grpcsrv.NewRegistryCredentialsServer(st, nil)
	_, err := srv.Add(context.Background(), &pb.RegistryCredentialAddRequest{})
	if err == nil {
		t.Fatalf("Add empty request: want error, got nil")
	}
}
