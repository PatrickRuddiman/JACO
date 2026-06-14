package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/PatrickRuddiman/jaco/internal/cliclient"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func init() {
	rootCmd.AddCommand(getCmd())
}

// getCmd is the `jaco get` family: read-only dumps of the current in-raft
// spec for deployments, replicas, and routes (issue #175). Every subcommand
// honors --output (table|json|yaml) and dials the same operator transport as
// `status` (local socket, or --server with a bearer token).
func getCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get",
		Short: "Read the current in-raft spec for a deployment, replica, or route",
		Long: "Dump the cluster state JACO actually stored, not just the fixed projection " +
			"`jaco status` shows. Use -o yaml to print the full deployment/replica/route spec.",
	}
	c.AddCommand(
		getDeploymentsCmd(),
		getDeploymentCmd(),
		getReplicasCmd(),
		getReplicaCmd(),
		getRoutesCmd(),
		getRouteCmd(),
	)
	return c
}

// addOperatorFlags registers the standard operator transport flags on a get
// subcommand and returns the operatorAuth they populate, plus marks the
// command as honoring --output so the root guard lets json/yaml through.
func addOperatorFlags(c *cobra.Command) *operatorAuth {
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[annotationHonorsOutput] = "true"
	a := &operatorAuth{}
	c.Flags().StringVar(&a.server, "server", "", "leader address (host:port); off-node only — omit to use the local socket")
	c.Flags().StringVar(&a.token, "token", "", "operator bearer token (or JACO_TOKEN); required with --server")
	c.Flags().StringVar(&a.caCert, "ca-cert", defaultCACertPath(), "path to cluster CA cert PEM")
	c.Flags().StringVar(&a.socket, "socket", socketDefault(), "local jacod unix socket (used when --server is omitted)")
	return a
}

// dialDeploy resolves the operator transport and returns a DeployClient plus a
// context (with the bearer token applied when on the TCP path) carrying a
// 30s deadline, and the cleanup to run when done.
func dialDeploy(a operatorAuth) (pb.DeployClient, context.Context, func(), error) {
	conn, withAuth, err := dialOperator(a)
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(withAuth(context.Background()), 30*time.Second)
	cleanup := func() {
		cancel()
		conn.Close()
	}
	return pb.NewDeployClient(conn), ctx, cleanup, nil
}

// --- deployments / deployment ------------------------------------------------

func getDeploymentsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "deployments",
		Short: "List deployments with their applied revision and service set",
		Args:  cobra.NoArgs,
	}
	a := addOperatorFlags(c)
	c.RunE = func(_ *cobra.Command, _ []string) error {
		client, ctx, cleanup, err := dialDeploy(*a)
		if err != nil {
			return err
		}
		defer cleanup()
		return runGetDeployments(ctx, client, os.Stdout)
	}
	return c
}

func getDeploymentCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "deployment <name>",
		Short: "Print the full in-raft spec for one deployment (jaco + compose yaml)",
		Args:  cobra.ExactArgs(1),
	}
	a := addOperatorFlags(c)
	c.RunE = func(_ *cobra.Command, args []string) error {
		client, ctx, cleanup, err := dialDeploy(*a)
		if err != nil {
			return err
		}
		defer cleanup()
		return runGetDeployment(ctx, client, args[0], os.Stdout)
	}
	return c
}

