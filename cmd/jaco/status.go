package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/PatrickRuddiman/jaco/internal/cliclient"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func init() {
	rootCmd.AddCommand(statusCmd())
}

func statusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status [deployment[/service]]",
		Short: "Print a snapshot of cluster deployments, replicas, and routes",
		Args:  cobra.MaximumNArgs(1),
		// Honors --output; renders -o json / -o yaml in renderStatusOut.
		Annotations: map[string]string{annotationHonorsOutput: "true"},
	}
	var server, opToken, caCertPath, socket string
	var watch bool
	c.Flags().StringVar(&server, "server", "", "leader address (host:port); off-node only — omit to use the local socket")
	c.Flags().StringVar(&opToken, "token", "", "operator bearer token (or JACO_TOKEN); required with --server")
	c.Flags().StringVar(&caCertPath, "ca-cert", defaultCACertPath(), "path to cluster CA cert PEM")
	c.Flags().StringVar(&socket, "socket", socketDefault(), "local jacod unix socket (used when --server is omitted)")
	c.Flags().BoolVarP(&watch, "watch", "w", false, "re-render on every state change (Ctrl-C to exit)")

	c.RunE = func(_ *cobra.Command, args []string) error {
		dep, svc := "", ""
		if len(args) == 1 {
			parts := strings.SplitN(args[0], "/", 2)
			dep = parts[0]
			if len(parts) == 2 {
				svc = parts[1]
			}
		}
		conn, withAuth, err := dialOperator(operatorAuth{server: server, token: opToken, caCert: caCertPath, socket: socket})
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx := withAuth(context.Background())
		deployClient := pb.NewDeployClient(conn)
		if !watch {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			return runStatus(ctx, deployClient, dep, svc, os.Stdout)
		}
		watchClient := pb.NewWatchClient(conn)
		return runStatusWatch(ctx, deployClient, watchClient, dep, svc, os.Stdout)
	}
	return c
}

// runStatus prints a single snapshot.
func runStatus(ctx context.Context, client pb.DeployClient, deployment, service string, out io.Writer) error {
	resp, err := client.Status(ctx, &pb.DeployStatusRequest{
		DeploymentFilter: deployment,
		ServiceFilter:    service,
	})
	if err != nil {
		return cliclient.FormatError(err)
	}
	return renderStatusOut(out, resp)
}

// renderStatusOut dispatches the snapshot on --output: json/yaml serialize the
// structured view (lowercase snake_case enums), table renders the human view.
func renderStatusOut(out io.Writer, resp *pb.DeployStatusResponse) error {
	return renderOutput(out, statusToView(resp), func() error {
		return renderStatus(out, resp)
	})
}

// runStatusWatch prints the initial snapshot, then re-renders on each
// SubscribeEvent received. Each snapshot is separated by a `---` marker so
// tests can count them.
func runStatusWatch(ctx context.Context, deploy pb.DeployClient, watch pb.WatchClient, deployment, service string, out io.Writer) error {
	if err := runStatus(ctx, deploy, deployment, service, out); err != nil {
		return err
	}
	stream, err := watch.Subscribe(ctx, &pb.SubscribeRequest{
		EntityTypes:      []string{"deployments", "replicas_observed", "routes"},
		DeploymentFilter: deployment,
	})
	if err != nil {
		return cliclient.FormatError(err)
	}
	for {
		_, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return cliclient.FormatError(err)
		}
		// Re-fetch the snapshot on any event. (Resync sends the same trigger
		// — re-fetch is idempotent.) The table and yaml views use a `---`
		// separator (a valid YAML document break); json omits it so the
		// stream is a sequence of concatenated JSON values that `jq` parses.
		if flagOutput != "json" {
			fmt.Fprintln(out, "---")
		}
		if rerr := runStatus(ctx, deploy, deployment, service, out); rerr != nil {
			return rerr
		}
	}
}

