package fsm_test

import (
	"bytes"
	"io"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestApply_RegistryCredentialUpsert_StoresAndAudits asserts the FSM keys
// the credential by canonical host (Docker Hub aliases fold to "docker.io"),
// stamps updated_at from cmd.ts when the caller didn't, and emits an audit
// event that carries registry+username but NOT the secret.
func TestApply_RegistryCredentialUpsert_StoresAndAudits(t *testing.T) {
	f, s, _ := newFSM(t)
	ts := timestamppb.Now()
	secret := []byte("super-secret-token")

	applyCmd(t, f, 7, &pb.Command{
		Identity: "operator",
		Ts:       ts,
		Payload: &pb.Command_RegistryCredentialUpsert{
			RegistryCredentialUpsert: &pb.RegistryCredentialUpsert{
				Credential: &pb.RegistryCredential{
					Registry: "index.docker.io", // legacy alias — must canonicalize
					Username: "alice",
					Secret:   secret,
				},
			},
		},
	})

	cred, ok := s.RegistryCredentials.Get("docker.io")
	if !ok {
		t.Fatalf("docker.io credential not stored after upsert")
	}
	if cred.GetUsername() != "alice" {
		t.Errorf("username = %q, want alice", cred.GetUsername())
	}
	if !bytes.Equal(cred.GetSecret(), secret) {
		t.Errorf("stored secret differs from upserted")
	}
	if cred.GetUpdatedAt() == nil || !cred.GetUpdatedAt().AsTime().Equal(ts.AsTime()) {
		t.Errorf("updated_at = %v, want stamped from cmd.ts %v", cred.GetUpdatedAt(), ts)
	}

	audits := s.AuditEvents.List()
	if len(audits) != 1 {
		t.Fatalf("audit count = %d, want 1", len(audits))
	}
	ev := audits[0]
	if ev.GetType() != pb.AuditEventType_AUDIT_EVENT_TYPE_REGISTRY_CREDENTIAL_UPSERT {
		t.Errorf("audit type = %v, want REGISTRY_CREDENTIAL_UPSERT", ev.GetType())
	}
	if ev.GetPayload()["registry"] != "docker.io" {
		t.Errorf("audit registry = %q, want docker.io", ev.GetPayload()["registry"])
	}
	if ev.GetPayload()["username"] != "alice" {
		t.Errorf("audit username = %q, want alice", ev.GetPayload()["username"])
	}
	// Critical security invariant: the secret MUST NOT appear in any audit
	// field. Scan every key + value for a match.
	for k, v := range ev.GetPayload() {
		if bytes.Contains([]byte(k), secret) || bytes.Contains([]byte(v), secret) {
			t.Errorf("audit payload leaked secret in field %q=%q", k, v)
		}
	}
}

// TestApply_RegistryCredentialRemove_RemovesAndAudits asserts Remove is
// idempotent and emits an audit event.
func TestApply_RegistryCredentialRemove_RemovesAndAudits(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Ts: timestamppb.Now(),
		Payload: &pb.Command_RegistryCredentialUpsert{
			RegistryCredentialUpsert: &pb.RegistryCredentialUpsert{
				Credential: &pb.RegistryCredential{
					Registry: "ghcr.io",
					Username: "ci",
					Secret:   []byte("pat"),
				},
			},
		},
	})
	applyCmd(t, f, 2, &pb.Command{
		Ts: timestamppb.Now(),
		Payload: &pb.Command_RegistryCredentialRemove{
			RegistryCredentialRemove: &pb.RegistryCredentialRemove{Registry: "ghcr.io"},
		},
	})
	if _, ok := s.RegistryCredentials.Get("ghcr.io"); ok {
		t.Errorf("ghcr.io still present after Remove")
	}
	// Idempotent: a second Remove must not error or panic.
	applyCmd(t, f, 3, &pb.Command{
		Ts: timestamppb.Now(),
		Payload: &pb.Command_RegistryCredentialRemove{
			RegistryCredentialRemove: &pb.RegistryCredentialRemove{Registry: "ghcr.io"},
		},
	})

	audits := s.AuditEvents.List()
	if len(audits) != 3 {
		t.Fatalf("audit count = %d, want 3 (1 upsert + 2 removes)", len(audits))
	}
	if audits[1].GetType() != pb.AuditEventType_AUDIT_EVENT_TYPE_REGISTRY_CREDENTIAL_REMOVE {
		t.Errorf("audit[1] type = %v, want REGISTRY_CREDENTIAL_REMOVE", audits[1].GetType())
	}
}

// TestSnapshotRestore_PreservesRegistryCredentials applies an upsert,
// snapshots, restores into a fresh FSM, and verifies the credential survives
// with secret intact (raft snapshot is the on-disk distribution channel for
// fresh nodes; losing the secret would break private pulls on restart).
func TestSnapshotRestore_PreservesRegistryCredentials(t *testing.T) {
	src, srcState, _ := newFSM(t)
	secret := []byte("rotate-me")
	applyCmd(t, src, 5, &pb.Command{
		Ts: timestamppb.Now(),
		Payload: &pb.Command_RegistryCredentialUpsert{
			RegistryCredentialUpsert: &pb.RegistryCredentialUpsert{
				Credential: &pb.RegistryCredential{
					Registry: "registry.example.com:5000",
					Username: "bob",
					Secret:   secret,
				},
			},
		},
	})

	snap, err := src.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	sink := newRecordingSink()
	if err := snap.Persist(sink); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	dst, dstState, _ := newFSM(t)
	if err := dst.Restore(io.NopCloser(bytes.NewReader(sink.data.Bytes()))); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	got, ok := dstState.RegistryCredentials.Get("registry.example.com:5000")
	if !ok {
		t.Fatalf("registry.example.com:5000 missing after Restore")
	}
	if got.GetUsername() != "bob" {
		t.Errorf("username = %q, want bob", got.GetUsername())
	}
	if !bytes.Equal(got.GetSecret(), secret) {
		t.Errorf("restored secret differs from original")
	}

	// Sanity: the source state still holds the same credential (Snapshot
	// must not have moved or mutated it).
	if srcCred, ok := srcState.RegistryCredentials.Get("registry.example.com:5000"); !ok || !bytes.Equal(srcCred.GetSecret(), secret) {
		t.Errorf("source state mutated by Snapshot")
	}

	// proto.Equal sanity check on the round-tripped value (defensive).
	want := &pb.RegistryCredential{
		Registry:  "registry.example.com:5000",
		Username:  "bob",
		Secret:    secret,
		UpdatedAt: got.GetUpdatedAt(),
	}
	if !proto.Equal(got, want) {
		t.Errorf("restored cred != original; got %+v want %+v", got, want)
	}
}
