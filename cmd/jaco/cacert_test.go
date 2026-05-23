package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultCACertPath_NoEnv(t *testing.T) {
	t.Setenv("JACO_CA_CERT", "")
	got := defaultCACertPath()
	if got != "/var/lib/jaco/node/ca.crt" {
		t.Errorf("defaultCACertPath() = %q; want /var/lib/jaco/node/ca.crt", got)
	}
}

func TestDefaultCACertPath_CustomEnv(t *testing.T) {
	t.Setenv("JACO_CA_CERT", "/custom/path/ca.pem")
	got := defaultCACertPath()
	if got != "/custom/path/ca.pem" {
		t.Errorf("defaultCACertPath() = %q; want /custom/path/ca.pem", got)
	}
}

func TestDefaultCACertPath_EmptyEnvFallsBack(t *testing.T) {
	t.Setenv("JACO_CA_CERT", "")
	got := defaultCACertPath()
	if got != "/var/lib/jaco/node/ca.crt" {
		t.Errorf("defaultCACertPath() with JACO_CA_CERT=\"\" = %q; want /var/lib/jaco/node/ca.crt", got)
	}
}

func TestReadCACert_EmptyPath(t *testing.T) {
	b, err := readCACert("")
	if err != nil {
		t.Fatalf("readCACert(\"\") returned error: %v", err)
	}
	if b != nil {
		t.Errorf("readCACert(\"\") = %v; want nil", b)
	}
}

func TestReadCACert_FileNotFound(t *testing.T) {
	path := "/nonexistent/path/ca.crt"
	_, err := readCACert(path)
	if err == nil {
		t.Fatal("readCACert with nonexistent path should return an error")
	}
	want := "ca cert not found at " + path + " — pass --ca-cert or set JACO_CA_CERT"
	if err.Error() != want {
		t.Errorf("readCACert error = %q; want %q", err.Error(), want)
	}
}

func TestReadCACert_ValidFile(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	content := []byte("fake pem content")
	if err := os.WriteFile(certPath, content, 0600); err != nil {
		t.Fatalf("write temp cert: %v", err)
	}
	got, err := readCACert(certPath)
	if err != nil {
		t.Fatalf("readCACert valid file: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("readCACert returned %q; want %q", got, content)
	}
}

func TestReadCACert_OtherReadError(t *testing.T) {
	// Use a directory path, which will trigger a read error that is not IsNotExist.
	dir := t.TempDir()
	_, err := readCACert(dir)
	if err == nil {
		t.Fatal("readCACert on a directory should return an error")
	}
	// Should NOT be the "not found" message.
	if strings.Contains(err.Error(), "ca cert not found at") {
		t.Errorf("unexpected 'ca cert not found' message for non-NotExist error: %v", err)
	}
}
