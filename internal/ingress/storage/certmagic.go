package storage

import (
	"context"

	"github.com/caddyserver/certmagic"
)

// CertMagicStorage satisfies caddy.StorageConverter. Caddy's storage-module
// loader type-asserts the registered "jaco" module to StorageConverter and
// calls this to obtain the certmagic.Storage it actually uses; without it the
// TLS automation policy panics ("not caddy.StorageConverter: missing method
// CertMagicStorage") and the ingress never binds :80/:443 (issue #28).
//
// JacoStorage matches certmagic.Storage shape-for-shape except Stat returns
// our own KeyInfo, so we hand back a thin adapter that only converts that.
func (s *JacoStorage) CertMagicStorage() (certmagic.Storage, error) {
	return certmagicStorage{s}, nil
}

// certmagicStorage adapts *JacoStorage to certmagic.Storage. It embeds the
// raft-backed store (Store/Load/Delete/Exists/List/Lock/Unlock match certmagic
// signatures verbatim) and overrides only Stat to return certmagic.KeyInfo.
type certmagicStorage struct{ *JacoStorage }

func (c certmagicStorage) Stat(ctx context.Context, key string) (certmagic.KeyInfo, error) {
	ki, err := c.JacoStorage.Stat(ctx, key)
	if err != nil {
		return certmagic.KeyInfo{}, err
	}
	return certmagic.KeyInfo{
		Key:        ki.Key,
		Modified:   ki.Modified,
		Size:       ki.Size,
		IsTerminal: ki.IsTerminal,
	}, nil
}
