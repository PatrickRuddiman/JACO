package grpcsrv

import (
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// watchServer implements jaco.v1.Watch. v1 supports the three entity types
// `jaco status -w` needs: deployments, replicas_observed, routes. Others
// return immediately with no events (the client should add them to the
// entity_types list when needed).
type watchServer struct {
	pb.UnimplementedWatchServer
	state   *state.State
	brokers *watch.Registry
}

// Subscribe opens broker subscriptions for the entity_types in the request,
// fans events into a single ordered output stream, and sends each as a
// SubscribeEvent. Returns when the client cancels the stream or any
// subscription closes.
func (w *watchServer) Subscribe(req *pb.SubscribeRequest, stream pb.Watch_SubscribeServer) error {
	depFilter := req.GetDeploymentFilter()

	// Per-type subscriptions — only spin up the ones the client asked for.
	requested := map[string]bool{}
	for _, t := range req.GetEntityTypes() {
		requested[t] = true
	}

	merged := make(chan *pb.SubscribeEvent, 256)
	done := make(chan struct{})
	defer close(done)

	if requested["deployments"] {
		sub := w.brokers.Deployments.Subscribe()
		defer sub.Cancel()
		go forwardDeployments(sub, merged, done, depFilter)
	}
	if requested["replicas_observed"] {
		sub := w.brokers.ReplicasObserved.Subscribe()
		defer sub.Cancel()
		go forwardReplicasObserved(sub, w.state, merged, done, depFilter)
	}
	if requested["routes"] {
		sub := w.brokers.Routes.Subscribe()
		defer sub.Cancel()
		go forwardRoutes(sub, merged, done, depFilter)
	}

	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case ev := <-merged:
			if ev == nil {
				continue
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
}

func forwardDeployments(sub *watch.Subscription[*pb.Deployment], out chan<- *pb.SubscribeEvent, done <-chan struct{}, depFilter string) {
	for ev := range sub.Events() {
		if depFilter != "" {
			name := ""
			if ev.After != nil {
				name = ev.After.GetName()
			} else if ev.Before != nil {
				name = ev.Before.GetName()
			}
			if name != depFilter {
				continue
			}
		}
		out <- &pb.SubscribeEvent{Payload: &pb.SubscribeEvent_Deployment{Deployment: &pb.DeploymentEvent{
			Kind:      kindToProto(ev.Kind),
			Before:    ev.Before,
			After:     ev.After,
			RaftIndex: ev.RaftIndex,
		}}}
		select {
		case <-done:
			return
		default:
		}
	}
}

func forwardReplicasObserved(sub *watch.Subscription[*pb.ReplicaObserved], st *state.State, out chan<- *pb.SubscribeEvent, done <-chan struct{}, depFilter string) {
	for ev := range sub.Events() {
		if depFilter != "" {
			id := ""
			if ev.After != nil {
				id = ev.After.GetId()
			} else if ev.Before != nil {
				id = ev.Before.GetId()
			}
			rd, ok := st.ReplicasDesired.Get(id)
			if !ok || rd.GetDeployment() != depFilter {
				continue
			}
		}
		out <- &pb.SubscribeEvent{Payload: &pb.SubscribeEvent_ReplicaObserved{ReplicaObserved: &pb.ReplicaObservedEvent{
			Kind:      kindToProto(ev.Kind),
			Before:    ev.Before,
			After:     ev.After,
			RaftIndex: ev.RaftIndex,
		}}}
		select {
		case <-done:
			return
		default:
		}
	}
}

func forwardRoutes(sub *watch.Subscription[*pb.Route], out chan<- *pb.SubscribeEvent, done <-chan struct{}, depFilter string) {
	for ev := range sub.Events() {
		if depFilter != "" {
			dep := ""
			if ev.After != nil {
				dep = ev.After.GetDeployment()
			} else if ev.Before != nil {
				dep = ev.Before.GetDeployment()
			}
			if dep != depFilter {
				continue
			}
		}
		out <- &pb.SubscribeEvent{Payload: &pb.SubscribeEvent_Route{Route: &pb.RouteEvent{
			Kind:      kindToProto(ev.Kind),
			Before:    ev.Before,
			After:     ev.After,
			RaftIndex: ev.RaftIndex,
		}}}
		select {
		case <-done:
			return
		default:
		}
	}
}

func kindToProto(k watch.Kind) pb.EventKind {
	switch k {
	case watch.KindAdded:
		return pb.EventKind_EVENT_KIND_ADDED
	case watch.KindUpdated:
		return pb.EventKind_EVENT_KIND_UPDATED
	case watch.KindRemoved:
		return pb.EventKind_EVENT_KIND_REMOVED
	case watch.KindResync:
		return pb.EventKind_EVENT_KIND_RESYNC
	}
	return pb.EventKind_EVENT_KIND_UNSPECIFIED
}
