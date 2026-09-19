package seal_test

import (
	"bytes"
	"io"
	"testing"

	hraft "github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

type memorySink struct {
	bytes.Buffer
	cancelled bool
}

func (*memorySink) ID() string   { return "test-snapshot" }
func (*memorySink) Close() error { return nil }
func (s *memorySink) Cancel() error {
	s.cancelled = true
	return nil
}

func TestProtectedFSMRoundTrip(t *testing.T) {
	keys, err := seal.New("current", map[string][]byte{"current": bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	protected, err := seal.WrapFSM(fsm.New(st, brokers), keys)
	if err != nil {
		t.Fatal(err)
	}
	cmd := &pb.Command{Payload: &pb.Command_Batch{Batch: &pb.Batch{Children: []*pb.Command{
		{Payload: &pb.Command_ClusterInit{ClusterInit: &pb.ClusterInit{
			ClusterId: "test-cluster", CaKey: []byte("synthetic-ca-private-key"),
		}}},
		{Payload: &pb.Command_DeploymentApply{DeploymentApply: &pb.DeploymentApply{
			Deployment: "app", ComposeYaml: []byte("synthetic-compose-secret"), JacoYaml: []byte("synthetic-jaco-secret"),
		}}},
		{Payload: &pb.Command_RegistryCredentialUpsert{RegistryCredentialUpsert: &pb.RegistryCredentialUpsert{
			Credential: &pb.RegistryCredential{Registry: "example.test", Secret: []byte("synthetic-registry-secret")},
		}}},
		{Payload: &pb.Command_CertBlobUpsert{CertBlobUpsert: &pb.CertBlobUpsert{
			Blob: &pb.CertBlob{Key: "acme/account/key", Value: []byte("synthetic-acme-secret")},
		}}},
	}}}}
	raw, err := proto.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := keys.Seal("raft-command", raw)
	if err != nil {
		t.Fatal(err)
	}
	if result := protected.Apply(&hraft.Log{Index: 42, Data: encrypted}); result != nil {
		t.Fatalf("apply: %v", result)
	}
	snapshot, err := protected.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Release()
	var sink memorySink
	if err := snapshot.Persist(&sink); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sink.Bytes(), []byte("synthetic-")) {
		t.Fatal("snapshot contains a secret marker")
	}
	restoredBrokers := watch.NewRegistry()
	restored := state.New(restoredBrokers)
	recovery, err := seal.WrapFSM(fsm.New(restored, restoredBrokers), keys)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatal(err)
	}
	dep, ok := restored.Deployments.Get("app")
	if !ok || string(dep.GetComposeYaml()) != "synthetic-compose-secret" || string(dep.GetJacoYaml()) != "synthetic-jaco-secret" {
		t.Fatal("restored deployment lost resolved manifests")
	}
	credential, ok := restored.RegistryCredentials.Get("example.test")
	if !ok || string(credential.GetSecret()) != "synthetic-registry-secret" {
		t.Fatal("restored registry credential changed")
	}
	blob, ok := restored.CertBlobs.Get("acme/account/key")
	if !ok || string(blob.GetValue()) != "synthetic-acme-secret" {
		t.Fatal("restored ACME blob changed")
	}
	if string(restored.Cluster.Get().GetCaKey()) != "synthetic-ca-private-key" {
		t.Fatal("restored CA authority changed")
	}
}

func TestRejectedRestoreDoesNotReplaceLiveState(t *testing.T) {
	keys, err := seal.New("one", map[string][]byte{"one": bytes.Repeat([]byte{1}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	st.Cluster.Set(&pb.ClusterMeta{ClusterId: "existing", CaKey: []byte("synthetic-existing-authority")}, 1)
	protected, err := seal.WrapFSM(fsm.New(st, brokers), keys)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range [][]byte{[]byte("not-a-protobuf-snapshot"), []byte{0xff}} {
		ciphertext, err := keys.Seal(seal.SnapshotPurpose, body)
		if err != nil {
			t.Fatal(err)
		}
		for _, corrupt := range []bool{false, true} {
			data := bytes.Clone(ciphertext)
			if corrupt {
				data[len(data)-1] ^= 1
			}
			if err := protected.Restore(io.NopCloser(bytes.NewReader(data))); err == nil {
				t.Fatal("invalid restore succeeded")
			}
			if st.Cluster.Get().GetClusterId() != "existing" || string(st.Cluster.Get().GetCaKey()) != "synthetic-existing-authority" {
				t.Fatal("failed restore partially replaced existing state")
			}
		}
	}
}
