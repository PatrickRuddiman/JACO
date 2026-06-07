package dns_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	jdns "github.com/PatrickRuddiman/jaco/internal/discovery/dns"
)

// TestReadHostResolvers_ParsesNameserverLines pins the core extraction:
// only `nameserver` directives are picked up, with comments stripped and
// blank lines skipped. The returned addresses MUST be normalized to
// host:port with default port 53.
func TestReadHostResolvers_ParsesNameserverLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resolv.conf")
	body := `# operator-friendly comment
search example.com
nameserver 1.1.1.1
nameserver 9.9.9.9   # quad9
options ndots:0
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := jdns.ReadHostResolvers(path)
	if err != nil {
		t.Fatalf("ReadHostResolvers: %v", err)
	}
	want := []string{"1.1.1.1:53", "9.9.9.9:53"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestReadHostResolvers_FiltersDockerEmbeddedResolver pins the
// loop-avoidance for 127.0.0.11. If the host's resolv.conf somehow
// has it (a docker-managed container's resolv.conf accidentally copied
// to the host, etc.), the resolver MUST NOT carry it through — every
// query would loop right back to us.
func TestReadHostResolvers_FiltersDockerEmbeddedResolver(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resolv.conf")
	body := `nameserver 127.0.0.11
nameserver 1.1.1.1
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := jdns.ReadHostResolvers(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "1.1.1.1:53" {
		t.Errorf("got %v, want [1.1.1.1:53] (127.0.0.11 must be filtered)", got)
	}
}

// TestReadHostResolvers_FiltersBridgeGateways pins the second
// loop-avoidance rule: any 10.244.*.1 is a JACO bridge gateway →
// MUST NOT be a forwarder.
func TestReadHostResolvers_FiltersBridgeGateways(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resolv.conf")
	body := `nameserver 10.244.0.1
nameserver 10.244.3.1
nameserver 8.8.8.8
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := jdns.ReadHostResolvers(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "8.8.8.8:53" {
		t.Errorf("got %v, want [8.8.8.8:53] (bridge gateways must be filtered)", got)
	}
}

// TestReadHostResolvers_EmptyFileIsNotAnError pins the "host has no
// usable resolvers" path: returns ([], nil) so the caller can decide
// whether to warn or fail based on whether dns.forwarders is set in
// jacod.yaml.
func TestReadHostResolvers_EmptyFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resolv.conf")
	if err := os.WriteFile(path, []byte("# nothing useful here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := jdns.ReadHostResolvers(path)
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

// TestReadHostResolvers_MissingFileIsError pins the "config file not
// present" path: the caller (daemon startup) needs to distinguish
// "I tried to read a default and got nothing" from "the path is wrong."
func TestReadHostResolvers_MissingFileIsError(t *testing.T) {
	got, err := jdns.ReadHostResolvers("/nonexistent/resolv.conf")
	if err == nil {
		t.Errorf("expected error for missing file, got %v", got)
	}
}
