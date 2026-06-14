package pull_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/image"

	"github.com/PatrickRuddiman/jaco/internal/runtime/pull"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestBuildRegistryAuth_EncodesUsernamePasswordServerAddr asserts the moby
// X-Registry-Auth blob is the URL-safe base64 of the JSON triplet, with the
// password coming out of cred.secret (held as bytes). Both the moby SDK and
// the registry daemon decode this exact shape — a wire-format regression
// silently breaks every private pull.
func TestBuildRegistryAuth_EncodesUsernamePasswordServerAddr(t *testing.T) {
	got, err := pull.BuildRegistryAuth(&pb.RegistryCredential{
		Registry: "ghcr.io",
		Username: "alice",
		Secret:   []byte("hunter2"),
	})
	if err != nil {
		t.Fatalf("BuildRegistryAuth: %v", err)
	}
	if got == "" {
		t.Fatalf("got empty auth string")
	}
	// EncodeAuthConfig uses URL-safe base64 per RFC4648 §5.
	raw, err := base64.URLEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("decode base64url: %v (input %q)", err, got)
	}
	var decoded struct {
		Username      string `json:"username"`
		Password      string `json:"password"`
		ServerAddress string `json:"serveraddress"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode JSON: %v (input %q)", err, raw)
	}
	if decoded.Username != "alice" {
		t.Errorf("username = %q, want alice", decoded.Username)
	}
	if decoded.Password != "hunter2" {
		t.Errorf("password = %q, want hunter2", decoded.Password)
	}
	if decoded.ServerAddress != "ghcr.io" {
		t.Errorf("serveraddress = %q, want ghcr.io", decoded.ServerAddress)
	}
}

// TestBuildRegistryAuth_NilReturnsEmpty asserts the documented "no auth"
// path: a nil credential returns ("", nil) so the caller can pass the result
// straight into image.PullOptions{RegistryAuth:...} for anonymous pulls.
func TestBuildRegistryAuth_NilReturnsEmpty(t *testing.T) {
	got, err := pull.BuildRegistryAuth(nil)
	if err != nil {
		t.Fatalf("BuildRegistryAuth(nil): %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestBuildRegistryAuth_ServerAddressIsHostOnly asserts that a
// namespace-scoped credential key ("ghcr.io/owner") presents a host-only
// ServerAddress ("ghcr.io") in the X-Registry-Auth blob. The moby daemon
// matches the auth config by registry hostname, so leaking the namespace into
// ServerAddress would make the daemon ignore the credential.
func TestBuildRegistryAuth_ServerAddressIsHostOnly(t *testing.T) {
	got, err := pull.BuildRegistryAuth(&pb.RegistryCredential{
		Registry: "ghcr.io/owner",
		Username: "alice",
		Secret:   []byte("hunter2"),
	})
	if err != nil {
		t.Fatalf("BuildRegistryAuth: %v", err)
	}
	raw, err := base64.URLEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("decode base64url: %v", err)
	}
	var decoded struct {
		ServerAddress string `json:"serveraddress"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if decoded.ServerAddress != "ghcr.io" {
		t.Errorf("serveraddress = %q, want ghcr.io (namespace must be stripped)", decoded.ServerAddress)
	}
}

// TestCanonicalHost_NormalizesDockerHubAndPreservesPort exercises the
// resolver-side canonicalization that pairs with the FSM-side normalization
// in canonicalRegistryHost. Both must agree or the per-pull lookup misses.
func TestCanonicalHost_NormalizesDockerHubAndPreservesPort(t *testing.T) {
	cases := []struct {
		ref  string
		want string
	}{
		{"alpine:3.18", "docker.io"},                                 // bare → Hub
		{"library/alpine", "docker.io"},                              // namespaced Hub
		{"docker.io/library/alpine", "docker.io"},                    // explicit Hub
		{"ghcr.io/owner/repo:tag", "ghcr.io"},                        // GHCR
		{"registry.example.com:5000/x", "registry.example.com:5000"}, // non-443
		{"REGISTRY.example.com/x", "registry.example.com"},           // lowercase
	}
	for _, c := range cases {
		got, err := pull.CanonicalHost(c.ref)
		if err != nil {
			t.Errorf("CanonicalHost(%q): %v", c.ref, err)
			continue
		}
		if got != c.want {
			t.Errorf("CanonicalHost(%q) = %q, want %q", c.ref, got, c.want)
		}
	}
}

