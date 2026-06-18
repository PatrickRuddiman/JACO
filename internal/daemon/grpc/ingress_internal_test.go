package grpc

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/discovery/bridge"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// seedHealthyTCPService wires the minimal state for one healthy replica of
// (deployment, service) on the default network with a known overlay IP, plus
// a TCPRoute publishing port on container 5432.
func seedHealthyTCPService(t *testing.T, st *state.State, deployment, service string, port int) {
	t.Helper()
	st.Deployments.Apply(&pb.Deployment{
		Name:     deployment,
		Services: []*pb.ServiceSpec{{Name: service, Networks: []string{"_default"}}},
	}, 1)
	id := deployment + "-" + service + "-0"
	st.ReplicasDesired.Apply(&pb.ReplicaDesired{Id: id, Deployment: deployment, Service: service}, 2)
	st.ReplicasObserved.Apply(&pb.ReplicaObserved{
		Id:           id,
		State:        pb.ReplicaState_REPLICA_STATE_RUNNING,
		LastHealthAt: timestamppb.Now(),
		Details:      map[string]string{"ip." + bridge.DockerNetworkName(deployment, "_default"): "10.244.7.2"},
	}, 3)
	st.TCPRoutes.Apply(&pb.TCPRoute{PublishedPort: int32(port), Deployment: deployment, Service: service, ContainerPort: 5432}, 4)
}

// TestIngressBuilder_EmitsTCPRoute: a TCPRoute with a healthy replica produces
// a layer4 server in the built config. (No bind-probe — caddy-l4 owns the
// listener and re-binding its own port on reload is idempotent.)
func TestIngressBuilder_EmitsTCPRoute(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	seedHealthyTCPService(t, st, "data", "db", 5432)

	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg, err := ingressBuilder(st, ingressACMEOpts{}, discard)()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !bytes.Contains(cfg, []byte("tcp_5432")) {
		t.Errorf("built config missing tcp_5432 server:\n%s", cfg)
	}
}

// TestShouldLoad guards the startup-vs-teardown gate: route-less configs are
// skipped only before caddy first starts; once running, they MUST load so a
// deleted route's listeners are torn down.
func TestShouldLoad(t *testing.T) {
	fallback := []byte(`{"apps":{"http":{"servers":{"jaco":{"routes":[{"handle":[{"handler":"static_response"}]}]}}}}}`)
	httpCfg := []byte(`{"apps":{"http":{"servers":{"jaco":{"routes":[{"handle":[{"handler":"reverse_proxy"}]}]}}}}}`)
	l4Cfg := []byte(`{"apps":{"layer4":{"servers":{"tcp_5432":{"listen":[":5432"]}}}}}`)
	cases := []struct {
		name    string
		started bool
		cfg     []byte
		want    bool
	}{
		{"startup + route-less -> skip", false, fallback, false},
		{"startup + http -> load", false, httpCfg, true},
		{"startup + layer4 -> load", false, l4Cfg, true},
		{"running + route-less -> load (teardown)", true, fallback, true},
		{"running + layer4 -> load", true, l4Cfg, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldLoad(tc.started, tc.cfg); got != tc.want {
				t.Errorf("shouldLoad(%v, ...) = %v, want %v", tc.started, got, tc.want)
			}
		})
	}
}

