package cachepoke

import (
	"errors"
	"testing"
)

// TestEvictManaged_NilCacheReturnsTypedError pins the only contract this
// package can exercise without a live Caddy: when the linkname'd
// `caddyCertCache` pointer is nil (Caddy's TLS app hasn't provisioned yet),
// EvictManaged MUST return ErrCacheUninitialized — never nil-deref. The
// real eviction path (cache populated, RemoveManaged actually drops a
// cached cert) is exercised by the testbed smoke run because it needs a
// live Caddy + a real cert in storage to be observable; standing up that
// scaffolding inside a Go unit test would require importing all of
// caddytls and bootstrapping a TLS app, which is heavier than the smoke.
func TestEvictManaged_NilCacheReturnsTypedError(t *testing.T) {
	// Cache pointer is nil because no caddytls TLS app has been provisioned
	// in this test process. The linkname binding still resolves; it's just
	// reading the initial zero-value pointer.
	if caddyCertCache != nil {
		t.Skipf("test assumes caddyCertCache is nil at test start; got %v — another test in this binary provisioned a caddytls TLS app", caddyCertCache)
	}
	err := EvictManaged("example.com")
	if !errors.Is(err, ErrCacheUninitialized) {
		t.Errorf("EvictManaged with nil cache: got %v, want ErrCacheUninitialized", err)
	}
}
