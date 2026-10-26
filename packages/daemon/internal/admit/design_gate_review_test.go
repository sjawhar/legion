package admit

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// reviewedAt is Dispatch's time of the reviews these tests deliver.
var reviewedAt = fixedNow.Add(-time.Hour)

// seedGate records gate.
func seedGate(t *testing.T, pool *pgxpool.Pool, gate record.DesignGate) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutGate(context.Background(), tx, gate); err != nil {
			t.Fatalf("seed gate: %v", err)
		}
	})
}

// storedGate is the issue's design gate, which must be recorded.
func storedGate(t *testing.T, pool *pgxpool.Pool, key string) *record.DesignGate {
	t.Helper()
	var gate *record.DesignGate
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if gate, err = record.NewStore().Gate(context.Background(), tx, key); err != nil {
			t.Fatalf("read gate: %v", err)
		}
	})
	if gate == nil {
		t.Fatalf("issue %s has no design gate", key)
	}
	return gate
}

func applyFact(t *testing.T, pool *pgxpool.Pool, admission *Admission, id string, fact intake.Fact) {
	t.Helper()
	source := "dispatch"
	if _, ok := fact.(intake.GateRegistered); ok {
		source = "api"
	}
	if _, err := intake.ApplyFact(context.Background(), pool, source, id, fact, admission.engine, admission); err != nil {
		t.Fatalf("apply %s: %v", id, err)
	}
}

func gateNotices(t *testing.T, pool *pgxpool.Pool, key string, kind record.NoticeKind) []record.Notice {
	t.Helper()
	var got []record.Notice
	for _, effect := range effects(t, pool) {
		if notice, ok := effect.payload.(record.Notice); ok && effect.issue == key && notice.Kind == kind {
			got = append(got, notice)
		}
	}
	return got
}

// A person who asks for changes between the architect's `dispatch request-approval` and its
// `register_gate` has answered the request before the registration: register_gate's read carries
// the changes request, so the root keeps its slot at admitted and its architect is told, whether a
// gate was registered before or this is the first registration.
func TestAChangesRequestBeforeRegistrationKeepsTheSlotAndTellsTheArchitect(t *testing.T) {
	for _, tc := range []struct {
		name  string
		prior *record.DesignGate
	}{
		{name: "a gate registered at an earlier version", prior: &record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 3}},
		{name: "the first registration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-100", "A")
			seedWaiting(t, pool, "LEGION-NEXT", "B")
			if tc.prior != nil {
				seedGate(t, pool, *tc.prior)
			}
			applyFact(t, pool, admission, "spec-v4", intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactVersion, Version: 4})
			applyFact(t, pool, admission, "changes-v4", intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root",
				Kind: intake.DispatchArtifactChangesRequested, Version: 4, Reason: "name the rollback path"})

			applyFact(t, pool, admission, "register-v4", intake.GateRegistered{
				Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 4, ChangesRequestedVersion: 4, ChangesReason: "name the rollback path",
			})

			if root := issue(t, pool, "LEGION-100"); root.Phase != phase.Admitted {
				t.Fatalf("root after registering a version a person already asked changes of = %#v, want admitted", root)
			}
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: fixedNow}})
			told := gateNotices(t, pool, "LEGION-100", "design-changes-requested")
			if len(told) == 0 || told[len(told)-1].Version != 4 || told[len(told)-1].Reason != "name the rollback path" {
				t.Fatalf("design-changes-requested notices = %#v, want the last at version 4 with the reason", told)
			}
			gate := storedGate(t, pool, "LEGION-100")
			if classify.DesignGateOpen(*gate) || gate.ToldChangesVersion == nil || *gate.ToldChangesVersion != 4 {
				t.Fatalf("gate after the registration = %#v, want it closed at version 4 with the changes request recorded as told", gate)
			}
		})
	}
}

// A person who approved the spec before the architect registered it has the gate open at
// registration: the root starts planning in its own slot.
func TestARegistrationDispatchAlreadyApprovedKeepsTheRootsSlotAndStartsPlanning(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-100", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")

	applyFact(t, pool, admission, "registered-approved-gate",
		intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 3, ApprovedVersion: 3, ApprovedMark: record.EventMark{Seq: 9, At: reviewedAt}})

	if root := issue(t, pool, "LEGION-100"); root.Phase != phase.Planning {
		t.Fatalf("root after approved registration = %#v, want planning", root)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: fixedNow}})
	if gate := storedGate(t, pool, "LEGION-100"); !classify.DesignGateOpen(*gate) || gate.ApprovedMark != (record.EventMark{Seq: 9, At: reviewedAt}) {
		t.Fatalf("gate after approved registration = %#v, want it open with the approval's mark", gate)
	}
}

