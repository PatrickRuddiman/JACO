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
// compose_service must be rejected with the documented error.
func TestParseJacoYAML_RejectsComposeServiceField(t *testing.T) {
	input := []byte(`deployment: d
services:
  - name: web
    replicas: 1
    compose_service: web
`)
	_, err := ParseJacoYAML(input)
	if err == nil {
		t.Fatal("expected error for compose_service field; got nil")
	}
	const want = "compose_service is no longer supported"
	if !contains(err.Error(), want) {
		t.Errorf("error = %q; want %q substring", err.Error(), want)
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
