//go:build unix

package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	caddycmd "github.com/caddyserver/caddy/v2/cmd"
)

func TestIngressLoaderExecUsesPrivateAdmin(t *testing.T) {
	for _, kind := range []string{"http", "layer4"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "jaco.json")
			adminDir := filepath.Join(dir, "admin")
			socketPath := filepath.Join(adminDir, "caddy.sock")
			adminAddr := "unix/" + socketPath + "|0600"
			if err := os.Mkdir(adminDir, 0o700); err != nil {
				t.Fatal(err)
			}
			argsPath := installIngressTestCaddy(t)
			trap := listenIngressTest(t)
			t.Setenv("CADDY_ADMIN", trap.Addr().String())
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
			t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
			bootstrap, err := json.Marshal(map[string]any{
				"admin":   map[string]string{"listen": adminAddr},
				"storage": map[string]string{"module": "file_system", "root": filepath.Join(dir, "storage")},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, bootstrap, 0o600); err != nil {
				t.Fatal(err)
			}
			stop := startIngressTestCaddy(t, configPath, socketPath)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "upstream")
			}))
			t.Cleanup(backend.Close)
			front := listenIngressTest(t)
			addr := front.Addr().String()
			cfg := ingressAdminTestConfig(t, kind, addr, backend.Listener.Addr().String(), filepath.Join(dir, "storage"))
			empty := ingressAdminTestConfig(t, kind, addr, "", filepath.Join(dir, "storage"))
			if err := front.Close(); err != nil {
				t.Fatal(err)
			}

			load := ingressLoaderExec(configPath, socketPath)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			for _, force := range []bool{false, false, true} {
				if err := load(ctx, cfg, force); err != nil {
					t.Fatalf("exec load force=%v: %v", force, err)
				}
				assertIngressLoaded(t, kind, addr)
				saved, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
				var parsed struct {
					Admin caddy.AdminConfig `json:"admin"`
				}
				if err := json.Unmarshal(saved, &parsed); err != nil {
					t.Fatal(err)
				}
				if parsed.Admin.Disabled || parsed.Admin.Listen != adminAddr {
					t.Fatalf("persisted admin = %+v, want %s enabled", parsed.Admin, adminAddr)
				}
				info, err := os.Stat(socketPath)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
					t.Fatalf("admin socket must be owned by the Caddy user and mode 0600: %v", info)
				}
				rawArgs, err := os.ReadFile(argsPath)
				if err != nil {
					t.Fatal(err)
				}
				var args []string
				if err := json.Unmarshal(rawArgs, &args); err != nil {
					t.Fatal(err)
				}
				address := slices.Index(args, "--address")
				if address < 0 || address+1 >= len(args) || args[address+1] != adminAddr || slices.Contains(args, "--force") != force {
					t.Fatalf("reload args = %v, want explicit Unix address and force=%v", args, force)
				}
			}
			if err := load(ctx, empty, false); err != nil {
				t.Fatalf("delete last route: %v", err)
			}
			if kind == "http" {
				assertIngressResponse(t, addr, http.StatusNotFound, "")
			} else {
				assertIngressPortBound(t, addr, false)
			}
			stop()
			if err := load(ctx, cfg, false); err == nil || !strings.Contains(err.Error(), socketPath) {
				t.Fatalf("missing Unix endpoint must fail without TCP fallback: %v", err)
			}
			// A separately managed Caddy must safely rebind its private
			// address on restart, including any stale socket file.
			stop = startIngressTestCaddy(t, configPath, socketPath)
			assertIngressLoaded(t, kind, addr)
			stop()
		})
	}
}

func TestIngressLoaderExecRejectsUnsafeAdminDirectory(t *testing.T) {
	for _, kind := range []string{"permissions", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			installIngressTestCaddy(t)
			dir := t.TempDir()
			adminDir := filepath.Join(dir, "admin")
			if kind == "symlink" {
				if err := os.Symlink(t.TempDir(), adminDir); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(adminDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(adminDir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			configPath := filepath.Join(dir, "jaco.json")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := ingressLoaderExec(configPath, filepath.Join(adminDir, "caddy.sock"))(ctx, []byte(`{}`), false)
			if err == nil || !strings.Contains(err.Error(), "caddy admin directory") {
				t.Fatalf("unsafe directory must fail before reload: %v", err)
			}
			if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsafe directory must not write config: %v", err)
			}
		})
	}
}

func installIngressTestCaddy(t *testing.T) string {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nexec \"$JACO_TEST_BINARY\" -test.run=^TestIngressCaddyCommand$ -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "caddy"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(dir, "args.json")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("JACO_TEST_BINARY", binary)
	t.Setenv("JACO_TEST_CADDY_ARGS", argsPath)
	return argsPath
}

func TestIngressCaddyCommand(t *testing.T) {
	argsPath := os.Getenv("JACO_TEST_CADDY_ARGS")
	if argsPath == "" {
		return
	}
	separator := slices.Index(os.Args, "--")
	if separator < 0 {
		t.Fatal("missing Caddy command separator")
	}
	args := os.Args[separator+1:]
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(argsPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{"caddy"}, args...)
	caddycmd.Main()
}

func startIngressTestCaddy(t *testing.T, configPath, socketPath string) func() {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "caddy.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	cmd := exec.Command("caddy", "run", "--config", configPath)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil {
				out, _ := os.ReadFile(logPath)
				t.Errorf("Caddy exited: %v\n%s", err, out)
			}
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("Caddy did not exit after SIGTERM")
		}
	}
	t.Cleanup(stop)
	client := &http.Client{
		Timeout: time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://127.0.0.1/config/")
		if err == nil {
			_, readErr := io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && readErr == nil {
				return stop
			}
		}
		select {
		case err := <-done:
			stopped = true
			out, _ := os.ReadFile(logPath)
			t.Fatalf("Caddy failed to start: %v\n%s", err, out)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Caddy did not listen on %s", socketPath)
	return stop
}
