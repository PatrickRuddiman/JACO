package firewall

import (
	"strings"
	"testing"
)

func TestRender_RejectsIdentifierCollision(t *testing.T) {
	in := RuleInput{Subnets: []Subnet{
		{Deployment: "demo", Network: "private-net", CIDR: "10.244.1.0/24"},
		{Deployment: "demo", Network: "private.net", CIDR: "10.244.2.0/24"},
	}}
	out, err := render(in, func(string, string) string { return "dep_net_collision" })
	if err == nil {
		t.Fatal("colliding identifiers must fail instead of merging scopes")
	}
	if out != "" {
		t.Fatalf("collision returned an applicable partial ruleset: %s", out)
	}
	for _, detail := range []string{"collision", "dep_net_collision", "demo", "private-net", "private.net"} {
		if !strings.Contains(err.Error(), detail) {
			t.Errorf("collision error %q omits %q", err, detail)
		}
	}
}