func runGetDeployments(ctx context.Context, client pb.DeployClient, out io.Writer) error {
	resp, err := client.Status(ctx, &pb.DeployStatusRequest{})
	if err != nil {
		return cliclient.FormatError(err)
	}
	views := make([]deploymentListView, 0, len(resp.GetDeployments()))
	for _, d := range resp.GetDeployments() {
		views = append(views, deploymentListView{
			Name:             d.GetName(),
			AppliedRevision:  d.GetAppliedRevision(),
			PreviousRevision: d.GetPreviousRevision(),
			Status:           enumString(d.GetStatus().String(), "DEPLOYMENT_STATUS_"),
			Services:         serviceNames(d.GetServices()),
		})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	return renderOutput(out, views, func() error {
		headers := []string{"DEPLOYMENT", "REVISION", "PREVIOUS", "STATUS", "SERVICES"}
		var rows [][]string
		for _, v := range views {
			rows = append(rows, []string{
				v.Name,
				strconv.FormatUint(v.AppliedRevision, 10),
				strconv.FormatUint(v.PreviousRevision, 10),
				strings.ToUpper(v.Status),
				strings.Join(v.Services, ","),
			})
		}
		fmt.Fprintln(out, "Deployments:")
		return cliclient.RenderTable(out, headers, rows)
	})
}

func runGetDeployment(ctx context.Context, client pb.DeployClient, name string, out io.Writer) error {
	resp, err := client.Status(ctx, &pb.DeployStatusRequest{DeploymentFilter: name})
	if err != nil {
		return cliclient.FormatError(err)
	}
	var dep *pb.Deployment
	for _, d := range resp.GetDeployments() {
		if d.GetName() == name {
			dep = d
			break
		}
	}
	if dep == nil {
		return fmt.Errorf("deployment %q not found", name)
	}
	view := deploymentToDetailView(dep)
	return renderOutput(out, view, func() error {
		return renderDeploymentDetail(out, view)
	})
}

func renderDeploymentDetail(out io.Writer, v deploymentDetailView) error {
	fmt.Fprintf(out, "Deployment:    %s\n", v.Name)
	fmt.Fprintf(out, "Revision:      %d (previous %d)\n", v.AppliedRevision, v.PreviousRevision)
	fmt.Fprintf(out, "Status:        %s\n", strings.ToUpper(v.Status))
	if v.AcmeEmail != "" {
		fmt.Fprintf(out, "ACME email:    %s\n", v.AcmeEmail)
	}
	headers := []string{"SERVICE", "REPLICAS", "PLACEMENT", "HOSTS", "NETWORKS"}
	var rows [][]string
	for _, s := range v.Services {
		rows = append(rows, []string{
			s.Name,
			strconv.FormatInt(int64(s.Replicas), 10),
			strings.ToUpper(s.Placement),
			strings.Join(s.Hosts, ","),
			strings.Join(s.Networks, ","),
		})
	}
	fmt.Fprintln(out, "\nServices:")
	if err := cliclient.RenderTable(out, headers, rows); err != nil {
		return err
	}
	if v.JacoYAML != "" {
		fmt.Fprintf(out, "\njaco.yaml:\n%s\n", ensureTrailingNewline(v.JacoYAML))
	}
	if v.ComposeYAML != "" {
		fmt.Fprintf(out, "\ncompose.yaml:\n%s\n", ensureTrailingNewline(v.ComposeYAML))
	}
	return nil
}

// --- replicas / replica ------------------------------------------------------

func getReplicasCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "replicas",
		Short: "List replicas with their deployment, service, state, and restart count",
		Args:  cobra.NoArgs,
	}
	a := addOperatorFlags(c)
	var deployment, service string
	c.Flags().StringVar(&deployment, "deployment", "", "filter to one deployment")
	c.Flags().StringVar(&service, "service", "", "filter to one service")
	c.RunE = func(_ *cobra.Command, _ []string) error {
		client, ctx, cleanup, err := dialDeploy(*a)
		if err != nil {
			return err
		}
		defer cleanup()
		return runGetReplicas(ctx, client, deployment, service, os.Stdout)
	}
	return c
}

func getReplicaCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "replica <id>",
		Short: "Print one replica's spec, state, restart count, and depends_on gates",
		Args:  cobra.ExactArgs(1),
	}
	a := addOperatorFlags(c)
	c.RunE = func(_ *cobra.Command, args []string) error {
		client, ctx, cleanup, err := dialDeploy(*a)
		if err != nil {
			return err
		}
		defer cleanup()
		return runGetReplica(ctx, client, args[0], os.Stdout)
	}
	return c
}

func runGetReplicas(ctx context.Context, client pb.DeployClient, deployment, service string, out io.Writer) error {
	resp, err := client.GetReplicas(ctx, &pb.GetReplicasRequest{
		DeploymentFilter: deployment,
		ServiceFilter:    service,
	})
	if err != nil {
		return cliclient.FormatError(err)
	}
	views := make([]replicaDetailView, 0, len(resp.GetReplicas()))
	for _, r := range resp.GetReplicas() {
		views = append(views, replicaToView(r))
	}
	return renderOutput(out, views, func() error {
		headers := []string{"REPLICA_ID", "DEPLOYMENT", "SERVICE", "STATE", "HOST", "RESTARTS", "IMAGE"}
		var rows [][]string
		for _, v := range views {
			rows = append(rows, []string{
				v.ID,
				v.Deployment,
				v.Service,
				strings.ToUpper(v.State),
				v.Host,
				strconv.FormatInt(int64(v.RestartCount), 10),
				v.Image,
			})
		}
		fmt.Fprintln(out, "Replicas:")
		return cliclient.RenderTable(out, headers, rows)
	})
}

