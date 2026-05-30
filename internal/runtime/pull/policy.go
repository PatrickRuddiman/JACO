package pull

// Policy is the per-deployment pull strategy (compose `pull_policy:`,
// issue #120). The validator restricts compose input to a closed enum;
// the runtime collapses build/missing/always into one "do call ImagePull"
// branch (the daemon manifest-checks; near-zero cost when up to date) and
// short-circuits only on PolicyNever.
//
// PolicyDefault is the empty value — operator did not set pull_policy.
// The runtime treats it the same as PolicyMissing, so existing manifests
// retain today's "always call ImagePull" behavior.
type Policy string

const (
	// PolicyDefault means "operator did not set pull_policy"; equivalent
	// to PolicyMissing.
	PolicyDefault Policy = ""
	// PolicyAlways forces a registry round-trip on every Start.
	PolicyAlways Policy = "always"
	// PolicyMissing pulls only when the daemon does not already cache the
	// image. JACO's existing implementation calls ImagePull
	// unconditionally — the daemon's manifest check is cheap enough that
	// this matches the spec's behavior in practice.
	PolicyMissing Policy = "missing"
	// PolicyNever forbids any pull. Start refuses to fall back to the
	// registry; if the image is absent the subsequent ContainerCreate
	// surfaces a typed error so air-gapped operators see exactly which
	// replica is missing its side-loaded image.
	PolicyNever Policy = "never"
	// PolicyBuild is accepted by the validator (since compose authors
	// frequently set it for `docker compose build` workflows) but JACO
	// never builds images; the runtime treats it as PolicyMissing.
	PolicyBuild Policy = "build"
)

// ShouldPull reports whether the lifecycle layer should call Pull for the
// given policy. Returns true for every policy except PolicyNever — the
// only value that materially changes runtime behavior.
func ShouldPull(p Policy) bool {
	return p != PolicyNever
}
