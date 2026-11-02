package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// parkedRootView is Dispatch's view of LEGION-208 as the daemon's prepare stage reads it for a
// person's approval: a labelled root with no parent, in status, whose primary document is
// artifact-208, the document the approvals here are of.
func parkedRootView(status string) *dispatch.Issue {
	return &dispatch.Issue{Key: "LEGION-208", Project: "LEGION", Title: "root", Status: status, Labels: []string{dispatch.LegionLabel},
		HandedOver: true, PrimaryArtifactID: "artifact-208"}
}

// approvalOf is an approval of version 2 of LEGION-208's document artifact-208 by actor, with view
// as what the prepare stage read of the issue (nil for a fact nothing prepared).
func approvalOf(actor dispatch.Actor, view *dispatch.Issue) intake.DispatchArtifact {
	return intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 2, Mark: askSeq(9), Actor: actor, Issue: view}
}

// personHandoverWrite is the row the daemon queues for a person's handover found at status.
func personHandoverWrite(status string) record.StatusWrite {
	return record.StatusWrite{Status: "todo", ObservedStatus: status, PersonHandover: true}
}

// statusWrites decodes the queued dispatch_status rows of issue, oldest first.
func statusWrites(t *testing.T, pool *pgxpool.Pool, issue string) []record.StatusWrite {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select payload from outbox where kind = 'dispatch_status' and issue = $1 order by id", issue)
	if err != nil {
		t.Fatalf("read status writes: %v", err)
	}
	defer rows.Close()
	var writes []record.StatusWrite
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("scan status write: %v", err)
		}
		var write record.StatusWrite
		if err := json.Unmarshal(payload, &write); err != nil {
			t.Fatalf("decode status write %s: %v", payload, err)
		}
		writes = append(writes, write)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate status writes: %v", err)
	}
	return writes
}

// personHandoverLines counts the lines of the engine's JSON log that say it queued a person's
// handover of issue from source, the one message admission logs its own under.
func personHandoverLines(t *testing.T, logged *bytes.Buffer, issue, source string) int {
	t.Helper()
	lines := 0
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == "admission: queued a person's handover" &&
			entry["issue"] == issue && entry["source"] == source {
			lines++
		}
	}
	return lines
}

// A person's approval of the primary document of a parked labelled root the daemon has no record
// of is the person's handover of it: the daemon queues the root's todo through its outbox, observed
// at the status the approval found so the delivery writes nothing once the status moved, marked as
// a person's handover, and tells no architect or controller of it — the root has no tree. The
// queue is logged once, as admission logs its own, with source approval.
func TestAPersonsApprovalOfAnUnrecordedParkedRootsSpecIsQueuedByTheDaemon(t *testing.T) {
	for _, status := range []string{"backlog", "icebox"} {
		t.Run(status, func(t *testing.T) {
			pool := migratedPool(t)
			var logged bytes.Buffer
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, slog.New(slog.NewJSONHandler(&logged, nil))))

			apply("approval", approvalOf(askPerson, parkedRootView(status)))

			assertOutboxKinds(t, pool, []string{"dispatch_status"})
			if got, want := statusWrites(t, pool, "LEGION-208"), []record.StatusWrite{personHandoverWrite(status)}; !reflect.DeepEqual(got, want) {
				t.Fatalf("status writes = %#v, want %#v", got, want)
			}
			if lines := personHandoverLines(t, &logged, "LEGION-208", "approval"); lines != 1 {
				t.Fatalf("queued-a-person's-handover lines with source approval = %d, want one; the engine logged:\n%s", lines, logged.String())
			}
		})
	}
}