func runGetReplica(ctx context.Context, client pb.DeployClient, id string, out io.Writer) error {
	resp, err := client.GetReplicas(ctx, &pb.GetReplicasRequest{ReplicaId: id})
	if err != nil {
		return cliclient.FormatError(err)
	}
	var rep *pb.ReplicaDetail
	for _, r := range resp.GetReplicas() {
		if r.GetId() == id {
			rep = r
			break
		}
	}
	if rep == nil {
		return fmt.Errorf("replica %q not found", id)
	}
	view := replicaToView(rep)
	return renderOutput(out, view, func() error {
		return renderReplicaDetail(out, view)
	})
}

func renderReplicaDetail(out io.Writer, v replicaDetailView) error {
	fmt.Fprintf(out, "Replica:       %s\n", v.ID)
	fmt.Fprintf(out, "Deployment:    %s\n", v.Deployment)
	fmt.Fprintf(out, "Service:       %s (index %d)\n", v.Service, v.Index)
	fmt.Fprintf(out, "State:         %s\n", strings.ToUpper(v.State))
	fmt.Fprintf(out, "Host:          %s\n", v.Host)
	fmt.Fprintf(out, "Image:         %s\n", v.Image)
	fmt.Fprintf(out, "Revision:      %d\n", v.Revision)
	fmt.Fprintf(out, "Container:     %s\n", v.ContainerID)
	fmt.Fprintf(out, "Restarts:      %d\n", v.RestartCount)
	if v.Code != "" || v.Message != "" {
		fmt.Fprintf(out, "Last reason:   %s %s\n", v.Code, v.Message)
	}
	if v.StartedAt != "" {
		fmt.Fprintf(out, "Started at:    %s\n", v.StartedAt)
	}
	if v.LastHealthAt != "" {
		fmt.Fprintf(out, "Last health:   %s\n", v.LastHealthAt)
	}
	if v.LastAttemptAt != "" {
		fmt.Fprintf(out, "Last attempt:  %s\n", v.LastAttemptAt)
	}
	if len(v.DependsOn) == 0 {
		return nil
	}
	headers := []string{"SERVICE", "CONDITION", "STATE", "SATISFIED"}
	var rows [][]string
	for _, d := range v.DependsOn {
		rows = append(rows, []string{
			d.Service,
			d.Condition,
			strings.ToUpper(d.State),
			strconv.FormatBool(d.Satisfied),
		})
	}
	fmt.Fprintln(out, "\nDepends on:")
	return cliclient.RenderTable(out, headers, rows)
}

// --- routes / route ----------------------------------------------------------

func getRoutesCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "routes",
		Short: "List ingress routes including the path prefix column",
		Args:  cobra.NoArgs,
	}
	a := addOperatorFlags(c)
	var domain string
	c.Flags().StringVar(&domain, "domain", "", "filter to one domain")
	c.RunE = func(_ *cobra.Command, _ []string) error {
		client, ctx, cleanup, err := dialDeploy(*a)
		if err != nil {
			return err
		}
		defer cleanup()
		return runGetRoutes(ctx, client, domain, os.Stdout)
	}
	return c
}

func getRouteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "route <domain>",
		Short: "Print every ingress entry for one domain in path-match order",
		Args:  cobra.ExactArgs(1),
	}
	a := addOperatorFlags(c)
	c.RunE = func(_ *cobra.Command, args []string) error {
		client, ctx, cleanup, err := dialDeploy(*a)
		if err != nil {
			return err
		}
		defer cleanup()
		return runGetRoute(ctx, client, args[0], os.Stdout)
	}
	return c
}

func runGetRoutes(ctx context.Context, client pb.DeployClient, domain string, out io.Writer) error {
	resp, err := client.Status(ctx, &pb.DeployStatusRequest{})
	if err != nil {
		return cliclient.FormatError(err)
	}
	views := routeViews(resp.GetRoutes(), domain)
	return renderOutput(out, views, func() error {
		fmt.Fprintln(out, "Routes:")
		return renderRouteTable(out, views)
	})
}

