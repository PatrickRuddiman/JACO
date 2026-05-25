package compose_test

import (
	"errors"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

func TestValidateReservedPort(t *testing.T) {
	cases := []struct {
		name     string
		yaml     string
		wantPort string // non-empty ⇒ expect a reserved_port error naming this port
	}{
		{
			name: "short 80:80 rejected",
			yaml: `services:
  web:
    image: nginx
    ports:
      - "80:80"
`,
			wantPort: "80",
		},
		{
			name: "short 443:443 rejected",
			yaml: `services:
  web:
    image: nginx
    ports:
      - "443:443"
`,
			wantPort: "443",
		},
		{
			name: "long-form published 80 rejected",
			yaml: `services:
  web:
    image: nginx
    ports:
      - published: 80
        target: 80
`,
			wantPort: "80",
		},
		{
			name: "published range covering 80 rejected",
			yaml: `services:
  web:
    image: nginx
    ports:
      - "79-81:79-81"
`,
			wantPort: "80",
		},
		{
			name: "host-IP-scoped published 443 rejected",
			yaml: `services:
  web:
    image: nginx
    ports:
      - "127.0.0.1:443:8443"
`,
			wantPort: "443",
		},
		{
			name: "container-side 80 is fine",
			yaml: `services:
  web:
    image: nginx
    ports:
      - "8080:80"
`,
		},
		{
			name: "bare container port 80 is fine",
			yaml: `services:
  web:
    image: nginx
    ports:
      - "80"
`,
		},
		{
			name: "ordinary published port is fine",
			yaml: `services:
  db:
    image: postgres
    ports:
      - "5432:5432"
`,
		},
		{
			name: "published range not covering reserved is fine",
			yaml: `services:
  app:
    image: app
    ports:
      - "8000-8100:8000-8100"
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := compose.Validate([]byte(tc.yaml))
			if tc.wantPort == "" {
				if err != nil {
					t.Fatalf("Validate: unexpected error: %v", err)
				}
				return
			}
			var ve *compose.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err is not *ValidationError: %T %v", err, err)
			}
			if ve.Code != "reserved_port" {
				t.Errorf("Code = %q, want reserved_port", ve.Code)
			}
			if ve.Details["port"] != tc.wantPort {
				t.Errorf("Details[port] = %q, want %q", ve.Details["port"], tc.wantPort)
			}
			if ve.Details["service"] == "" || ve.Details["entry"] == "" {
				t.Errorf("Details missing service/entry: %+v", ve.Details)
			}
		})
	}
}
