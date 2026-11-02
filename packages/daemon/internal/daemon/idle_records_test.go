package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// podReleaseRecords is every release, resume and hold the rig's project has recorded.
func (r *idleRig) records(t *testing.T) store.PodReleaseRecords {
	t.Helper()
	records, err := r.st.PodReleaseRecords(context.Background(), "legion", time.Time{})
	if err != nil {
		t.Fatalf("read the release records: %v", err)
	}
	return records
}

// INFRA-945 R6: a completed release is recorded with the run it released, the roles whose claims
// it suspended and the size of the bundle it saved, so a deployment can count what release did
// without reading the daemon's log.
func TestAReleaseRecordsItsRolesAndTheBundleItSaved(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()

	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	// Nothing is recorded until the release completes: the bundle is the teardown's to save.
	if records := rig.records(t); len(records.Releases) != 0 {
		t.Fatalf("releases recorded before the teardown = %+v, want none", records.Releases)
	}
	rig.drain(t)

	records := rig.records(t)
	if len(records.Releases) != 1 {
		t.Fatalf("releases = %+v, want the one the teardown completed", records.Releases)
	}
	recorded := records.Releases[0]
	if recorded.Project != "legion" || recorded.Issue != rig.issue.Key || recorded.Generation != 1 {
		t.Errorf("release = %+v, want %s at generation 1 of legion", recorded, rig.issue.Key)
	}
	if len(recorded.Roles) != 1 || recorded.Roles[0] != "implementer" {
		t.Errorf("release roles = %v, want the one role whose claim it suspended", recorded.Roles)
	}
	// The rig's launcher answers a four-byte bundle, which is what the release saved.
	if recorded.BundleBytes != 4 {
		t.Errorf("release bundle = %d bytes, want the 4 the rig's launcher answered", recorded.BundleBytes)
	}
	if recorded.At.IsZero() {
		t.Error("the release was recorded with no time")
	}
	if len(records.Holds) != 0 {
		t.Errorf("holds = %+v, want none: the pod was released, not kept", records.Holds)
	}
}

// A pod the rule read as idle and still did not release is a hold, recorded with the rule's own
// name for the reason. A pod that is simply not idle yet is not: the idle rule reports a hold for
// every working pod on every tick, and recording those would drown the ones that mean something.
func TestAHeldPodRecordsTheRulesOwnReason(t *testing.T) {
	ctx := context.Background()

	t.Run("quiet and held", func(t *testing.T) {
		rig := newIdleRig(t)
		// The session reported background work of its own: the pod is quiet, and the rule keeps it.
		if _, err := rig.pool.Exec(ctx, `update claims set activity_background = true, activity_kinds = 'async-job'
			where issue = $1`, rig.issue.Key); err != nil {
			t.Fatalf("report background work: %v", err)
		}

		rig.w.releaseIdlePod(ctx, rig.issue.Key)

		if got := rig.rows(t); len(got) != 0 {
			t.Fatalf("queued rows = %v, want none: the pod is held", got)
		}
		records := rig.records(t)
		if len(records.Holds) != 1 {
			t.Fatalf("holds = %+v, want the one pass that kept the pod", records.Holds)
		}
		hold := records.Holds[0]
		if hold.Project != "legion" || hold.Issue != rig.issue.Key ||
			hold.Reason != workflow.IdleHoldBackgroundWork.String() {
			t.Fatalf("hold = %+v, want %s held for %s", hold, rig.issue.Key, workflow.IdleHoldBackgroundWork)
		}
		if len(records.Releases) != 0 {
			t.Errorf("releases = %+v, want none", records.Releases)
		}

		// Each pass that keeps the pod records itself, so a deployment counts passes rather than pods.
		rig.w.releaseIdlePod(ctx, rig.issue.Key)
		if records := rig.records(t); len(records.Holds) != 2 {
			t.Fatalf("holds after a second pass = %+v, want one per pass", records.Holds)
		}
	})

	t.Run("not idle yet", func(t *testing.T) {
		rig := newIdleRig(t)
		if _, err := rig.pool.Exec(ctx, `update claims set updated_at = now(), activity_at = now()
			where issue = $1`, rig.issue.Key); err != nil {
			t.Fatalf("bring the pod back to now: %v", err)
		}

		rig.w.releaseIdlePod(ctx, rig.issue.Key)

		records := rig.records(t)
		if len(records.Holds) != 0 || len(records.Releases) != 0 {
			t.Fatalf("records = %+v, want none: the pod is working, not held", records)
		}
	})
}

// A release the teardown could not finish keeps the pod, and the reason it read is recorded as a
// hold in the same vocabulary the tick uses.
func TestAReleaseThatCouldNotFinishRecordsItsHold(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.rt.bundleErr = errors.New("the launcher is gone")

	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	rig.drain(t)

	records := rig.records(t)
	if len(records.Releases) != 0 {
		t.Fatalf("releases = %+v, want none: the release did not finish", records.Releases)
	}
	if len(records.Holds) != 1 || records.Holds[0].Reason != workflow.IdleHoldBundleSave.String() {
		t.Fatalf("holds = %+v, want the one %s", records.Holds, workflow.IdleHoldBundleSave)
	}
}
