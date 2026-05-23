package grpc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// dynamicTLS holds an atomically-swappable *tls.Certificate. The daemon
// starts with a self-signed bootstrap cert here; rebindTLS swaps in the
// cluster-CA-signed node cert after OpenRaft persists the files. The
// listener's Config.GetCertificate reads via Load so swaps don't
// interrupt in-flight handshakes.
type dynamicTLS struct {
	cert atomic.Pointer[tls.Certificate]
}

func newDynamicTLS(initial tls.Certificate) *dynamicTLS {
	d := &dynamicTLS{}
	d.cert.Store(&initial)
	return d
}

func (d *dynamicTLS) swap(c tls.Certificate) { d.cert.Store(&c) }

func (d *dynamicTLS) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	return d.cert.Load(), nil
}

// bootstrapCert generates a self-signed ECDSA P-256 keypair valid for
// 24h. Used at daemon construction time before any cluster CA exists.
func bootstrapCert(hostname string) (tls.Certificate, error) {
	if hostname == "" {
		hostname = "jacod-bootstrap"
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("bootstrapCert: keypair: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()),
		Subject:      pkix.Name{CommonName: hostname + " (bootstrap)"},
		NotBefore:    now,
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{hostname},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("bootstrapCert: cert: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("bootstrapCert: marshal key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("bootstrapCert: load: %w", err)
	}
	return cert, nil
}

// bootstrapTLSConfig wraps bootstrapCert into a server *tls.Config with
// GetCertificate plugged into a dynamicTLS — rebindTLS can later swap
// the underlying cert with a cluster-CA-signed one without recreating
// the listener.
func bootstrapTLSConfig(hostname string) (*tls.Config, *dynamicTLS, error) {
	cert, err := bootstrapCert(hostname)
	if err != nil {
		return nil, nil, err
	}
	d := newDynamicTLS(cert)
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		NextProtos:     []string{"h2"},
		GetCertificate: d.GetCertificate,
	}, d, nil
}

// clusterNodeCert loads the cluster-CA-signed node cert from
// $dataDir/node/<hostname>.crt + .key. Used after OpenRaft / Init /
// Join writes those files; the result feeds dynamicTLS.swap so the
// listener's cert flips without recreating the socket.
func clusterNodeCert(dataDir, hostname string) (tls.Certificate, error) {
	nodeDir := filepath.Join(dataDir, "node")
	certPath := filepath.Join(nodeDir, hostname+".crt")
	keyPath := filepath.Join(nodeDir, hostname+".key")

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("clusterNodeCert: read %s: %w", certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("clusterNodeCert: read %s: %w", keyPath, err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("clusterNodeCert: load keypair: %w", err)
	}
	return cert, nil
}