// A registration whose read is older than the gate's newest version changes no gate state, but it
// still queues the gate seed at the gate's version: that read of Dispatch is what catches a
// review whose own event was lost.
func TestAStaleRegistrationStillQueuesItsGateSeed(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-100", "A")
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 5})
	applyFact(t, pool, admission, "register-v4", intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4})
	var seeds []record.GateSeed
	for _, effect := range effects(t, pool) {
		if seed, ok := effect.payload.(record.GateSeed); ok && effect.issue == "LEGION-100" {
			seeds = append(seeds, seed)
		}
	}
	if len(seeds) != 1 || seeds[0].Version != 5 || seeds[0].ArtifactID != "artifact-root" {
		t.Fatalf("gate seeds = %#v, want one at the gate's version 5", seeds)
	}
	if gate := storedGate(t, pool, "LEGION-100"); gate.LatestVersion != 5 {
		t.Fatalf("gate after a stale registration = %#v, want it still at version 5", gate)
	}
}

// One changes request reaches the architect once, whichever read carries it first: the live event,
// a registration's read, or the gate seed's. The gate records the request it told, by its version,
// its reason and where Dispatch recorded it, and a read of the same review tells nothing again,
// including a read that gives the review's time and not its sequence (a review Dispatch recorded
// before it kept one). Another review at the same version is a new act of a person and is told,
// with another reason or with the same one.
func TestAChangesRequestIsToldOnceWhicheverReadCarriesIt(t *testing.T) {
	changes := intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root",
		Kind: intake.DispatchArtifactChangesRequested, Version: 4, Reason: "name the rollback path", Mark: record.EventMark{Seq: 9, At: reviewedAt}}
	seeded := changes
	registration := intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 4,
		ChangesRequestedVersion: 4, ChangesReason: "name the rollback path", ChangesRequestedMark: changes.Mark}
	second := changes
	second.Reason, second.Mark = "and name who runs the restore", record.EventMark{Seq: 12, At: reviewedAt.Add(time.Minute)}
	repeated := changes
	repeated.Mark = record.EventMark{Seq: 12, At: reviewedAt.Add(time.Minute)}
	timeOnly := changes
	timeOnly.Mark = record.EventMark{At: reviewedAt}
	v5 := intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested, Version: 5, Reason: "name the rollback path", Mark: changes.Mark}

	for _, tc := range []struct {
		name    string
		order   []intake.Fact
		reasons []string
	}{
		{name: "the live event, then the registration", order: []intake.Fact{changes, registration, seeded},
			reasons: []string{"name the rollback path"}},
		{name: "the registration, then the live event", order: []intake.Fact{registration, seeded, changes},
			reasons: []string{"name the rollback path"}},
		// The live event reached no gate, so the seed's read is the only thing that can tell it.
		{name: "only the seed carries it",
			order:   []intake.Fact{intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 4}, seeded},
			reasons: []string{"name the rollback path"}},
		// A registration whose read predates the review the live event told rebuilds the gate from
		// Dispatch; it keeps the told record, so the seed's read of that review tells nothing.
		{name: "the live event, then a registration whose read predates it, then the seed",
			order:   []intake.Fact{changes, intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 4}, seeded},
			reasons: []string{"name the rollback path"}},
		// A registration of another document starts with no told record, so that document's own
		// review, even with the same reason, is told.
		{name: "another document registered after the first one's review was told",
			order: []intake.Fact{changes,
				intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-other", Version: 4},
				intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-other", Kind: intake.DispatchArtifactChangesRequested,
					Version: 4, Reason: "name the rollback path", Mark: record.EventMark{Seq: 14, At: reviewedAt.Add(2 * time.Minute)}}},
			reasons: []string{"name the rollback path", "name the rollback path"}},
		// A registration whose read saw v4 commits after a live v5 changes request was told: it
		// rolls nothing back, so the seed's read of the v5 review tells nothing again.
		{name: "a registration of an older version after a newer review was told",
			order:   []intake.Fact{v5, intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4}, v5},
			reasons: []string{"name the rollback path"}},
		// The caller registers v4 while Dispatch already shows v5, whose changes request the live
		// event told first: the gate is registered at v5, keeping the told record.
		{name: "a caller's older version with Dispatch at the newer one",
			order:   []intake.Fact{v5, intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 5}, v5},
			reasons: []string{"name the rollback path"}},
		{name: "a second review at the same version", order: []intake.Fact{changes, second},
			reasons: []string{"name the rollback path", "and name who runs the restore"}},
		// The live event carries the review's sequence; the seed reads it from a Dispatch that shows
		// its time alone. The time is the event's own, so the seed's read is the same review.
		{name: "the live event with its sequence, then the seed's read with its time alone", order: []intake.Fact{changes, timeOnly},
			reasons: []string{"name the rollback path"}},
		{name: "the seed's read with its time alone, then the live event with its sequence", order: []intake.Fact{timeOnly, changes},
			reasons: []string{"name the rollback path"}},
		// A person who repeats a review's version and wording later has written another review,
		// with its own event, and it is told once; the seed's read of that later review is the same
		// review again.
		{name: "the same reason again at the same version, a later review", order: []intake.Fact{changes, repeated, repeated},
			reasons: []string{"name the rollback path", "name the rollback path"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-100", "A")
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
			for i, fact := range tc.order {
				applyFact(t, pool, admission, fmt.Sprintf("fact-%d", i), fact)
			}
			if root := issue(t, pool, "LEGION-100"); root.Phase != phase.Admitted {
				t.Fatalf("root after the reads = %#v, want it still at admitted", root)
			}
			var reasons []string
			for _, notice := range gateNotices(t, pool, "LEGION-100", "design-changes-requested") {
				reasons = append(reasons, notice.Reason)
			}
			if !reflect.DeepEqual(reasons, tc.reasons) {
				t.Fatalf("design-changes-requested reasons = %#v, want one per review: %#v", reasons, tc.reasons)
			}
		})
	}
}

