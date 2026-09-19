package backup_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/backup"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/testutil"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func archiveParts(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"meta.json", "snapshot.bin"} {
		data := entries[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(data)), Mode: 0o600}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func readArchiveParts(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	tarReader := tar.NewReader(reader)
	out := make(map[string][]byte)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[header.Name], err = io.ReadAll(tarReader)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestBackupLegacyReencryptionAndAuthenticatedRestore(t *testing.T) {
	cert, private, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte("synthetic-backup-compose-secret")
	snapshot, err := proto.Marshal(&pb.FSMSnapshot{
		Cluster:             &pb.ClusterMeta{ClusterId: "backup-test", CaCert: cert, CaKey: private},
		Deployments:         []*pb.Deployment{{Name: "app", ComposeYaml: marker}},
		RegistryCredentials: []*pb.RegistryCredential{{Registry: "registry.test", Secret: []byte("synthetic-backup-registry-secret")}},
		CertBlobs:           []*pb.CertBlob{{Key: "account/private.key", Value: []byte("synthetic-backup-ACME-key")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := json.Marshal(backup.Meta{SchemaVersion: 1, ClusterID: "backup-test", SnapshotIndex: 12, SnapshotTerm: 3})
	if err != nil {
		t.Fatal(err)
	}
	legacy := archiveParts(t, map[string][]byte{"meta.json": metadata, "snapshot.bin": snapshot})
	keys := testutil.StateKeys(t)
	var converted bytes.Buffer
	opts := backup.ReencryptOptions{
		Reader: bytes.NewReader(legacy), Writer: &converted, SourceKeys: keys, TargetKeys: keys,
	}
	if err := backup.Reencrypt(opts); err == nil || converted.Len() != 0 {
		t.Fatal("legacy archive accepted without explicit opt-in")
	}
	opts.Reader = bytes.NewReader(legacy)
	opts.AllowLegacyPlaintext = true
	if err := backup.Reencrypt(opts); err != nil {
		t.Fatal(err)
	}
	parts := readArchiveParts(t, converted.Bytes())
	for _, data := range parts {
		if bytes.Contains(data, marker) || bytes.Contains(data, private) || bytes.Contains(data, []byte("synthetic-backup-")) {
			t.Fatal("expanded archive contains plaintext secret material")
		}
	}
	meta, err := backup.ReadMeta(bytes.NewReader(converted.Bytes()), keys)
	if err != nil || meta.SchemaVersion != 2 {
		t.Fatalf("authenticated metadata unavailable: %v", err)
	}
	wrong, err := seal.New("test-only", map[string][]byte{"test-only": bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	badDir := filepath.Join(t.TempDir(), "wrong")
	if err := backup.Import(backup.ImportOptions{DataDir: badDir, Reader: bytes.NewReader(converted.Bytes()), LocalID: "restore-a", Keys: wrong}); err == nil {
		t.Fatal("wrong-key restore accepted")
	}
	if _, err := os.Stat(badDir); !os.IsNotExist(err) {
		t.Fatal("wrong-key restore wrote partial state")
	}
	var altered backup.Meta
	if err := json.Unmarshal(parts["meta.json"], &altered); err != nil {
		t.Fatal(err)
	}
	altered.SnapshotIndex++
	parts["meta.json"], err = json.Marshal(altered)
	if err != nil {
		t.Fatal(err)
	}
	tampered := archiveParts(t, parts)
	if err := backup.Import(backup.ImportOptions{DataDir: badDir, Reader: bytes.NewReader(tampered), LocalID: "restore-a", Keys: keys}); err == nil {
		t.Fatal("tampered backup replay metadata accepted")
	}
	if _, err := os.Stat(badDir); !os.IsNotExist(err) {
		t.Fatal("tampered restore wrote partial state")
	}
	dir := t.TempDir()
	if err := backup.Import(backup.ImportOptions{DataDir: dir, Reader: bytes.NewReader(converted.Bytes()), LocalID: "restore-a", Keys: keys}); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, marker) || bytes.Contains(data, private) || bytes.Contains(data, []byte("synthetic-backup-")) {
			t.Errorf("restored durable artifact contains plaintext: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	node, err := raftnode.New(raftnode.Config{
		DataDir: dir, LocalID: "restore-a", BindAddr: "127.0.0.1:0", Keys: keys,
		FSM: fsm.New(st, brokers), LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { node.Shutdown() })
	deployment, ok := st.Deployments.Get("app")
	if !ok || !bytes.Equal(deployment.GetComposeYaml(), marker) || !bytes.Equal(st.Cluster.Get().GetCaKey(), private) {
		t.Fatal("authorized restore did not recover secret-bearing state")
	}
	registry, registryOK := st.RegistryCredentials.Get("registry.test")
	account, accountOK := st.CertBlobs.Get("account/private.key")
	if !registryOK || !accountOK || string(registry.GetSecret()) != "synthetic-backup-registry-secret" ||
		string(account.GetValue()) != "synthetic-backup-ACME-key" {
		t.Fatal("backup restore lost authorized registry or ACME secrets")
	}
}
