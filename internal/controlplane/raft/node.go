// Package raftnode wires hashicorp/raft with a bolt log store, file snapshot
// store, and TCP transport. The single exported type is Node; lifecycle is
// New -> (use Apply / Leader / IsLeader) -> Shutdown.
//
// The package name is `raftnode` rather than `raft` so callers can import this
// alongside `github.com/hashicorp/raft` without an alias.
package raftnode

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	hraft "github.com/hashicorp/raft"
	boltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
	"github.com/PatrickRuddiman/jaco/internal/logging"
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
	Keys          *seal.Keyring
	LogOutput     io.Writer
	// Logger logs leadership transitions (INFO) and Apply commit errors
	// (ERROR). nil → discard. The daemon passes a subsystem=raft logger.
	Logger *slog.Logger
}

// Node owns a running raft.Raft and the stores backing it.
type Node struct {
	Raft      *hraft.Raft
	boltStore *boltdb.BoltStore // concrete handle so Shutdown can release the file lock
	snapStore hraft.SnapshotStore
	transport hraft.Transport
	logger    *slog.Logger
	stopCh    chan struct{}
	keys      *seal.Keyring
}

// New constructs and starts a raft node. If cfg.Bootstrap is true the node
// bootstraps a single-voter cluster (itself); otherwise it starts as a
// follower expecting an existing cluster.
func New(cfg Config) (_ *Node, resultErr error) {
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
	if cfg.Keys == nil {
		return nil, fmt.Errorf("config: independently provisioned state encryption Keys are required")
	}
	if err := seal.ValidateRaftDataDir(cfg.DataDir, cfg.Keys); err != nil {
		return nil, err
	}
	protectedFSM, err := seal.WrapFSM(cfg.FSM, cfg.Keys)
	if err != nil {
		return nil, err
	}

	logOut := cfg.LogOutput
	if logOut == nil {
		logOut = os.Stderr
	}

	raftDir := filepath.Join(cfg.DataDir, "raft")
	if err := os.MkdirAll(raftDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir raft data dir: %w", err)
	}

	logPath := filepath.Join(raftDir, "log.db")
	_, statErr := os.Stat(logPath)
	fresh := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !fresh {
		return nil, fmt.Errorf("stat raft log: %w", statErr)
	}
	store, err := boltdb.NewBoltStore(logPath)
	if err != nil {
		return nil, fmt.Errorf("bolt store: %w", err)
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, store.Close())
		}
	}()
	if fresh {
		if err := cfg.Keys.MarkFreshStore(store); err != nil {
			return nil, fmt.Errorf("mark encrypted raft store: %w", err)
		}
	} else if err := cfg.Keys.CheckStore(store); err != nil {
		return nil, err
	}
	logs, err := seal.WrapLogs(store, cfg.Keys)
	if err != nil {
		return nil, fmt.Errorf("authenticate raft logs: %w", err)
	}

	rawSnapshots, err := hraft.NewFileSnapshotStore(raftDir, 3, logOut)
	if err != nil {
		return nil, fmt.Errorf("snapshot store: %w", err)
	}
	snaps, err := seal.WrapSnapshots(rawSnapshots, cfg.Keys)
	if err != nil {
		return nil, fmt.Errorf("authenticate raft snapshots: %w", err)
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
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, trans.Close())
		}
	}()

	raftCfg := hraft.DefaultConfig()
	raftCfg.LocalID = hraft.ServerID(cfg.LocalID)
	raftCfg.HeartbeatTimeout = 250 * time.Millisecond
	raftCfg.ElectionTimeout = 1 * time.Second
	raftCfg.CommitTimeout = 50 * time.Millisecond
	raftCfg.LeaderLeaseTimeout = 250 * time.Millisecond
	raftCfg.SnapshotInterval = 120 * time.Second
	raftCfg.SnapshotThreshold = 8192
	raftCfg.LogOutput = logOut

	r, err := hraft.NewRaft(raftCfg, protectedFSM, logs, store, snaps, trans)
	if err != nil {
		return nil, fmt.Errorf("new raft: %w", err)
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, r.Shutdown().Error())
		}
	}()

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

	logger := cfg.Logger
	if logger == nil {
		logger = logging.Discard()
	}
	n := &Node{
		Raft:      r,
		boltStore: store,
		snapStore: snaps,
		transport: trans,
		logger:    logger,
		stopCh:    make(chan struct{}),
		keys:      cfg.Keys,
	}
	go n.watchLeadership()
	return n, nil
}

