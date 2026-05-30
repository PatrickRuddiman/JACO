package grpcsrv

import (
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// intPtr returns a pointer to an int literal. Used to populate
// JacoServiceDecl.Replicas (a *int) from inline test fixtures.
func intPtr(v int) *int { return &v }

// TestValidateJacoYAML_RejectsEveryBranch — drives the validation
// rule each guard fires on. The helper is package-private; covered
// in-package.
func TestValidateJacoYAML_RejectsEveryBranch(t *testing.T) {
	cases := []struct {
		name string
		j    *JacoYAML
		want string // substring of message
	}{
		{
			name: "empty deployment",
			j:    &JacoYAML{},
			want: "deployment name is required",
		},
		{
			name: "service name empty",
			j: &JacoYAML{
				Deployment: "d",
				Services:   []JacoServiceDecl{{Name: "", Placement: "spread"}},
			},
			want: "service name is required",
		},
		{
			name: "negative replicas",
			j: &JacoYAML{
				Deployment: "d",
				Services:   []JacoServiceDecl{{Name: "w", Replicas: intPtr(-1), Placement: "spread"}},
			},
			want: "replicas must be >= 0",
		},
		{
			name: "unknown placement",
			j: &JacoYAML{
				Deployment: "d",
				Services:   []JacoServiceDecl{{Name: "w", Placement: "rotate"}},
			},
			want: "unknown placement",
		},
		{
			name: "hosts placement empty hosts",
			j: &JacoYAML{
				Deployment: "d",
				Services:   []JacoServiceDecl{{Name: "w", Placement: "hosts"}},
			},
			want: "hosts is empty",
		},
		{
			name: "duplicate service name",
			j: &JacoYAML{
				Deployment: "d",
				Services: []JacoServiceDecl{
					{Name: "w", Placement: "spread"},
					{Name: "w", Placement: "spread"},
				},
			},
			want: "declared more than once",
		},
		{
			name: "global placement with explicit replicas",
			j: &JacoYAML{
				Deployment: "d",
				Services:   []JacoServiceDecl{{Name: "w", Placement: "global", Replicas: intPtr(3)}},
			},
			want: "placement=global",
		},
		{
			name: "route empty domain",
			j: &JacoYAML{
				Deployment: "d",
				Services:   []JacoServiceDecl{{Name: "w", Placement: "spread"}},
				Routes:     []JacoRouteDecl{{Service: "w", Port: 80, TLS: "auto"}},
			},
			want: "route domain is required",
		},
		{
			name: "route empty service",
			j: &JacoYAML{
				Deployment: "d",
				Services:   []JacoServiceDecl{{Name: "w", Placement: "spread"}},
				Routes:     []JacoRouteDecl{{Domain: "a", Port: 80, TLS: "auto"}},
			},
			want: "service is required",
		},
		{
			name: "route bad port",
			j: &JacoYAML{
				Deployment: "d",
				Services:   []JacoServiceDecl{{Name: "w", Placement: "spread"}},
				Routes:     []JacoRouteDecl{{Domain: "a", Service: "w", Port: 0, TLS: "auto"}},
			},
			want: "port must be > 0",
		},
		{
			name: "route bad TLS",
			j: &JacoYAML{
				Deployment: "d",
				Services:   []JacoServiceDecl{{Name: "w", Placement: "spread"}},
				Routes:     []JacoRouteDecl{{Domain: "a", Service: "w", Port: 80, TLS: "rotated"}},
			},
			want: "unknown tls",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, msg, ok := validateJacoYAML(c.j)
			if ok {
				t.Fatalf("validation passed; expected fail on %s", c.name)
			}
			if !contains(msg, c.want) {
				t.Errorf("msg = %q, want %q substring", msg, c.want)
			}
		})
	}
}

// TestValidateJacoYAML_HappyPath — every guard passes.
func TestValidateJacoYAML_HappyPath(t *testing.T) {
	j := &JacoYAML{
		Deployment: "d",
		Services: []JacoServiceDecl{
			{Name: "w", Placement: "spread", Replicas: intPtr(2)},
			{Name: "api", Placement: "hosts", Hosts: []string{"host-a"}, Replicas: intPtr(1)},
		},
		Routes: []JacoRouteDecl{
			{Domain: "a.example", Service: "w", Port: 80, TLS: "auto"},
			{Domain: "b.example", Service: "api", Port: 8080, TLS: "off"},
		},
	}
	_, _, ok := validateJacoYAML(j)
	if !ok {
		t.Errorf("happy-path JacoYAML failed validation")
	}
}