// TestCanonicalHost_RejectsEmptyAndGarbage covers the error paths so the
// reconciler's auth resolver surfaces a malformed ref as a pull failure
// rather than silently looking up the empty-string key.
func TestCanonicalHost_RejectsEmptyAndGarbage(t *testing.T) {
	for _, ref := range []string{"", "  ", "://", "no spaces allowed"} {
		if _, err := pull.CanonicalHost(ref); err == nil {
			t.Errorf("CanonicalHost(%q): want error, got nil", ref)
		}
	}
}

// TestCanonicalRepo_ReturnsHostAndRepositoryPath asserts the full
// "host[:port]/<repository-path>" form the per-namespace resolver matches
// against: Docker Hub folds to docker.io (and bare names gain the implicit
// library/ namespace), GHCR keeps its owner/repo path, and a non-default
// port is preserved on the host segment.
func TestCanonicalRepo_ReturnsHostAndRepositoryPath(t *testing.T) {
	cases := []struct {
		ref  string
		want string
	}{
		{"alpine:3.18", "docker.io/library/alpine"},
		{"library/alpine", "docker.io/library/alpine"},
		{"docker.io/myorg/app:v1", "docker.io/myorg/app"},
		{"ghcr.io/owner/repo:tag", "ghcr.io/owner/repo"},
		{"GHCR.IO/owner/repo", "ghcr.io/owner/repo"},
		{"registry.example.com:5000/team/svc", "registry.example.com:5000/team/svc"},
	}
	for _, c := range cases {
		got, err := pull.CanonicalRepo(c.ref)
		if err != nil {
			t.Errorf("CanonicalRepo(%q): %v", c.ref, err)
			continue
		}
		if got != c.want {
			t.Errorf("CanonicalRepo(%q) = %q, want %q", c.ref, got, c.want)
		}
	}
}

// TestCanonicalRepo_RejectsGarbage mirrors CanonicalHost's error contract so
// a malformed ref fails the pull rather than mis-keying the lookup.
func TestCanonicalRepo_RejectsGarbage(t *testing.T) {
	for _, ref := range []string{"", "  ", "://", "no spaces allowed"} {
		if _, err := pull.CanonicalRepo(ref); err == nil {
			t.Errorf("CanonicalRepo(%q): want error, got nil", ref)
		}
	}
}

// TestMatchCredentialKey_LongestPrefixWins is the core per-namespace
// resolution contract: the most specific stored key beats the bare host, an
// image under an unconfigured namespace falls back to the bare host, and a
// host with no stored credential at all resolves to nothing (anonymous pull).
func TestMatchCredentialKey_LongestPrefixWins(t *testing.T) {
	stored := map[string]bool{
		"ghcr.io":                 true,
		"ghcr.io/company":         true,
		"ghcr.io/company/private": true,
	}
	lookup := func(k string) bool { return stored[k] }

	cases := []struct {
		repo    string
		wantKey string
		wantOK  bool
	}{
		{"ghcr.io/company/private/app", "ghcr.io/company/private", true}, // exact deepest
		{"ghcr.io/company/other", "ghcr.io/company", true},               // namespace match
		{"ghcr.io/orphan/x", "ghcr.io", true},                            // host fallback
		{"docker.io/library/alpine", "", false},                          // nothing stored
	}
	for _, c := range cases {
		gotKey, gotOK := pull.MatchCredentialKey(c.repo, lookup)
		if gotKey != c.wantKey || gotOK != c.wantOK {
			t.Errorf("MatchCredentialKey(%q) = (%q,%v), want (%q,%v)", c.repo, gotKey, gotOK, c.wantKey, c.wantOK)
		}
	}
}

// TestMatchCredentialKey_BareHostOnly covers the degenerate input (a host
// with no path) so callers that pass a bare host still get a single lookup.
func TestMatchCredentialKey_BareHostOnly(t *testing.T) {
	lookup := func(k string) bool { return k == "ghcr.io" }
	if got, ok := pull.MatchCredentialKey("ghcr.io", lookup); !ok || got != "ghcr.io" {
		t.Errorf("MatchCredentialKey(ghcr.io) = (%q,%v), want (ghcr.io,true)", got, ok)
	}
}