// watchLeadership logs every leadership transition at INFO until Shutdown
// closes stopCh. raft's LeaderCh delivers true when this node acquires
// leadership and false when it loses it.
func (n *Node) watchLeadership() {
	ch := n.Raft.LeaderCh()
	for {
		select {
		case <-n.stopCh:
			return
		case isLeader, ok := <-ch:
			if !ok {
				return
			}
			if isLeader {
				n.logger.Info("raft leadership acquired", "leader", string(n.transport.LocalAddr()))
			} else {
				n.logger.Info("raft leadership lost", "leader", string(n.transport.LocalAddr()))
			}
		}
	}
}

// Apply submits cmd to the raft log. Returns the assigned log index on commit.
// timeout==0 means use the default (5s, matching the spec's apply budget).
func (n *Node) Apply(cmd []byte, timeout time.Duration) (uint64, error) {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	encrypted, err := n.keys.Seal(seal.CommandPurpose, cmd)
	if err != nil {
		return 0, err
	}
	f := n.Raft.Apply(encrypted, timeout)
	if err := f.Error(); err != nil {
		// ErrNotLeader is an expected control-flow signal (followers forward to
		// the leader), so it stays at DEBUG; everything else is a genuine
		// commit failure logged at ERROR.
		if err == hraft.ErrNotLeader {
			n.logger.Debug("raft apply rejected: not leader")
		} else {
			n.logger.Error("raft apply failed", "error", err)
		}
		return 0, err
	}
	if err, ok := f.Response().(error); ok {
		n.logger.Error("raft FSM apply failed", "error", err)
		return 0, err
	}
	return f.Index(), nil
}

func (n *Node) StateKeys() *seal.Keyring { return n.keys }

// Leader returns the current leader's transport address, or empty if unknown.
func (n *Node) Leader() hraft.ServerAddress {
	return n.Raft.Leader()
}

// IsLeader reports whether the local node is currently the raft leader.
func (n *Node) IsLeader() bool {
	return n.Raft.State() == hraft.Leader
}

// GetConfiguration returns the latest raft configuration. Thin pass-
// through; exists so packages that consume only the Raft interface
// (e.g. internal/controlplane/raft/membership) can depend on *Node
// directly without dragging hashicorp/raft into their seams.
func (n *Node) GetConfiguration() hraft.ConfigurationFuture {
	return n.Raft.GetConfiguration()
}

// AddVoter is a thin pass-through; see GetConfiguration for why.
func (n *Node) AddVoter(id hraft.ServerID, addr hraft.ServerAddress, prevIndex uint64, timeout time.Duration) hraft.IndexFuture {
	return n.Raft.AddVoter(id, addr, prevIndex, timeout)
}

// DemoteVoter is a thin pass-through; see GetConfiguration for why.
func (n *Node) DemoteVoter(id hraft.ServerID, prevIndex uint64, timeout time.Duration) hraft.IndexFuture {
	return n.Raft.DemoteVoter(id, prevIndex, timeout)
}

// LocalAddr returns the bound transport address; useful for join exchanges
// when BindAddr used port 0.
func (n *Node) LocalAddr() hraft.ServerAddress {
	return n.transport.LocalAddr()
}

// Shutdown stops the raft node and releases the bolt log-store file lock so
// the same data dir can be re-opened immediately after.
func (n *Node) Shutdown() error {
	if n.stopCh != nil {
		select {
		case <-n.stopCh:
			// already closed
		default:
			close(n.stopCh)
		}
	}
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
