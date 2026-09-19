package migration

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
)

type artifact struct {
	path  string
	cache bool
}

func inventory(root string) ([]artifact, error) {
	var files []artifact
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch relative {
			case "raft", "node", "wg", "ingress", filepath.Join("ingress", "cache"):
				return nil
			case filepath.Join("raft", "snapshots"):
				// WalkSnapshots performs a complete, read-only format inventory.
				return filepath.SkipDir
			default:
				return fmt.Errorf("migration: unrecognized directory %s; retain and inspect it", relative)
			}
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("migration: refusing non-regular artifact %s", relative)
		}
		switch {
		case relative == filepath.Join("raft", "log.db"):
			return nil
		case filepath.Dir(relative) == filepath.Join("ingress", "cache"):
			if _, err := seal.CachePurpose(entry.Name()); err != nil {
				return err
			}
			files = append(files, artifact{path: relative, cache: true})
		case relative == filepath.Join("wg", "private.key"), relative == "restore.txt":
			if err := validateAncillary(root, relative); err != nil {
				return err
			}
			files = append(files, artifact{path: relative})
		case filepath.Dir(relative) == "node":
			if err := validateIdentity(root, entry.Name()); err != nil {
				return err
			}
			files = append(files, artifact{path: relative})
		default:
			return fmt.Errorf("migration: unrecognized artifact %s; migrate backups separately and retain the original", relative)
		}
		return nil
	})
	return files, err
}

func validateIdentity(root, name string) error {
	path := filepath.Join(root, "node", name)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	defer clear(data)
	switch {
	case name == "join.json":
		var metadata struct {
			ClusterID string   `json:"cluster_id"`
			PeerAddrs []string `json:"peer_addrs"`
			Hostname  string   `json:"hostname"`
			Advertise string   `json:"advertise"`
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&metadata); err != nil {
			return errors.New("migration: unsupported join metadata")
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return errors.New("migration: trailing join metadata")
		}
	case strings.HasSuffix(name, ".crt"):
		for remaining := data; ; {
			block, rest, err := strictPEM(remaining)
			if err != nil || block.Type != "CERTIFICATE" {
				return errors.New("migration: certificate file contains unexpected private/opaque data")
			}
			if _, err := x509.ParseCertificate(block.Bytes); err != nil {
				return errors.New("migration: invalid node certificate")
			}
			if len(bytes.TrimSpace(rest)) == 0 {
				break
			}
			remaining = rest
		}
	case strings.HasSuffix(name, ".key") && name != "ca.key":
		if _, rest, err := strictPEM(data); err != nil || len(bytes.TrimSpace(rest)) != 0 {
			return errors.New("migration: unsupported node identity key file")
		}
		cert, err := os.ReadFile(filepath.Join(root, "node", strings.TrimSuffix(name, ".key")+".crt"))
		if err != nil {
			return err
		}
		pair, err := tls.X509KeyPair(cert, data)
		if err != nil {
			return errors.New("migration: node identity certificate/key mismatch")
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil || leaf.IsCA {
			return errors.New("migration: refusing to copy a standalone CA private key")
		}
	default:
		return fmt.Errorf("migration: unrecognized node artifact %s", name)
	}
	return nil
}

func strictPEM(data []byte) (*pem.Block, []byte, error) {
	normalized := bytes.TrimSpace(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")))
	block, rest := pem.Decode(normalized)
	if block == nil || len(block.Headers) != 0 ||
		!bytes.Equal(bytes.TrimSpace(normalized[:len(normalized)-len(rest)]), bytes.TrimSpace(pem.EncodeToMemory(block))) {
		return nil, nil, errors.New("migration: unexpected data in PEM identity")
	}
	return block, rest, nil
}

func validateAncillary(root, relative string) error {
	data, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil {
		return err
	}
	defer clear(data)
	if relative == filepath.Join("wg", "private.key") {
		if _, err := wgtypes.ParseKey(strings.TrimSpace(string(data))); err != nil {
			return errors.New("migration: invalid local WireGuard identity")
		}
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 4 {
		return errors.New("migration: unsupported restore marker")
	}
	names := []string{"cluster_id", "snapshot_index", "taken_at", "imported_at"}
	for i, line := range lines {
		name, value, ok := strings.Cut(line, "=")
		if !ok || name != names[i] {
			return errors.New("migration: unexpected restore marker field")
		}
		switch name {
		case "cluster_id":
			if value == "" || strings.ContainsAny(value, " \t\r") {
				return errors.New("migration: invalid restore cluster identifier")
			}
		case "snapshot_index":
			if index, err := strconv.ParseUint(value, 10, 64); err != nil || index == 0 {
				return errors.New("migration: invalid restore index")
			}
		case "taken_at", "imported_at":
			if value == "" && name == "taken_at" {
				continue
			}
			if _, err := time.Parse(time.RFC3339, value); err != nil {
				return errors.New("migration: invalid restore timestamp")
			}
		}
	}
	return nil
}
