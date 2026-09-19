package grpc

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func TestInternalRPCDefaultDeniesOtherCommandVariants(t *testing.T) {
	s, conn := internalRPCLeader(t)
	allowed := map[string]bool{
		"node_update_self": true, "node_status_update": true, "replica_observed_update": true,
		"cert_lock": true, "cert_unlock": true, "cert_blob_upsert": true, "cert_blob_remove": true,
		"challenge_token_store": true, "audit_append": true,
	}
	fields := (&pb.Command{}).ProtoReflect().Descriptor().Oneofs().ByName("payload").Fields()
	before := s.Raft().Raft.LastIndex()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		if allowed[string(field.Name())] {
			continue
		}
		t.Run(string(field.Name()), func(t *testing.T) {
			cmd := &pb.Command{Identity: "bootstrap"}
			cmd.ProtoReflect().Mutable(field)
			if err := submitRPCCommand(t, conn, cmd); status.Code(err) != codes.PermissionDenied {
				t.Errorf("unlisted command %s: %v, want PermissionDenied", field.Name(), err)
			}
		})
	}
	if s.Raft().Raft.LastIndex() != before {
		t.Fatal("denied command was appended to Raft")
	}
}

func TestInternalRPCRejectsMalformedCommands(t *testing.T) {
	s, conn := internalRPCLeader(t)
	empty, err := proto.Marshal(&pb.Command{Identity: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	valid := &pb.Command{Payload: &pb.Command_NodeStatusUpdate{NodeStatusUpdate: &pb.NodeStatusUpdate{Hostname: "worker"}}}
	unknown, err := proto.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	unknown = protowire.AppendVarint(protowire.AppendTag(unknown, 60000, protowire.VarintType), 1)
	deep := valid
	for range 64 {
		deep = &pb.Command{Payload: &pb.Command_Batch{Batch: &pb.Batch{Children: []*pb.Command{deep}}}}
	}
	deepBytes, err := proto.Marshal(deep)
	if err != nil {
		t.Fatal(err)
	}
	before := s.Raft().Raft.LastIndex()
	for name, data := range map[string][]byte{
		"empty": nil, "bad protobuf": {0xff}, "missing payload": empty,
		"unknown field": unknown, "recursion limit": deepBytes,
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := pb.NewInternalClient(conn).Submit(ctx, &pb.SubmitRequest{CommandBytes: data})
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("malformed command: %v, want InvalidArgument", err)
			}
		})
	}
	if s.Raft().Raft.LastIndex() != before {
		t.Fatal("malformed command was appended to Raft")
	}
}
