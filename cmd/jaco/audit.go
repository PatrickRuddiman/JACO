package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/PatrickRuddiman/jaco/internal/cliclient"
	grpcsrv "github.com/PatrickRuddiman/jaco/internal/controlplane/grpc"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func init() {
	c := &cobra.Command{
		Use:   "audit",
		Short: "Query the cluster audit log",
		// Honors --output; renders -o json / -o yaml in its own RunE.
		Annotations: map[string]string{annotationHonorsOutput: "true"},
	}
	var (
		server, opToken, caCertPath, socket string
		since                               string
		typesFlag                           string
		follow                              bool
	)
	c.Flags().StringVar(&server, "server", "", "leader address (host:port); off-node only — omit to use the local socket")
	c.Flags().StringVar(&opToken, "token", "", "operator bearer token (or JACO_TOKEN); required with --server")
	c.Flags().StringVar(&caCertPath, "ca-cert", defaultCACertPath(), "path to cluster CA cert PEM")
	c.Flags().StringVar(&socket, "socket", socketDefault(), "local jacod unix socket (used when --server is omitted)")
	c.Flags().StringVar(&since, "since", "", "only events newer than this duration (e.g. 1h, 30m)")
	c.Flags().StringVar(&typesFlag, "type", "", "comma list of audit types to include (e.g. apply,token_revoke)")
	c.Flags().BoolVarP(&follow, "follow", "f", false, "stream new events as they arrive")

	c.RunE = func(_ *cobra.Command, _ []string) error {
		req := &pb.AuditQueryRequest{Follow: follow}
		if since != "" {
			d, err := time.ParseDuration(since)
			if err != nil {
				return fmt.Errorf("--since: %w", err)
			}
			req.Since = timestamppb.New(time.Now().Add(-d))
		}
		for _, raw := range strings.Split(typesFlag, ",") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			t, ok := grpcsrv.ParseAuditType(raw)
			if !ok {
				return fmt.Errorf("--type: unknown audit type %q", raw)
			}
			req.Types = append(req.Types, t)
		}

		conn, withAuth, err := dialOperator(operatorAuth{server: server, token: opToken, caCert: caCertPath, socket: socket})
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := contextForStream(follow)
		defer cancel()
		ctx = withAuth(ctx)

		stream, err := pb.NewAuditClient(conn).Query(ctx, req)
		if err != nil {
			return cliclient.FormatError(err)
		}

		enc := json.NewEncoder(os.Stdout)
		switch flagOutput {
		case "json":
			if follow {
				// NDJSON for follow mode (one object per line, flushed).
				return streamAuditJSON(stream, enc)
			}
			// Buffer all events into a JSON array for non-follow.
			return collectAuditJSON(stream, enc)
		case "yaml":
			if follow {
				// One YAML document per event (yaml.Encoder emits `---`
				// separators), flushed as they arrive.
				return streamAuditYAML(stream, os.Stdout)
			}
			return collectAuditYAML(stream, os.Stdout)
		default:
			return streamAuditTable(stream)
		}
	}
	rootCmd.AddCommand(c)
}

// contextForStream returns a context that doesn't time out when follow=true
// (the user cancels with Ctrl-C); otherwise a 30s deadline.
func contextForStream(follow bool) (context.Context, context.CancelFunc) {
	if follow {
		return context.WithCancel(context.Background())
	}
	return context.WithTimeout(context.Background(), 30*time.Second)
}

type auditEventJSON struct {
	Type      string            `json:"type" yaml:"type"`
	Identity  string            `json:"identity,omitempty" yaml:"identity,omitempty"`
	Ts        string            `json:"ts,omitempty" yaml:"ts,omitempty"`
	RaftIndex uint64            `json:"raft_index" yaml:"raft_index"`
	Payload   map[string]string `json:"payload,omitempty" yaml:"payload,omitempty"`
}

func eventToJSON(ev *pb.AuditEvent) auditEventJSON {
	out := auditEventJSON{
		Type:      grpcsrv.AuditTypeToString(ev.GetType()),
		Identity:  ev.GetIdentity(),
		RaftIndex: ev.GetRaftIndex(),
		Payload:   ev.GetPayload(),
	}
	if t := ev.GetTs(); t != nil {
		out.Ts = t.AsTime().UTC().Format(time.RFC3339)
	}
	return out
}

func streamAuditJSON(stream pb.Audit_QueryClient, enc *json.Encoder) error {
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return cliclient.FormatError(err)
		}
		if err := enc.Encode(eventToJSON(ev)); err != nil {
			return err
		}
	}
}

func collectAuditJSON(stream pb.Audit_QueryClient, enc *json.Encoder) error {
	var all []auditEventJSON
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return cliclient.FormatError(err)
		}
		all = append(all, eventToJSON(ev))
	}
	return enc.Encode(all)
}

func collectAuditYAML(stream pb.Audit_QueryClient, out io.Writer) error {
	var all []auditEventJSON
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return cliclient.FormatError(err)
		}
		all = append(all, eventToJSON(ev))
	}
	return cliclient.RenderYAML(out, all)
}

func streamAuditYAML(stream pb.Audit_QueryClient, out io.Writer) error {
	enc := yaml.NewEncoder(out)
	enc.SetIndent(2)
	defer enc.Close()
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return cliclient.FormatError(err)
		}
		if err := enc.Encode(eventToJSON(ev)); err != nil {
			return err
		}
		flushIfPossible(out)
	}
}

// flushIfPossible flushes w when it exposes a Flush method, so follow-mode
// streaming output reaches the consumer promptly.
func flushIfPossible(w io.Writer) {
	if f, ok := w.(interface{ Flush() error }); ok {
		_ = f.Flush()
	}
}

func streamAuditTable(stream pb.Audit_QueryClient) error {
	fmt.Printf("%-32s %-26s %-20s %s\n", "TYPE", "TS", "IDENTITY", "PAYLOAD")
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return cliclient.FormatError(err)
		}
		ts := ""
		if t := ev.GetTs(); t != nil {
			ts = t.AsTime().UTC().Format(time.RFC3339)
		}
		payload := ""
		for k, v := range ev.GetPayload() {
			if payload != "" {
				payload += " "
			}
			payload += fmt.Sprintf("%s=%s", k, v)
		}
		fmt.Printf("%-32s %-26s %-20s %s\n", grpcsrv.AuditTypeToString(ev.GetType()), ts, ev.GetIdentity(), payload)
	}
}