func runGetRoute(ctx context.Context, client pb.DeployClient, domain string, out io.Writer) error {
	resp, err := client.Status(ctx, &pb.DeployStatusRequest{})
	if err != nil {
		return cliclient.FormatError(err)
	}
	views := routeViews(resp.GetRoutes(), domain)
	if len(views) == 0 {
		return fmt.Errorf("route %q not found", domain)
	}
	return renderOutput(out, views, func() error {
		fmt.Fprintf(out, "Route: %s\n", domain)
		return renderRouteTable(out, views)
	})
}

func renderRouteTable(out io.Writer, views []routeDetailView) error {
	headers := []string{"DOMAIN", "PATH", "DEPLOYMENT", "SERVICE", "PORT", "TLS", "STRIP_PATH"}
	var rows [][]string
	for _, v := range views {
		path := v.Path
		if path == "" {
			path = "/"
		}
		rows = append(rows, []string{
			v.Domain,
			path,
			v.Deployment,
			v.Service,
			strconv.FormatInt(int64(v.Port), 10),
			v.TLS,
			strconv.FormatBool(v.StripPath),
		})
	}
	return cliclient.RenderTable(out, headers, rows)
}

// --- views -------------------------------------------------------------------

type deploymentListView struct {
	Name             string   `json:"name" yaml:"name"`
	AppliedRevision  uint64   `json:"applied_revision" yaml:"applied_revision"`
	PreviousRevision uint64   `json:"previous_revision" yaml:"previous_revision"`
	Status           string   `json:"status" yaml:"status"`
	Services         []string `json:"services" yaml:"services"`
}

type serviceSpecView struct {
	Name      string   `json:"name" yaml:"name"`
	Replicas  int32    `json:"replicas" yaml:"replicas"`
	Placement string   `json:"placement" yaml:"placement"`
	Hosts     []string `json:"hosts,omitempty" yaml:"hosts,omitempty"`
	Networks  []string `json:"networks,omitempty" yaml:"networks,omitempty"`
}

type deploymentDetailView struct {
	Name             string            `json:"name" yaml:"name"`
	AppliedRevision  uint64            `json:"applied_revision" yaml:"applied_revision"`
	PreviousRevision uint64            `json:"previous_revision" yaml:"previous_revision"`
	Status           string            `json:"status" yaml:"status"`
	AcmeEmail        string            `json:"acme_email,omitempty" yaml:"acme_email,omitempty"`
	Services         []serviceSpecView `json:"services" yaml:"services"`
	JacoYAML         string            `json:"jaco_yaml,omitempty" yaml:"jaco_yaml,omitempty"`
	ComposeYAML      string            `json:"compose_yaml,omitempty" yaml:"compose_yaml,omitempty"`
}

type dependencyView struct {
	Service   string `json:"service" yaml:"service"`
	Condition string `json:"condition,omitempty" yaml:"condition,omitempty"`
	State     string `json:"state" yaml:"state"`
	Satisfied bool   `json:"satisfied" yaml:"satisfied"`
}

type replicaDetailView struct {
	ID            string            `json:"id" yaml:"id"`
	Deployment    string            `json:"deployment" yaml:"deployment"`
	Service       string            `json:"service" yaml:"service"`
	Index         int32             `json:"index" yaml:"index"`
	State         string            `json:"state" yaml:"state"`
	Host          string            `json:"host" yaml:"host"`
	Image         string            `json:"image" yaml:"image"`
	Revision      uint64            `json:"revision" yaml:"revision"`
	ContainerID   string            `json:"container_id,omitempty" yaml:"container_id,omitempty"`
	RestartCount  int32             `json:"restart_count" yaml:"restart_count"`
	Code          string            `json:"code,omitempty" yaml:"code,omitempty"`
	Message       string            `json:"message,omitempty" yaml:"message,omitempty"`
	Details       map[string]string `json:"details,omitempty" yaml:"details,omitempty"`
	StartedAt     string            `json:"started_at,omitempty" yaml:"started_at,omitempty"`
	LastHealthAt  string            `json:"last_health_at,omitempty" yaml:"last_health_at,omitempty"`
	LastAttemptAt string            `json:"last_attempt_at,omitempty" yaml:"last_attempt_at,omitempty"`
	DependsOn     []dependencyView  `json:"depends_on,omitempty" yaml:"depends_on,omitempty"`
}

