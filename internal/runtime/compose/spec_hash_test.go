package compose_test

import (
	"bytes"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

// TestServiceSpecHash_EnvValueChangeFlipsHash pins the load-bearing
// invariant for issue #148: any change in env VALUE produces a different
// hash. This is exactly what the scheduler needs to detect drift —
// pre-#148 the scheduler only compared (Host, Image) and missed env-only
// edits entirely.
func TestServiceSpecHash_EnvValueChangeFlipsHash(t *testing.T) {
	before := []byte(`services:
  api:
    image: api:1
    environment:
      DB_PASS: hunter2
`)
	after := []byte(`services:
  api:
    image: api:1
    environment:
      DB_PASS: hunter3
`)
	a, err := compose.ServiceSpecHash(before, "api")
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	b, err := compose.ServiceSpecHash(after, "api")
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Errorf("env-value change did not change hash; both = %x", a)
	}
}

// TestServiceSpecHash_StableUnderCosmeticEdits asserts comments and
// whitespace don't trigger recreation. The hash is computed from the
// yaml.v3 round-trip, which strips comments. This keeps the operator from
// suffering a full stack recreate just for adding a `# why` to the file.
func TestServiceSpecHash_StableUnderCosmeticEdits(t *testing.T) {
	a := []byte(`services:
  api:
    image: api:1
    environment:
      DB_PASS: hunter2
`)
	b := []byte(`# operator comment
services:
  api:
    # explain what this service does
    image: api:1

    environment:
      DB_PASS: hunter2 # the password
`)
	hashA, err := compose.ServiceSpecHash(a, "api")
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	hashB, err := compose.ServiceSpecHash(b, "api")
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if !bytes.Equal(hashA, hashB) {
		t.Errorf("cosmetic edit changed hash; a=%x b=%x", hashA, hashB)
	}
}

// TestServiceSpecHash_HealthcheckChangeFlipsHash pins the "structural
// change beyond env" half of #148: edits the structural comparator
// (Host/Image-only) missed today MUST flip the hash. Healthcheck command
// is the canonical example.
func TestServiceSpecHash_HealthcheckChangeFlipsHash(t *testing.T) {
	before := []byte(`services:
  api:
    image: api:1
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost/healthz"]
`)
	after := []byte(`services:
  api:
    image: api:1
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost/readyz"]
`)
	a, err := compose.ServiceSpecHash(before, "api")
	if err != nil {
		t.Fatal(err)
	}
	b, err := compose.ServiceSpecHash(after, "api")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Errorf("healthcheck change did not change hash; both = %x", a)
	}
}

// TestServiceSpecHash_PerServiceIsolation asserts editing service B does
// not flip service A's hash. Without isolation, a single env edit to ANY
// service would recreate every replica in the deployment — defeating the
// rolling-deploy mechanic.
func TestServiceSpecHash_PerServiceIsolation(t *testing.T) {
	before := []byte(`services:
  api:
    image: api:1
    environment:
      DB_PASS: hunter2
  worker:
    image: worker:1
    environment:
      QUEUE: jobs
`)
	after := []byte(`services:
  api:
    image: api:1
    environment:
      DB_PASS: hunter2
  worker:
    image: worker:1
    environment:
      QUEUE: priority
`)
	apiBefore, _ := compose.ServiceSpecHash(before, "api")
	apiAfter, _ := compose.ServiceSpecHash(after, "api")
	if !bytes.Equal(apiBefore, apiAfter) {
		t.Errorf("api hash flipped despite api being unchanged; before=%x after=%x", apiBefore, apiAfter)
	}
	workerBefore, _ := compose.ServiceSpecHash(before, "worker")
	workerAfter, _ := compose.ServiceSpecHash(after, "worker")
	if bytes.Equal(workerBefore, workerAfter) {
		t.Errorf("worker hash unchanged despite worker.QUEUE edit; both = %x", workerBefore)
	}
}

// TestServiceSpecHash_MissingService surfaces a clean error for a service
// name not in the compose document (programming bug; caller misused).
func TestServiceSpecHash_MissingService(t *testing.T) {
	body := []byte(`services:
  api:
    image: api:1
`)
	_, err := compose.ServiceSpecHash(body, "ghost")
	if err == nil {
		t.Fatalf("expected error for missing service")
	}
}