// A registration whose read of Dispatch began before a review the daemon has already applied at
// that version is the older reader: the person acted after it. The gate keeps that review — a
// told changes request is not told again, an approval is not rolled back.
func TestARegistrationDoesNotHideTheReviewTheGateAlreadyHolds(t *testing.T) {
	reviewed := record.EventMark{Seq: 11, At: reviewedAt.Add(time.Minute)}
	for _, tc := range []struct {
		name string
		gate record.DesignGate
		open bool
	}{
		{
			name: "a changes request the architect was told of",
			gate: record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4,
				ToldChangesVersion: new(4), ToldChangesReason: "name the rollback path", ToldChangesMark: reviewed},
		},
		{
			name: "an approval that opened the gate",
			gate: record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4,
				ApprovedVersion: new(4), ApprovedMark: reviewed},
			open: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-100", "A")
			seedWaiting(t, pool, "LEGION-NEXT", "B")
			seedGate(t, pool, tc.gate)

			applyFact(t, pool, admission, "register-v4", intake.GateRegistered{
				Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4,
				ChangesRequestedVersion: 4, ChangesReason: "name the rollback path",
				ChangesRequestedMark: record.EventMark{Seq: 9, At: reviewedAt.Add(-time.Minute)},
				Since:                record.EventMark{Seq: 10},
			})

			if got := issue(t, pool, "LEGION-100"); got.Phase != phase.Admitted {
				t.Fatalf("root after a registration older than the review the gate holds = %#v, want it admitted", got)
			}
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: fixedNow}})
			if told := gateNotices(t, pool, "LEGION-100", "design-changes-requested"); len(told) != 0 {
				t.Fatalf("design-changes-requested notices = %#v, want none: the architect already heard the review the gate holds", told)
			}
			if gate := storedGate(t, pool, "LEGION-100"); classify.DesignGateOpen(*gate) != tc.open {
				t.Fatalf("gate after the registration = %#v, want open=%t", gate, tc.open)
			}
		})
	}
}

// A review the person has already followed with another at that version changes nothing: the
// daemon applied the newer one first, and this one reached it late. An approval delivered after
// the changes request that superseded it does not reopen the gate, and a changes request delivered
// after a newer approval does not close it or tell the architect.
func TestAReviewAnotherSupersededChangesNoGateState(t *testing.T) {
	superseded := record.EventMark{Seq: 18, At: reviewedAt.Add(-time.Minute)}
	newer := record.EventMark{Seq: 19, At: reviewedAt}
	for _, tc := range []struct {
		name string
		gate record.DesignGate
		late intake.DispatchArtifact
		open bool
	}{
		{
			name: "an approval older than the changes request the gate records",
			gate: record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4,
				ToldChangesVersion: new(4), ToldChangesReason: "name the rollback path", ToldChangesMark: newer},
			late: intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactApproved, Version: 4, Mark: superseded},
		},
		{
			name: "a changes request older than the approval the gate records",
			gate: record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4,
				ApprovedVersion: new(4), ApprovedMark: newer},
			late: intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested,
				Version: 4, Reason: "name the rollback path", Mark: superseded},
			open: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-100", "A")
			seedGate(t, pool, tc.gate)

			applyFact(t, pool, admission, "superseded-review", tc.late)

			if told := gateNotices(t, pool, "LEGION-100", "design-changes-requested"); len(told) != 0 {
				t.Fatalf("design-changes-requested notices = %#v, want none for a superseded review", told)
			}
			if opened := gateNotices(t, pool, "LEGION-100", "design-approved"); len(opened) != 0 {
				t.Fatalf("design-approved notices = %#v, want none for a superseded review", opened)
			}
			if gate := storedGate(t, pool, "LEGION-100"); classify.DesignGateOpen(*gate) != tc.open || !reflect.DeepEqual(*gate, tc.gate) {
				t.Fatalf("gate after the superseded review = %#v, want it unchanged: %#v", gate, tc.gate)
			}
		})
	}
}

