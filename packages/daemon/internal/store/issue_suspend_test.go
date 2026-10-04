package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

func TestIssueSuspensionFencesReadmissionAndWaitsForAllStoredRoles(t *testing.T) {
	for _, tc := range []struct {
		name     string
		change   string
		state    supervise.ClaimState
		start    int64
		wantAct  bool
		wantWait string
	}{
		{name: "stopped roles retain resources", state: supervise.StateSuspended, wantAct: true},
		{name: "held turn", state: supervise.StateWorking, wantWait: "stored claim"},
		{name: "initial launch", state: supervise.StateLaunching, wantWait: "stored claim"},
		{name: "uncertain launch", state: supervise.StateLaunchUncertain, wantWait: "stored claim"},
		{name: "root readmitted before a new claim", state: supervise.StateSuspended, change: "update issues set generation=2, linger_until=null where key='LEGION-208'"},
		{name: "newer start already ran", state: supervise.StateWorking, start: 11},
		{name: "newer start still queued", state: supervise.StateSuspended, change: `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error) values (11,'supervise','LEGION-208','{"op":"start","tree":"LEGION-208","role":"architect","generation":1}',0,now(),'')`},
		{name: "role stop still pending", state: supervise.StateSuspended, change: `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error) values (9,'supervise','LEGION-208','{"op":"suspend","tree":"LEGION-208","role":"architect","generation":1}',0,now(),'')`, wantWait: "role stop effects"},
		{name: "tree cleanup already reserved", state: supervise.StateSuspended, change: `update tree_lifecycles set cleanup_started = true where tree = 'LEGION-208'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openLingeringTree(t, st)
			c := supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"), Project: "legion", Tree: rootIssue, Issue: rootIssue,
				Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, State: tc.state, LastStartRow: tc.start}
			if err := st.PutClaim(ctx, c); err != nil {
				t.Fatal(err)
			}
			if tc.change != "" {
				if _, err := st.Pool().Exec(ctx, tc.change); err != nil {
					t.Fatal(err)
				}
			}
			reservedBefore := cleanupStarted(t, st)
			act, err := st.IssueSuspension(ctx, "legion", IssueClose{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: 10}, record.OutOfWorkflow)
			if tc.wantWait != "" {
				if !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), tc.wantWait) || act {
					t.Fatalf("suspension = %t, %v; want the wait %q", act, err, tc.wantWait)
				}
			} else if err != nil || act != tc.wantAct {
				t.Fatalf("suspension = %t, %v; want act %t", act, err, tc.wantAct)
			}
			if !reservedBefore && cleanupStarted(t, st) {
				t.Fatal("suspension reserved destructive cleanup")
			}
		})
	}
}

func cleanupStarted(t *testing.T, st *Store) bool {
	t.Helper()
	var started bool
	if err := st.Pool().QueryRow(context.Background(), `select cleanup_started from tree_lifecycles
		where project = 'legion' and tree = $1`, rootIssue).Scan(&started); err != nil {
		t.Fatal(err)
	}
	return started
}
