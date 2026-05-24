package grpcsrv

import (
	"testing"
)

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
			name: "no services",
			j:    &JacoYAML{Deployment: "d"},
			want: "at least one service is required",
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
				Services:   []JacoServiceDecl{{Name: "w", Replicas: -1, Placement: "spread"}},
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
			name: "route empty domain",
			j: &JacoYAML{
				Deployment: "d",
				Services:   []JacoServiceDecl{{Name: "w", Placement: "spread"}},
				Routes:     []JacoRouteDecl{{Service: "w", Port: 80, TLS: "auto"}},
			},
			want: "route domain is required",
		},
		{
			name: "route references unknown service",
			j: &JacoYAML{
				Deployment: "d",
				Services:   []JacoServiceDecl{{Name: "w", Placement: "spread"}},
				Routes:     []JacoRouteDecl{{Domain: "a", Service: "ghost", Port: 80, TLS: "auto"}},
			},
			want: "references unknown service",
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
			{Name: "w", Placement: "spread", Replicas: 2},
			{Name: "api", Placement: "hosts", Hosts: []string{"host-a"}, Replicas: 1},
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
}

// TestValidationFault_Error — formatter for the validationFault type
// surfaces its Message verbatim.
func TestValidationFault_Error(t *testing.T) {
	if got := (&validationFault{Code: "c", Message: "m"}).Error(); got != "m" {
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
