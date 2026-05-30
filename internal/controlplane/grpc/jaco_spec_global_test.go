package grpcsrv

import (
	"strings"
	"testing"
)

// placement: global must parse, validate, and map to the GLOBAL enum.
// Per issue #99, a non-nil `replicas` alongside `placement: global` is now
// rejected (mutually exclusive) — global runs one replica per ready node,
// so any explicit count is a likely authoring mistake. Omitting replicas
// is the supported form.
func TestJacoSpec_GlobalPlacementParsesAndMaps(t *testing.T) {
	const manifest = `deployment: sample
services:
  - name: web
    placement: global
`
	j, err := ParseJacoYAML([]byte(manifest))
	if err != nil {
		t.Fatalf("ParseJacoYAML(global) error = %v, want nil", err)
	}
	if got := j.Services[0].Placement; got != "global" {
		t.Fatalf("parsed placement = %q, want global", got)
	}

	if err := ValidateJacoYAMLBytes([]byte(manifest)); err != nil {
		t.Fatalf("ValidateJacoYAMLBytes(global) error = %v, want nil", err)
	}

	if got := placementToProto("global"); got.String() != "PLACEMENT_MODE_GLOBAL" {
		t.Fatalf("placementToProto(global) = %v, want PLACEMENT_MODE_GLOBAL", got)
	}
}

// TestJacoSpec_GlobalRejectsExplicitReplicas — issue #99: `placement: global`
// + explicit `replicas:` is rejected. Both fields are mutually exclusive.
func TestJacoSpec_GlobalRejectsExplicitReplicas(t *testing.T) {
	const manifest = `deployment: sample
services:
  - name: web
    placement: global
    replicas: 5
`
	err := ValidateJacoYAMLBytes([]byte(manifest))
	if err == nil {
		t.Fatal("ValidateJacoYAMLBytes accepted placement=global + replicas; want rejection")
	}
	if !strings.Contains(err.Error(), "placement=global") {
		t.Errorf("error = %q; want substring 'placement=global'", err.Error())
	}
}