type routeDetailView struct {
	Domain     string `json:"domain" yaml:"domain"`
	Path       string `json:"path" yaml:"path"`
	Deployment string `json:"deployment" yaml:"deployment"`
	Service    string `json:"service" yaml:"service"`
	Port       int32  `json:"port" yaml:"port"`
	TLS        string `json:"tls" yaml:"tls"`
	StripPath  bool   `json:"strip_path" yaml:"strip_path"`
}

func serviceNames(svcs []*pb.ServiceSpec) []string {
	names := make([]string, 0, len(svcs))
	for _, s := range svcs {
		names = append(names, s.GetName())
	}
	sort.Strings(names)
	return names
}

func deploymentToDetailView(d *pb.Deployment) deploymentDetailView {
	services := make([]serviceSpecView, 0, len(d.GetServices()))
	for _, s := range d.GetServices() {
		services = append(services, serviceSpecView{
			Name:      s.GetName(),
			Replicas:  s.GetReplicas(),
			Placement: enumString(s.GetPlacement().String(), "PLACEMENT_MODE_"),
			Hosts:     s.GetHosts(),
			Networks:  s.GetNetworks(),
		})
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	return deploymentDetailView{
		Name:             d.GetName(),
		AppliedRevision:  d.GetAppliedRevision(),
		PreviousRevision: d.GetPreviousRevision(),
		Status:           enumString(d.GetStatus().String(), "DEPLOYMENT_STATUS_"),
		AcmeEmail:        d.GetAcmeEmail(),
		Services:         services,
		JacoYAML:         string(d.GetJacoYaml()),
		ComposeYAML:      string(d.GetComposeYaml()),
	}
}

func replicaToView(r *pb.ReplicaDetail) replicaDetailView {
	deps := make([]dependencyView, 0, len(r.GetDependsOn()))
	for _, d := range r.GetDependsOn() {
		deps = append(deps, dependencyView{
			Service:   d.GetService(),
			Condition: d.GetCondition(),
			State:     enumString(d.GetState().String(), "REPLICA_STATE_"),
			Satisfied: d.GetSatisfied(),
		})
	}
	if len(deps) == 0 {
		deps = nil
	}
	return replicaDetailView{
		ID:            r.GetId(),
		Deployment:    r.GetDeployment(),
		Service:       r.GetService(),
		Index:         r.GetIndex(),
		State:         enumString(r.GetState().String(), "REPLICA_STATE_"),
		Host:          r.GetHost(),
		Image:         r.GetImage(),
		Revision:      r.GetRevision(),
		ContainerID:   r.GetContainerId(),
		RestartCount:  r.GetRestartCount(),
		Code:          r.GetCode(),
		Message:       r.GetMessage(),
		Details:       r.GetDetails(),
		StartedAt:     formatTime(r.GetStartedAt()),
		LastHealthAt:  formatTime(r.GetLastHealthAt()),
		LastAttemptAt: formatTime(r.GetLastAttemptAt()),
		DependsOn:     deps,
	}
}

// routeViews filters routes to domain (when set) and projects them to the
// structured view. Order is preserved as the server emits it (the ingress
// reconciler stores routes longest-prefix-first), with a stable domain+path
// tiebreak so output is deterministic.
func routeViews(routes []*pb.Route, domain string) []routeDetailView {
	views := make([]routeDetailView, 0, len(routes))
	for _, rt := range routes {
		if domain != "" && rt.GetDomain() != domain {
			continue
		}
		tls := "off"
		if rt.GetTlsAuto() {
			tls = "auto"
		}
		views = append(views, routeDetailView{
			Domain:     rt.GetDomain(),
			Path:       rt.GetPath(),
			Deployment: rt.GetDeployment(),
			Service:    rt.GetService(),
			Port:       rt.GetPort(),
			TLS:        tls,
			StripPath:  rt.GetStripPath(),
		})
	}
	sort.SliceStable(views, func(i, j int) bool {
		if views[i].Domain != views[j].Domain {
			return views[i].Domain < views[j].Domain
		}
		return views[i].Path < views[j].Path
	})
	return views
}

func ensureTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
