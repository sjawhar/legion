// A conditional claim's precondition is weighed twice: once on the claim this call reads, and
// again by the conditional write itself. These cases force the second one, the race the
// precondition exists for, by landing another claim between the read and the write.

package store

import (
	"context"
	"errors"
	"slices"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// racingUpdateKeyValue runs a hook before the first conditional update reaches the bucket, so
// the claim that update is conditional on can be moved under it. The hook's own writes go
// through this wrapper too and are not hooked again, and the update itself is the real one: the
// loss it suffers is the server's, not one this wrapper invented.
type racingUpdateKeyValue struct {
	natsgo.KeyValue

	updates           int
	beforeFirstUpdate func()
}

func (kv *racingUpdateKeyValue) Update(key string, value []byte, revision uint64) (uint64, error) {
	kv.updates++
	if kv.updates == 1 {
		kv.beforeFirstUpdate()
	}
	return kv.KeyValue.Update(key, value, revision)
}

// raceConditionalClaim seeds role for seed, hands the claimant the revision it reads, and lands
// winner's claim between that read and the conditional write. It returns the claim's error and
// the revision the claimant required.
func raceConditionalClaim(t *testing.T, role, seed, winner, claimant string) (*Registry, error, uint64) {
	t.Helper()
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg, _ := coldRegistry(t, conn)

	if _, err := reg.SetRole(seed, "example-host", role, false); err != nil {
		t.Fatalf("seed %s's claim on %s: %v", seed, role, err)
	}
	read, err := reg.RoleClaim(context.Background(), role)
	if err != nil {
		t.Fatalf("read %s's claim: %v", role, err)
	}

	racing := &racingUpdateKeyValue{KeyValue: reg.roleKV}
	racing.beforeFirstUpdate = func() {
		if _, err := reg.SetRole(winner, "example-host", role, false); err != nil {
			t.Errorf("the claim that wins the race: %v", err)
		}
	}
	reg.roleKV = bus.KeyValue{KeyValue: racing}

	required := read.Revision
	_, err = reg.SetRoleWithPrevious(context.Background(), claimant, "example-host", role, "", false, &required)
	if racing.updates == 0 {
		t.Fatalf("the conditional claim never reached a conditional update, so it never raced")
	}
	return reg, err, required
}

// A conditional claim that passes the precondition and then loses the conditional write lost it
// to a claim that landed in between: the caller is told its revision is gone, not that the store
// failed, and the claim it made is nowhere.
func TestAConditionalClaimThatLosesTheConditionalWriteIsAMismatchNotAStoreFailure(t *testing.T) {
	const (
		role     = "raced-role"
		seed     = "ses_seed"
		winner   = "ses_winner"
		claimant = "ses_claimant"
	)
	reg, err, required := raceConditionalClaim(t, role, seed, winner, claimant)

	var mismatch *ErrRoleClaimRevisionMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("a conditional claim that lost the conditional write: %v, want *ErrRoleClaimRevisionMismatch", err)
	}
	if mismatch.Role != role || mismatch.Expected != required || mismatch.Holder != winner {
		t.Fatalf("the mismatch says role %q, expected %d, holder %q; want %q, %d, %q",
			mismatch.Role, mismatch.Expected, mismatch.Holder, role, required, winner)
	}
	if mismatch.Actual == required || mismatch.Actual == 0 {
		t.Fatalf("the mismatch says the bucket holds revision %d; want the revision the winning claim wrote, past %d", mismatch.Actual, required)
	}
	assertRoleHolder(t, reg, role, winner)
	if holdsTopic(t, reg, claimant, role) {
		t.Fatalf("the refused claimant %s kept role %s's topic: the claim that lost was not rolled back", claimant, role)
	}
}

// The same race whose winner names the claimant itself. An unconditional claim reads that as the
// concurrent success it is (TestSetRoleTreatsSameSessionCASConflictAsConcurrentSuccess); a
// conditional one is still refused, because the revision its caller sequenced on is gone. The
// session now holds the role, so its record keeps the role's topic.
func TestAConditionalClaimLostToAClaimOfItsOwnSessionIsStillAMismatch(t *testing.T) {
	const (
		role     = "raced-own-role"
		seed     = "ses_own_seed"
		claimant = "ses_own_claimant"
	)
	reg, err, required := raceConditionalClaim(t, role, seed, claimant, claimant)

	var mismatch *ErrRoleClaimRevisionMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("a conditional claim lost to a claim of its own session: %v, want *ErrRoleClaimRevisionMismatch", err)
	}
	if mismatch.Holder != claimant || mismatch.Expected != required {
		t.Fatalf("the mismatch says holder %q at expected %d; want %q at %d", mismatch.Holder, mismatch.Expected, claimant, required)
	}
	assertRoleHolder(t, reg, role, claimant)
	if !holdsTopic(t, reg, claimant, role) {
		t.Fatalf("the refused claim took role %s's topic from %s, which holds the role", role, claimant)
	}
}

func holdsTopic(t *testing.T, reg *Registry, sessionID, role string) bool {
	t.Helper()
	item, err := reg.Get(context.Background(), sessionID)
	if errors.Is(err, natsgo.ErrKeyNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("read %s's interest record: %v", sessionID, err)
	}
	return slices.Contains(item.Topics, contracts.RoleTopicPrefix+role)
}
