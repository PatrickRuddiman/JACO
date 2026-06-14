package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestServiceUnitRuntimeDirNotShadowed guards against a regression of issue
// #167: the local-control unix socket must stay reachable on the host by
// jaco-group members.
//
// jacod binds the socket under /run/jaco. systemd's RuntimeDirectory=jaco
// creates that dir host-global and bind-mounts it into the unit's (sandboxed)
// mount namespace, so the socket is visible to the host. Listing the
// symlinked /var/run/jaco (or a bare /run) in ReadWritePaths makes systemd
// add a SECOND, namespace-private bind mount that shadows the RuntimeDirectory
// mount — the socket then lands in jacod's private mount view and is invisible
// to root + jaco-group members. This test parses the shipped unit and fails if
// those shadowing paths reappear. Pure file parse — no systemd needed.
func TestServiceUnitRuntimeDirNotShadowed(t *testing.T) {
	unitPath := filepath.Join("..", "..", "build", "jaco.service")
	data, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read %s: %v", unitPath, err)
	}

	var runtimeDir, readWritePaths string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "RuntimeDirectory="):
			runtimeDir = strings.TrimPrefix(line, "RuntimeDirectory=")
		case strings.HasPrefix(line, "ReadWritePaths="):
			readWritePaths = strings.TrimPrefix(line, "ReadWritePaths=")
		}
	}

	if !strings.Contains(runtimeDir, "jaco") {
		t.Errorf("RuntimeDirectory must include jaco so /run/jaco is host-visible; got %q", runtimeDir)
	}

	for _, bad := range []string{"/var/run/jaco", "/run"} {
		for _, p := range strings.Fields(readWritePaths) {
			if p == bad {
				t.Errorf("ReadWritePaths must not list %q — it shadows the host-visible "+
					"RuntimeDirectory=jaco mount and re-hides the control socket (issue #167); got %q",
					bad, readWritePaths)
			}
		}
	}
}

// unitValues parses a (trivial) systemd unit into a map of key -> list of
// values, ignoring comments and section headers. A key may legally appear more
// than once (e.g. RuntimeDirectory), so values accumulate.
func unitValues(t *testing.T, rel ...string) map[string][]string {
	t.Helper()
	unitPath := filepath.Join(append([]string{"..", ".."}, rel...)...)
	data, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read %s: %v", unitPath, err)
	}
	out := map[string][]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		out[k] = append(out[k], v)
	}
	return out
}

func hasValue(vals []string, want string) bool {
	for _, v := range vals {
		for _, f := range strings.Fields(v) {
			if f == want {
				return true
			}
		}
	}
	return false
}

// TestSocketActivationWiring guards the issue #167 fix: jacod inherits its
// local-control socket from systemd (jaco.socket) instead of binding it inside
// its own sandboxed mount namespace. PID 1 creates and binds the socket in the
// host namespace, so it is always reachable by jaco-group members. This test
// asserts jaco.socket exists with the right listen path / owner / group, and
// that jaco.service is wired to require + order after it. Pure file parse.
func TestSocketActivationWiring(t *testing.T) {
	sock := unitValues(t, "build", "jaco.socket")

	if !hasValue(sock["ListenStream"], "/run/jaco/jaco.sock") {
		t.Errorf("jaco.socket must ListenStream=/run/jaco/jaco.sock (host /run); got %v", sock["ListenStream"])
	}
	if !hasValue(sock["SocketGroup"], "jaco") {
		t.Errorf("jaco.socket must set SocketGroup=jaco so group members get rw; got %v", sock["SocketGroup"])
	}
	if !hasValue(sock["SocketMode"], "0660") {
		t.Errorf("jaco.socket must set SocketMode=0660 (owner+group rw); got %v", sock["SocketMode"])
	}
	if !hasValue(sock["RuntimeDirectory"], "jaco") {
		t.Errorf("jaco.socket must provision RuntimeDirectory=jaco before binding; got %v", sock["RuntimeDirectory"])
	}
	if !hasValue(sock["WantedBy"], "sockets.target") {
		t.Errorf("jaco.socket [Install] must be WantedBy=sockets.target; got %v", sock["WantedBy"])
	}

	svc := unitValues(t, "build", "jaco.service")
	if !hasValue(svc["Requires"], "jaco.socket") {
		t.Errorf("jaco.service must Requires=jaco.socket so the socket is created in the host "+
			"namespace before the daemon (issue #167); got %v", svc["Requires"])
	}
	if !hasValue(svc["After"], "jaco.socket") {
		t.Errorf("jaco.service must order After=jaco.socket; got %v", svc["After"])
	}
}
