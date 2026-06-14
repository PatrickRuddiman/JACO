package pull

import (
	"errors"
	"fmt"
	"strings"

	"github.com/distribution/reference"
	"github.com/docker/docker/api/types/registry"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// DockerHubHost is the canonical key under which Docker Hub credentials are
// stored. The distribution/reference parser normalizes bare image names
// ("alpine", "library/alpine") to "docker.io/library/...", so callers that
// canonicalize via CanonicalHost will see "docker.io" for any Hub ref —
// including the legacy "index.docker.io/v1/" form some clients still emit.
const DockerHubHost = "docker.io"

// AuthResolver returns the base64-encoded X-Registry-Auth header value for a
// given image reference, or "" when no credential should be attached for that
// pull. An error is fatal for that pull attempt (the reconciler surfaces it as
// PULL_FAILED on ReplicaObserved) — distinct from an empty string, which means
// "anonymous pull, proceed".
//
// Production builds the closure in the reconciler against
// state.RegistryCredentials; tests pass a hand-rolled fn (or nil — Pull treats
// nil as "always anonymous").
type AuthResolver func(ref string) (string, error)

// CanonicalHost returns the canonical registry host key for ref. Docker Hub
// is normalized to "docker.io"; everything else returns the host as parsed
// by distribution/reference, lower-cased so a credential stored once is
// found regardless of how the operator typed the host. A non-default port
// (e.g. ":5000") is preserved verbatim. An empty string or parse failure
// returns the error so the caller doesn't silently mis-key the credential
// lookup.
func CanonicalHost(ref string) (string, error) {
	if ref == "" {
		return "", errors.New("pull: empty image reference")
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return "", fmt.Errorf("pull: parse %q: %w", ref, err)
	}
	host := strings.ToLower(reference.Domain(named))
	// ParseNormalizedNamed maps bare names to "docker.io" already; the legacy
	// "index.docker.io" form some operators still type is the only Hub alias
	// we have to fold ourselves.
	if host == "index.docker.io" {
		return DockerHubHost, nil
	}
	return host, nil
}

// CanonicalRepo returns the canonical "host[:port]/<repository-path>" key for
// an image reference — e.g. "ghcr.io/owner/repo" for "ghcr.io/owner/repo:tag",
// or "docker.io/library/alpine" for "alpine:3.18". It is the full path the
// per-namespace credential resolver matches against via MatchCredentialKey:
// the host is canonicalized exactly like CanonicalHost (Docker Hub aliases →
// "docker.io", non-default port preserved) and the repository path is the
// lower-cased remainder distribution/reference parsed out. A parse failure
// returns the error so the reconciler surfaces a malformed ref as a pull
// failure rather than silently looking up the empty key.
func CanonicalRepo(ref string) (string, error) {
	host, err := CanonicalHost(ref)
	if err != nil {
		return "", err
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return "", fmt.Errorf("pull: parse %q: %w", ref, err)
	}
	path := reference.Path(named) // already lower-cased by the parser grammar
	if path == "" {
		return host, nil
	}
	return host + "/" + path, nil
}

// MatchCredentialKey resolves the most specific stored credential key for the
// canonical image repo path ("host[:port]/<repository-path>") via
// longest-prefix matching: it tries the full path first, then trims one
// trailing path segment at a time, down to the bare host. The first key for
// which lookup reports true wins — so a namespace-scoped credential
// ("ghcr.io/owner") always beats the bare-host fallback ("ghcr.io"), and an
// image under an unconfigured namespace falls back to the bare host if one is
// stored. Returns ("", false) when nothing matches (anonymous pull).
//
// repo MUST be the output of CanonicalRepo; callers that only have a bare host
// can pass it directly (it degenerates to a single host lookup).
func MatchCredentialKey(repo string, lookup func(key string) bool) (string, bool) {
	candidate := repo
	for {
		if candidate == "" {
			return "", false
		}
		if lookup(candidate) {
			return candidate, true
		}
		i := strings.LastIndex(candidate, "/")
		if i < 0 {
			return "", false
		}
		candidate = candidate[:i]
	}
}

// RegistryHost strips any "/namespace" suffix from a canonical credential key,
// returning the bare "host[:port]". A key without a namespace is returned
// unchanged. Used to keep the moby X-Registry-Auth ServerAddress host-only:
// the daemon matches the auth blob by registry hostname, so a namespaced key
// like "ghcr.io/owner" must present ServerAddress "ghcr.io".
func RegistryHost(key string) string {
	if i := strings.IndexByte(key, '/'); i >= 0 {
		return key[:i]
	}
	return key
}

// BuildRegistryAuth encodes cred for the moby daemon's X-Registry-Auth
// header: base64(JSON{username, password, serveraddress}). Returns "" for a
// nil credential so callers can pass the result straight into
// image.PullOptions{RegistryAuth: ...} — an empty string is the documented
// "no auth" value.
//
// We use registry.EncodeAuthConfig (URL-safe base64 per the moby SDK) rather
// than encoding by hand: the SDK rotates the exact JSON shape under us, and
// the daemon's docker-content-trust / scope-token paths cross-check this
// blob.
func BuildRegistryAuth(cred *pb.RegistryCredential) (string, error) {
	if cred == nil {
		return "", nil
	}
	cfg := registry.AuthConfig{
		Username:      cred.GetUsername(),
		Password:      string(cred.GetSecret()),
		ServerAddress: RegistryHost(cred.GetRegistry()),
	}
	encoded, err := registry.EncodeAuthConfig(cfg)
	if err != nil {
		return "", fmt.Errorf("pull: encode registry auth: %w", err)
	}
	return encoded, nil
}
