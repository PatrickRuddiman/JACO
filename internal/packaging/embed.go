// Package packaging hosts the release-signing public key + tarball
// verification used by `jaco self-upgrade`. The embedded pubkey is replaced
// by the release CI before each tagged build; the placeholder shipped in
// the repo is a dummy that fails verification (intentional — operators
// should pull released binaries, not unsigned dev builds, through
// self-upgrade).
package packaging

import _ "embed"

// EmbeddedPubKey is the minisign public key (1-line `untrusted comment` +
// 1-line base64 body) that the release CI commits in
// packaging/release-pubkey.txt before each tag.
//
//go:embed release-pubkey.txt
var EmbeddedPubKey string
