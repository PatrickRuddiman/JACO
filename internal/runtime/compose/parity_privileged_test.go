package compose_test

import (
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

// TestValidate_AcceptsPrivilegedWithLabel — issue #119: a service that opts
// into `privileged:` or `security_opt:` MUST also carry the matching
// `jaco.io/allow-privileged: "true"` label. With the label the validator
// passes; both the map-form and list-form label syntax are accepted.
func TestValidate_AcceptsPrivilegedWithLabel(t *testing.T) {
	cases := map[string]string{
		"privileged_map_labels": `services:
  probe:
    image: nginx:1.27
    privileged: true
    labels:
      jaco.io/allow-privileged: "true"
networks:
  default: {}
`,
		"security_opt_map_labels": `services:
  probe:
    image: nginx:1.27
    security_opt:
      - seccomp=unconfined
    labels:
      jaco.io/allow-privileged: "true"
networks:
  default: {}
`,
		"both_fields_list_labels": `services:
  probe:
    image: nginx:1.27
    privileged: true
    security_opt:
      - seccomp=unconfined
      - apparmor=unconfined
    labels:
      - jaco.io/allow-privileged=true
networks:
  default: {}
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if err := compose.Validate([]byte(body)); err != nil {
				t.Fatalf("Validate(%s): unexpected err: %v", name, err)
			}
		})
	}
}

// TestValidate_RejectsPrivilegedWithoutLabel — issue #119: omitting the
// label, providing the wrong value, or putting it on the wrong service all
// reject with a typed validation_failed error that names the offending
// service and the gated fields.
func TestValidate_RejectsPrivilegedWithoutLabel(t *testing.T) {
	cases := map[string]struct {
		body       string
		wantFields string // expected Details["fields"]
	}{
		"privileged_no_label": {
			body: `services:
  probe:
    image: nginx:1.27
    privileged: true
networks:
  default: {}
`,
			wantFields: "privileged",
		},
		"security_opt_no_label": {
			body: `services:
  probe:
    image: nginx:1.27
    security_opt:
      - seccomp=unconfined
networks:
  default: {}
`,
			wantFields: "security_opt",
		},
		"both_no_label": {
			body: `services:
  probe:
    image: nginx:1.27
    privileged: true
    security_opt:
      - seccomp=unconfined
networks:
  default: {}
`,
			wantFields: "privileged,security_opt",
		},
		"wrong_label_value": {
			body: `services:
  probe:
    image: nginx:1.27
    privileged: true
    labels:
      jaco.io/allow-privileged: "yes"
networks:
  default: {}
`,
			wantFields: "privileged",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := compose.Validate([]byte(tc.body))
			if err == nil {
				t.Fatalf("Validate(%s): want error, got nil", name)
			}
			ve, ok := err.(*compose.ValidationError)
			if !ok {
				t.Fatalf("err type = %T, want *compose.ValidationError", err)
			}
			if ve.Code != "validation_failed" {
				t.Errorf("Code = %q, want validation_failed", ve.Code)
			}
			if ve.Details["service"] != "probe" {
				t.Errorf("Details[service] = %q, want probe", ve.Details["service"])
			}
			if ve.Details["fields"] != tc.wantFields {
				t.Errorf("Details[fields] = %q, want %q", ve.Details["fields"], tc.wantFields)
			}
			if !strings.Contains(ve.Message, "jaco.io/allow-privileged") {
				t.Errorf("Message %q does not mention jaco.io/allow-privileged", ve.Message)
			}
		})
	}
}

// TestValidate_PrivilegedZeroValueNoRegression — services that set neither
// `privileged:` nor `security_opt:` MUST pass even without the label. Guards
// against an accidental "always require the label" regression.
func TestValidate_PrivilegedZeroValueNoRegression(t *testing.T) {
	body := []byte(`services:
  web:
    image: nginx:1.27
    labels:
      app: web
networks:
  default: {}
`)
	if err := compose.Validate(body); err != nil {
		t.Fatalf("Validate: unexpected err: %v", err)
	}
}

// TestToContainerSpec_ProjectsPrivileged — `Privileged` and `SecurityOpt`
// fall through into ContainerSpec verbatim so the lifecycle layer can
// forward them to docker (#119).
func TestToContainerSpec_ProjectsPrivileged(t *testing.T) {
	svc := types.ServiceConfig{
		Image:       "nginx:1.27",
		Privileged:  true,
		SecurityOpt: []string{"seccomp=unconfined", "apparmor=unconfined"},
	}
	spec := compose.ToContainerSpec(svc, compose.SpecOptions{
		ClusterID: "c", Deployment: "d", Service: "probe", ReplicaID: "r", ReplicaIndex: 0,
	})
	if !spec.Privileged {
		t.Errorf("Privileged = false, want true")
	}
	if len(spec.SecurityOpt) != 2 {
		t.Fatalf("len(SecurityOpt) = %d, want 2", len(spec.SecurityOpt))
	}
	if spec.SecurityOpt[0] != "seccomp=unconfined" {
		t.Errorf("SecurityOpt[0] = %q", spec.SecurityOpt[0])
	}
	if spec.SecurityOpt[1] != "apparmor=unconfined" {
		t.Errorf("SecurityOpt[1] = %q", spec.SecurityOpt[1])
	}
}

// TestToContainerSpec_PrivilegedZeroValuesStayZero — when compose declares
// neither field, the projected spec carries zero/nil so the lifecycle
// builder emits docker's defaults (no --privileged, no --security-opt).
func TestToContainerSpec_PrivilegedZeroValuesStayZero(t *testing.T) {
	spec := compose.ToContainerSpec(types.ServiceConfig{Image: "nginx:1.27"}, compose.SpecOptions{
		ClusterID: "c", Deployment: "d", Service: "web", ReplicaID: "r", ReplicaIndex: 0,
	})
	if spec.Privileged {
		t.Errorf("Privileged = true, want false")
	}
	if spec.SecurityOpt != nil {
		t.Errorf("SecurityOpt = %v, want nil", spec.SecurityOpt)
	}
}
