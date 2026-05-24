package ca

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net"
	"time"
)

// GenerateNodeKeypair returns an Ed25519 private key + a CSR for the given
// hostname. The CSR always carries hostname in DNSNames. Any non-nil IPs in
// the variadic ips argument are added to IPAddresses (deduped). If hostname
// itself parses as an IP literal it is also added to IPAddresses so that both
// dial forms (DNS name and IP address) pass TLS verification.
func GenerateNodeKeypair(hostname string, ips ...net.IP) (keyPEM, csrPEM []byte, err error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate ed25519 key: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal node key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	csrTemplate := &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: hostname},
		DNSNames: []string{hostname},
	}

	// Collect IP SANs: explicit caller-supplied IPs first, then the hostname
	// itself if it parses as an IP literal.
	seen := make(map[string]bool)
	for _, ip := range ips {
		if ip == nil {
			continue
		}
		key := ip.String()
		if !seen[key] {
			seen[key] = true
			csrTemplate.IPAddresses = append(csrTemplate.IPAddresses, ip)
		}
	}
	if ip := net.ParseIP(hostname); ip != nil {
		key := ip.String()
		if !seen[key] {
			seen[key] = true
			csrTemplate.IPAddresses = append(csrTemplate.IPAddresses, ip)
		}
	}

	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTemplate, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("create CSR: %w", err)
	}
	csrPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	return keyPEM, csrPEM, nil
}

// SignNodeCSR validates the CSR and signs it with the cluster CA. The
// returned cert carries the CSR's Subject + SANs verbatim; key usage covers
// TLS server + client auth for the gRPC layer.
func SignNodeCSR(csrPEM, caCertPEM, caKeyPEM []byte) ([]byte, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("decode CSR PEM")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("CSR signature: %w", err)
	}

	caCert, caKey, err := ParseCA(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, err
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      csr.Subject,
		DNSNames:     csr.DNSNames,
		IPAddresses:  csr.IPAddresses,
		NotBefore:    time.Now().Add(-clockSkew),
		NotAfter:     time.Now().Add(certLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, caCert, csr.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("sign cert: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// ensure ed25519 import is exercised (gofmt would otherwise drop it on tidy if
// no symbol referenced it; the priv generated above is *ed25519.PrivateKey).
var _ ed25519.PrivateKey