// Only a person's own approval of a parked labelled root's primary document queues it. A session's
// review is not a person's act (Dispatch refuses a bearer's review, and the daemon checks the actor
// as well); a carried approval is Dispatch's bookkeeping of an earlier approval, not a new act; a
// changes request approves nothing; a fact with no view of the issue — the gate seed's, the boot
// catch-up's — was not prepared as a person's approval; and a child, an unlabelled root, a document
// that is not the issue's primary one, or a root that is not parked is not Legion's handover to
// queue. Each writes nothing and wakes nobody.
func TestOnlyAPersonsApprovalOfAParkedLabelledRootsPrimaryDocumentQueuesIt(t *testing.T) {
	carried := approvalOf(askPerson, parkedRootView("backlog"))
	carried.Reason = intake.CarriedApprovalReasonPrefix + " 2: the answer to the decision block was the whole change"
	changes := approvalOf(askPerson, parkedRootView("backlog"))
	changes.Kind, changes.Reason = intake.DispatchArtifactChangesRequested, "tighten it"
	child := parkedRootView("backlog")
	child.Parent = new("LEGION-200")
	unlabelled := parkedRootView("backlog")
	unlabelled.Labels, unlabelled.HandedOver = nil, false
	otherDocument := parkedRootView("backlog")
	otherDocument.PrimaryArtifactID = "artifact-plan"
	for _, tc := range []struct {
		name string
		fact intake.DispatchArtifact
	}{
		{"a session's approval", approvalOf(askArchitect, parkedRootView("backlog"))},
		{"a carried approval", carried},
		{"a person's changes request", changes},
		{"a fact with no view of the issue", approvalOf(askPerson, nil)},
		{"a child's document", approvalOf(askPerson, child)},
		{"an unlabelled root", approvalOf(askPerson, unlabelled)},
		{"a document that is not the primary one", approvalOf(askPerson, otherDocument)},
		{"a root in todo", approvalOf(askPerson, parkedRootView("todo"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))

			apply("approval", tc.fact)

			assertOutboxKinds(t, pool, []string{})
		})
	}
}

// lingeringRoot is LEGION-208 recorded where a root's leaving the workflow leaves it: its phase
// done, its tree lingering, in status.
func lingeringRoot(status string) record.Issue {
	until := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
	return record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: phase.Done, Generation: 1, Status: status, Rank: "U", HandedOver: true, LingerUntil: &until}
}

// A parked labelled root the daemon recorded — one that ran and was parked, its tree lingering —
// is queued by a person's approval of its spec as an unrecorded one is: the write re-admits it as a
// new generation, as a person's todo does. The gate its earlier run registered is not applied: the
// re-admission clears it and the new architect's register_gate reads the approval from Dispatch,
// so the gate row stands as the park left it and no design-approved notice is written for a tree
// with no architect.
func TestAPersonsApprovalOfARecordedParkedRootsSpecQueuesItAndLeavesItsGateAlone(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, lingeringRoot("backlog"))
	gate := record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2}
	seedGate(t, pool, gate)
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))

	apply("approval", approvalOf(askPerson, parkedRootView("backlog")))

	assertOutboxKinds(t, pool, []string{"dispatch_status"})
	if got, want := statusWrites(t, pool, "LEGION-208"), []record.StatusWrite{personHandoverWrite("backlog")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("status writes = %#v, want %#v", got, want)
	}
	if got := gateState(t, pool, "LEGION-208"); !reflect.DeepEqual(got, gate) {
		t.Fatalf("the parked root's gate = %#v, want it as the park left it, %#v", got, gate)
	}
	if kinds := noticeKinds(t, pool, "LEGION-208"); len(kinds) != 0 {
		t.Fatalf("notices for a parked root with no architect = %v, want none", kinds)
	}
}

// A root whose tree is live takes a person's approval on its gate as today, whatever Dispatch shows
// of its status: a slotted root whose person approved and then parked it, the approval prepared
// after the park and applied before the park's event, opens its gate and is told design-approved,
// and the park's event, not the approval, takes the tree out. A lingering root that is not parked
// — closed as done — is not one the approval queues either, and keeps today's behaviour: its gate
// takes the approval, so a reopen inside the linger sees it.
func TestAnApprovalOfALiveOrClosedRootsSpecIsAppliedToItsGateAsToday(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, pool *pgxpool.Pool)
		view *dispatch.Issue
	}{
		{"a slotted root Dispatch shows parked", func(t *testing.T, pool *pgxpool.Pool) {
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
				Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U", HandedOver: true})
			seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
		}, parkedRootView("backlog")},
		{"a lingering root closed as done", func(t *testing.T, pool *pgxpool.Pool) {
			seedIssue(t, pool, lingeringRoot("done"))
		}, parkedRootView("done")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			tc.seed(t, pool)
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2})
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))

			apply("approval", approvalOf(askPerson, tc.view))

			if writes := statusWrites(t, pool, "LEGION-208"); slices.ContainsFunc(writes, func(write record.StatusWrite) bool { return write.PersonHandover }) {
				t.Fatalf("status writes = %#v, want no person's handover for a root the approval does not queue", writes)
			}
			if gate := gateState(t, pool, "LEGION-208"); gate.ApprovedVersion == nil || *gate.ApprovedVersion != 2 {
				t.Fatalf("the root's gate = %#v, want it opened at version 2 as today", gate)
			}
			if kinds := noticeKinds(t, pool, "LEGION-208"); !containsNotice(kinds, "design-approved") {
				t.Fatalf("notices = %v, want design-approved as today", kinds)
			}
		})
	}
}