// A review of a version below the gate's latest changes nothing on the gate that owns the
// document: the person has a newer version to review. A late approval of an older version does not
// clear the record that the architect was told the newer version's changes request, so nothing
// tells it again; a late changes request of an older version does not close a gate open at the
// newer one, which would refuse READY while Dispatch shows the newer version approved.
func TestAReviewOfAnOlderVersionChangesNoGateState(t *testing.T) {
	reviewed := record.EventMark{Seq: 11, At: reviewedAt}
	late := record.EventMark{Seq: 9, At: reviewedAt.Add(-time.Minute)}
	for _, tc := range []struct {
		name string
		gate record.DesignGate
		late intake.DispatchArtifact
		open bool
	}{
		{
			name: "an approval of the older version after the newer one's told changes request",
			gate: record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 2,
				ToldChangesVersion: new(2), ToldChangesReason: "name the rollback path", ToldChangesMark: reviewed},
			late: intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactApproved, Version: 1, Mark: late},
		},
		{
			name: "a changes request of the older version after the newer one's approval",
			gate: record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 2, ApprovedVersion: new(2), ApprovedMark: reviewed},
			late: intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested,
				Version: 1, Reason: "name the rollback path", Mark: late},
			open: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-100", "A")
			seedGate(t, pool, tc.gate)

			applyFact(t, pool, admission, "late-older-review", tc.late)

			gate := storedGate(t, pool, "LEGION-100")
			if classify.DesignGateOpen(*gate) != tc.open || gate.LatestVersion != 2 {
				t.Fatalf("gate after a late review of version 1 = %#v, want it at version 2 with open=%t", gate, tc.open)
			}
			if !tc.open && (gate.ToldChangesVersion == nil || *gate.ToldChangesVersion != 2 || gate.ToldChangesMark != reviewed) {
				t.Fatalf("gate after a late approval of version 1 = %#v, want the told changes request of version 2 kept", gate)
			}
			if told := gateNotices(t, pool, "LEGION-100", "design-changes-requested"); len(told) != 0 {
				t.Fatalf("design-changes-requested notices = %#v, want none for a review of an older version", told)
			}
		})
	}
}

// A registration Dispatch shows approved at a later version than the one registered (a person's
// answer to a decision block carried the approval to it) opens the gate at that later version, and
// the architect is told that version.
func TestAnApprovalAtALaterVersionTellsTheArchitectThatVersion(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-100", "A")
	applyFact(t, pool, admission, "register-v3-approved-v4", intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 3, ApprovedVersion: 4})
	if told := gateNotices(t, pool, "LEGION-100", "design-approved"); len(told) != 1 || told[0].Version != 4 {
		t.Fatalf("design-approved notices = %#v, want one at version 4", told)
	}
}

// With the design gate off, the gate is approved at registration and no artifact event moves it:
// a changes request the seed or a live event carries must not close it, which would refuse every
// READY of the tree, and the architect is told nothing.
func TestAnArtifactEventChangesNoGateWhenTheDesignGateIsOff(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmissionWithGate(t, 1, config.DesignGateOff, nil)
	seedSlotted(t, pool, "LEGION-100", "A")
	applyFact(t, pool, admission, "register-v4", intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4})
	applyFact(t, pool, admission, "changes-v4", intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root",
		Kind: intake.DispatchArtifactChangesRequested, Version: 4, Reason: "name the rollback path", Mark: record.EventMark{Seq: 11}})
	applyFact(t, pool, admission, "spec-v5", intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root",
		Kind: intake.DispatchArtifactVersion, Version: 5, Mark: record.EventMark{Seq: 12}})
	if gate := storedGate(t, pool, "LEGION-100"); !classify.DesignGateOpen(*gate) {
		t.Fatalf("gate after a changes request and a version with the design gate off = %#v, want it still open", gate)
	}
	if told := gateNotices(t, pool, "LEGION-100", "design-changes-requested"); len(told) != 0 {
		t.Fatalf("design-changes-requested notices with the design gate off = %#v, want none", told)
	}
}
