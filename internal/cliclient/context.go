// Package cliclient holds the gRPC client builder, cluster context loader,
// and (in later tasks) the output renderers shared by every non-serve
// subcommand. See slices/cli.md.
package cliclient

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Context is one entry in clusters.yaml. ServerAddrs lists every cluster node
// the CLI should try in order; CACertPath pins TLS; Token is the operator
// bearer.
type Context struct {
	Name        string   `yaml:"name"`
	ServerAddrs []string `yaml:"server_addrs"`
	CACertPath  string   `yaml:"ca_cert_path"`
	Token       string   `yaml:"token"`
}

// Clusters is the on-disk file shape.
type Clusters struct {
	CurrentContext string    `yaml:"current_context"`
	Contexts       []Context `yaml:"contexts"`
}

// DefaultClustersPath returns ${XDG_CONFIG_HOME}/jaco/clusters.yaml (when set)
// or ${HOME}/.config/jaco/clusters.yaml.
func DefaultClustersPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "jaco", "clusters.yaml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "jaco", "clusters.yaml")
}

// Load reads clusters.yaml from path. The file must be regular and exactly
// mode 0600 — looser permissions leak the operator token to other local
// users. The mode-mismatch error message includes the actual octal mode so
// the operator can chmod accordingly.
func Load(path string) (*Clusters, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		return nil, fmt.Errorf("%s: expected mode 0600, got %#o", path, perm)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c Clusters
	if err := yaml.Unmarshal(body, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

// ResolveOptions tunes Resolve for testing — leave Path empty to use
// DefaultClustersPath, or set it to a tempdir-relative path.
type ResolveOptions struct {
	Path string
}

// Resolve loads clusters.yaml (when present and well-formed), picks the
// active context (JACO_CONTEXT env override falls back to CurrentContext),
// then layers env overrides — JACO_SERVER (single addr or comma list),
// JACO_TOKEN, JACO_CA_CERT. The returned Context is validated: ServerAddrs
// must be non-empty and Token must be set.
//
// When the file is absent, Resolve still succeeds if the env supplies enough
// to dial. When the file is present but mode-mismatched or unparseable,
// Resolve hard-errors.
func Resolve(opts ResolveOptions) (*Context, error) {
	path := opts.Path
	if path == "" {
		path = DefaultClustersPath()
	}

	var ctx Context
	if _, err := os.Stat(path); err == nil {
		clusters, err := Load(path)
		if err != nil {
			return nil, err
		}
		name := os.Getenv("JACO_CONTEXT")
		if name == "" {
			name = clusters.CurrentContext
		}
		if name == "" && len(clusters.Contexts) > 0 {
			name = clusters.Contexts[0].Name
		}
		found := false
		for i := range clusters.Contexts {
			if clusters.Contexts[i].Name == name {
				ctx = clusters.Contexts[i]
				found = true
				break
			}
		}
		if name != "" && !found {
			return nil, fmt.Errorf("context %q not found in %s", name, path)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	applyEnv(&ctx)

	if len(ctx.ServerAddrs) == 0 {
		return nil, fmt.Errorf("no server address (set JACO_SERVER or configure %s)", path)
	}
	if ctx.Token == "" {
		return nil, fmt.Errorf("no bearer token (set JACO_TOKEN or configure %s)", path)
	}
	if ctx.CACertPath == "" {
		return nil, fmt.Errorf("no CA cert path (set JACO_CA_CERT or configure %s)", path)
	}
	return &ctx, nil
}

// applyEnv overlays the JACO_* env vars onto ctx. Each variable fully
// replaces the matching field when set.
func applyEnv(ctx *Context) {
	if v := os.Getenv("JACO_SERVER"); v != "" {
		parts := strings.Split(v, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		ctx.ServerAddrs = out
	}
	if v := os.Getenv("JACO_TOKEN"); v != "" {
		ctx.Token = v
	}
	if v := os.Getenv("JACO_CA_CERT"); v != "" {
		ctx.CACertPath = v
	}
}
