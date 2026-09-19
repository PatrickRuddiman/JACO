package migration

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
)

func TestInventoryRejectsOpaqueIdentityMaterial(t *testing.T) {
	cert, key, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	nodeKey, csr, err := ca.GenerateNodeKeypair("node-a")
	if err != nil {
		t.Fatal(err)
	}
	nodeCert, err := ca.SignNodeCSR(csr, cert, key)
	if err != nil {
		t.Fatal(err)
	}
	rotationCert, _, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	restore := []byte("cluster_id=test\nsnapshot_index=2\ntaken_at=2026-01-01T00:00:00Z\nimported_at=2026-01-01T01:00:00Z\n")
	for _, test := range []struct {
		name, path string
		value      []byte
		valid      bool
	}{
		{"certificate", "node/ca.crt", cert, true},
		{"ca-bundle", "node/ca.crt", append(bytes.Clone(cert), rotationCert...), true},
		{"leaf-chain", "node/node-a.crt", append(bytes.Clone(nodeCert), cert...), true},
		{"certificate-with-private-key", "node/ca.crt", append(bytes.Clone(cert), key...), false},
		{"certificate-with-opaque-tail", "node/ca.crt", append(bytes.Clone(cert), []byte("synthetic-hidden-secret\n")...), false},
		{"bundle-with-opaque-middle", "node/ca.crt", append(append(bytes.Clone(cert), []byte("synthetic-hidden-secret\n")...), rotationCert...), false},
		{"empty-certificate", "node/ca.crt", nil, false},
		{"prefixed-certificate", "node/ca.crt", append([]byte("synthetic-hidden-secret\n"), cert...), false},
		{"node-key", "node/node-a.key", nodeKey, true},
		{"prefixed-key", "node/node-a.key", append([]byte("synthetic-hidden-secret\n"), nodeKey...), false},
		{"key-with-extra-block", "node/node-a.key", append(bytes.Clone(nodeKey), cert...), false},
		{"standalone-ca", "node/ca.key", key, false},
		{"wireguard", "wg/private.key", []byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))), true},
		{"opaque-wireguard", "wg/private.key", []byte("synthetic-hidden-secret"), false},
		{"restore-marker", "restore.txt", restore, true},
		{"opaque-marker", "restore.txt", append(bytes.Clone(restore), []byte("secret=synthetic-hidden-secret\n")...), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "node"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "node", "node-a.crt"), nodeCert, 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, filepath.FromSlash(test.path))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, test.value, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := inventory(dir)
			if (err == nil) != test.valid {
				t.Fatalf("inventory valid=%v, error=%v", test.valid, err)
			}
		})
	}
}
