// Package ca generates and parses the cluster CA and signs per-node server
// certificates. Both the CA and node keys are Ed25519; certificates are valid
// for ten years (the cluster lifetime is dominated by operator action, not by
// cert expiry).
package ca

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

const (
	caCommonName = "JACO Cluster CA"
	certLifetime = 10 * 365 * 24 * time.Hour
	clockSkew    = 1 * time.Minute
)

// GenerateClusterCA returns a self-signed Ed25519 CA cert + private key as PEM
// blocks. The cert is valid for ~10 years; serial is 128 random bits.
func GenerateClusterCA() (caCertPEM, caKeyPEM []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate ed25519 key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}

	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: caCommonName},
		NotBefore:             time.Now().Add(-clockSkew),
		NotAfter:              time.Now().Add(certLifetime),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature | x509.KeyUsageCRLSign,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("create CA cert: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal CA key: %w", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		nil
}

// ParseCA decodes a CA cert + key pair from PEM. Used by SignNodeCSR and by
// the gRPC server cred loader.
func ParseCA(caCertPEM, caKeyPEM []byte) (*x509.Certificate, ed25519.PrivateKey, error) {
	certBlock, _ := pem.Decode(caCertPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("decode CA cert PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA cert: %w", err)
	}

	keyBlock, _ := pem.Decode(caKeyPEM)
	if keyBlock == nil || keyBlock.Type != "PRIVATE KEY" {
		return nil, nil, fmt.Errorf("decode CA key PEM")
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}
	key, ok := keyAny.(ed25519.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("CA key is not Ed25519 (got %T)", keyAny)
	}
	return cert, key, nil
}

func randomSerial() (*big.Int, error) {
	max := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	return n, nil
}
