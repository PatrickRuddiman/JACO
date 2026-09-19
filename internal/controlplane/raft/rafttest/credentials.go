// Package rafttest provisions real, isolated cluster identities for Raft tests.
package rafttest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
)

func NewCA(t testing.TB) (cert, key []byte) {
	t.Helper()
	cert, key, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func Issue(t testing.TB, dir, id string, caCert, caKey []byte) {
	t.Helper()
	key, csr, err := ca.GenerateNodeKeypair(id)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.SignNodeCSR(csr, caCert, caKey)
	if err != nil {
		t.Fatal(err)
	}
	WriteCredentials(t, dir, id, cert, key, caCert)
}

func WriteCredentials(t testing.TB, dir, id string, cert, key, caCert []byte) {
	t.Helper()
	dir = filepath.Join(dir, "node")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{id + ".crt": cert, id + ".key": key, "ca.crt": caCert} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
