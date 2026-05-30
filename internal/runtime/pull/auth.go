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
		ServerAddress: cred.GetRegistry(),
	}
	encoded, err := registry.EncodeAuthConfig(cfg)
	if err != nil {
		return "", fmt.Errorf("pull: encode registry auth: %w", err)
	}
	return encoded, nil
}
