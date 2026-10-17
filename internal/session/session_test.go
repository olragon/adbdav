package session

import "testing"

// `-cache 0` is documented as "disables". New used to substitute a 2 s
// default for a zero TTL, which made that documentation false and made a
// benchmark of the uncached path quietly measure the cached one instead:
// PROPFIND came back in 0.9 ms against a 1.4 s device round trip.
func TestZeroCacheTTLIsNotSilentlyDefaulted(t *testing.T) {
	if ttl := New(Config{}).Config().CacheTTL; ttl != 0 {
		t.Fatalf("CacheTTL = %v, want 0", ttl)
	}
}
