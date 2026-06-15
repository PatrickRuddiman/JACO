package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/PatrickRuddiman/jaco/internal/cliclient"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func getRouteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "route <domain>",
		Short: "Show the realized ingress routes Caddy serves for a domain",
		Long: "Print the routes Caddy actually serves for a domain, ordered the way\n" +
			"it evaluates them: path-scoped routes longest-prefix-first, the\n" +
			"catch-all last. Each row shows its upstream service/port and how many\n" +
			"replicas are currently healthy (READY) — a route with 0 ready\n" +
			"upstreams returns 503 for every matching request.",
		Args: cobra.ExactArgs(1),
		// Honors --output; renders -o json / -o yaml via renderGetRouteOut.
		Annotations: map[string]string{annotationHonorsOutput: "true"},
	}
	var server, opToken, caCertPath, socket string
	c.Flags().StringVar(&server, "server", "", "leader address (host:port); off-node only — omit to use the local socket")
	c.Flags().StringVar(&opToken, "token", "", "operator bearer token (or JACO_TOKEN); required with --server")
	c.Flags().StringVar(&caCertPath, "ca-cert", defaultCACertPath(), "path to cluster CA cert PEM")
	c.Flags().StringVar(&socket, "socket", socketDefault(), "local jacod unix socket (used when --server is omitted)")

	c.RunE = func(_ *cobra.Command, args []string) error {
		conn, withAuth, err := dialOperator(operatorAuth{server: server, token: opToken, caCert: caCertPath, socket: socket})
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx := withAuth(context.Background())
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return runGetRoute(ctx, pb.NewDeployClient(conn), args[0], os.Stdout)
	}
	return c
}

func runGetRoute(ctx context.Context, client pb.DeployClient, domain string, out io.Writer) error {
	resp, err := client.GetRoute(ctx, &pb.GetRouteRequest{Domain: domain})
	if err != nil {
		return cliclient.FormatError(err)
	}
	return renderGetRouteOut(out, resp)
}

func renderGetRouteOut(out io.Writer, resp *pb.GetRouteResponse) error {
	return renderOutput(out, getRouteToView(resp), func() error {
		return renderGetRoute(out, resp)
	})
}

// renderGetRoute writes the human table: one row per realized route, in the
// order Caddy evaluates them.
func renderGetRoute(out io.Writer, resp *pb.GetRouteResponse) error {
	fmt.Fprintf(out, "Routes for %s:\n", resp.GetDomain())
	headers := []string{"PATH", "SERVICE", "PORT", "TLS", "STRIP", "FALLBACK", "READY"}
	var rows [][]string
	for _, r := range resp.GetRoutes() {
		tls := "off"
		if r.GetTlsAuto() {
			tls = "auto"
		}
		rows = append(rows, []string{
			r.GetPath(),
			r.GetService(),
			strconv.FormatInt(int64(r.GetPort()), 10),
			tls,
			boolYesNo(r.GetStripPath()),
			boolYesNo(r.GetCatchAll()),
			fmt.Sprintf("%d/%d", r.GetReadyReplicas(), r.GetTotalReplicas()),
		})
	}
	return cliclient.RenderTable(out, headers, rows)
}

func boolYesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// --- structured (-o json / -o yaml) view --------------------------------------

type getRouteView struct {
	Domain string             `json:"domain" yaml:"domain"`
	Routes []realizedRouteView `json:"routes" yaml:"routes"`
}

type realizedRouteView struct {
	Path          string `json:"path" yaml:"path"`
	CatchAll      bool   `json:"catch_all" yaml:"catch_all"`
	Deployment    string `json:"deployment" yaml:"deployment"`
	Service       string `json:"service" yaml:"service"`
	Port          int32  `json:"port" yaml:"port"`
	TLS           string `json:"tls" yaml:"tls"`
	StripPath     bool   `json:"strip_path" yaml:"strip_path"`
	ReadyReplicas int32  `json:"ready_replicas" yaml:"ready_replicas"`
	TotalReplicas int32  `json:"total_replicas" yaml:"total_replicas"`
}

func getRouteToView(resp *pb.GetRouteResponse) getRouteView {
	v := getRouteView{
		Domain: resp.GetDomain(),
		Routes: make([]realizedRouteView, 0, len(resp.GetRoutes())),
	}
	for _, r := range resp.GetRoutes() {
		tls := "off"
		if r.GetTlsAuto() {
			tls = "auto"
		}
		v.Routes = append(v.Routes, realizedRouteView{
			Path:          r.GetPath(),
			CatchAll:      r.GetCatchAll(),
			Deployment:    r.GetDeployment(),
			Service:       r.GetService(),
			Port:          r.GetPort(),
			TLS:           tls,
			StripPath:     r.GetStripPath(),
			ReadyReplicas: r.GetReadyReplicas(),
			TotalReplicas: r.GetTotalReplicas(),
		})
	}
	return v
}
