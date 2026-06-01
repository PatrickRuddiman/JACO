package compose_test

import (
	"reflect"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

// TestToContainerSpec_DependsOnListForm — issue #130. compose-go normalises
// list-form `depends_on: [api]` into the same map[string]ServiceDependency
// shape as long-form, with Condition empty. ToContainerSpec must normalise
// the empty condition to service_started so the reconciler gate compares
// against a single source of truth.
func TestToContainerSpec_DependsOnListForm(t *testing.T) {
	svc := types.ServiceConfig{
		Name:  "web",
		Image: "nginx:1.27",
		DependsOn: types.DependsOnConfig{
			"api": {Required: true}, // empty Condition (list form default)
		},
	}
	spec := compose.ToContainerSpec(svc, compose.SpecOptions{Deployment: "stack", Service: "web"})
	want := []compose.Dependency{{Service: "api", Condition: compose.DependencyConditionStarted, Required: true}}
	if !reflect.DeepEqual(spec.DependsOn, want) {
		t.Fatalf("DependsOn = %#v, want %#v", spec.DependsOn, want)
	}
}

// TestToContainerSpec_DependsOnLongForm — issue #130. Explicit
// `condition: service_healthy` and `required: false` are projected
// verbatim, with output sorted by service name for determinism.
func TestToContainerSpec_DependsOnLongForm(t *testing.T) {
	svc := types.ServiceConfig{
		Name:  "web",
		Image: "nginx:1.27",
		DependsOn: types.DependsOnConfig{
			"redis": {Condition: compose.DependencyConditionHealthy, Required: true},
			"api":   {Condition: compose.DependencyConditionStarted, Required: false},
		},
	}
	spec := compose.ToContainerSpec(svc, compose.SpecOptions{Deployment: "stack", Service: "web"})
	want := []compose.Dependency{
		{Service: "api", Condition: compose.DependencyConditionStarted, Required: false},
		{Service: "redis", Condition: compose.DependencyConditionHealthy, Required: true},
	}
	if !reflect.DeepEqual(spec.DependsOn, want) {
		t.Fatalf("DependsOn = %#v, want %#v", spec.DependsOn, want)
	}
}

// TestToContainerSpec_DependsOnEmpty — no depends_on declared → spec
// carries nil so the reconciler gate short-circuits without allocating.
func TestToContainerSpec_DependsOnEmpty(t *testing.T) {
	svc := types.ServiceConfig{Name: "web", Image: "nginx:1.27"}
	spec := compose.ToContainerSpec(svc, compose.SpecOptions{Deployment: "stack", Service: "web"})
	if spec.DependsOn != nil {
		t.Fatalf("DependsOn = %#v, want nil", spec.DependsOn)
	}
}
