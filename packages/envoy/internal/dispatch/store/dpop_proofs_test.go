package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

// proofFixture is a migrated database of the test's own and the replay store on it.
func proofFixture(t *testing.T) *DPoPProofs {
	t.Helper()
	database := migratedEmptyStore(t)
	return NewDPoPProofs(database.Pool)
}

// The first presentation of a proof is not a replay and the second is, and a proof reusing a
// 'jti' against another target URI is neither: RFC 9449 Section 4.2 requires a 'jti' to be unique
// only in the context of the target URI, so keying on it alone would refuse a conforming client.
func TestADPoPProofIsUsedOncePerTargetURI(t *testing.T) {
	ctx := context.Background()
	proofs := proofFixture(t)
	exp := time.Now().Add(time.Minute)

	used, err := proofs.CheckAndSetDPoPProofUsed(ctx, "jti-1", "jkt", "", "GET", "https://dispatch.test/a", exp)
	if err != nil || used {
		t.Fatalf("the first presentation: used = %v, err = %v; want not used", used, err)
	}
	used, err = proofs.CheckAndSetDPoPProofUsed(ctx, "jti-1", "jkt", "", "GET", "https://dispatch.test/a", exp)
	if err != nil || !used {
		t.Fatalf("the replay: used = %v, err = %v; want used", used, err)
	}
	used, err = proofs.CheckAndSetDPoPProofUsed(ctx, "jti-1", "jkt", "", "GET", "https://dispatch.test/b", exp)
	if err != nil || used {
		t.Fatalf("the same jti against another target URI: used = %v, err = %v; want not used", used, err)
	}
}

// Two requests presenting one proof at the same moment cannot both see it unused: the check and
// the store are one statement, so exactly one of them is told it is a replay.
func TestTwoPresentationsOfOneProofAtOnceLeaveExactlyOneUnused(t *testing.T) {
	ctx := context.Background()
	proofs := proofFixture(t)
	exp := time.Now().Add(time.Minute)

	const presentations = 8
	outcomes := make([]bool, presentations)
	failures := make([]error, presentations)
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for i := range presentations {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			outcomes[i], failures[i] = proofs.CheckAndSetDPoPProofUsed(ctx, "jti-at-once", "jkt", "", "GET", "https://dispatch.test/a", exp)
		}()
	}
	start.Done()
	done.Wait()

	unused := 0
	for i, err := range failures {
		if err != nil {
			t.Fatalf("presentation %d: %v", i, err)
		}
		if !outcomes[i] {
			unused++
		}
	}
	if unused != 1 {
		t.Fatalf("%d of %d presentations saw the proof unused, want exactly 1", unused, presentations)
	}
}

// A record whose proof can no longer be presented is taken over by the next proof of the same
// key, so a 'jti' a client reuses long after its proof expired is not refused as a replay.
func TestAnExpiredRecordIsTakenOver(t *testing.T) {
	ctx := context.Background()
	proofs := proofFixture(t)

	// Past by more than jtiRetentionMargin, which the record's own expiry adds.
	expired := time.Now().Add(-2 * jtiRetentionMargin)
	if used, err := proofs.CheckAndSetDPoPProofUsed(ctx, "jti-expired", "jkt", "", "GET", "https://dispatch.test/a", expired); err != nil || used {
		t.Fatalf("the expired proof's own presentation: used = %v, err = %v", used, err)
	}
	used, err := proofs.CheckAndSetDPoPProofUsed(ctx, "jti-expired", "jkt", "", "GET", "https://dispatch.test/a", time.Now().Add(time.Minute))
	if err != nil || used {
		t.Fatalf("a fresh proof of an expired record's key: used = %v, err = %v; want not used", used, err)
	}
}

// The sweep deletes what can no longer be presented and keeps what can, so the table holds the
// last few minutes of proofs rather than every proof ever presented.
func TestSweepDeletesOnlyTheRecordsThatCanNoLongerBePresented(t *testing.T) {
	ctx := context.Background()
	proofs := proofFixture(t)

	for _, jti := range []string{"jti-gone-1", "jti-gone-2"} {
		if _, err := proofs.CheckAndSetDPoPProofUsed(ctx, jti, "jkt", "", "GET", "https://dispatch.test/a", time.Now().Add(-2*jtiRetentionMargin)); err != nil {
			t.Fatalf("record %s: %v", jti, err)
		}
	}
	if _, err := proofs.CheckAndSetDPoPProofUsed(ctx, "jti-live", "jkt", "", "GET", "https://dispatch.test/a", time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("record the live proof: %v", err)
	}

	deleted, err := proofs.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("Sweep deleted %d records, want the 2 that had expired", deleted)
	}
	if used, err := proofs.CheckAndSetDPoPProofUsed(ctx, "jti-live", "jkt", "", "GET", "https://dispatch.test/a", time.Now().Add(time.Minute)); err != nil || !used {
		t.Fatalf("after the sweep the live record: used = %v, err = %v; want used", used, err)
	}
}
