package bootstrap_test

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/bootstrap"
	"github.com/PatrickRuddiman/jaco/internal/daemon/netdetect"
)

func TestRunCertificateCoversAdvertisedIdentities(t *testing.T) {
	for _, tc := range []struct {
		name, raft, grpc string
		want             []string
	}{
		{"dns", "localhost:0", "node-a.private:7000", []string{"localhost", "node-a.private"}},
		{"ip", "127.0.0.1:0", "127.0.0.2:7000", []string{"127.0.0.1", "127.0.0.2"}},
		{"bind-fallback", "", "", []string{"127.0.0.1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := bootstrap.Run(bootstrap.Options{
				DataDir: dir, Name: "node-a", BindAddr: "127.0.0.1:0",
				AdvertiseAddr: tc.raft, ListenAdvertiseAddr: tc.grpc,
			})
			if err != nil {
				t.Fatal(err)
			}
			certPEM, err := os.ReadFile(filepath.Join(dir, "node", "node-a.crt"))
			if err != nil {
				t.Fatal(err)
			}
			block, _ := pem.Decode(certPEM)
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			want := append([]string{"node-a"}, tc.want...)
			for _, ip := range netdetect.LocalIPs() {
				want = append(want, ip.String())
			}
			for _, identity := range want {
				if err := cert.VerifyHostname(identity); err != nil {
					t.Errorf("bootstrap certificate does not cover %q: %v", identity, err)
				}
			}
		})
	}
}
