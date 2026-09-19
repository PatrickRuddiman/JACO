package grpcsrv

import (
	"bytes"
	"context"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/admission"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func TestStatusRedactsManifestsWithoutChangingRuntimeState(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.Deployments.Apply(&pb.Deployment{
		Name: "app", AppliedRevision: 7,
		JacoYaml:    []byte("synthetic-jaco-secret"),
		ComposeYaml: []byte("synthetic-compose-env-secret"),
		Services:    []*pb.ServiceSpec{{Name: "web", Replicas: 2}},
	}, 1)
	server := &deployServer{state: st}
	response, err := server.Status(context.Background(), &pb.DeployStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := proto.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("synthetic-")) {
		t.Fatal("routine status leaked manifest secrets")
	}
	if len(response.GetDeployments()) != 1 || response.GetDeployments()[0].GetAppliedRevision() != 7 ||
		len(response.GetDeployments()[0].GetServices()) != 1 {
		t.Fatal("redaction removed non-secret operational fields")
	}
	live, _ := st.Deployments.Get("app")
	if string(live.GetComposeYaml()) != "synthetic-compose-env-secret" || string(live.GetJacoYaml()) != "synthetic-jaco-secret" {
		t.Fatal("redaction modified runtime state")
	}
}

type secretViewStream struct {
	grpc.ServerStream
	ctx    context.Context
	ready  chan struct{}
	once   sync.Once
	events chan *pb.SubscribeEvent
}

func (s *secretViewStream) Context() context.Context {
	s.once.Do(func() { close(s.ready) })
	return s.ctx
}

func (s *secretViewStream) Send(event *pb.SubscribeEvent) error {
	s.events <- event
	return nil
}

func TestWatchRedactsBeforeAndAfterWithoutChangingInternalEvents(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	st.Deployments.Apply(&pb.Deployment{
		Name: "app", ComposeYaml: []byte("synthetic-old-compose"), JacoYaml: []byte("synthetic-old-jaco"),
	}, 1)
	internal := brokers.Deployments.Subscribe()
	defer internal.Cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream := &secretViewStream{ctx: ctx, ready: make(chan struct{}), events: make(chan *pb.SubscribeEvent, 1)}
	server := &watchServer{state: st, brokers: brokers}
	finished := make(chan error, 1)
	go func() {
		finished <- server.Subscribe(&pb.SubscribeRequest{EntityTypes: []string{"deployments"}}, stream)
	}()
	select {
	case <-stream.ready:
	case <-ctx.Done():
		t.Fatal("watch did not subscribe")
	}
	st.Deployments.Apply(&pb.Deployment{
		Name: "app", ComposeYaml: []byte("synthetic-new-compose"), JacoYaml: []byte("synthetic-new-jaco"),
	}, 2)
	select {
	case event := <-stream.events:
		encoded, err := proto.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(encoded, []byte("synthetic-")) {
			t.Error("routine watch leaked before/after secrets")
		}
		if event.GetDeployment().GetKind() != pb.EventKind_EVENT_KIND_UPDATED {
			t.Error("watch update metadata changed")
		}
	case <-ctx.Done():
		t.Fatal("watch did not deliver update")
	}
	select {
	case event := <-internal.Events():
		if string(event.Before.GetComposeYaml()) != "synthetic-old-compose" ||
			string(event.After.GetComposeYaml()) != "synthetic-new-compose" {
			t.Error("redaction changed internal runtime events")
		}
	case <-ctx.Done():
		t.Fatal("internal event not delivered")
	}
	cancel()
	<-finished
}

func TestStatusSecretExportRequiresExplicitOperatorRequest(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.Deployments.Apply(&pb.Deployment{Name: "app", ComposeYaml: []byte("synthetic-export-secret")}, 1)
	hash := sha256.Sum256([]byte("synthetic-operator-token"))
	st.Tokens.Apply(&pb.Token{Identity: "operator", HashedSecret: hash[:]}, 2)
	server := &deployServer{state: st}
	request := &pb.DeployStatusRequest{DeploymentFilter: "app", IncludeSecrets: true}
	if _, err := server.Status(context.Background(), request); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unauthenticated export: got %v, want PermissionDenied", err)
	}
	call := func(req *pb.DeployStatusRequest) (*pb.DeployStatusResponse, error) {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer synthetic-operator-token"))
		response, err := admission.UnaryInterceptor(st)(ctx, req,
			&grpc.UnaryServerInfo{FullMethod: pb.Deploy_Status_FullMethodName},
			func(ctx context.Context, _ any) (any, error) { return server.Status(ctx, req) })
		if err != nil {
			return nil, err
		}
		return response.(*pb.DeployStatusResponse), nil
	}
	response, err := call(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.GetDeployments()) != 1 || string(response.GetDeployments()[0].GetComposeYaml()) != "synthetic-export-secret" {
		t.Fatal("authorized explicit export did not preserve the exact manifest")
	}
	response, err = call(&pb.DeployStatusRequest{DeploymentFilter: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.GetDeployments()[0].GetComposeYaml()) != 0 {
		t.Fatal("operator routine status implicitly exported secrets")
	}
	if _, err := call(&pb.DeployStatusRequest{IncludeSecrets: true}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unscoped export: got %v, want InvalidArgument", err)
	}
}
