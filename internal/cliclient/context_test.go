package cliclient_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/cliclient"
)

const sampleYAML = `current_context: prod
contexts:
  - name: prod
    server_addrs:
      - n1.example:7000
      - n2.example:7000
    ca_cert_path: /etc/jaco/ca.crt
    token: prod-token
  - name: staging
    server_addrs:
      - s1.example:7000
    ca_cert_path: /etc/jaco/staging-ca.crt
    token: staging-token
`

func writeClusters(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "clusters.yaml")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write: %v", err)
	}
	// os.WriteFile honors the mode under umask; chmod explicitly.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	return path
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"JACO_CONTEXT", "JACO_SERVER", "JACO_TOKEN", "JACO_CA_CERT"} {
		t.Setenv(k, "")
	}
}

func TestLoad_RejectsTooPermissiveMode(t *testing.T) {
	clearEnv(t)
	path := writeClusters(t, sampleYAML, 0o644)
	_, err := cliclient.Load(path)
	if err == nil {
		t.Fatalf("expected mode-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "0600") || !strings.Contains(err.Error(), "0644") {
		t.Errorf("error message must include both expected and actual modes; got %q", err.Error())
	}
}

func TestLoad_AcceptsMode0600(t *testing.T) {
	clearEnv(t)
	path := writeClusters(t, sampleYAML, 0o600)
	clusters, err := cliclient.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if clusters.CurrentContext != "prod" {
		t.Errorf("CurrentContext = %q, want prod", clusters.CurrentContext)
	}
	if got := len(clusters.Contexts); got != 2 {
		t.Errorf("len(Contexts) = %d, want 2", got)
	}
}

func TestResolve_UsesCurrentContext(t *testing.T) {
	clearEnv(t)
	path := writeClusters(t, sampleYAML, 0o600)
	ctx, err := cliclient.Resolve(cliclient.ResolveOptions{Path: path})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ctx.Name != "prod" {
		t.Errorf("ctx.Name = %q, want prod", ctx.Name)
	}
	if len(ctx.ServerAddrs) != 2 {
		t.Errorf("ServerAddrs len = %d, want 2", len(ctx.ServerAddrs))
	}
	if ctx.Token != "prod-token" {
		t.Errorf("Token = %q, want prod-token", ctx.Token)
	}
}

func TestResolve_JacoContextSelectsStaging(t *testing.T) {
	clearEnv(t)
	t.Setenv("JACO_CONTEXT", "staging")
	path := writeClusters(t, sampleYAML, 0o600)
	ctx, err := cliclient.Resolve(cliclient.ResolveOptions{Path: path})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ctx.Name != "staging" || ctx.Token != "staging-token" {
		t.Errorf("ctx = %+v; want staging context", ctx)
	}
}

func TestResolve_EnvOverridesReplaceFields(t *testing.T) {
	clearEnv(t)
	t.Setenv("JACO_SERVER", "env-host:7000")
	t.Setenv("JACO_TOKEN", "env-token")
	t.Setenv("JACO_CA_CERT", "/env/ca.crt")
	path := writeClusters(t, sampleYAML, 0o600)
	ctx, err := cliclient.Resolve(cliclient.ResolveOptions{Path: path})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := ctx.ServerAddrs; len(got) != 1 || got[0] != "env-host:7000" {
		t.Errorf("ServerAddrs = %v, want [env-host:7000]", got)
	}
	if ctx.Token != "env-token" {
		t.Errorf("Token = %q, want env-token", ctx.Token)
	}
	if ctx.CACertPath != "/env/ca.crt" {
		t.Errorf("CACertPath = %q, want /env/ca.crt", ctx.CACertPath)
	}
}

func TestResolve_JacoServerCommaList(t *testing.T) {
	clearEnv(t)
	t.Setenv("JACO_SERVER", "a:7000, b:7000 , c:7000")
	path := writeClusters(t, sampleYAML, 0o600)
	ctx, err := cliclient.Resolve(cliclient.ResolveOptions{Path: path})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"a:7000", "b:7000", "c:7000"}
	if len(ctx.ServerAddrs) != len(want) {
		t.Fatalf("ServerAddrs len = %d, want %d", len(ctx.ServerAddrs), len(want))
	}
	for i := range want {
		if ctx.ServerAddrs[i] != want[i] {
			t.Errorf("ServerAddrs[%d] = %q, want %q", i, ctx.ServerAddrs[i], want[i])
		}
	}
}

func TestResolve_EnvOnlyWhenFileMissing(t *testing.T) {
	clearEnv(t)
	t.Setenv("JACO_SERVER", "env-only:7000")
	t.Setenv("JACO_TOKEN", "env-token")
	t.Setenv("JACO_CA_CERT", "/env/ca.crt")
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	ctx, err := cliclient.Resolve(cliclient.ResolveOptions{Path: missing})
	if err != nil {
		t.Fatalf("Resolve env-only: %v", err)
	}
	if ctx.ServerAddrs[0] != "env-only:7000" {
		t.Errorf("ctx = %+v", ctx)
	}
}

func TestResolve_ErrorsWhenNoServerAddrs(t *testing.T) {
	clearEnv(t)
	t.Setenv("JACO_TOKEN", "x")
	t.Setenv("JACO_CA_CERT", "/x")
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	_, err := cliclient.Resolve(cliclient.ResolveOptions{Path: missing})
	if err == nil {
		t.Fatalf("expected error when ServerAddrs is empty")
	}
}

func TestResolve_UnknownContextRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv("JACO_CONTEXT", "does-not-exist")
	path := writeClusters(t, sampleYAML, 0o600)
	_, err := cliclient.Resolve(cliclient.ResolveOptions{Path: path})
	if err == nil {
		t.Fatalf("expected error for unknown context")
	}
}
