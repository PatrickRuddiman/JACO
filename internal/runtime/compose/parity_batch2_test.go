package compose_test

import (
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

// TestValidate_AcceptsBatch2Fields — issues #115, #116, #120: the new
// service-level keys must all be accepted by the closed-field validator.
// One sub-test per field so a regression names the exact key that broke.
func TestValidate_AcceptsBatch2Fields(t *testing.T) {
	cases := map[string]string{
		// #115 host devices
		"devices": "    devices: [\"/dev/fuse:/dev/fuse:rwm\"]\n",
		// #116 modern GPUs (long form; `gpus: all` is the short form,
		// covered by TestValidate_AcceptsGpusAll below).
		"gpus": "    gpus:\n      - capabilities: [\"gpu\"]\n        count: 1\n",
		// #120 pull strategies — one per accepted enum value
		"pull_policy_always":  "    pull_policy: always\n",
		"pull_policy_missing": "    pull_policy: missing\n",
		"pull_policy_never":   "    pull_policy: never\n",
		"pull_policy_build":   "    pull_policy: build\n",
	}
	for field, line := range cases {
		t.Run(field, func(t *testing.T) {
			body := []byte("services:\n  web:\n    image: nginx:1.27\n" + line + "networks:\n  default: {}\n")
			if err := compose.Validate(body); err != nil {
				t.Fatalf("Validate(%s): unexpected err: %v", field, err)
			}
		})
	}
}

// TestValidate_AcceptsGpusAll — `gpus: all` is the short-form compose-spec
// shorthand for "every GPU on the host"; the validator must accept it
// (compose-go expands it into a single DeviceRequest at load time).
func TestValidate_AcceptsGpusAll(t *testing.T) {
	body := []byte("services:\n  web:\n    image: nginx:1.27\n    gpus: all\nnetworks:\n  default: {}\n")
	if err := compose.Validate(body); err != nil {
		t.Fatalf("Validate(gpus: all): unexpected err: %v", err)
	}
}

// TestValidate_RejectsUnsupportedPullPolicy — the validator must refuse
// `pull_policy: daily` (and weekly) explicitly so an operator who sets it
// expecting compose's full enum sees a typed error instead of silent
// fallback to JACO's default.
func TestValidate_RejectsUnsupportedPullPolicy(t *testing.T) {
	for _, value := range []string{"daily", "weekly", "never_ever"} {
		t.Run(value, func(t *testing.T) {
			body := []byte("services:\n  web:\n    image: nginx:1.27\n    pull_policy: " + value + "\nnetworks:\n  default: {}\n")
			err := compose.Validate(body)
			if err == nil {
				t.Fatalf("Validate(pull_policy: %s): expected error, got nil", value)
			}
			ve, ok := err.(*compose.ValidationError)
			if !ok {
				t.Fatalf("Validate: want *ValidationError, got %T: %v", err, err)
			}
			if ve.Code != "validation_failed" {
				t.Errorf("Code = %q, want validation_failed", ve.Code)
			}
			if !strings.Contains(ve.Message, "pull_policy") {
				t.Errorf("Message %q does not mention pull_policy", ve.Message)
			}
			if ve.Details["value"] != value {
				t.Errorf("Details[value] = %q, want %q", ve.Details["value"], value)
			}
		})
	}
}

// TestToContainerSpec_ProjectsDevices — short and long form host-device
// bindings flow into ContainerSpec.Devices verbatim (issue #115).
func TestToContainerSpec_ProjectsDevices(t *testing.T) {
	svc := types.ServiceConfig{
		Image: "nginx:1.27",
		Devices: []types.DeviceMapping{
			{Source: "/dev/fuse", Target: "/dev/fuse", Permissions: "rwm"},
			{Source: "/dev/snd", Target: "/dev/snd", Permissions: ""},
		},
	}
	spec := compose.ToContainerSpec(svc, compose.SpecOptions{
		ClusterID: "c", Deployment: "d", Service: "web", ReplicaID: "r", ReplicaIndex: 0,
	})
	if len(spec.Devices) != 2 {
		t.Fatalf("len(Devices) = %d, want 2", len(spec.Devices))
	}
	if spec.Devices[0] != (compose.DeviceMapping{Source: "/dev/fuse", Target: "/dev/fuse", Permissions: "rwm"}) {
		t.Errorf("Devices[0] = %+v", spec.Devices[0])
	}
	if spec.Devices[1].Permissions != "" {
		t.Errorf("Devices[1].Permissions = %q, want empty (docker default)", spec.Devices[1].Permissions)
	}
}

// TestToContainerSpec_ProjectsGPUs — both `gpus: all` (Count=-1) and the
// long form land on ContainerSpec.GPURequests (issue #116). Capabilities
// remain a flat AND list at this layer; the lifecycle layer is responsible
// for wrapping into docker's OR-of-AND form.
func TestToContainerSpec_ProjectsGPUs(t *testing.T) {
	svc := types.ServiceConfig{
		Image: "nvidia/cuda:12",
		Gpus: []types.DeviceRequest{
			{
				Driver:       "nvidia",
				Count:        types.DeviceCount(2),
				Capabilities: []string{"gpu", "compute"},
				IDs:          []string{"GPU-uuid-a", "GPU-uuid-b"},
				Options:      types.Mapping{"runtime": "nvidia"},
			},
		},
	}
	spec := compose.ToContainerSpec(svc, compose.SpecOptions{
		ClusterID: "c", Deployment: "d", Service: "web", ReplicaID: "r", ReplicaIndex: 0,
	})
	if len(spec.GPURequests) != 1 {
		t.Fatalf("len(GPURequests) = %d, want 1", len(spec.GPURequests))
	}
	got := spec.GPURequests[0]
	if got.Driver != "nvidia" || got.Count != 2 {
		t.Errorf("GPURequests[0] driver/count = %q/%d, want nvidia/2", got.Driver, got.Count)
	}
	if len(got.Capabilities) != 2 || got.Capabilities[0] != "gpu" || got.Capabilities[1] != "compute" {
		t.Errorf("Capabilities = %v, want [gpu compute]", got.Capabilities)
	}
	if len(got.DeviceIDs) != 2 || got.DeviceIDs[0] != "GPU-uuid-a" {
		t.Errorf("DeviceIDs = %v", got.DeviceIDs)
	}
	if got.Options["runtime"] != "nvidia" {
		t.Errorf("Options[runtime] = %q, want nvidia", got.Options["runtime"])
	}
}

// TestToContainerSpec_ProjectsPullPolicy — pull_policy lands on
// ContainerSpec.PullPolicy verbatim; the runtime decides the semantics.
func TestToContainerSpec_ProjectsPullPolicy(t *testing.T) {
	for _, p := range []string{"always", "missing", "never", "build", ""} {
		t.Run("p="+p, func(t *testing.T) {
			svc := types.ServiceConfig{Image: "nginx:1.27", PullPolicy: p}
			spec := compose.ToContainerSpec(svc, compose.SpecOptions{
				ClusterID: "c", Deployment: "d", Service: "web", ReplicaID: "r", ReplicaIndex: 0,
			})
			if spec.PullPolicy != p {
				t.Errorf("PullPolicy = %q, want %q", spec.PullPolicy, p)
			}
		})
	}
}

// TestToContainerSpec_Batch2ZeroValues — when compose declares no devices,
// gpus, or pull_policy, the projected spec carries nil/empty so the
// lifecycle builder emits docker's defaults.
func TestToContainerSpec_Batch2ZeroValues(t *testing.T) {
	spec := compose.ToContainerSpec(types.ServiceConfig{Image: "nginx:1.27"}, compose.SpecOptions{
		ClusterID: "c", Deployment: "d", Service: "web", ReplicaID: "r", ReplicaIndex: 0,
	})
	if spec.Devices != nil {
		t.Errorf("Devices = %v, want nil", spec.Devices)
	}
	if spec.GPURequests != nil {
		t.Errorf("GPURequests = %v, want nil", spec.GPURequests)
	}
	if spec.PullPolicy != "" {
		t.Errorf("PullPolicy = %q, want empty", spec.PullPolicy)
	}
}
