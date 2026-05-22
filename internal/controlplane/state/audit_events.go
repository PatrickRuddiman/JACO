package state

import (
	"sync"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
	"google.golang.org/protobuf/proto"
)

// AuditEvents is an append-only log. Unlike the keyed stores above, audit
// entries have no natural primary key and are queried by time + type; the FSM
// appends one per applied Command. Task 09 wires up Audit.Query on top.
type AuditEvents struct {
	mu     sync.RWMutex
	log    []*pb.AuditEvent
	broker *watch.Broker[*pb.AuditEvent]
}

func newAuditEvents(b *watch.Broker[*pb.AuditEvent]) *AuditEvents {
	return &AuditEvents{broker: b}
}

// Append adds an event to the tail and publishes it.
func (a *AuditEvents) Append(ev *pb.AuditEvent) {
	stored := proto.Clone(ev).(*pb.AuditEvent)
	a.mu.Lock()
	a.log = append(a.log, stored)
	a.mu.Unlock()
	a.broker.Publish(watch.Event[*pb.AuditEvent]{
		Kind: watch.KindAdded, After: proto.Clone(stored).(*pb.AuditEvent), RaftIndex: ev.GetRaftIndex(),
	})
}

// List returns a snapshot of all entries.
func (a *AuditEvents) List() []*pb.AuditEvent {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]*pb.AuditEvent, len(a.log))
	for i, ev := range a.log {
		out[i] = proto.Clone(ev).(*pb.AuditEvent)
	}
	return out
}

// Len reports the number of recorded events.
func (a *AuditEvents) Len() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.log)
}
