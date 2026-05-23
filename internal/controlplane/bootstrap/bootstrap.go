// Package bootstrap initializes a new JACO cluster on this node: generates
// the cluster id, cluster CA, this node's signed server cert, and the initial
// operator token; raft-bootstraps as a single voter; applies the ClusterInit
// command; and returns the operator token (hex) for the CLI to print.
package bootstrap

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// Options carry every input Run needs. DataDir and Name are required.
type Options struct {
	DataDir  string
	Name     string
	BindAddr string // raft transport bind; "" defaults to 127.0.0.1:0
	// AdvertiseAddr is the host:port peers will dial to reach this node's
	// raft transport. Required when BindAddr is unspecified (0.0.0.0); when
	// empty, raft derives advertise from BindAddr — fine for tests that
	// bind to a real loopback IP, broken for production 0.0.0.0 binds.
	AdvertiseAddr string
	LogOut        io.Writer // raft log destination; nil silences
}

// Result is what the CLI prints (and tests assert against).
type Result struct {
	ClusterID     string
	OperatorToken string
}

// Run executes the bootstrap. On success, writes node cert + key + CA into
// ${DataDir}/node/, initializes the raft store under ${DataDir}/raft/, applies
// the ClusterInit command, and returns the operator token (cleartext, hex).
func Run(opts Options) (*Result, error) {
	if opts.Name == "" {
		return nil, fmt.Errorf("Name is required")
	}
	if opts.DataDir == "" {
		return nil, fmt.Errorf("DataDir is required")
	}
	if opts.BindAddr == "" {
		opts.BindAddr = "127.0.0.1:0"
	}
	if opts.LogOut == nil {
		opts.LogOut = io.Discard
	}

	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	if _, err := os.Stat(filepath.Join(opts.DataDir, "raft", "log.db")); err == nil {
		return nil, fmt.Errorf("raft state already exists at %s; refusing to overwrite", opts.DataDir)
	}

	clusterID := uuid.NewString()

	caCertPEM, caKeyPEM, err := ca.GenerateClusterCA()
	if err != nil {
		return nil, fmt.Errorf("generate cluster CA: %w", err)
	}

	// Parse the advertise IP (if any) so it ends up as an IP SAN on the node cert.
	// AdvertiseAddr is "host:port"; strip the port and try to parse as IP.
	var advertiseIP net.IP
	if opts.AdvertiseAddr != "" {
		if host, _, err := net.SplitHostPort(opts.AdvertiseAddr); err == nil {
			advertiseIP = net.ParseIP(host) // nil when host is a DNS name — that's fine
		}
	}
	nodeKeyPEM, csrPEM, err := ca.GenerateNodeKeypair(opts.Name, advertiseIP)
	if err != nil {
		return nil, fmt.Errorf("generate node keypair: %w", err)
	}
	nodeCertPEM, err := ca.SignNodeCSR(csrPEM, caCertPEM, caKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("sign node CSR: %w", err)
	}

	nodeDir := filepath.Join(opts.DataDir, "node")
	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		return nil, fmt.Errorf("create node dir: %w", err)
	}
	keyPath := filepath.Join(nodeDir, opts.Name+".key")
	crtPath := filepath.Join(nodeDir, opts.Name+".crt")
	caPath := filepath.Join(nodeDir, "ca.crt")
	if err := os.WriteFile(keyPath, nodeKeyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("write node key: %w", err)
	}
	if err := os.WriteFile(crtPath, nodeCertPEM, 0o644); err != nil {
		return nil, fmt.Errorf("write node cert: %w", err)
	}
	if err := os.WriteFile(caPath, caCertPEM, 0o644); err != nil {
		return nil, fmt.Errorf("write CA cert: %w", err)
	}

	operatorToken, tokenHash, err := newOperatorToken()
	if err != nil {
		return nil, err
	}

	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)

	rnode, err := raftnode.New(raftnode.Config{
		DataDir:       opts.DataDir,
		BindAddr:      opts.BindAddr,
		AdvertiseAddr: opts.AdvertiseAddr,
		LocalID:       opts.Name,
		Bootstrap:     true,
		FSM:           f,
		LogOutput:     opts.LogOut,
	})
	if err != nil {
		return nil, fmt.Errorf("start raft: %w", err)
	}
	defer func() { _ = rnode.Shutdown() }()

	if err := waitForLeader(rnode, 10*time.Second); err != nil {
		return nil, err
	}

	initCmd := &pb.Command{
		ClusterId: clusterID,
		Identity:  "bootstrap",
		Ts:        timestamppb.Now(),
		Payload: &pb.Command_ClusterInit{ClusterInit: &pb.ClusterInit{
			ClusterId:                 clusterID,
			CaCert:                    caCertPEM,
			CaKey:                     caKeyPEM,
			OperatorTokenHashedSecret: tokenHash[:],
			SelfHostname:              opts.Name,
			SelfAddress:               string(rnode.LocalAddr()),
		}},
	}
	cmdBytes, err := proto.Marshal(initCmd)
	if err != nil {
		return nil, fmt.Errorf("marshal ClusterInit: %w", err)
	}
	if _, err := rnode.Apply(cmdBytes, 5*time.Second); err != nil {
		return nil, fmt.Errorf("apply ClusterInit: %w", err)
	}

	return &Result{ClusterID: clusterID, OperatorToken: operatorToken}, nil
}

// newOperatorToken returns a 32-byte hex-encoded operator token (the cleartext
// printed once to stdout) and its SHA-256 hash (the form stored in raft).
func newOperatorToken() (token string, hash [32]byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", [32]byte{}, fmt.Errorf("read random: %w", err)
	}
	t := hex.EncodeToString(b)
	return t, sha256.Sum256([]byte(t)), nil
}

func waitForLeader(n *raftnode.Node, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if n.IsLeader() {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("bootstrap raft never became leader within %s", timeout)
}
