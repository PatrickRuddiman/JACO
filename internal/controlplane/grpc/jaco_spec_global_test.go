package grpcsrv

import "testing"

// placement: global must parse, validate, and map to the GLOBAL enum. A
// non-zero replicas alongside global is accepted (ignored), not rejected.
func TestJacoSpec_GlobalPlacementParsesAndMaps(t *testing.T) {
	const manifest = `deployment: sample
services:
  - name: web
    placement: global
    replicas: 5
`
	j, err := ParseJacoYAML([]byte(manifest))
	if err != nil {
		t.Fatalf("ParseJacoYAML(global) error = %v, want nil", err)
	}
	if got := j.Services[0].Placement; got != "global" {
		t.Fatalf("parsed placement = %q, want global", got)
	}

	if err := ValidateJacoYAMLBytes([]byte(manifest)); err != nil {
		t.Fatalf("ValidateJacoYAMLBytes(global+replicas) error = %v, want nil (replicas ignored, not rejected)", err)
	}

	if got := placementToProto("global"); got.String() != "PLACEMENT_MODE_GLOBAL" {
		t.Fatalf("placementToProto(global) = %v, want PLACEMENT_MODE_GLOBAL", got)
	}
}
