package state

import (
	"sync"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
	"google.golang.org/protobuf/proto"
)

// Cluster holds the singleton ClusterMeta entity (cluster_id + CA material).
// Written exactly once by ClusterInit on bootstrap and read by every node-cert
// signing / backup path thereafter.
type Cluster struct {
	mu     sync.RWMutex
	meta   *pb.ClusterMeta
	broker *watch.Broker[*pb.ClusterMeta]
}

func newCluster(b *watch.Broker[*pb.ClusterMeta]) *Cluster {
	return &Cluster{broker: b}
}

// Get returns a copy of the cluster meta, or nil if unset.
func (c *Cluster) Get() *pb.ClusterMeta {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.meta == nil {
		return nil
	}
	return proto.Clone(c.meta).(*pb.ClusterMeta)
}

// Set replaces the meta and publishes the event. Adds on first set, Updates
// thereafter.
func (c *Cluster) Set(m *pb.ClusterMeta, raftIndex uint64) {
	stored := proto.Clone(m).(*pb.ClusterMeta)
	c.mu.Lock()
	before := c.meta
	c.meta = stored
	c.mu.Unlock()
	if before == nil {
		c.broker.Publish(watch.Event[*pb.ClusterMeta]{
			Kind: watch.KindAdded, After: proto.Clone(stored).(*pb.ClusterMeta), RaftIndex: raftIndex,
		})
	} else {
		c.broker.Publish(watch.Event[*pb.ClusterMeta]{
			Kind:      watch.KindUpdated,
			Before:    proto.Clone(before).(*pb.ClusterMeta),
			After:     proto.Clone(stored).(*pb.ClusterMeta),
			RaftIndex: raftIndex,
		})
	}
}
