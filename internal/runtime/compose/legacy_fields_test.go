package compose_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

// TestLoadBytes_TranslatesLegacyComposeFields — issue #122: every v1/v2
// legacy service key the modern compose spec dropped must surface as a
// typed ValidationError with code "legacy_compose_field" and a Details
// map naming the modern equivalent, rather than compose-go's opaque
// "compose load: ... additional properties 'X' not allowed".
func TestLoadBytes_TranslatesLegacyComposeFields(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		field    string
		modernEq string // substring of Details["modern_equivalent"]
	}{
		{
			name:     "log_driver",
			body:     "services:\n  web:\n    image: nginx\n    log_driver: json-file\n",
			field:    "log_driver",
			modernEq: "logging.driver",
		},
		{
			name:     "log_opt",
			body:     "services:\n  web:\n    image: nginx\n    log_opt:\n      max-size: 10m\n",
			field:    "log_opt",
			modernEq: "logging.options",
		},
		{
			name:     "net",
			body:     "services:\n  web:\n    image: nginx\n    net: bridge\n",
			field:    "net",
			modernEq: "network_mode",
		},
		{
			name:     "volume_driver",
			body:     "services:\n  web:\n    image: nginx\n    volume_driver: local\n",
			field:    "volume_driver",
			modernEq: "long-form",
		},
		{
			name:     "dockerfile",
			body:     "services:\n  web:\n    image: nginx\n    dockerfile: foo\n",
			field:    "dockerfile",
			modernEq: "build.dockerfile",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compose.LoadBytes([]byte(tc.body), "test.yml")
			if err == nil {
				t.Fatalf("expected typed legacy_compose_field error, got nil")
			}
			var ve *compose.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err is not ValidationError: %T %v", err, err)
			}
			if ve.Code != "legacy_compose_field" {
				t.Errorf("code = %q, want legacy_compose_field", ve.Code)
			}
			if ve.Details["field"] != tc.field {
				t.Errorf("details.field = %q, want %q", ve.Details["field"], tc.field)
			}
			if !strings.Contains(ve.Details["modern_equivalent"], tc.modernEq) {
				t.Errorf("details.modern_equivalent = %q, want substring %q",
					ve.Details["modern_equivalent"], tc.modernEq)
			}
		})
	}
}

// TestLoadBytes_NonLegacyUnknownFieldStaysGenericComposeLoad — a non-legacy
// unknown field (i.e. a typo) must still surface as the generic
// "compose load: ..." wrap so we don't accidentally claim every unknown
// field is a v1/v2 spelling.
func TestLoadBytes_NonLegacyUnknownFieldStaysGenericComposeLoad(t *testing.T) {
	body := "services:\n  web:\n    image: nginx\n    not_a_real_compose_field: yes\n"
	_, err := compose.LoadBytes([]byte(body), "test.yml")
	if err == nil {
		t.Fatalf("expected compose load error, got nil")
	}
	var ve *compose.ValidationError
	if errors.As(err, &ve) && ve.Code == "legacy_compose_field" {
		t.Errorf("non-legacy unknown field misclassified as legacy_compose_field: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "compose load:") {
		t.Errorf("err = %q, want 'compose load:' prefix", err.Error())
	}
}
