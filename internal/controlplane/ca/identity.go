package ca

import (
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net"
	"strings"
	"time"
)

// SignNodeCSRForIdentity signs only the names approved by the operator,
// not the additional identities a CSR may request.
func SignNodeCSRForIdentity(csrPEM, caCertPEM, caKeyPEM []byte, hostname string, sans []string) ([]byte, error) {
	csr, err := parseNodeCSR(csrPEM)
	if err != nil {
		return nil, err
	}
	identity, err := nodeIdentity(hostname, sans)
	if err != nil {
		return nil, err
	}
	if csr.Subject.CommonName != hostname {
		return nil, fmt.Errorf("CSR subject must match approved node %q", hostname)
	}
	csr.Subject = identity.Subject
	csr.DNSNames = identity.DNSNames
	csr.IPAddresses = identity.IPAddresses
	return signNodeCSR(csr, caCertPEM, caKeyPEM)
}

// ValidateNodeIdentity checks an exact enrollment scope and any advertised
// endpoints against that scope. Empty optional endpoints are ignored.
func ValidateNodeIdentity(hostname string, sans []string, addresses ...string) error {
	identity, err := nodeIdentity(hostname, sans)
	if err != nil {
		return err
	}
	for _, address := range addresses {
		if address == "" {
			continue
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil || host == "" || port == "" {
			return fmt.Errorf("invalid advertised node address %q", address)
		}
		if err := identity.VerifyHostname(host); err != nil {
			return fmt.Errorf("advertised host %q is not approved for node %q", host, hostname)
		}
	}
	return nil
}

func nodeIdentity(hostname string, sans []string) (*x509.Certificate, error) {
	if hostname == "" {
		return nil, fmt.Errorf("approved node name is required")
	}
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: hostname}}
	seen := make(map[string]bool)
	for _, name := range append([]string{hostname}, sans...) {
		if ip := net.ParseIP(name); ip != nil {
			if ip.IsUnspecified() {
				return nil, fmt.Errorf("unspecified IP is not a node identity")
			}
			if !seen[ip.String()] {
				cert.IPAddresses = append(cert.IPAddresses, ip)
				seen[ip.String()] = true
			}
			continue
		}
		name = strings.ToLower(strings.TrimSuffix(name, "."))
		if !validNodeDNSName(name) {
			return nil, fmt.Errorf("invalid node DNS name %q", name)
		}
		if !seen[name] {
			cert.DNSNames = append(cert.DNSNames, name)
			seen[name] = true
		}
	}
	return cert, nil
}

func validNodeDNSName(name string) bool {
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if ch != '-' && !(ch >= 'a' && ch <= 'z') && !(ch >= '0' && ch <= '9') {
				return false
			}
		}
	}
	return true
}

func parseNodeCSR(csrPEM []byte) (*x509.CertificateRequest, error) {
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
	return csr, nil
}

func signNodeCSR(csr *x509.CertificateRequest, caCertPEM, caKeyPEM []byte) ([]byte, error) {
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
