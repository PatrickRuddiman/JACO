package migration_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	boltdb "github.com/hashicorp/raft-boltdb/v2"
	"google.golang.org/protobuf/proto"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/migration"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

var secretMarkers = [][]byte{
	[]byte("synthetic-compose-secret"), []byte("synthetic-jaco-secret"),
	[]byte("synthetic-CA-private-key"), []byte("synthetic-registry-password"),
	[]byte("synthetic-ACME-private-key"), []byte("synthetic-deleted-free-page-secret"),
}

func stateKeys(t *testing.T, id string, value byte) *seal.Keyring {
	t.Helper()
	keys, err := seal.New(id, map[string][]byte{id: bytes.Repeat([]byte{value}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func makeLegacy(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	raftDir := filepath.Join(dir, "raft")
	if err := os.Mkdir(raftDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := boltdb.NewBoltStore(filepath.Join(raftDir, "log.db"))
	if err != nil {
		t.Fatal(err)
	}
	configuration := hraft.Configuration{Servers: []hraft.Server{{
		ID: "node-a", Address: "127.0.0.1:7000", Suffrage: hraft.Voter,
	}}}
	cmd := &pb.Command{Payload: &pb.Command_Batch{Batch: &pb.Batch{Children: []*pb.Command{
		{Payload: &pb.Command_ClusterInit{ClusterInit: &pb.ClusterInit{ClusterId: "legacy-cluster", CaKey: secretMarkers[2]}}},
		{Payload: &pb.Command_DeploymentApply{DeploymentApply: &pb.DeploymentApply{
			Deployment: "app", ComposeYaml: secretMarkers[0], JacoYaml: secretMarkers[1],
		}}},
		{Payload: &pb.Command_RegistryCredentialUpsert{RegistryCredentialUpsert: &pb.RegistryCredentialUpsert{
			Credential: &pb.RegistryCredential{Registry: "registry.test", Secret: secretMarkers[3]},
		}}},
		{Payload: &pb.Command_CertBlobUpsert{CertBlobUpsert: &pb.CertBlobUpsert{
			Blob: &pb.CertBlob{Key: "account/private.key", Value: secretMarkers[4]},
		}}},
	}}}}
	data, err := proto.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	logs := []*hraft.Log{
		{Index: 1, Term: 1, Type: hraft.LogConfiguration, Data: hraft.EncodeConfiguration(configuration)},
		{Index: 2, Term: 1, Type: hraft.LogCommand, Data: data},
	}
	for i := uint64(3); i <= 6; i++ {
		logs = append(logs, &hraft.Log{Index: i, Term: 1, Type: hraft.LogNoop})
	}
	if err := store.StoreLogs(logs); err != nil {
		t.Fatal(err)
	}
	if err := store.SetUint64([]byte("CurrentTerm"), 1); err != nil {
		t.Fatal(err)
	}
	if err := store.SetUint64([]byte("LastVoteTerm"), 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Set([]byte("LastVoteCand"), []byte("node-a")); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreLog(&hraft.Log{Index: 7, Term: 1, Type: hraft.LogCommand, Data: bytes.Repeat(secretMarkers[5], 4096)}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteRange(7, 7); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	brokers := watch.NewRegistry()
	f := fsm.New(state.New(brokers), brokers)
	if result := f.Apply(logs[1]); result != nil {
		t.Fatal(result)
	}
	snapshots, err := hraft.NewFileSnapshotStore(raftDir, 10, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_, transport := hraft.NewInmemTransport("node-a")
	defer transport.Close()
	for index := uint64(2); index <= 6; index++ {
		sink, err := snapshots.Create(hraft.SnapshotVersionMax, index, 1, configuration, 1, transport)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := f.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if err := snapshot.Persist(sink); err != nil {
			t.Fatal(err)
		}
		snapshot.Release()
	}
	cacheDir := filepath.Join(dir, "ingress", "cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("account/private.key"))
	if err := os.WriteFile(filepath.Join(cacheDir, hex.EncodeToString(sum[:])), secretMarkers[4], 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fileDigests(t *testing.T, dir string, forbidSecrets bool) map[string][32]byte {
	t.Helper()
	out := make(map[string][32]byte)
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if forbidSecrets {
			for i, marker := range secretMarkers {
				if bytes.Contains(data, marker) {
					t.Errorf("plaintext marker %d in %s", i, path)
				}
			}
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out[relative] = sha256.Sum256(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLegacyCopyRotationAndRestart(t *testing.T) {
	source := makeLegacy(t)
	before := fileDigests(t, source, false)
	raw, err := os.ReadFile(filepath.Join(source, "raft", "log.db"))
	if err != nil || !bytes.Contains(raw, secretMarkers[5]) {
		t.Fatal("fixture must contain a physically retained deleted secret")
	}
	one, two := stateKeys(t, "one", 1), stateKeys(t, "two", 2)
	dest := filepath.Join(t.TempDir(), "encrypted")
	opts := migration.Options{SourceDir: source, TargetDir: dest, SourceKeys: one, TargetKeys: one}
	if _, err := migration.Copy(opts); err == nil {
		t.Fatal("legacy input accepted without explicit opt-in")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("rejected migration created destination")
	}
	opts.AllowLegacyPlaintext = true
	report, err := migration.Copy(opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.Logs != 6 || report.Snapshots != 5 || report.CacheFiles != 1 {
		t.Fatalf("incomplete history inventory: %+v", report)
	}
	after := fileDigests(t, source, false)
	for path, digest := range before {
		if after[path] != digest {
			t.Errorf("source artifact changed: %s", path)
		}
	}
	fileDigests(t, dest, true)
	rotated := filepath.Join(t.TempDir(), "rotated")
	if _, err := migration.Copy(migration.Options{
		SourceDir: dest, TargetDir: rotated, SourceKeys: two, TargetKeys: two,
	}); err == nil {
		t.Fatal("wrong source key accepted")
	}
	if _, err := os.Stat(rotated); !os.IsNotExist(err) {
		t.Fatal("failed authentication created destination")
	}
	if _, err := migration.Copy(migration.Options{
		SourceDir: dest, TargetDir: rotated, SourceKeys: one, TargetKeys: two,
	}); err != nil {
		t.Fatal(err)
	}
	fileDigests(t, rotated, true)
	if err := seal.ValidateRaftDataDir(rotated, two); err != nil {
		t.Fatal(err)
	}
	if err := seal.ValidateRaftDataDir(rotated, one); err == nil {
		t.Fatal("retired key can open rotated state")
	}
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	node, err := raftnode.New(raftnode.Config{
		DataDir: rotated, Keys: two, LocalID: "node-a", BindAddr: "127.0.0.1:0",
		FSM: fsm.New(st, brokers), LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { node.Shutdown() })
	deadline := time.Now().Add(5 * time.Second)
	for !node.IsLeader() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	deployment, ok := st.Deployments.Get("app")
	if !node.IsLeader() || !ok || !bytes.Equal(deployment.GetComposeYaml(), secretMarkers[0]) ||
		!bytes.Equal(deployment.GetJacoYaml(), secretMarkers[1]) || !bytes.Equal(st.Cluster.Get().GetCaKey(), secretMarkers[2]) {
		t.Fatal("authorized migrated state did not survive restart")
	}
	registry, registryOK := st.RegistryCredentials.Get("registry.test")
	account, accountOK := st.CertBlobs.Get("account/private.key")
	if !registryOK || !accountOK || !bytes.Equal(registry.GetSecret(), secretMarkers[3]) ||
		!bytes.Equal(account.GetValue(), secretMarkers[4]) {
		t.Fatal("migration/rotation lost authorized registry or ACME secrets")
	}
}

func TestCopyRefusesActiveSourceAndUnknownArtifacts(t *testing.T) {
	source := makeLegacy(t)
	keys := stateKeys(t, "one", 1)
	locked, err := boltdb.NewBoltStore(filepath.Join(source, "raft", "log.db"))
	if err != nil {
		t.Fatal(err)
	}
	opts := migration.Options{
		SourceDir: source, TargetDir: filepath.Join(t.TempDir(), "new"),
		SourceKeys: keys, TargetKeys: keys, AllowLegacyPlaintext: true,
	}
	if _, err := migration.Copy(opts); err == nil {
		t.Fatal("active source database was migrated")
	}
	if err := locked.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "old-backup.tar.gz"), secretMarkers[2], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := migration.Copy(opts); err == nil {
		t.Fatal("unknown historical artifact was silently dropped or copied as plaintext")
	}
	if _, err := os.Stat(opts.TargetDir); !os.IsNotExist(err) {
		t.Fatal("invalid source created destination")
	}
}