// TestRegistryHost_StripsNamespace asserts the bare-host extraction used to
// keep the X-Registry-Auth ServerAddress host-only.
func TestRegistryHost_StripsNamespace(t *testing.T) {
	cases := map[string]string{
		"ghcr.io":                        "ghcr.io",
		"ghcr.io/owner":                  "ghcr.io",
		"ghcr.io/owner/team":             "ghcr.io",
		"registry.example.com:5000/team": "registry.example.com:5000",
	}
	for in, want := range cases {
		if got := pull.RegistryHost(in); got != want {
			t.Errorf("RegistryHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// recordingPuller captures the RegistryAuth value passed into ImagePull so
// the test can assert auth threading end-to-end without standing up a real
// docker daemon.
type recordingPuller struct {
	mu     sync.Mutex
	called int
	lastRA string
}

func (p *recordingPuller) ImagePull(_ context.Context, _ string, opts image.PullOptions) (io.ReadCloser, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.called++
	p.lastRA = opts.RegistryAuth
	return io.NopCloser(strings.NewReader(`{"status":"ok"}`)), nil
}

// TestPull_AuthResolverPopulatesRegistryAuth_WhenMatching asserts that when
// the resolver returns a non-empty blob (i.e. a matching credential exists in
// the replicated store), pull.Pull threads it into image.PullOptions so the
// docker daemon presents it as X-Registry-Auth. This is the precise behavior
// issue #101 calls out as the gap.
func TestPull_AuthResolverPopulatesRegistryAuth_WhenMatching(t *testing.T) {
	want := "base64-encoded-blob"
	p := &recordingPuller{}
	resolver := func(ref string) (string, error) {
		if ref != "ghcr.io/owner/private:tag" {
			t.Errorf("resolver got ref %q, want ghcr.io/owner/private:tag", ref)
		}
		return want, nil
	}
	clock := newFakeClock()
	if err := pull.Pull(context.Background(), p, "ghcr.io/owner/private:tag", resolver, clock.After, nil); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if p.called != 1 {
		t.Errorf("ImagePull calls = %d, want 1", p.called)
	}
	if p.lastRA != want {
		t.Errorf("RegistryAuth = %q, want %q", p.lastRA, want)
	}
}

// TestPull_AuthResolverEmpty_NoRegistryAuth covers the anonymous-pull path:
// resolver returns "" (no matching credential), Pull must NOT populate
// RegistryAuth — passing an empty blob would still tell the daemon "use this
// (empty) auth" on some versions.
func TestPull_AuthResolverEmpty_NoRegistryAuth(t *testing.T) {
	p := &recordingPuller{}
	resolver := func(string) (string, error) { return "", nil }
	clock := newFakeClock()
	if err := pull.Pull(context.Background(), p, "alpine:3.18", resolver, clock.After, nil); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if p.lastRA != "" {
		t.Errorf("RegistryAuth = %q, want empty for anonymous pull", p.lastRA)
	}
}

// TestPull_NilAuthResolver_NoRegistryAuth covers tests / call-sites that
// don't care about auth at all. nil resolver = "always anonymous".
func TestPull_NilAuthResolver_NoRegistryAuth(t *testing.T) {
	p := &recordingPuller{}
	clock := newFakeClock()
	if err := pull.Pull(context.Background(), p, "alpine:3.18", nil, clock.After, nil); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if p.lastRA != "" {
		t.Errorf("RegistryAuth = %q, want empty when resolver is nil", p.lastRA)
	}
}

// TestPull_ResolverErrorRetriesWithBackoff asserts that a resolver error is
// surfaced as a failed-then-retry transition (not a hard error) so a
// transient resolver outage doesn't permanently fail the deploy. The pull
// loop keeps spinning until ctx is cancelled — matching the same posture as
// a transient ImagePull failure.
func TestPull_ResolverErrorRetriesWithBackoff(t *testing.T) {
	p := &recordingPuller{}
	clock := newFakeClock()
	calls := 0
	resolver := func(string) (string, error) {
		calls++
		if calls >= 3 {
			return "blob", nil
		}
		return "", errors.New("transient resolver failure")
	}
	if err := pull.Pull(context.Background(), p, "ghcr.io/x/y:tag", resolver, clock.After, nil); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if calls != 3 {
		t.Errorf("resolver calls = %d, want 3", calls)
	}
	if p.lastRA != "blob" {
		t.Errorf("RegistryAuth at success = %q, want blob", p.lastRA)
	}
	// Two failed resolver attempts → two backoff sleeps (1s, 2s).
	if got := clock.Delays(); len(got) != 2 || got[0] != 1e9 || got[1] != 2e9 {
		t.Errorf("backoff delays = %v, want [1s 2s]", got)
	}
}
