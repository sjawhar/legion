package store

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			ensure(t, st, rootIssue, rootSandbox)
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
			_, act, err := st.IssueSuspension(ctx, "legion", runtime.IssueResourceKey{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, StopRow: 10})
			if tc.wantWait != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantWait) || act {
					t.Fatalf("suspension = %t, %v; want pending %q", act, err, tc.wantWait)
				}
			} else if err != nil || act != tc.wantAct {
				t.Fatalf("suspension = %t, %v; want act %t", act, err, tc.wantAct)
			}
			if resources := resourcesOf(t, st, rootIssue); resources.CleanupStarted {
				t.Fatal("suspension reserved destructive cleanup")
			}
		})
	}
}