// A copy of the parked root's gate that a hand-over carried to a child of its tree, now a root of
// its own, belongs to a tree that is its own and is applied as today: the approval opens the copy
// carried closed and its architect is told carried-gate-opened. The parked owner's own gate is
// skipped and the owner queued.
func TestACarriedCopyOfAParkedRootsGateTakesTheApprovalWhileTheOwnerIsQueued(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, lingeringRoot("backlog"))
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-209", Project: "LEGION",
		Title: "carried child", Parent: &parent, Phase: phase.Merging, Generation: 2, Status: "retro", Rank: "V", HandedOver: true})
	owner := record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2, ApprovedVersion: new(1)}
	seedGate(t, pool, owner)
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-209", ArtifactID: "artifact-208", LatestVersion: 2, ApprovedVersion: new(1)})
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))

	apply("approval", approvalOf(askPerson, parkedRootView("backlog")))

	if got, want := statusWrites(t, pool, "LEGION-208"), []record.StatusWrite{personHandoverWrite("backlog")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the owner's status writes = %#v, want %#v", got, want)
	}
	if writes := statusWrites(t, pool, "LEGION-209"); len(writes) != 0 {
		t.Fatalf("the carried root's status writes = %#v, want none", writes)
	}
	if got := gateState(t, pool, "LEGION-208"); !reflect.DeepEqual(got, owner) {
		t.Fatalf("the owner's gate = %#v, want it as the park left it, %#v", got, owner)
	}
	if copied := gateState(t, pool, "LEGION-209"); copied.ApprovedVersion == nil || *copied.ApprovedVersion != 2 {
		t.Fatalf("the carried copy = %#v, want it opened at version 2", copied)
	}
	if kinds := noticeKinds(t, pool, "LEGION-208"); len(kinds) != 0 {
		t.Fatalf("the owner's notices = %v, want none", kinds)
	}
	if kinds := noticeKinds(t, pool, "LEGION-209"); !slices.Equal(kinds, []record.NoticeKind{"carried-gate-opened"}) {
		t.Fatalf("the carried root's notices = %v, want carried-gate-opened once", kinds)
	}
}

// One row of a person's handover at a time: while a marked row is pending for the root, a second
// approval — the stream's redelivery under another event id, or the person approving again — queues
// nothing, since the pending row carries the same write, at either status a parked root holds. The
// workflow's other status rows suppress nothing: a hand-over row, also a todo, pending for the root
// leaves the person's handover queued beside it.
func TestAPendingPersonsHandoverSuppressesASecondAndAHandOverRowDoesNot(t *testing.T) {
	for _, status := range []string{"backlog", "icebox"} {
		t.Run("a pending marked row at "+status, func(t *testing.T) {
			pool := migratedPool(t)
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))

			apply("approval", approvalOf(askPerson, parkedRootView(status)))
			apply("approval-redelivered", approvalOf(askPerson, parkedRootView(status)))

			if got, want := statusWrites(t, pool, "LEGION-208"), []record.StatusWrite{personHandoverWrite(status)}; !reflect.DeepEqual(got, want) {
				t.Fatalf("status writes = %#v, want the one row %#v", got, want)
			}
		})
	}
	t.Run("a pending hand-over row", func(t *testing.T) {
		pool := migratedPool(t)
		handOver := record.StatusWrite{Status: "todo", ObservedStatus: "backlog", HandOver: true, HandOverTree: "LEGION-200", HandOverLinger: 3}
		row, err := record.NewOutboxRow("LEGION-208", handOver, time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
			return record.NewStore().Enqueue(context.Background(), tx, row)
		}); err != nil {
			t.Fatalf("queue the hand-over row: %v", err)
		}
		apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))

		apply("approval", approvalOf(askPerson, parkedRootView("backlog")))

		if got, want := statusWrites(t, pool, "LEGION-208"), []record.StatusWrite{handOver, personHandoverWrite("backlog")}; !reflect.DeepEqual(got, want) {
			t.Fatalf("status writes = %#v, want the hand-over row and the person's handover, %#v", got, want)
		}
	})
}