// renderStatus prints three tables: deployments, replicas, routes.
func renderStatus(out io.Writer, resp *pb.DeployStatusResponse) error {
	// Deployments table.
	depHeaders := []string{"DEPLOYMENT", "REVISION", "PREVIOUS", "STATUS"}
	var depRows [][]string
	for _, d := range resp.GetDeployments() {
		depRows = append(depRows, []string{
			d.GetName(),
			strconv.FormatUint(d.GetAppliedRevision(), 10),
			strconv.FormatUint(d.GetPreviousRevision(), 10),
			strings.TrimPrefix(d.GetStatus().String(), "DEPLOYMENT_STATUS_"),
		})
	}
	fmt.Fprintln(out, "Deployments:")
	if err := cliclient.RenderTable(out, depHeaders, depRows); err != nil {
		return err
	}

	// Replicas table.
	repHeaders := []string{"REPLICA_ID", "STATE", "HOST", "CONTAINER_ID", "LAST_HEALTH_AT"}
	var repRows [][]string
	for _, r := range resp.GetReplicas() {
		last := ""
		if t := r.GetLastHealthAt(); t != nil {
			last = t.AsTime().UTC().Format(time.RFC3339)
		}
		repRows = append(repRows, []string{
			r.GetId(),
			strings.TrimPrefix(r.GetState().String(), "REPLICA_STATE_"),
			r.GetHost(),
			r.GetContainerId(),
			last,
		})
	}
	fmt.Fprintln(out, "\nReplicas:")
	if err := cliclient.RenderTable(out, repHeaders, repRows); err != nil {
		return err
	}

	// Routes table. PATH distinguishes path-scoped routes that share a domain
	// (empty = catch-all); without it same-domain routes render as
	// indistinguishable duplicate rows (issue #174).
	rtHeaders := []string{"DOMAIN", "PATH", "DEPLOYMENT", "SERVICE", "PORT", "TLS"}
	var rtRows [][]string
	for _, rt := range resp.GetRoutes() {
		tls := "off"
		if rt.GetTlsAuto() {
			tls = "auto"
		}
		rtRows = append(rtRows, []string{
			rt.GetDomain(),
			rt.GetPath(),
			rt.GetDeployment(),
			rt.GetService(),
			strconv.FormatInt(int64(rt.GetPort()), 10),
			tls,
		})
	}
	fmt.Fprintln(out, "\nRoutes:")
	if err := cliclient.RenderTable(out, rtHeaders, rtRows); err != nil {
		return err
	}

	// Certs table (issue #41): per-domain ACME cert state. Omitted entirely
	// when no managed cert is observable (no tls: auto routes, or certs not
	// issued yet).
	if len(resp.GetCerts()) == 0 {
		return nil
	}
	certHeaders := []string{"DOMAIN", "ENVIRONMENT", "NOT_AFTER", "LAST_RENEWAL_AT"}
	var certRows [][]string
	for _, cs := range resp.GetCerts() {
		env := cs.GetEnvironment()
		if env == "" {
			env = "unknown"
		}
		notAfter := ""
		if t := cs.GetNotAfter(); t != nil {
			notAfter = t.AsTime().UTC().Format(time.RFC3339)
		}
		lastRenewal := ""
		if t := cs.GetLastRenewalAt(); t != nil {
			lastRenewal = t.AsTime().UTC().Format(time.RFC3339)
		}
		certRows = append(certRows, []string{cs.GetDomain(), env, notAfter, lastRenewal})
	}
	fmt.Fprintln(out, "\nCerts:")
	return cliclient.RenderTable(out, certHeaders, certRows)
}

// --- structured (-o json / -o yaml) view --------------------------------------

// statusView is the JSON/YAML shape of a status snapshot. Enum fields are
// lowercase snake_case (e.g. replica state "running", deployment status
// "active") per the casing convention documented in docs/cli/status.md.
type statusView struct {
	Deployments []deploymentView `json:"deployments" yaml:"deployments"`
	Replicas    []replicaView    `json:"replicas" yaml:"replicas"`
	Routes      []routeView      `json:"routes" yaml:"routes"`
	Certs       []certView       `json:"certs,omitempty" yaml:"certs,omitempty"`
}

