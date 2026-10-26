// This file is package api rather than api_test, the package every other test file in this
// directory is in, for one reason: the derivation row below asserts perAddressFactor itself, and
// a constant is not worth exporting merely so a test can read it.
package api

import (
	"testing"

	"github.com/sjawhar/envoy/internal/broker/ratelimit"
)

// mountedRereadBuckets are the two shared buckets a broker process can mount: cmd/broker mounts
// GCPRereadOverallLimit under gcp-secret-manager and DefaultRereadOverallLimit otherwise. Both
// rows below run over both of them, because the per-address limit is derived from whichever one
// this process has, and a property that held for only one of the two would be exactly the bug
// the derivation exists to fix.
var mountedRereadBuckets = []struct {
	name    string
	overall ratelimit.Limit
}{
	{"DefaultRereadOverallLimit", DefaultRereadOverallLimit},
	{"GCPRereadOverallLimit", GCPRereadOverallLimit},
}

// TestOneAddressNeverSustainsMoreThanHalfTheMountedRereadBucket asserts the safety property on
// its own, written blind to perAddressFactor: one address's bucket refills at most half as fast
// as the shared bucket this process mounts, so no single address can hold the fleet's reread
// budget at zero by merely keeping up with the shared refill.
//
// It is a floor on safety and not a pin on the number. A later factor of three is tighter than
// this asks and passes; anything below two is looser and fails. That asymmetry is the whole
// point: this is the one thing in the tree that goes red if someone makes perAddressFactor 0.5,
// which would put the one-address starve back. A row that recomputed the production formula from
// the production inputs would pass for any factor at all and would prove only that Go's
// arithmetic is deterministic.
//
// handlers_secrets_test.go's TestOneAddressNeverOutlastsTheSharedRereadBucket proves the other
// half, that Register actually mounts this derivation; this row is about the derivation's own
// output.
func TestOneAddressNeverSustainsMoreThanHalfTheMountedRereadBucket(t *testing.T) {
	for _, mounted := range mountedRereadBuckets {
		t.Run(mounted.name, func(t *testing.T) {
			address := RereadLimitFor(mounted.overall)
			if address.Every < 2*mounted.overall.Every {
				t.Fatalf("a process mounting %s (one reread every %s) gives one address one every %s; want at least %s — an address that refills faster than half the shared bucket can hold the fleet's reread budget at zero on its own, and the route takes no credential",
					mounted.name, mounted.overall.Every, address.Every, 2*mounted.overall.Every)
			}
		})
	}
}

// TestRereadLimitForDerivesTheAddressBucketFromTheMountedOne pins the derivation itself on exact
// durations: the shared bucket's Every multiplied by perAddressFactor, and the one depth every
// address gets (DefaultRereadBurst), which no replica count and no store divides.
//
// It is asserted here rather than through an answer's Retry-After because Retry-After cannot
// carry it: retryAfterSeconds rounds a bucket's Every up to whole seconds, so a header could not
// tell a derived 600 ms from a chosen 1 s should a later resize of either shared bucket ever go
// sub-second. The behavioural row reads the header, which is right for what it proves; an exact
// duration has to be read where it is computed.
func TestRereadLimitForDerivesTheAddressBucketFromTheMountedOne(t *testing.T) {
	for _, mounted := range mountedRereadBuckets {
		t.Run(mounted.name, func(t *testing.T) {
			address := RereadLimitFor(mounted.overall)
			if want := perAddressFactor * mounted.overall.Every; address.Every != want {
				t.Fatalf("RereadLimitFor(%s).Every = %s; want %s (%d × the mounted bucket's %s)",
					mounted.name, address.Every, want, perAddressFactor, mounted.overall.Every)
			}
			if address.Burst != DefaultRereadBurst {
				t.Fatalf("RereadLimitFor(%s).Burst = %d; want %d — one person's write burst is the same depth whatever bucket the process mounts",
					mounted.name, address.Burst, DefaultRereadBurst)
			}
		})
	}
}
