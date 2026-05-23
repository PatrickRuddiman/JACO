// Package raftnode wires hashicorp/raft with a bolt log store, file snapshot
// store, and TCP transport. The single exported type is Node; lifecycle is
// New -> (use Apply / Leader / IsLeader) -> Shutdown.
//
// The package name is `raftnode` rather than `raft` so callers can import this
// alongside `github.com/hashicorp/raft` without an alias.
package raftnode

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	hraft "github.com/hashicorp/raft"
	boltdb "github.com/hashicorp/raft-boltdb/v2"
)

// Config holds everything New needs. All fields except LogOutput +
// AdvertiseAddr are required.
type Config struct {
	DataDir  string
	BindAddr string
	// AdvertiseAddr is the host:port peers should dial to reach this node's
	// raft transport. When empty, raft derives it from BindAddr; when
	// BindAddr is unspecified (0.0.0.0 / ::) that derivation fails with
	// "local bind address is not advertisable" — callers should resolve a
	// real interface IP (see internal/daemon/netdetect) and pass it here.
	AdvertiseAddr string
	LocalID       string
	Bootstrap     bool
	FSM           hraft.FSM
	LogOutput     io.Writer
}

// Node owns a running raft.Raft and the stores backing it.
type Node struct {
	Raft      *hraft.Raft
	boltStore *boltdb.BoltStore // concrete handle so Shutdown can release the file lock
	snapStore hraft.SnapshotStore
	transport hraft.Transport
}

// New constructs and starts a raft node. If cfg.Bootstrap is true the node
// bootstraps a single-voter cluster (itself); otherwise it starts as a
// follower expecting an existing cluster.
func New(cfg Config) (*Node, error) {
	if cfg.FSM == nil {
		return nil, fmt.Errorf("config: FSM is required")
	}
	if cfg.LocalID == "" {
		return nil, fmt.Errorf("config: LocalID is required")
	}
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("config: DataDir is required")
	}
	if cfg.BindAddr == "" {
		return nil, fmt.Errorf("config: BindAddr is required")
	}

	logOut := cfg.LogOutput
	if logOut == nil {
		logOut = os.Stderr
	}

	raftDir := filepath.Join(cfg.DataDir, "raft")
	if err := os.MkdirAll(raftDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir raft data dir: %w", err)
	}

	store, err := boltdb.NewBoltStore(filepath.Join(raftDir, "log.db"))
	if err != nil {
		return nil, fmt.Errorf("bolt store: %w", err)
	}

	snaps, err := hraft.NewFileSnapshotStore(raftDir, 3, logOut)
	if err != nil {
		return nil, fmt.Errorf("snapshot store: %w", err)
	}

	var advertise net.Addr
	if cfg.AdvertiseAddr != "" {
		resolved, err := net.ResolveTCPAddr("tcp", cfg.AdvertiseAddr)
		if err != nil {
			return nil, fmt.Errorf("resolve advertise %q: %w", cfg.AdvertiseAddr, err)
		}
		advertise = resolved
	}
	trans, err := hraft.NewTCPTransport(cfg.BindAddr, advertise, 3, 10*time.Second, logOut)
	if err != nil {
		return nil, fmt.Errorf("tcp transport: %w", err)
	}

	raftCfg := hraft.DefaultConfig()
	raftCfg.LocalID = hraft.ServerID(cfg.LocalID)
	raftCfg.HeartbeatTimeout = 250 * time.Millisecond
	raftCfg.ElectionTimeout = 1 * time.Second
	raftCfg.CommitTimeout = 50 * time.Millisecond
	raftCfg.LeaderLeaseTimeout = 250 * time.Millisecond
	raftCfg.SnapshotInterval = 120 * time.Second
	raftCfg.SnapshotThreshold = 8192
	raftCfg.LogOutput = logOut

	r, err := hraft.NewRaft(raftCfg, cfg.FSM, store, store, snaps, trans)
	if err != nil {
		return nil, fmt.Errorf("new raft: %w", err)
	}

	if cfg.Bootstrap {
		bc := hraft.Configuration{
			Servers: []hraft.Server{{
				Suffrage: hraft.Voter,
				ID:       hraft.ServerID(cfg.LocalID),
				Address:  trans.LocalAddr(),
			}},
		}
		if f := r.BootstrapCluster(bc); f.Error() != nil {
			return nil, fmt.Errorf("bootstrap cluster: %w", f.Error())
		}
	}

	return &Node{
		Raft:      r,
		boltStore: store,
		snapStore: snaps,
		transport: trans,
	}, nil
}

// Apply submits cmd to the raft log. Returns the assigned log index on commit.
// timeout==0 means use the default (5s, matching the spec's apply budget).
func (n *Node) Apply(cmd []byte, timeout time.Duration) (uint64, error) {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	f := n.Raft.Apply(cmd, timeout)
	if err := f.Error(); err != nil {
		return 0, err
	}
	return f.Index(), nil
}

// Leader returns the current leader's transport address, or empty if unknown.
func (n *Node) Leader() hraft.ServerAddress {
	return n.Raft.Leader()
}

// IsLeader reports whether the local node is currently the raft leader.
func (n *Node) IsLeader() bool {
	return n.Raft.State() == hraft.Leader
}

// LocalAddr returns the bound transport address; useful for join exchanges
// when BindAddr used port 0.
func (n *Node) LocalAddr() hraft.ServerAddress {
	return n.transport.LocalAddr()
}

// Shutdown stops the raft node and releases the bolt log-store file lock so
// the same data dir can be re-opened immediately after.
func (n *Node) Shutdown() error {
	var firstErr error
	if f := n.Raft.Shutdown(); f.Error() != nil {
		firstErr = f.Error()
	}
	if n.boltStore != nil {
		if err := n.boltStore.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if closer, ok := n.transport.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
