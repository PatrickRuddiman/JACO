package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"

	"github.com/PatrickRuddiman/jaco/internal/ingress/config"
)

func TestIngressLoaderEmbeddedDisablesAdmin(t *testing.T) {
	t.Setenv("JACO_INGRESS_EXEC", "")
	for _, kind := range []string{"http", "layer4"} {
		t.Run(kind, func(t *testing.T) {
			// Occupy an isolated stand-in for localhost:2019. An implicit
			// admin listener must fail rather than contact a live daemon.
			admin := listenIngressTest(t)
			oldAdmin, oldAutosave := caddy.DefaultAdminListen, caddy.ConfigAutosavePath
			caddy.DefaultAdminListen = admin.Addr().String()
			caddy.ConfigAutosavePath = filepath.Join(t.TempDir(), "autosave.json")

			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "upstream")
			}))
			t.Cleanup(backend.Close)
			front := listenIngressTest(t)
			addr := front.Addr().String()
			storageDir := t.TempDir()
			cfg := ingressAdminTestConfig(t, kind, addr, backend.Listener.Addr().String(), storageDir)
			empty := ingressAdminTestConfig(t, kind, addr, "", storageDir)
			load := ingressLoader(slog.New(slog.NewTextHandler(io.Discard, nil)))
			t.Cleanup(func() {
				if err := caddy.Stop(); err != nil {
					t.Error(err)
				}
				caddy.DefaultAdminListen, caddy.ConfigAutosavePath = oldAdmin, oldAutosave
			})

			if err := load(context.Background(), empty, false); err != nil {
				t.Fatalf("cold route-less load: %v", err)
			}
			if err := front.Close(); err != nil {
				t.Fatal(err)
			}
			for _, force := range []bool{false, false, true} {
				if err := load(context.Background(), cfg, force); err != nil {
					t.Fatalf("load force=%v attempted an unwanted admin listener: %v", force, err)
				}
				assertIngressLoaded(t, kind, addr)
			}
			if err := load(context.Background(), empty, false); err != nil {
				t.Fatalf("delete last route: %v", err)
			}
			if kind == "http" {
				assertIngressResponse(t, addr, http.StatusNotFound, "")
			} else {
				assertIngressPortBound(t, addr, false)
			}
			if got := caddy.ListenerUsage("tcp", admin.Addr().String()); got != 0 {
				t.Errorf("default admin listener usage = %d, want 0", got)
			}
		})
	}
}

func TestIngressLoaderExecRequiresCaddy(t *testing.T) {
	t.Setenv("JACO_INGRESS_EXEC", "1")
	t.Setenv("PATH", t.TempDir())
	load := ingressLoader(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := load(context.Background(), []byte(`{}`), false); err == nil || !strings.Contains(err.Error(), "find caddy binary") {
		t.Fatalf("missing external Caddy must return an explicit error: %v", err)
	}
}

func listenIngressTest(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func assertIngressLoaded(t *testing.T, kind, addr string) {
	t.Helper()
	if kind == "http" {
		assertIngressResponse(t, addr, http.StatusOK, "upstream")
	} else {
		assertIngressPortBound(t, addr, true)
	}
}

func assertIngressPortBound(t *testing.T, addr string, bound bool) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if ln != nil {
		_ = ln.Close()
	}
	if bound && !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("expected ingress listener at %s, bind error: %v", addr, err)
	}
	if !bound && err != nil {
		t.Fatalf("deleted ingress listener still owns %s: %v", addr, err)
	}
}

// Keep the generated admin policy intact; isolate only the public listeners
// and disk storage. Both HTTP and L4 proxy to the same test HTTP backend.
func ingressAdminTestConfig(t *testing.T, kind, addr, backend, storageDir string) []byte {
	t.Helper()
	var routes []config.Route
	var tcpRoutes []config.TCPRoute
	var replicas []config.ReplicaObservedView
	var services map[string]config.ServiceMeta
	if backend != "" {
		host, port, err := net.SplitHostPort(backend)
		if err != nil {
			t.Fatal(err)
		}
		backendPort, err := strconv.Atoi(port)
		if err != nil {
			t.Fatal(err)
		}
		if kind == "http" {
			routes = []config.Route{{Domain: "ingress.test", Deployment: "test", Service: "web", Port: backendPort}}
		} else {
			_, port, err = net.SplitHostPort(addr)
			if err != nil {
				t.Fatal(err)
			}
			publishedPort, err := strconv.Atoi(port)
			if err != nil {
				t.Fatal(err)
			}
			tcpRoutes = []config.TCPRoute{{PublishedPort: publishedPort, Deployment: "test", Service: "web", ContainerPort: backendPort}}
		}
		replicas = []config.ReplicaObservedView{{ID: "test-web-0", Deployment: "test", Service: "web", State: "running", LastHealthAt: time.Now()}}
		services = map[string]config.ServiceMeta{
			config.MetaKey("test", "web"): {Deployment: "test", Service: "web", ReplicaIPs: map[string]string{"test-web-0": host}},
		}
	}
	cfg, err := config.BuildCaddyConfig(routes, tcpRoutes, replicas, services, config.BuildOpts{})
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(cfg, &root); err != nil {
		t.Fatal(err)
	}
	apps := root["apps"].(map[string]any)
	for name, server := range apps["http"].(map[string]any)["servers"].(map[string]any) {
		listen := "127.0.0.1:0"
		if name == "jaco_https" {
			listen = "127.0.0.2:0"
		}
		if kind == "http" && name == "jaco_http" {
			listen = addr
		}
		server.(map[string]any)["listen"] = []string{listen}
	}
	if layer4, ok := apps["layer4"].(map[string]any); ok {
		for _, server := range layer4["servers"].(map[string]any) {
			server.(map[string]any)["listen"] = []string{addr}
		}
	}
	root["storage"] = map[string]any{"module": "file_system", "root": storageDir}
	cfg, err = json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func assertIngressResponse(t *testing.T, addr string, status int, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "ingress.test"
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != status || string(got) != body {
		t.Fatalf("response = %d %q, want %d %q", resp.StatusCode, got, status, body)
	}
}