// TestValidateJacoYAML_EmptyServicesAccepted — issue #99: a slim jaco.yaml
// that declares only routes (no `services:`) is valid intrinsically; compose
// supplies the container set, and route → service cross-validation runs at
// Apply time against the merged set.
func TestValidateJacoYAML_EmptyServicesAccepted(t *testing.T) {
	cases := []struct {
		name string
		j    *JacoYAML
	}{
		{
			name: "nil services slice",
			j: &JacoYAML{
				Deployment: "d",
				Routes: []JacoRouteDecl{
					{Domain: "a.example", Service: "anything", Port: 80, TLS: "auto"},
				},
			},
		},
		{
			name: "empty services slice",
			j: &JacoYAML{
				Deployment: "d",
				Services:   []JacoServiceDecl{},
				Routes: []JacoRouteDecl{
					{Domain: "a.example", Service: "anything", Port: 80, TLS: "auto"},
				},
			},
		},
		{
			name: "routes also empty",
			j:    &JacoYAML{Deployment: "d"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, msg, ok := validateJacoYAML(c.j)
			if !ok {
				t.Fatalf("validation rejected slim jaco.yaml: code=%q msg=%q", code, msg)
			}
		})
	}
}

// TestValidateJacoYAML_RouteUnknownServiceDeferred — validateJacoYAML no
// longer rejects routes whose service field doesn't appear in jaco.yaml's
// `services:` list. The compose project may supply the service; the check
// runs at Apply time against the merged set (deploy.go).
func TestValidateJacoYAML_RouteUnknownServiceDeferred(t *testing.T) {
	j := &JacoYAML{
		Deployment: "d",
		Services:   []JacoServiceDecl{{Name: "w", Placement: "spread"}},
		Routes:     []JacoRouteDecl{{Domain: "a", Service: "ghost", Port: 80, TLS: "auto"}},
	}
	if _, msg, ok := validateJacoYAML(j); !ok {
		t.Fatalf("validateJacoYAML rejected route→unknown-service in-package; that check moved to Apply: %q", msg)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestParseJacoYAML_RejectsComposeServiceField — any YAML that includes
// compose_service must be rejected with the documented error, no matter
// where in the document the key appears.
func TestParseJacoYAML_RejectsComposeServiceField(t *testing.T) {
	const want = "compose_service is no longer supported"
	cases := []struct {
		name  string
		input []byte
	}{
		{
			name: "under services entry (original placement)",
			input: []byte(`deployment: d
services:
  - name: web
    replicas: 1
    compose_service: web
`),
		},
		{
			name: "at top level",
			input: []byte(`deployment: d
compose_service: web
services:
  - name: web
    replicas: 1
`),
		},
		{
			name: "nested under a non-services key",
			input: []byte(`deployment: d
metadata:
  legacy:
    compose_service: web
services:
  - name: web
    replicas: 1
`),
		},
		{
			name: "inside a routes entry",
			input: []byte(`deployment: d
services:
  - name: web
    replicas: 1
routes:
  - domain: example.com
    service: web
    port: 80
    compose_service: web
`),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseJacoYAML(tc.input)
			if err == nil {
				t.Fatal("expected error for compose_service field; got nil")
			}
			if !contains(err.Error(), want) {
				t.Errorf("error = %q; want %q substring", err.Error(), want)
			}
		})
	}
}

// TestParseJacoYAML_HappyPath — valid YAML without compose_service
// parses cleanly with name as the compose key.
func TestParseJacoYAML_HappyPath(t *testing.T) {
	input := []byte(`deployment: myapp
services:
  - name: web
    replicas: 2
`)
	j, err := ParseJacoYAML(input)
	if err != nil {
		t.Fatalf("ParseJacoYAML: %v", err)
	}
	if len(j.Services) != 1 || j.Services[0].Name != "web" {
		t.Errorf("unexpected services: %+v", j.Services)
	}
	if j.Services[0].Placement != "spread" {
		t.Errorf("default placement = %q; want spread", j.Services[0].Placement)
	}
	if j.Services[0].Replicas == nil || *j.Services[0].Replicas != 2 {
		t.Errorf("Replicas = %v; want *2", j.Services[0].Replicas)
	}
}

// TestParseJacoYAML_ReplicasUnsetStaysNil — issue #99: omitting `replicas:`
// on a JACO entry must round-trip as nil so MergeServiceDefaults can tell
// "no override" apart from "explicit zero".
func TestParseJacoYAML_ReplicasUnsetStaysNil(t *testing.T) {
	input := []byte(`deployment: myapp
services:
  - name: web
`)
	j, err := ParseJacoYAML(input)
	if err != nil {
		t.Fatalf("ParseJacoYAML: %v", err)
	}
	if got := j.Services[0].Replicas; got != nil {
		t.Errorf("Replicas = %v; want nil (unset)", *got)
	}
}

// TestParseJacoYAML_ReplicasZeroHonored — explicit `replicas: 0` parses to
// a pointer to zero, distinguishable from "unset" (nil).
func TestParseJacoYAML_ReplicasZeroHonored(t *testing.T) {
	input := []byte(`deployment: myapp
services:
  - name: web
    replicas: 0
`)
	j, err := ParseJacoYAML(input)
	if err != nil {
		t.Fatalf("ParseJacoYAML: %v", err)
	}
	if j.Services[0].Replicas == nil {
		t.Fatal("Replicas = nil; want *0")
	}
	if *j.Services[0].Replicas != 0 {
		t.Errorf("*Replicas = %d; want 0", *j.Services[0].Replicas)
	}
}

// TestValidationError_Error — formatter for the ValidationError type
// surfaces its Message verbatim.
func TestValidationError_Error(t *testing.T) {
	if got := (&ValidationError{Code: "c", Message: "m"}).Error(); got != "m" {
		t.Errorf("Error = %q, want m", got)
	}
}

// TestParseJacoYAML_PathRoundTrip — path is unmarshaled and copied into pb.Route.
func TestParseJacoYAML_PathRoundTrip(t *testing.T) {
	yaml := `
deployment: myapp
services:
  - name: api
    replicas: 1
    placement: spread
  - name: web
    replicas: 2
    placement: spread
routes:
  - domain: jaco.sh
    path: /api/
    service: api
    port: 8080
  - domain: jaco.sh
    service: web
    port: 80
`
	j, err := ParseJacoYAML([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseJacoYAML: %v", err)
	}
	if len(j.Routes) != 2 {
		t.Fatalf("routes len = %d, want 2", len(j.Routes))
	}
	if j.Routes[0].Path != "/api/" {
		t.Errorf("Routes[0].Path = %q, want /api/", j.Routes[0].Path)
	}
	if j.Routes[1].Path != "" {
		t.Errorf("Routes[1].Path = %q, want empty (catch-all)", j.Routes[1].Path)
	}

	// Verify toRoutes copies Path into pb.Route.
	pbs := toRoutes("myapp", j.Routes)
	if pbs[0].Path != "/api/" {
		t.Errorf("pb.Route[0].Path = %q, want /api/", pbs[0].Path)
	}
	if pbs[1].Path != "" {
		t.Errorf("pb.Route[1].Path = %q, want empty", pbs[1].Path)
	}
}

// TestToTCPRoutes verifies only explicit published TCP ports become routes:
// bare/ephemeral, UDP, and host-IP-scoped entries produce no listener.
func TestToTCPRoutes(t *testing.T) {
	project, err := compose.LoadBytes([]byte(`services:
  db:
    image: postgres:16
    ports:
      - "5432:5432"
      - "9999"
      - "53:53/udp"
      - "127.0.0.1:6443:6443"
`), "x.yml")
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}

	got := toTCPRoutes("data", project)
	if len(got) != 1 {
		t.Fatalf("toTCPRoutes len = %d, want 1: %+v", len(got), got)
	}
	r := got[0]
	if r.GetPublishedPort() != 5432 || r.GetContainerPort() != 5432 ||
		r.GetService() != "db" || r.GetDeployment() != "data" {
		t.Errorf("unexpected route: %+v", r)
	}
}

// TestValidateJacoYAML_RouteConflict — (domain, path) is the uniqueness
// key: any duplicate must be rejected, regardless of whether the other
// fields (service, port, tls) agree.
func TestValidateJacoYAML_RouteConflict(t *testing.T) {
	t.Run("conflict on path /api/ different services", func(t *testing.T) {
		j := &JacoYAML{
			Deployment: "d",
			Services: []JacoServiceDecl{
				{Name: "api1", Placement: "spread"},
				{Name: "api2", Placement: "spread"},
			},
			Routes: []JacoRouteDecl{
				{Domain: "jaco.sh", Path: "/api/", Service: "api1", Port: 80, TLS: "auto"},
				{Domain: "jaco.sh", Path: "/api/", Service: "api2", Port: 80, TLS: "auto"},
			},
		}
		_, msg, ok := validateJacoYAML(j)
		if ok {
			t.Fatal("validation passed; expected conflict rejection")
		}
		want := `route conflict: domain "jaco.sh" path "/api/" is declared more than once; (domain, path) combinations must be unique`
		if msg != want {
			t.Errorf("msg = %q, want %q", msg, want)
		}
	})

	t.Run("conflict on catch-all (empty path)", func(t *testing.T) {
		j := &JacoYAML{
			Deployment: "d",
			Services: []JacoServiceDecl{
				{Name: "web1", Placement: "spread"},
				{Name: "web2", Placement: "spread"},
			},
			Routes: []JacoRouteDecl{
				{Domain: "jaco.sh", Path: "", Service: "web1", Port: 80, TLS: "auto"},
				{Domain: "jaco.sh", Path: "", Service: "web2", Port: 80, TLS: "auto"},
			},
		}
		_, msg, ok := validateJacoYAML(j)
		if ok {
			t.Fatal("validation passed; expected catch-all conflict rejection")
		}
		want := `route conflict: domain "jaco.sh" path "" is declared more than once; (domain, path) combinations must be unique`
		if msg != want {
			t.Errorf("msg = %q, want %q", msg, want)
		}
	})

	t.Run("conflict on same service different port", func(t *testing.T) {
		// Regression: previously the conflict check only fired when the
		// service field differed. Two routes with the same (domain, path)
		// and same service but different port slipped through and produced
		// nondeterministic Caddy config.
		j := &JacoYAML{
			Deployment: "d",
			Services: []JacoServiceDecl{
				{Name: "api", Placement: "spread"},
			},
			Routes: []JacoRouteDecl{
				{Domain: "jaco.sh", Path: "/api/", Service: "api", Port: 8080, TLS: "auto"},
				{Domain: "jaco.sh", Path: "/api/", Service: "api", Port: 9090, TLS: "auto"},
			},
		}
		_, msg, ok := validateJacoYAML(j)
		if ok {
			t.Fatal("validation passed; expected port-divergence rejection")
		}
		want := `route conflict: domain "jaco.sh" path "/api/" is declared more than once; (domain, path) combinations must be unique`
		if msg != want {
			t.Errorf("msg = %q, want %q", msg, want)
		}
	})

	t.Run("conflict on same service different tls", func(t *testing.T) {
		// Regression: tls divergence also slipped through the old check.
		j := &JacoYAML{
			Deployment: "d",
			Services: []JacoServiceDecl{
				{Name: "api", Placement: "spread"},
			},
			Routes: []JacoRouteDecl{
				{Domain: "jaco.sh", Path: "/api/", Service: "api", Port: 8080, TLS: "auto"},
				{Domain: "jaco.sh", Path: "/api/", Service: "api", Port: 8080, TLS: "off"},
			},
		}
		_, _, ok := validateJacoYAML(j)
		if ok {
			t.Fatal("validation passed; expected tls-divergence rejection")
		}
	})

	t.Run("no conflict when paths differ", func(t *testing.T) {
		j := &JacoYAML{
			Deployment: "d",
			Services: []JacoServiceDecl{
				{Name: "api", Placement: "spread"},
				{Name: "web", Placement: "spread"},
			},
			Routes: []JacoRouteDecl{
				{Domain: "jaco.sh", Path: "/api/", Service: "api", Port: 8080, TLS: "auto"},
				{Domain: "jaco.sh", Path: "", Service: "web", Port: 80, TLS: "auto"},
			},
		}
		_, _, ok := validateJacoYAML(j)
		if !ok {
			t.Error("validation rejected valid multi-path routes")
		}
	})
}

// TestValidateJacoYAML_MixedTLSRejected — a domain mixing tls:auto and tls:off
// routes is rejected with code route_tls_mixed (issue #46): Caddy can't
// half-redirect a domain. Same-mode routes on a domain stay valid.
func TestValidateJacoYAML_MixedTLSRejected(t *testing.T) {
	mixed := &JacoYAML{
		Deployment: "d",
		Services: []JacoServiceDecl{
			{Name: "web", Placement: "spread"},
			{Name: "api", Placement: "spread"},
		},
		Routes: []JacoRouteDecl{
			{Domain: "jaco.sh", Path: "/", Service: "web", Port: 80, TLS: "auto"},
			{Domain: "jaco.sh", Path: "/api/", Service: "api", Port: 80, TLS: "off"},
		},
	}
	code, msg, ok := validateJacoYAML(mixed)
	if ok {
		t.Fatal("validation passed; expected route_tls_mixed rejection")
	}
	if code != "route_tls_mixed" {
		t.Errorf("code = %q, want route_tls_mixed (msg=%q)", code, msg)
	}

	// Both routes tls:auto on one domain is fine (the common case).
	same := &JacoYAML{
		Deployment: "d",
		Services:   []JacoServiceDecl{{Name: "web", Placement: "spread"}, {Name: "api", Placement: "spread"}},
		Routes: []JacoRouteDecl{
			{Domain: "jaco.sh", Path: "/", Service: "web", Port: 80, TLS: "auto"},
			{Domain: "jaco.sh", Path: "/api/", Service: "api", Port: 80, TLS: "auto"},
		},
	}
	if _, msg, ok := validateJacoYAML(same); !ok {
		t.Errorf("same-mode routes rejected: %q", msg)
	}
}

// TestParseJacoYAML_DeploymentACMEOff — a top-level `acme: off` (issue #41)
// implicitly disables TLS on every route that didn't set tls explicitly, but
// a route may still opt back in with tls: auto.
func TestParseJacoYAML_DeploymentACMEOff(t *testing.T) {
	yaml := `
deployment: dev
acme: off
services:
  - name: web
    replicas: 1
routes:
  - domain: a.example.com
    service: web
    port: 80
  - domain: b.example.com
    service: web
    port: 80
    tls: auto
`
	j, err := ParseJacoYAML([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseJacoYAML: %v", err)
	}
	if j.Routes[0].TLS != "off" {
		t.Errorf("route a (no tls under acme:off) = %q, want off", j.Routes[0].TLS)
	}
	if j.Routes[1].TLS != "auto" {
		t.Errorf("route b (explicit tls:auto) = %q, want auto (route override wins)", j.Routes[1].TLS)
	}
	// And the projection onto pb.Route carries through.
	pbs := toRoutes("dev", j.Routes)
	if pbs[0].GetTlsAuto() {
		t.Errorf("pb route a TlsAuto = true; want false under acme:off")
	}
	if !pbs[1].GetTlsAuto() {
		t.Errorf("pb route b TlsAuto = false; want true (override)")
	}
}

// TestParseJacoYAML_NoACMEKeyDefaultsAuto — without acme:off, a blank route
// tls still defaults to auto (no regression).
func TestParseJacoYAML_NoACMEKeyDefaultsAuto(t *testing.T) {
	yaml := `
deployment: prod
services:
  - name: web
    replicas: 1
routes:
  - domain: a.example.com
    service: web
    port: 80
`
	j, err := ParseJacoYAML([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseJacoYAML: %v", err)
	}
	if j.Routes[0].TLS != "auto" {
		t.Errorf("blank route tls = %q, want auto", j.Routes[0].TLS)
	}
}

// TestValidateJacoYAML_RejectsUnknownACME — acme must be on|off|empty.
func TestValidateJacoYAML_RejectsUnknownACME(t *testing.T) {
	err := ValidateJacoYAMLBytes([]byte(`
deployment: d
acme: maybe
services:
  - name: web
    replicas: 1
`))
	if err == nil {
		t.Fatalf("expected rejection of acme: maybe")
	}
}

// TestMergeServiceDefaults_ComposeReplicasFlowThrough — issue #99 acceptance
// criterion: compose `deploy.replicas: 3` flows through when JACO omits
// replicas.
func TestMergeServiceDefaults_ComposeReplicasFlowThrough(t *testing.T) {
	project, err := compose.LoadBytes([]byte(`services:
  web:
    image: nginx:1.27
    deploy:
      replicas: 3
`), "x.yml")
	if err != nil {
		t.Fatalf("compose.LoadBytes: %v", err)
	}
	jaco := &JacoYAML{
		Deployment: "sample",
		Services:   []JacoServiceDecl{{Name: "web", Placement: "spread"}}, // no replicas override
	}
	specs, err := MergeServiceDefaults(jaco, project)
	if err != nil {
		t.Fatalf("MergeServiceDefaults: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("specs len = %d, want 1", len(specs))
	}
	if got := specs[0].GetReplicas(); got != 3 {
		t.Errorf("ServiceSpec.Replicas = %d, want 3 (from compose deploy.replicas)", got)
	}
}

// TestMergeServiceDefaults_JacoReplicasOverrideCompose — JACO override
// wins over compose's deploy.replicas, including an explicit zero (which
// is documented as legal and distinct from "unset").
func TestMergeServiceDefaults_JacoReplicasOverrideCompose(t *testing.T) {
	project, err := compose.LoadBytes([]byte(`services:
  web:
    image: nginx:1.27
    deploy:
      replicas: 3
`), "x.yml")
	if err != nil {
		t.Fatalf("compose.LoadBytes: %v", err)
	}
	cases := []struct {
		name     string
		override int
	}{
		{"override to 5", 5},
		{"override to 0", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			jaco := &JacoYAML{
				Deployment: "sample",
				Services:   []JacoServiceDecl{{Name: "web", Placement: "spread", Replicas: intPtr(c.override)}},
			}
			specs, err := MergeServiceDefaults(jaco, project)
			if err != nil {
				t.Fatalf("MergeServiceDefaults: %v", err)
			}
			if got := specs[0].GetReplicas(); got != int32(c.override) {
				t.Errorf("ServiceSpec.Replicas = %d, want %d (JACO override wins)", got, c.override)
			}
		})
	}
}

// TestMergeServiceDefaults_DefaultReplicasIsOne — no JACO override, no
// compose deploy.replicas: the default is one replica.
func TestMergeServiceDefaults_DefaultReplicasIsOne(t *testing.T) {
	project, err := compose.LoadBytes([]byte(`services:
  web:
    image: nginx:1.27
`), "x.yml")
	if err != nil {
		t.Fatalf("compose.LoadBytes: %v", err)
	}
	jaco := &JacoYAML{Deployment: "sample"}
	specs, err := MergeServiceDefaults(jaco, project)
	if err != nil {
		t.Fatalf("MergeServiceDefaults: %v", err)
	}
	if got := specs[0].GetReplicas(); got != 1 {
		t.Errorf("ServiceSpec.Replicas = %d, want 1 (default)", got)
	}
}

// TestMergeServiceDefaults_ComposeOnlyServiceAppears — issue #99: a compose
// service with no JACO entry still produces a ServiceSpec with
// placement=spread and the compose-derived networks/replicas.
func TestMergeServiceDefaults_ComposeOnlyServiceAppears(t *testing.T) {
	project, err := compose.LoadBytes([]byte(`services:
  worker:
    image: busybox:1.36
    networks: [backplane]
networks:
  backplane: {}
`), "x.yml")
	if err != nil {
		t.Fatalf("compose.LoadBytes: %v", err)
	}
	jaco := &JacoYAML{Deployment: "sample"}
	specs, err := MergeServiceDefaults(jaco, project)
	if err != nil {
		t.Fatalf("MergeServiceDefaults: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("specs len = %d, want 1", len(specs))
	}
	s := specs[0]
	if s.GetName() != "worker" {
		t.Errorf("Name = %q, want worker", s.GetName())
	}
	if s.GetPlacement() != pb.ServiceSpec_PLACEMENT_MODE_SPREAD {
		t.Errorf("Placement = %v, want SPREAD", s.GetPlacement())
	}
	if s.GetReplicas() != 1 {
		t.Errorf("Replicas = %d, want 1", s.GetReplicas())
	}
	if got, want := s.GetNetworks(), []string{"backplane"}; !equalStrings(got, want) {
		t.Errorf("Networks = %v, want %v (compose default)", got, want)
	}
}

// TestMergeServiceDefaults_JacoNetworksOverrideCompose — when JACO declares
// `networks:`, it wins over the compose service's networks (issue #99: per-
// service networks are no longer double-declared; compose is the default).
func TestMergeServiceDefaults_JacoNetworksOverrideCompose(t *testing.T) {
	project, err := compose.LoadBytes([]byte(`services:
  web:
    image: nginx:1.27
    networks: [frontend]
networks:
  frontend: {}
  backend: {}
`), "x.yml")
	if err != nil {
		t.Fatalf("compose.LoadBytes: %v", err)
	}
	jaco := &JacoYAML{
		Deployment: "sample",
		Services: []JacoServiceDecl{
			{Name: "web", Placement: "spread", Networks: []string{"backend"}},
		},
	}
	specs, err := MergeServiceDefaults(jaco, project)
	if err != nil {
		t.Fatalf("MergeServiceDefaults: %v", err)
	}
	if got, want := specs[0].GetNetworks(), []string{"backend"}; !equalStrings(got, want) {
		t.Errorf("Networks = %v, want %v (JACO override)", got, want)
	}
}

// TestMergeServiceDefaults_ComposeNetworksHonored — when JACO leaves
// `networks:` unset, the compose-declared per-service networks are used
// (sorted alphabetically for determinism).
func TestMergeServiceDefaults_ComposeNetworksHonored(t *testing.T) {
	project, err := compose.LoadBytes([]byte(`services:
  gateway:
    image: nginx:1.27
    networks: [frontend, backend]
networks:
  frontend: {}
  backend: {}
`), "x.yml")
	if err != nil {
		t.Fatalf("compose.LoadBytes: %v", err)
	}
	jaco := &JacoYAML{
		Deployment: "sample",
		Services:   []JacoServiceDecl{{Name: "gateway", Placement: "spread"}}, // no networks
	}
	specs, err := MergeServiceDefaults(jaco, project)
	if err != nil {
		t.Fatalf("MergeServiceDefaults: %v", err)
	}
	if got, want := specs[0].GetNetworks(), []string{"backend", "frontend"}; !equalStrings(got, want) {
		t.Errorf("Networks = %v, want %v (compose, sorted)", got, want)
	}
}

// TestMergeServiceDefaults_SortedByName — output is sorted by service name
// so the raft command + scheduler observe a deterministic order.
func TestMergeServiceDefaults_SortedByName(t *testing.T) {
	project, err := compose.LoadBytes([]byte(`services:
  zeta:
    image: nginx:1.27
  alpha:
    image: nginx:1.27
  middle:
    image: nginx:1.27
`), "x.yml")
	if err != nil {
		t.Fatalf("compose.LoadBytes: %v", err)
	}
	jaco := &JacoYAML{Deployment: "sample"}
	specs, err := MergeServiceDefaults(jaco, project)
	if err != nil {
		t.Fatalf("MergeServiceDefaults: %v", err)
	}
	got := []string{specs[0].GetName(), specs[1].GetName(), specs[2].GetName()}
	want := []string{"alpha", "middle", "zeta"}
	if !equalStrings(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// TestMergeServiceDefaults_JacoUnknownComposeService — defense-in-depth:
// if a JACO override names a service the compose file doesn't declare,
// MergeServiceDefaults returns an error (deploy.go runs the same check
// earlier and is the user-visible source of truth).
func TestMergeServiceDefaults_JacoUnknownComposeService(t *testing.T) {
	project, err := compose.LoadBytes([]byte(`services:
  web:
    image: nginx:1.27
`), "x.yml")
	if err != nil {
		t.Fatalf("compose.LoadBytes: %v", err)
	}
	jaco := &JacoYAML{
		Deployment: "sample",
		Services:   []JacoServiceDecl{{Name: "ghost", Placement: "spread"}},
	}
	if _, err := MergeServiceDefaults(jaco, project); err == nil {
		t.Fatal("MergeServiceDefaults accepted a JACO entry with no matching compose service")
	}
}

// TestMergeServiceDefaults_PlacementOverride — JACO `placement:` overrides
// the default spread; hosts list flows through.
func TestMergeServiceDefaults_PlacementOverride(t *testing.T) {
	project, err := compose.LoadBytes([]byte(`services:
  db:
    image: postgres:16
`), "x.yml")
	if err != nil {
		t.Fatalf("compose.LoadBytes: %v", err)
	}
	jaco := &JacoYAML{
		Deployment: "data",
		Services: []JacoServiceDecl{{
			Name: "db", Placement: "hosts", Hosts: []string{"storage-1"}, Replicas: intPtr(1),
		}},
	}
	specs, err := MergeServiceDefaults(jaco, project)
	if err != nil {
		t.Fatalf("MergeServiceDefaults: %v", err)
	}
	s := specs[0]
	if s.GetPlacement() != pb.ServiceSpec_PLACEMENT_MODE_HOSTS {
		t.Errorf("Placement = %v, want HOSTS", s.GetPlacement())
	}
	if got, want := s.GetHosts(), []string{"storage-1"}; !equalStrings(got, want) {
		t.Errorf("Hosts = %v, want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
