package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

const readyPacket = "READY #42 at head (approved at head) for LEGION-208 (https://github.com/sjawhar/legion/pull/42)\n\nno file changes above the approved head"

// The daemon, not the merger, tells the human a pull request is ready to merge: the merger's READY
// completion carries the packet as its summary, and merging -> awaiting_merge posts it verbatim on
// the Dispatch issue and, when the project names a merge queue role, publishes it there too.
func TestTheDaemonPostsTheMergersREADYPacket(t *testing.T) {
	for _, tc := range []struct {
		name, mergeQueue string
	}{
		{name: "no merge queue"},
		{name: "a merge queue role", mergeQueue: "merge-queue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4, ApprovedVersion: new(4)})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})
			engine := readyEngine(tc.mergeQueue)

			result, err := intake.ApplyFact(context.Background(), pool, "api", "ready", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true, Summary: readyPacket, Commit: "head"}, engine, admissionStub{})
			if err != nil || result.Refusal != nil {
				t.Fatalf("ApplyFact READY = %#v, %v", result, err)
			}
			assertPhase(t, pool, phase.AwaitingMerge)
			if got := messageBodies(t, pool); len(got) != 1 || got[0] != readyPacket {
				t.Fatalf("Dispatch messages = %q, want the READY packet once", got)
			}
			published := mergeQueuePublishes(t, pool)
			if tc.mergeQueue == "" && len(published) != 0 {
				t.Fatalf("merge queue publishes = %v, want none without a merge queue role", published)
			}
			if tc.mergeQueue != "" && (len(published) != 1 || published[0] != (record.MergeQueuePublish{Role: tc.mergeQueue, Packet: readyPacket})) {
				t.Fatalf("merge queue publishes = %v, want the packet to %s once", published, tc.mergeQueue)
			}
		})
	}
}

// A READY the design gate refused is posted when a human approves: the packet is kept with the
// merger's completion across the refusal, and nothing is posted before the approval.
func TestAREADYTheGateRefusedIsPostedWhenAHumanApproves(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})
	engine := readyEngine("merge-queue")

	result, err := intake.ApplyFact(context.Background(), pool, "api", "ready", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true, Summary: readyPacket, Commit: "head"}, engine, admissionStub{})
	if err != nil || result.Refusal == nil || result.Refusal.Code != "DESIGN_GATE_CLOSED" {
		t.Fatalf("ApplyFact READY = %#v, %v; want the design gate's refusal", result, err)
	}
	if got := messageBodies(t, pool); len(got) != 0 {
		t.Fatalf("Dispatch messages after the refusal = %q, want none", got)
	}
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "approval", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 4}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact approval: %v", err)
	}
	assertPhase(t, pool, phase.AwaitingMerge)
	if got := messageBodies(t, pool); len(got) != 1 || got[0] != readyPacket {
		t.Fatalf("Dispatch messages after the approval = %q, want the kept READY packet once", got)
	}
	if got := mergeQueuePublishes(t, pool); len(got) != 1 || got[0].Packet != readyPacket {
		t.Fatalf("merge queue publishes = %v, want the kept packet once", got)
	}
}

// The daemon posts the READY packet as one Dispatch message, and Dispatch refuses a body over its
// message cap (2,000 UTF-16 units) on every attempt, so a packet it cannot post never reaches the
// human. The merger's completion is refused instead, naming how far over it is, and nothing is
// recorded or queued: the merger sends a packet that fits. The cap counts UTF-16 units, as Dispatch
// does, so a character outside the Basic Multilingual Plane counts twice.
func TestAREADYPacketDispatchCannotPostIsRefused(t *testing.T) {
	const emoji = "\U0001F600"
	for _, tc := range []struct {
		name, packet string
		refused      bool
	}{
		{name: "one unit over the cap", packet: emoji + strings.Repeat("x", 1999), refused: true},
		{name: "at the cap", packet: emoji + strings.Repeat("x", 1998)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4, ApprovedVersion: new(4)})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})

			result, err := intake.ApplyFact(context.Background(), pool, "api", "ready", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true, Summary: tc.packet, Commit: "head"}, readyEngine("merge-queue"), admissionStub{})
			if err != nil {
				t.Fatalf("ApplyFact READY: %v", err)
			}
			if !tc.refused {
				if result.Refusal != nil {
					t.Fatalf("READY at the cap = %#v, want it accepted", result.Refusal)
				}
				assertPhase(t, pool, phase.AwaitingMerge)
				if got := messageBodies(t, pool); len(got) != 1 || got[0] != tc.packet {
					t.Fatalf("Dispatch messages = %d, want the packet at the cap posted once", len(got))
				}
				return
			}
			if result.Refusal == nil || result.Refusal.Code != "READY_PACKET_TOO_LONG" || !strings.Contains(result.Refusal.Message, "1 characters over Dispatch's 2000-character message limit (2001/2000)") {
				t.Fatalf("READY over the cap = %#v, want READY_PACKET_TOO_LONG naming 2001/2000", result.Refusal)
			}
			assertPhase(t, pool, phase.Merging)
			if got := messageBodies(t, pool); len(got) != 0 {
				t.Fatalf("Dispatch messages = %d, want none for a packet Dispatch refuses", len(got))
			}
			if got := mergeQueuePublishes(t, pool); len(got) != 0 {
				t.Fatalf("merge queue publishes = %d, want none", len(got))
			}
			var stored string
			if err := pool.QueryRow(t.Context(), "select summary from phases where issue = 'LEGION-208' and role = 'merger'").Scan(&stored); err != nil || stored != "" {
				t.Fatalf("merger row summary = %d bytes, %v; want nothing recorded", len(stored), err)
			}
		})
	}
}

