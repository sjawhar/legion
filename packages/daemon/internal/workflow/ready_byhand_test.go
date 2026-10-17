package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

const (
	byHandNote     = "MANUAL_MERGE_REQUIRED: pull request #42 matches a by_hand merge rule; do not auto-arm it"
	armedPacketL1  = "READY #42 at head (approved at approved) for LEGION-208 (https://github.com/sjawhar/legion/pull/42)"
	armedMergeLine = "Merge: the daemon submits pull request #42 at head once READY is accepted"
	armedPacket    = armedPacketL1 + "\n" + armedMergeLine + "\n\nA docs/solutions/legion/retro-LEGION-208.md"
)

// A READY a person merges is told as one: the packet the daemon posts on the issue and publishes to
// the merge queue role starts with READY's by-hand note, and the merger's line asking for the merge
// to be submitted is replaced, so no reader of the packet arms it. That holds whether READY is
// accepted at once or refused while the design gate was closed and told when a human approves.
func TestAByHandREADYIsToldWithItsNoteAndNoSubmitLine(t *testing.T) {
	for _, tc := range []struct {
		name     string
		approved bool
	}{
		{"accepted at once", true},
		{"refused by the design gate, then approved", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
			gate := record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4}
			if tc.approved {
				gate.ApprovedVersion = new(4)
			}
			seedGate(t, pool, gate)
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})
			apply := applyFacts(t, pool, readyEngine("merge-queue"))

			result := apply("ready", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true,
				Summary: armedPacket, Commit: "head", MergeNote: byHandNote})
			if !tc.approved {
				if refusalCode(result) != "DESIGN_GATE_CLOSED" {
					t.Fatalf("READY with the gate closed = %#v, want DESIGN_GATE_CLOSED", result)
				}
				apply("approval", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 4})
			}
			assertPhase(t, pool, phase.AwaitingMerge)
			told := messageBodies(t, pool)
			if len(told) != 1 {
				t.Fatalf("Dispatch messages = %q, want the READY once", told)
			}
			packet := told[0]
			if !strings.HasPrefix(packet, byHandNote+"\n\n") || !strings.Contains(packet, armedPacketL1) || !strings.Contains(packet, "A docs/solutions/legion/retro-LEGION-208.md") {
				t.Fatalf("told READY %q, want the by-hand note first, then the merger's packet", packet)
			}
			merges := 0
			for line := range strings.Lines(packet) {
				if strings.HasPrefix(line, "Merge:") {
					merges++
				}
			}
			if strings.Contains(packet, armedMergeLine) || merges != 1 {
				t.Fatalf("told READY %q, want the merger's Merge line replaced, one Merge line left", packet)
			}
			if published := mergeQueuePublishes(t, pool); len(published) != 1 || published[0].Packet != packet {
				t.Fatalf("merge queue publishes = %v, want the same packet the issue was told", published)
			}
		})
	}
}

// The route holds READY's commit to GitHub's head, and passes the head the daemon's record held then
// (RecordedHead), which follows GitHub only by webhook. A READY is refused only when the record
// moved after the route read it, to a head other than the commit: a push that landed in between.
// A record still where the route read it is lagging, not moved, and READY is accepted however
// stale it is; so is one that caught up to the commit. The refusal records nothing, so the
// merger's packet at the new head is applied.
func TestAREADYIsRefusedOnlyWhenTheRecordMovedAfterTheRouteReadIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded string // the head the record holds as the completion commits
		refused  bool
	}{
		{"the record moved past the commit", "moved", true},
		{"the record still lags where the route read it", "stale", false},
		{"the record caught up to the commit", "head", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4, ApprovedVersion: new(4)})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: tc.recorded})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})
			apply := applyFacts(t, pool, readyEngine(""))

			// GitHub's head was the commit when the route read it, and the record was "stale".
			result := apply("ready", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true,
				Summary: readyPacket, Commit: "head", RecordedHead: "stale"})
			if !tc.refused {
				if result.Refusal != nil {
					t.Fatalf("READY with the record at %q = %#v, want it accepted", tc.recorded, result.Refusal)
				}
				assertPhase(t, pool, phase.AwaitingMerge)
				return
			}
			if refusalCode(result) != "READY_HEAD_MOVED" || !strings.Contains(result.Refusal.Message, "moved") {
				t.Fatalf("READY at a head the record moved past = %#v, want READY_HEAD_MOVED naming the recorded head", result)
			}
			assertPhase(t, pool, phase.Merging)
			var summary, commit string
			if err := pool.QueryRow(context.Background(), "select summary, handoff_commit from phases where issue = 'LEGION-208' and role = 'merger'").Scan(&summary, &commit); err != nil || summary != "" || commit != "" {
				t.Fatalf("the merger's row after the refusal = %q at %q, %v; want nothing recorded", summary, commit, err)
			}
			apply("ready-new-head", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true,
				Summary: readyPacket, Commit: "moved", RecordedHead: "moved"})
			assertPhase(t, pool, phase.AwaitingMerge)
		})
	}
}

// A READY the design gate refused is held, packet and all, until a human approves the gate, which
// then advances the merge without a second READY. A push that moves the head past that READY's
// commit while it is held voids it: the architect is told READY_HEAD_MOVED, and the approval
// advances nothing, since the held packet names a head no READY covers any more. A push that lands
// on the held READY's own commit - its webhook arriving late - voids nothing.
func TestAPushPastAGateHeldREADYVoidsIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pushed string
		voided bool
	}{
		{"a push past the held READY's commit", "moved", true},
		{"a late push of the held READY's own commit", "head", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})
			apply := applyFacts(t, pool, readyEngine(""))
			if result := apply("ready", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true,
				Summary: readyPacket, Commit: "head", RecordedHead: "head"}); refusalCode(result) != "DESIGN_GATE_CLOSED" {
				t.Fatalf("READY with the gate closed = %#v, want DESIGN_GATE_CLOSED", result)
			}

			apply("push", intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: tc.pushed})
			apply("approval", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 4})

			moved := false
			for _, notice := range architectNotices(t, pool) {
				if notice.Kind == "ready-refused" && strings.HasPrefix(notice.Reason, "READY_HEAD_MOVED:") && strings.Contains(notice.Reason, tc.pushed) {
					moved = true
				}
			}
			if !tc.voided {
				assertPhase(t, pool, phase.AwaitingMerge)
				if moved {
					t.Fatal("the architect was told READY_HEAD_MOVED for a push of the held READY's own commit")
				}
				return
			}
			assertPhase(t, pool, phase.Merging)
			if !moved {
				t.Fatalf("architect notices %+v, want a ready-refused naming READY_HEAD_MOVED and the new head", architectNotices(t, pool))
			}
			for _, body := range messageBodies(t, pool) {
				if strings.Contains(body, "READY #42") {
					t.Fatalf("the voided READY was told: %q", body)
				}
			}
		})
	}
}