func TestConfigHasLoadableRoute(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		want bool
	}{
		{"fallback only", `{"apps":{"http":{"servers":{"jaco":{"routes":[{"handle":[{"handler":"static_response"}]}]}}}}}`, false},
		{"http reverse_proxy", `{"apps":{"http":{"servers":{"jaco":{"routes":[{"handle":[{"handler":"reverse_proxy"}]}]}}}}}`, true},
		{"layer4 only", `{"apps":{"layer4":{"servers":{"tcp_5432":{"listen":[":5432"]}}}}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := configHasLoadableRoute([]byte(tc.cfg)); got != tc.want {
				t.Errorf("configHasLoadableRoute = %v, want %v", got, tc.want)
			}
		})
	}
}

// stagingBlobKey / prodBlobKey build certmagic-style storage keys for a
// domain's leaf cert under the LE staging / prod CA namespaces, matching how
// loadStagingChain / prodCertIssued classify keys (staging keys contain
// "staging"; both contain "/<domain>/" and end ".crt").
func stagingBlobKey(domain string) string {
	return "certificates/acme-staging-v02.api.letsencrypt.org-directory/" + domain + "/" + domain + ".crt"
}
func prodBlobKey(domain string) string {
	return "certificates/acme-v02.api.letsencrypt.org-directory/" + domain + "/" + domain + ".crt"
}

// prodKeyBlobKey / stagingKeyBlobKey build the matching private-key (.key)
// storage keys, so tests can assert prodCertResourceComplete needs BOTH halves.
func prodKeyBlobKey(domain string) string {
	return "certificates/acme-v02.api.letsencrypt.org-directory/" + domain + "/" + domain + ".key"
}
func stagingKeyBlobKey(domain string) string {
	return "certificates/acme-staging-v02.api.letsencrypt.org-directory/" + domain + "/" + domain + ".key"
}

func tlsAutoRoute(domain string) *pb.Route {
	return &pb.Route{Domain: domain, Deployment: "s", Service: "web", Port: 80, TlsAuto: true}
}

// TestStagingDomainsFromState: only tls:auto domains with a staging blob and no
// prod blob are "in their staging window" — the set every node renders the
// staging policy for so it can serve the replicated staging leaf (issue #182).
func TestStagingDomainsFromState(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.Routes.Apply(tlsAutoRoute("staging-only.example.com"), 1)
	st.Routes.Apply(tlsAutoRoute("promoted.example.com"), 2)
	st.Routes.Apply(tlsAutoRoute("prod-only.example.com"), 3)
	st.Routes.Apply(tlsAutoRoute("no-cert.example.com"), 4)
	// A staging blob for a non-tls:auto domain must be ignored.
	st.Routes.Apply(&pb.Route{Domain: "manual.example.com", Deployment: "s", Service: "web", Port: 80, TlsAuto: false}, 5)

	st.CertBlobs.Apply(&pb.CertBlob{Key: stagingBlobKey("staging-only.example.com"), Value: []byte("x")}, 10)
	// promoted still has its (not-yet-GC'd) staging blob AND a prod blob -> excluded.
	st.CertBlobs.Apply(&pb.CertBlob{Key: stagingBlobKey("promoted.example.com"), Value: []byte("x")}, 11)
	st.CertBlobs.Apply(&pb.CertBlob{Key: prodBlobKey("promoted.example.com"), Value: []byte("x")}, 12)
	st.CertBlobs.Apply(&pb.CertBlob{Key: prodBlobKey("prod-only.example.com"), Value: []byte("x")}, 13)
	st.CertBlobs.Apply(&pb.CertBlob{Key: stagingBlobKey("manual.example.com"), Value: []byte("x")}, 14)

	got := stagingDomainsFromState(st)
	want := map[string]bool{"staging-only.example.com": true}
	if len(got) != len(want) {
		t.Fatalf("stagingDomainsFromState = %v, want %v", got, want)
	}
	for d := range want {
		if !got[d] {
			t.Errorf("stagingDomainsFromState missing %q: %v", d, got)
		}
	}
}

// TestStagingDomainsForBuilder: the builder set unions replicated state with the
// controller's in-flight in-memory set ONLY on the leader. A follower must
// ignore the controller set (it never runs the controller) and render purely
// from replicated state (issue #182).
func TestStagingDomainsForBuilder(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.Routes.Apply(tlsAutoRoute("from-state.example.com"), 1)
	st.CertBlobs.Apply(&pb.CertBlob{Key: stagingBlobKey("from-state.example.com"), Value: []byte("x")}, 2)

	// A brand-new domain the leader just marked for staging, before any blob
	// has landed in raft — only the leader knows about it.
	ctrlStaging := func() map[string]bool { return map[string]bool{"in-flight.example.com": true} }

	leader := stagingDomainsForBuilder(st, ctrlStaging, func() bool { return true })
	if !leader["from-state.example.com"] || !leader["in-flight.example.com"] || len(leader) != 2 {
		t.Errorf("leader set = %v, want both replicated + in-flight", leader)
	}

	follower := stagingDomainsForBuilder(st, ctrlStaging, func() bool { return false })
	if follower["in-flight.example.com"] {
		t.Errorf("follower must ignore the controller in-memory set: %v", follower)
	}
	if !follower["from-state.example.com"] || len(follower) != 1 {
		t.Errorf("follower set = %v, want only replicated state", follower)
	}
}

// TestReconcileStagingCache: a follower that observed a domain in its staging
// window evicts its cached staging leaf exactly once when the promotion
// replicates (staging blob cleared, prod blob landed), then stops tracking it
// (issue #182). EvictManaged is a no-op in tests (Caddy's cert cache isn't
// provisioned), so this asserts the once-per-promotion tracking semantics.
func TestReconcileStagingCache(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.Routes.Apply(tlsAutoRoute("web.example.com"), 1)
	stagingKey := stagingBlobKey("web.example.com")
	st.CertBlobs.Apply(&pb.CertBlob{Key: stagingKey, Value: []byte("staging")}, 2)

	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	seen := map[string]bool{}

	// In its staging window -> tracked, not yet promoted.
	s.reconcileStagingCache(st, seen, false)
	if !seen["web.example.com"] {
		t.Fatalf("expected staging domain tracked, seen=%v", seen)
	}

	// Promotion replicates: leader cleared the staging blob and a prod blob lands.
	st.CertBlobs.Remove(stagingKey, 3)
	st.CertBlobs.Apply(&pb.CertBlob{Key: prodBlobKey("web.example.com"), Value: []byte("prod")}, 4)

	s.reconcileStagingCache(st, seen, false)
	if seen["web.example.com"] {
		t.Errorf("expected promoted domain pruned from tracking, seen=%v", seen)
	}
}

// TestReconcileStagingCache_PrunesVanishedDomain: a tracked domain that leaves
// the staging set without a prod cert (e.g. its tls:auto route was removed) is
// pruned so the tracking map stays bounded.
func TestReconcileStagingCache_PrunesVanishedDomain(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.Routes.Apply(tlsAutoRoute("gone.example.com"), 1)
	stagingKey := stagingBlobKey("gone.example.com")
	st.CertBlobs.Apply(&pb.CertBlob{Key: stagingKey, Value: []byte("staging")}, 2)

	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	seen := map[string]bool{}
	s.reconcileStagingCache(st, seen, false)
	if !seen["gone.example.com"] {
		t.Fatalf("expected tracked, seen=%v", seen)
	}

	// Route no longer tls:auto and the staging blob is gone, with no prod cert.
	st.Routes.Apply(&pb.Route{Domain: "gone.example.com", Deployment: "s", Service: "web", Port: 80, TlsAuto: false}, 3)
	st.CertBlobs.Remove(stagingKey, 4)
	s.reconcileStagingCache(st, seen, false)
	if seen["gone.example.com"] {
		t.Errorf("expected vanished domain pruned, seen=%v", seen)
	}
}

// makeCertPEM mints a throwaway self-signed leaf for cn and returns its PEM
// encoding plus the raw DER, so tests can store a certmagic-style .crt blob and
// assert prodLeafDER recovers exactly that leaf.
func makeCertPEM(t *testing.T, cn string) (pemBytes, der []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err = x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("createcert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), der
}

// TestProdCertResourceComplete: the follower's level-trigger only fires once
// BOTH halves of a non-staging resource (leaf .crt AND key .key) have
// replicated — the gate that keeps certmagic on the load path (never obtain).
func TestProdCertResourceComplete(t *testing.T) {
	st := state.New(watch.NewRegistry())

	// crt only -> incomplete (certmagic would try to OBTAIN -> never gate true).
	st.CertBlobs.Apply(&pb.CertBlob{Key: prodBlobKey("crt-only.example.com"), Value: []byte("x")}, 1)
	if prodCertResourceComplete(st, "crt-only.example.com") {
		t.Errorf("crt-only must be incomplete")
	}

	// crt + key -> complete.
	st.CertBlobs.Apply(&pb.CertBlob{Key: prodBlobKey("full.example.com"), Value: []byte("x")}, 2)
	st.CertBlobs.Apply(&pb.CertBlob{Key: prodKeyBlobKey("full.example.com"), Value: []byte("k")}, 3)
	if !prodCertResourceComplete(st, "full.example.com") {
		t.Errorf("crt+key must be complete")
	}

	// A staging crt+key must NOT count as a complete prod resource.
	st.CertBlobs.Apply(&pb.CertBlob{Key: stagingBlobKey("staging.example.com"), Value: []byte("x")}, 4)
	st.CertBlobs.Apply(&pb.CertBlob{Key: stagingKeyBlobKey("staging.example.com"), Value: []byte("k")}, 5)
	if prodCertResourceComplete(st, "staging.example.com") {
		t.Errorf("staging resource must not count as complete prod")
	}

	// No domain-segment overlap: a subdomain's resource must not satisfy the parent.
	st.CertBlobs.Apply(&pb.CertBlob{Key: prodBlobKey("sub.parent.example.com"), Value: []byte("x")}, 6)
	st.CertBlobs.Apply(&pb.CertBlob{Key: prodKeyBlobKey("sub.parent.example.com"), Value: []byte("k")}, 7)
	if prodCertResourceComplete(st, "parent.example.com") {
		t.Errorf("subdomain resource must not satisfy parent")
	}
}

// TestProdLeafDER: prodLeafDER returns the leaf (first CERTIFICATE block,
// certmagic stores chains leaf-first) of the non-staging .crt blob, ignoring
// any staging blob, so followerServesProdLeaf compares against the right cert.
func TestProdLeafDER(t *testing.T) {
	st := state.New(watch.NewRegistry())

	leafPEM, leafDER := makeCertPEM(t, "web.example.com")
	interPEM, _ := makeCertPEM(t, "intermediate")
	chain := append(append([]byte{}, leafPEM...), interPEM...)

	// A staging blob for the same domain must be ignored.
	stagingPEM, stagingDER := makeCertPEM(t, "web.example.com staging")
	st.CertBlobs.Apply(&pb.CertBlob{Key: stagingBlobKey("web.example.com"), Value: stagingPEM}, 1)
	st.CertBlobs.Apply(&pb.CertBlob{Key: prodBlobKey("web.example.com"), Value: chain}, 2)

	got := prodLeafDER(st, "web.example.com")
	if !bytes.Equal(got, leafDER) {
		t.Errorf("prodLeafDER returned the wrong cert (leaf mismatch)")
	}
	if bytes.Equal(got, stagingDER) {
		t.Errorf("prodLeafDER must not return the staging leaf")
	}

	// No prod blob -> nil.
	if prodLeafDER(st, "missing.example.com") != nil {
		t.Errorf("prodLeafDER for absent domain must be nil")
	}
}

// TestFollowerProdReloadTargets exercises the level-trigger core: a complete
// prod resource the cache doesn't yet serve is a reload target and is NOT
// latched (so it retries); once the cache serves it, it latches and stops being
// a target; an incomplete resource is re-armed; and a domain that left tls:auto
// is pruned from the latch map.
func TestFollowerProdReloadTargets(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.Routes.Apply(tlsAutoRoute("serve.example.com"), 1)
	st.Routes.Apply(tlsAutoRoute("need.example.com"), 2)
	st.Routes.Apply(tlsAutoRoute("incomplete.example.com"), 3)

	// serve + need have complete prod resources; incomplete has only a .crt.
	st.CertBlobs.Apply(&pb.CertBlob{Key: prodBlobKey("serve.example.com"), Value: []byte("x")}, 10)
	st.CertBlobs.Apply(&pb.CertBlob{Key: prodKeyBlobKey("serve.example.com"), Value: []byte("k")}, 11)
	st.CertBlobs.Apply(&pb.CertBlob{Key: prodBlobKey("need.example.com"), Value: []byte("x")}, 12)
	st.CertBlobs.Apply(&pb.CertBlob{Key: prodKeyBlobKey("need.example.com"), Value: []byte("k")}, 13)
	st.CertBlobs.Apply(&pb.CertBlob{Key: prodBlobKey("incomplete.example.com"), Value: []byte("x")}, 14)

	// Pre-seed the latch with a stale (no-longer-tls:auto) domain and the
	// incomplete one, to assert pruning + re-arming.
	loaded := map[string]bool{"stale.example.com": true, "incomplete.example.com": true}
	serves := func(d string) bool { return d == "serve.example.com" }

	targets := followerProdReloadTargets(st, loaded, serves)

	if len(targets) != 1 || targets[0] != "need.example.com" {
		t.Fatalf("targets = %v, want [need.example.com]", targets)
	}
	if !loaded["serve.example.com"] {
		t.Errorf("serving domain must be latched")
	}
	if loaded["need.example.com"] {
		t.Errorf("not-yet-serving domain must NOT be latched (so it retries)")
	}
	if loaded["incomplete.example.com"] {
		t.Errorf("incomplete domain must be re-armed (dropped from latch)")
	}
	if loaded["stale.example.com"] {
		t.Errorf("domain no longer tls:auto must be pruned from latch")
	}

	// Next tick: the cache now serves need.example.com -> it latches and drops
	// out of the target set (the self-heal converges).
	serves2 := func(d string) bool { return d == "serve.example.com" || d == "need.example.com" }
	targets = followerProdReloadTargets(st, loaded, serves2)
	if len(targets) != 0 {
		t.Fatalf("after serving, targets = %v, want none", targets)
	}
	if !loaded["need.example.com"] {
		t.Errorf("need.example.com must latch once served")
	}
}

// TestLeafDERMatches guards the exact-match used to confirm the cache serves
// the replicated leaf.
func TestLeafDERMatches(t *testing.T) {
	a, b, c := []byte("aaa"), []byte("bbb"), []byte("ccc")
	if !leafDERMatches([][]byte{a, b}, b) {
		t.Errorf("expected match when want is present")
	}
	if leafDERMatches([][]byte{a, b}, c) {
		t.Errorf("expected no match when want is absent")
	}
	if leafDERMatches(nil, a) {
		t.Errorf("empty cache must not match")
	}
}