type deploymentView struct {
	Name             string `json:"name" yaml:"name"`
	AppliedRevision  uint64 `json:"applied_revision" yaml:"applied_revision"`
	PreviousRevision uint64 `json:"previous_revision" yaml:"previous_revision"`
	Status           string `json:"status" yaml:"status"`
}

type replicaView struct {
	ID           string `json:"id" yaml:"id"`
	State        string `json:"state" yaml:"state"`
	Host         string `json:"host" yaml:"host"`
	ContainerID  string `json:"container_id" yaml:"container_id"`
	LastHealthAt string `json:"last_health_at,omitempty" yaml:"last_health_at,omitempty"`
}

type routeView struct {
	Domain     string `json:"domain" yaml:"domain"`
	// Path is the URL path prefix this route matches; "" means catch-all.
	// Surfaced so same-domain path-scoped routes are distinguishable (#174).
	Path       string `json:"path" yaml:"path"`
	Deployment string `json:"deployment" yaml:"deployment"`
	Service    string `json:"service" yaml:"service"`
	Port       int32  `json:"port" yaml:"port"`
	// TLS mirrors the table column: "auto" when ACME-managed, else "off".
	TLS string `json:"tls" yaml:"tls"`
	// StripPath reports whether the matched path prefix is stripped before
	// proxying upstream (only meaningful when Path is non-empty).
	StripPath bool `json:"strip_path" yaml:"strip_path"`
}

type certView struct {
	Domain        string `json:"domain" yaml:"domain"`
	Environment   string `json:"environment" yaml:"environment"`
	NotAfter      string `json:"not_after,omitempty" yaml:"not_after,omitempty"`
	LastRenewalAt string `json:"last_renewal_at,omitempty" yaml:"last_renewal_at,omitempty"`
}

// statusToView builds the structured snapshot from the proto response. Slices
// are non-nil (so empty sections serialize as `[]`, not `null`); timestamps
// render as RFC3339 UTC and are omitted when unset.
func statusToView(resp *pb.DeployStatusResponse) statusView {
	v := statusView{
		Deployments: make([]deploymentView, 0, len(resp.GetDeployments())),
		Replicas:    make([]replicaView, 0, len(resp.GetReplicas())),
		Routes:      make([]routeView, 0, len(resp.GetRoutes())),
	}
	for _, d := range resp.GetDeployments() {
		v.Deployments = append(v.Deployments, deploymentView{
			Name:             d.GetName(),
			AppliedRevision:  d.GetAppliedRevision(),
			PreviousRevision: d.GetPreviousRevision(),
			Status:           enumString(d.GetStatus().String(), "DEPLOYMENT_STATUS_"),
		})
	}
	for _, r := range resp.GetReplicas() {
		v.Replicas = append(v.Replicas, replicaView{
			ID:           r.GetId(),
			State:        enumString(r.GetState().String(), "REPLICA_STATE_"),
			Host:         r.GetHost(),
			ContainerID:  r.GetContainerId(),
			LastHealthAt: formatTime(r.GetLastHealthAt()),
		})
	}
	for _, rt := range resp.GetRoutes() {
		tls := "off"
		if rt.GetTlsAuto() {
			tls = "auto"
		}
		v.Routes = append(v.Routes, routeView{
			Domain:     rt.GetDomain(),
			Path:       rt.GetPath(),
			Deployment: rt.GetDeployment(),
			Service:    rt.GetService(),
			Port:       rt.GetPort(),
			TLS:        tls,
			StripPath:  rt.GetStripPath(),
		})
	}
	for _, cs := range resp.GetCerts() {
		env := cs.GetEnvironment()
		if env == "" {
			env = "unknown"
		}
		v.Certs = append(v.Certs, certView{
			Domain:        cs.GetDomain(),
			Environment:   env,
			NotAfter:      formatTime(cs.GetNotAfter()),
			LastRenewalAt: formatTime(cs.GetLastRenewalAt()),
		})
	}
	return v
}

// formatTime renders a protobuf timestamp as RFC3339 UTC, or "" when unset.
func formatTime(t *timestamppb.Timestamp) string {
	if t == nil {
		return ""
	}
	return t.AsTime().UTC().Format(time.RFC3339)
}
