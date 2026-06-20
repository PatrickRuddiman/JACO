// Package cachepoke reaches into Caddy's package-private TLS certificate
// cache to evict managed certificates by subject. Used by the stage-first
// promote path (issue #163) to drop the cached staging leaf the moment we
// flip the automation policy to prod — without it, Caddy keeps serving the
// staging cert from cache because the cache outlives Stop+Load and
// `certmagic.Cache` is keyed by Subject without per-CA-URL discrimination
// (both LE staging and LE prod issuers hash to "" via `Issuer.Key()`).
//
// # Why this package exists
//
// Caddy v2 v2.11.3's `caddytls` keeps its cert cache as a package-private
// `*certmagic.Cache` global (`modules/caddytls/tls.go:47`). The TLS app
// instance doesn't expose it, and the admin endpoint doesn't expose
// per-domain cache eviction either. The upstream-friendly fix is a Caddy
// PR adding either an exported accessor or an admin route; until that
// lands, JACO uses `go:linkname` to bind to the private symbol.
//
// `go:linkname` against a third-party package is fragile to upstream
// changes: a rename or type change in `caddytls.certCache` produces a
// link-time error, not a silent breakage. Acceptable for a stop-gap.
// Bumping `caddy/v2` MUST sanity-check this package still compiles.
//
// # Concurrency
//
// `certmagic.Cache.RemoveManaged` takes its own internal mutex
// (`certmagic@v0.25.3/cache.go:411`), so concurrent calls are safe.
// Reading the package-private `certCache` pointer is technically racy
// against Caddy's startup which writes it under a separate
// `certCacheMu`, but in practice the pointer is set exactly once during
// the first TLS app provision and then never mutated. `EvictManaged`
// guards against a nil read (Caddy not yet provisioned) by returning a
// typed error instead of dereferencing.
package cachepoke

import (
	"errors"
	_ "unsafe" // required for go:linkname

	"github.com/caddyserver/certmagic"
	_ "github.com/caddyserver/caddy/v2/modules/caddytls" // ensures caddytls is linked so the linkname'd symbol resolves
)

// caddyCertCache is bound at link time to caddytls's package-private
// `certCache` global. NEVER assign to this variable — it shares storage
// with Caddy's own.
//
//go:linkname caddyCertCache github.com/caddyserver/caddy/v2/modules/caddytls.certCache
var caddyCertCache *certmagic.Cache

// ErrCacheUninitialized is returned by EvictManaged when Caddy has not yet
// provisioned its TLS app (and therefore its cert cache pointer is still
// nil). This happens during daemon startup before the ingress reloader
// fires; the caller should treat it as a no-op — there can't be any
// cached cert to evict if the cache itself doesn't exist yet.
var ErrCacheUninitialized = errors.New("cachepoke: caddytls cert cache not yet provisioned")

// LeafDERs returns the raw DER bytes of every certificate in Caddy's
// in-process cert cache whose subject matches domain (exact or wildcard).
// The stage-first follower path uses it to confirm — before it stops
// force-reloading — that certmagic has actually loaded the replicated prod
// leaf into the cache, rather than trusting that a forced caddy.Load
// "succeeded" (it returns nil even when certmagic then schedules an async
// obtain that fails, which is exactly the race the level-triggered reload
// must survive). Returns ErrCacheUninitialized when Caddy has not yet
// provisioned its TLS app (cache pointer still nil) — the caller treats that
// as "not serving yet" and keeps retrying.
func LeafDERs(domain string) ([][]byte, error) {
	cache := caddyCertCache
	if cache == nil {
		return nil, ErrCacheUninitialized
	}
	matches := cache.AllMatchingCertificates(domain)
	out := make([][]byte, 0, len(matches))
	for _, c := range matches {
		if c.Leaf != nil && len(c.Leaf.Raw) > 0 {
			out = append(out, c.Leaf.Raw)
		}
	}
	return out, nil
}

// EvictManaged removes ALL managed certificates from Caddy's in-process
// cert cache whose Subject matches the given domain, regardless of issuer.
// This is exactly the operation the stage-first promote path needs: after
// flipping the automation policy from staging to prod, the cached staging
// leaf is meaningless — drop it so the next handshake misses cache,
// looks at storage (which the daemon also wipes of staging blobs on
// promote), finds nothing under the prod-CA-namespaced key, and triggers
// `certmagic.Manager.ObtainCert` against the now-prod issuer.
//
// Per certmagic@v0.25.3/cache.go:411 `RemoveManaged`, passing
// `IssuerKey: ""` matches certs for the subject from any issuer, which is
// the behavior we want here (the cache may hold either or both of the
// staging and prod leaves at the moment of promote).
//
// Returns ErrCacheUninitialized when the linkname'd cache pointer is
// nil — the caller should log + continue, not abort. Any other error
// surfacing from certmagic would be returned as-is, but the current
// `RemoveManaged` signature is `(subjects)` with no return, so this
// function only ever returns ErrCacheUninitialized or nil.
func EvictManaged(domain string) error {
	cache := caddyCertCache
	if cache == nil {
		return ErrCacheUninitialized
	}
	cache.RemoveManaged([]certmagic.SubjectIssuer{{Subject: domain}})
	return nil
}