// A READY the gate refused belongs to that merging phase. When the merger sends the issue back
// (its head must return to review), the refusal is void: the approval that arrives while the next
// merger verifies does not move the issue on, and the old packet is never posted.
func TestABackwardMoveOutOfMergingVoidsTheRefusedREADY(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implementer-claim"})
	engine := readyEngine("")
	apply := func(id string, fact intake.Fact) intake.Result {
		t.Helper()
		result, err := intake.ApplyFact(context.Background(), pool, "api", id, fact, engine, admissionStub{})
		if err != nil {
			t.Fatalf("ApplyFact %s: %v", id, err)
		}
		return result
	}

	if result := apply("ready", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true, Summary: readyPacket, Commit: "head"}); result.Refusal == nil {
		t.Fatal("READY with the gate closed was not refused")
	}
	apply("back-to-retro", intake.BackwardMove{Issue: "LEGION-208", Requester: claim.RoleMerger, To: phase.Retro, Reason: "a commit above the approved head changes product code"})
	assertPhase(t, pool, phase.Retro)
	apply("retro-done", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implementer-claim", Generation: 1, Summary: "retro recorded", Commit: "retro"})
	assertPhase(t, pool, phase.Merging)
	apply("approval", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 4})
	assertPhase(t, pool, phase.Merging)
	if got := messageBodies(t, pool); len(got) != 0 {
		t.Fatalf("Dispatch messages = %q, want the void READY never posted", got)
	}
}

// A tree that lingers has left the workflow, and linger holds every member where it stood. An
// approval that still reaches the lingering root's gate advances no member: no planner starts for
// an admitted child, and a child whose READY that gate refused is neither moved on nor has its
// packet posted or published to the merge queue.
func TestAnApprovalOnALingeringTreeAdvancesNoMember(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "V"})
	seedIssue(t, pool, record.Issue{Key: "LEGION-210", Tree: "LEGION-208", Project: "LEGION", Title: "waiting child", Parent: &parent, Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "W"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 5, ApprovedVersion: new(4)})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleMerger, Claim: "merger-claim"})
	engine := readyEngine("merge-queue")
	apply := func(id string, fact intake.Fact) intake.Result {
		t.Helper()
		result, err := intake.ApplyFact(context.Background(), pool, "api", id, fact, engine, admissionStub{})
		if err != nil {
			t.Fatalf("ApplyFact %s: %v", id, err)
		}
		return result
	}

	if result := apply("ready", intake.HandoffComplete{Issue: "LEGION-209", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true, Summary: readyPacket, Commit: "head"}); result.Refusal == nil || result.Refusal.Code != "DESIGN_GATE_CLOSED" {
		t.Fatalf("the child's READY with the tree's gate closed = %#v, want DESIGN_GATE_CLOSED", result.Refusal)
	}
	apply("close-root", intake.DispatchIssue{Key: "LEGION-208", Seq: 2, Type: "issue.closed", Status: "done", Title: "root", Rank: "U"})
	var lingering bool
	if err := pool.QueryRow(t.Context(), "select linger_until is not null from issues where key = 'LEGION-208'").Scan(&lingering); err != nil || !lingering {
		t.Fatalf("the closed root lingers = %v, %v; want it lingering", lingering, err)
	}
	apply("approval", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 5})
	for key, want := range map[string]phase.Phase{"LEGION-209": phase.Merging, "LEGION-210": phase.Admitted} {
		var got phase.Phase
		if err := pool.QueryRow(t.Context(), "select phase from issues where key = $1", key).Scan(&got); err != nil || got != want {
			t.Fatalf("the lingering tree's %s is in %q, %v; want it held in %s", key, got, err, want)
		}
	}
	if got := messageBodies(t, pool); len(got) != 0 {
		t.Fatalf("Dispatch messages = %q, want no READY for a closed tree", got)
	}
	if got := mergeQueuePublishes(t, pool); len(got) != 0 {
		t.Fatalf("merge queue publishes = %v, want none for a closed tree", got)
	}
}

func readyEngine(mergeQueue string) *Engine {
	return New(record.NewStore(), Config{
		Project: "LEGION", DesignGate: config.DesignGateRootIssues, MergeQueueRole: mergeQueue, Linger: time.Hour,
		Clock: func() time.Time { return time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC) },
	}, nil)
}

// messageBodies is every Dispatch message the outbox holds, in order.
func messageBodies(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select payload->>'body' from outbox where kind = 'dispatch_message' order by id")
	if err != nil {
		t.Fatalf("read Dispatch messages: %v", err)
	}
	defer rows.Close()
	bodies := []string{}
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			t.Fatalf("scan Dispatch message: %v", err)
		}
		bodies = append(bodies, body)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate Dispatch messages: %v", err)
	}
	return bodies
}

// mergeQueuePublishes is every merge-queue publish the outbox holds, in order.
func mergeQueuePublishes(t *testing.T, pool *pgxpool.Pool) []record.MergeQueuePublish {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select payload->>'role', payload->>'packet' from outbox where kind = 'merge_queue_publish' order by id")
	if err != nil {
		t.Fatalf("read merge queue publishes: %v", err)
	}
	defer rows.Close()
	published := []record.MergeQueuePublish{}
	for rows.Next() {
		var publish record.MergeQueuePublish
		if err := rows.Scan(&publish.Role, &publish.Packet); err != nil {
			t.Fatalf("scan merge queue publish: %v", err)
		}
		published = append(published, publish)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate merge queue publishes: %v", err)
	}
	return published
}
