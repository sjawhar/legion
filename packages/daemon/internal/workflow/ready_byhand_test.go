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
	byHandNote    = "MANUAL_MERGE_REQUIRED: pull request #42 matches a by_hand merge rule; do not auto-arm it"
	armedPacket   = "READY #42 at head (approved at approved) for LEGION-208 (https://github.com/sjawhar/legion/pull/42)\nMerge: submit on normal acceptance with gh pr merge 42 -R sjawhar/legion --auto --squash --match-head-commit head\n\nA docs/solutions/legion/retro-LEGION-208.md"
	armedPacketL1 = "READY #42 at head (approved at approved) for LEGION-208 (https://github.com/sjawhar/legion/pull/42)"
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
			if strings.Contains(packet, "gh pr merge") || strings.Contains(packet, "--auto") {
				t.Fatalf("told READY %q still asks for the merge to be submitted", packet)
			}
			if published := mergeQueuePublishes(t, pool); len(published) != 1 || published[0].Packet != packet {
				t.Fatalf("merge queue publishes = %v, want the same packet the issue was told", published)
			}
		})
	}
}

// A READY whose commit is not the head the record holds for the pull request is refused: a push
// that landed after the route read the head on GitHub, but before this completion committed, would
// otherwise leave the issue awaiting a merge of a head no READY named, which nothing withdraws.
// Nothing of the READY is recorded, so the merger's packet at the new head is applied.
func TestAREADYForAHeadTheRecordHasMovedPastIsRefused(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4, ApprovedVersion: new(4)})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "moved"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})
	apply := applyFacts(t, pool, readyEngine(""))

	result := apply("ready-old-head", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true, Summary: readyPacket, Commit: "head"})
	if refusalCode(result) != "READY_HEAD_MOVED" || !strings.Contains(result.Refusal.Message, "moved") {
		t.Fatalf("READY at a head the record moved past = %#v, want READY_HEAD_MOVED naming the recorded head", result)
	}
	assertPhase(t, pool, phase.Merging)
	var summary, commit string
	if err := pool.QueryRow(context.Background(), "select summary, handoff_commit from phases where issue = 'LEGION-208' and role = 'merger'").Scan(&summary, &commit); err != nil || summary != "" || commit != "" {
		t.Fatalf("the merger's row after the refusal = %q at %q, %v; want nothing recorded", summary, commit, err)
	}
	apply("ready-new-head", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true, Summary: readyPacket, Commit: "moved"})
	assertPhase(t, pool, phase.AwaitingMerge)
}
