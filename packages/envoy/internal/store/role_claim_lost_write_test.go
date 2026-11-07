// A claim whose write did not land reads the record that stands, and what it may answer for it:
// success only where the claim standing there is the claimant's own process's, since a session
// id names no process and the claim that stands under the claimant's session id can be a second
// process's (RoleClaim.HeldByProcess, SetRoleWithPrevious).

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// A claim whose write failed is answered the success it would have been only where the claim
// that stands names the claimant's own session and the very process the claimant stated. The
// race is the real one: two processes of one session id claim a role at the same moment, the
// loser's conditional write meets the winner's record, and the loser then reads a claim under
// its own session id. Reading that as its own concurrent success is how two processes came to
// believe they held one role (cmd/listener's TestConcurrentSoftClaimsOfOneUnheldRoleAre
// GrantedToExactlyOneProcess); the process the claim names is what tells the two apart.
//
// A claimant that states no process is told nothing either way: an absent instance names no
// process and matches nothing (RoleClaim.HeldByProcess), so such a claimant cannot be shown that
// the claim standing under its session id is its own, and the claim it made is refused like any
// other loser's. It is the rule the strict test keeps everywhere else, and the loose one -
// taking two absent instances for one process - would hand back exactly the false success this
// arm is being narrowed to stop, since a claim naming no process is the one record any process
// of that session id could have written.
//
// The role's lane topic stays in the interest record in every row, including the refusals: one
// record serves every process running under one session id, the session does hold the role, and
// a rollback would leave that standing claim with no interest half naming it.
func TestAClaimWhoseWriteFailedIsSuccessOnlyForTheClaimantsOwnProcess(t *testing.T) {
	const (
		role   = "legion-controller"
		holder = "ses_claiming_under_a_lost_write"
	)
	for name, tc := range map[string]struct {
		claimant string
		standing string
		granted  bool
	}{
		"the standing claim is the claimant's own process": {claimant: roleClaimInstance, standing: roleClaimInstance, granted: true},
		"a second process under the claimant's session id": {claimant: roleClaimInstance, standing: roleClaimSecondInstance},
		"a claimant that states no process":                {claimant: "", standing: ""},
	} {
		t.Run(name, func(t *testing.T) {
			conn, cleanup := connectNATS(t)
			t.Cleanup(cleanup)
			reg, _ := coldRegistry(t, conn)
			racing := &racingCreateKeyValue{KeyValue: reg.roleKV.Legacy()}
			racing.beforeFirstCreate = func() {
				claimRoleAs(t, reg, holder, "example-host", role, tc.standing)
			}
			reg.roleKV = bus.NewKeyValue(racing, reg.roleKV.WatchHandle(), reg.roleKV.Streams())

			_, err := reg.SetRoleWithPrevious(context.Background(), RoleClaimRequest{
				SessionID:        holder,
				MachineID:        "example-host",
				ClientInstanceID: tc.claimant,
				Role:             role,
				Soft:             true,
			})

			if racing.creates == 0 {
				t.Fatalf("the claim never reached a conditional write, so it never raced")
			}
			if tc.granted {
				if err != nil {
					t.Fatalf("the claim lost its write to its own process's claim and answered %v, want the success it would have been had it won", err)
				}
			} else {
				var held *ErrRoleHeld
				if !errors.As(err, &held) {
					t.Fatalf("the claim lost its write to the claim of process %q and answered %v, want *ErrRoleHeld: it holds no role it was not granted", tc.standing, err)
				}
				if held.Role != role || held.Holder != holder {
					t.Fatalf("the refusal names role %q held by %q, want %q held by %q", held.Role, held.Holder, role, holder)
				}
			}
			stood := storedRoleClaim(t, reg, role)
			if stood.HolderSessionID != holder || stood.ClientInstanceID != tc.standing {
				t.Fatalf("the claim that stands is %+v, want %q's claim of process %q, exactly as the winner wrote it", stood, holder, tc.standing)
			}
			assertInterestTopicsForSession(t, reg, holder, contracts.RoleTopicPrefix+role)
		})
	}
}
