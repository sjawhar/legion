package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

const readyPacket = "READY #42 at head (approved at approved) for LEGION-208 (https://github.com/sjawhar/legion/pull/42)\n\nA docs/solutions/legion/retro-LEGION-208.md\nD .legion/LEGION-208/review.json"

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
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
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

// READY owns a durable merge submission for its exact head, whether the READY is accepted at once
// or held by the design gate until a human approves. A READY a person merges queues none. The
// outbox keeps retrying that submission while GitHub's requirements are pending; a later push
// makes the row stale instead of letting it merge another head.
func TestTheDaemonQueuesAMergeForTheREADYHead(t *testing.T) {
	const byHand = "MANUAL_MERGE_REQUIRED: pull request #42 matches a by_hand merge rule; do not auto-arm it"
	for _, tc := range []struct {
		name      string
		approved  bool
		mergeNote string
		want      []record.ReadyMerge
	}{
		{name: "accepted at once", approved: true, want: []record.ReadyMerge{{Number: 42, Head: "head"}}},
		{name: "refused by the design gate, then approved", want: []record.ReadyMerge{{Number: 42, Head: "head"}}},
		{name: "merged by hand", approved: true, mergeNote: byHand},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
			gate := record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4}
			if tc.approved {
				gate.ApprovedVersion = new(4)
			}
			seedGate(t, pool, gate)
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})
			apply := applyFacts(t, pool, readyEngine(""))

			result := apply("ready", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true,
				Summary: readyPacket, Commit: "head", RecordedHead: "head", MergeNote: tc.mergeNote})
			if !tc.approved {
				if refusalCode(result) != "DESIGN_GATE_CLOSED" {
					t.Fatalf("READY with the gate closed = %#v, want DESIGN_GATE_CLOSED", result)
				}
				if got := readyMerges(t, pool); len(got) != 0 {
					t.Fatalf("READY merges before the approval = %v, want none", got)
				}
				apply("approval", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 4})
			}
			assertPhase(t, pool, phase.AwaitingMerge)
			if got := readyMerges(t, pool); !slices.Equal(got, tc.want) {
				t.Fatalf("READY merges = %v, want %v", got, tc.want)
			}
		})
	}
}

// readyMerges is every READY merge the outbox holds, in order.
func readyMerges(t *testing.T, pool *pgxpool.Pool) []record.ReadyMerge {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select (payload->>'number')::int, payload->>'head' from outbox where kind = 'ready_merge' order by id")
	if err != nil {
		t.Fatalf("read READY merges: %v", err)
	}
	defer rows.Close()
	var merges []record.ReadyMerge
	for rows.Next() {
		var merge record.ReadyMerge
		if err := rows.Scan(&merge.Number, &merge.Head); err != nil {
			t.Fatalf("scan READY merge: %v", err)
		}
		merges = append(merges, merge)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate READY merges: %v", err)
	}
	return merges
}

// A pull request closed while its READY awaits the merge ends that READY's merge row once the close
// reaches the record. Reopened at the READY head, the READY still stands, so the daemon submits its
// merge again; reopened at another head, the READY is withdrawn and nothing is submitted, and a
// READY a person merges is never submitted. A tree that lingers queues the row as any other does:
// its executor reads the lingering tree as awaiting nothing and submits nothing (the daemon
// package's TestAREADYMergeRowSubmitsOnlyTheAwaitedREADYHead).
func TestAReopenAtTheREADYHeadSubmitsTheMergeAgain(t *testing.T) {
	const byHand = "MANUAL_MERGE_REQUIRED: pull request #42 matches a by_hand merge rule; do not auto-arm it"
	for _, tc := range []struct {
		name, head, mergeNote string
		lingers               bool
		want                  []record.ReadyMerge
	}{
		{name: "at the READY head", head: "head", want: []record.ReadyMerge{{Number: 42, Head: "head"}}},
		{name: "at another head", head: "moved"},
		{name: "at the READY head of a by-hand READY", head: "head", mergeNote: byHand},
		{name: "at the READY head in a lingering tree", head: "head", lingers: true, want: []record.ReadyMerge{{Number: 42, Head: "head"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			issue := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "U"}
			if tc.lingers {
				until, parent := time.Now().Add(time.Hour), "LEGION-207"
				seedIssue(t, pool, record.Issue{Key: "LEGION-207", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-207", Project: "LEGION", Title: "closed root", Phase: phase.Done,
					Generation: 1, Status: "done", Rank: "T", LingerUntil: &until})
				issue.Tree, issue.Parent, issue.Title = "LEGION-207", &parent, "child"
			}
			seedIssue(t, pool, issue)
			seedPR(t, pool, record.PullRequest{State: record.PullRequestClosed, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "head", Summary: readyPacket, MergeNote: tc.mergeNote})
			applyRefusingNothing(t, pool, readyEngine(""), intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: tc.head, Reopened: true})
			if got := readyMerges(t, pool); !slices.Equal(got, tc.want) {
				t.Fatalf("READY merges after the reopen = %v, want %v", got, tc.want)
			}
		})
	}
}

// A ready_merge that was already waiting when a pull request closed must be reused when the pull
// request reopens at that READY head. Two rows would submit the same mutation and turn one
// permanent GitHub refusal into two architect notices.
func TestAReopenAtTheREADYHeadReusesItsWaitingMergeRow(t *testing.T) {
	pool := migratedPool(t)
	records := record.NewStore()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestClosed, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "head", Summary: readyPacket})
	row, err := record.NewOutboxRow("LEGION-208", record.ReadyMerge{Number: 42, Head: "head"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.Enqueue(context.Background(), tx, row)
	}); err != nil {
		t.Fatalf("queue the waiting READY merge: %v", err)
	}
	applyRefusingNothing(t, pool, readyEngine(""), intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Reopened: true})
	if got, want := readyMerges(t, pool), []record.ReadyMerge{{Number: 42, Head: "head"}}; !slices.Equal(got, want) {
		t.Fatalf("READY merges after the reopen = %v, want one row %v", got, want)
	}
}

// The merge is the transition (LEGION-800): the merge queue failing a READY's merge, which the
// ready_merge row reads from GitHub (intake.MergeFailed), moves the issue from awaiting_merge to
// implementing as a red head does. The implementer is started with the failing run named, the
// architect is told checks-red, the READY's withdrawal is posted and published to the merge queue
// role, and its disarm is queued. Only the READY the issue still awaits moves: another READY head,
// a READY a person merges, an issue no longer awaiting its merge, a pull request the record holds
// closed, and a tree that lingers change nothing.
func TestAMergeTheQueueFailedSendsTheAwaitedREADYBackToImplementing(t *testing.T) {
	const byHand = "MANUAL_MERGE_REQUIRED: pull request #42 matches a by_hand merge rule; do not auto-arm it"
	const run = "https://github.com/sjawhar/legion/actions/runs/7"
	const reason = "the merge queue took pull request #42 out at READY's head head at 2026-10-16T05:10:00Z (failed_checks): its merge_group run on group-sha, the commit the queue built for it, failed: integration (failure, " + run + ")"
	for _, tc := range []struct {
		name             string
		at               phase.Phase
		ready, mergeNote string
		pr               record.PullRequestState
		lingers, moves   bool
	}{
		{name: "awaiting that READY", at: phase.AwaitingMerge, ready: "head", pr: record.PullRequestOpen, moves: true},
		{name: "awaiting another READY head", at: phase.AwaitingMerge, ready: "older", pr: record.PullRequestOpen},
		{name: "awaiting a READY a person merges", at: phase.AwaitingMerge, ready: "head", mergeNote: byHand, pr: record.PullRequestOpen},
		{name: "back in implementing", at: phase.Implementing, ready: "head", pr: record.PullRequestOpen},
		{name: "on a pull request the record holds closed", at: phase.AwaitingMerge, ready: "head", pr: record.PullRequestClosed},
		{name: "in a lingering tree", at: phase.AwaitingMerge, ready: "head", pr: record.PullRequestOpen, lingers: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			issue := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: tc.at, Generation: 1, Status: "retro", Rank: "U"}
			if tc.lingers {
				until, parent := time.Now().Add(time.Hour), "LEGION-207"
				seedIssue(t, pool, record.Issue{Key: "LEGION-207", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-207", Project: "LEGION", Title: "closed root", Phase: phase.Done,
					Generation: 1, Status: "done", Rank: "T", LingerUntil: &until})
				issue.Tree, issue.Parent, issue.Title = "LEGION-207", &parent, "child"
			}
			seedIssue(t, pool, issue)
			seedPR(t, pool, record.PullRequest{State: tc.pr, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: tc.ready, Summary: readyPacket, MergeNote: tc.mergeNote})
			applyRefusingNothing(t, pool, readyEngine("merge-queue"), intake.MergeFailed{Issue: "LEGION-208", Number: 42, Head: "head", Reason: reason})
			task := startTask(t, pool, claim.RoleImplementer)
			publishes := mergeQueuePublishes(t, pool)
			if !tc.moves {
				if got := issuePhase(t, pool); got != tc.at || task != "" || len(noticeKinds(t, pool, "LEGION-208")) != 0 || len(publishes) != 0 {
					t.Fatalf("phase %s, implementer task %q, notices %v, publishes %v; want %s and nothing told", got, task, noticeKinds(t, pool, "LEGION-208"), publishes, tc.at)
				}
				return
			}
			if got := issuePhase(t, pool); got != phase.Implementing {
				t.Fatalf("phase %s after the merge queue failed READY's merge, want implementing", got)
			}
			if !strings.Contains(task, run) || !strings.Contains(task, "integration") {
				t.Fatalf("the implementer's task %q, want it to name the failing run", task)
			}
			if kinds := noticeKinds(t, pool, "LEGION-208"); !slices.Contains(kinds, "checks-red") {
				t.Fatalf("notices %v, want the architect told checks-red", kinds)
			}
			if len(publishes) != 1 || !strings.Contains(publishes[0].Packet, "READY withdrawn") || !strings.Contains(publishes[0].Packet, run) {
				t.Fatalf("merge queue publishes %v, want the READY's withdrawal naming the failing run", publishes)
			}
			var disarms int
			if err := pool.QueryRow(context.Background(), "select count(*) from outbox where kind = 'auto_merge_disable'").Scan(&disarms); err != nil || disarms != 1 {
				t.Fatalf("disarms %d (%v), want the withdrawal's one", disarms, err)
			}
		})
	}
}

// A merge group with many failing runs makes a long reason, and Dispatch refuses a message over its
// cap on every attempt (LEGION-800 review). The READY's withdrawal posted on the issue is cut to fit
// one message, saying so, while the implementer's task, which no cap bounds, carries every run.
func TestAWithdrawalOfAMergeTheQueueFailedFitsOneDispatchMessage(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "head", Summary: readyPacket})
	var runs []string
	for i := range 40 {
		runs = append(runs, fmt.Sprintf("integration-shard-%02d (failure, https://github.com/sjawhar/legion/actions/runs/700000%02d)", i, i))
	}
	reason := "the merge queue took pull request #42 out at READY's head head at 2026-10-16T05:10:00Z (failed_checks): its merge_group run on group-sha, the commit the queue built for it, failed: " + strings.Join(runs, "; ")
	applyRefusingNothing(t, pool, readyEngine(""), intake.MergeFailed{Issue: "LEGION-208", Number: 42, Head: "head", Reason: reason})
	var body string
	if err := pool.QueryRow(context.Background(), "select payload->>'body' from outbox where kind = 'dispatch_message'").Scan(&body); err != nil {
		t.Fatalf("read the withdrawal: %v", err)
	}
	if length := dispatch.MessageBodyLength(body); length > record.MessagePostLimit || !strings.HasPrefix(body, "READY withdrawn for LEGION-208") || !strings.Contains(body, "cut to fit") {
		t.Fatalf("withdrawal of %d characters (limit %d) %q, want it within one message, saying it was cut", length, record.MessagePostLimit, body)
	}
	if task := startTask(t, pool, claim.RoleImplementer); !strings.Contains(task, runs[len(runs)-1]) {
		t.Fatalf("the implementer's task %q, want every failing run in it", task)
	}
}

// A READY the design gate refused is posted when a human approves: the packet is kept with the
// merger's completion across the refusal, and nothing is posted before the approval.
func TestAREADYTheGateRefusedIsPostedWhenAHumanApproves(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
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

// A READY the gate refused before migration 0016 kept the merger's packet reaches the approval with
// none. Posting it would leave a message of the outbox marker alone, and a merge queue publish
// without a packet would fail the approval's whole transaction, so the gate would never record the
// approval. Instead the approval is recorded, that READY is void, the issue stays in merging with
// nothing posted or published, and the tree's architect is told by a named reason.
func TestAnApprovalVoidsAREADYTheGateRefusedWithNoPacketAndTellsTheArchitect(t *testing.T) {
	for _, tc := range []struct {
		name, mergeQueue string
	}{
		{name: "no merge queue"},
		{name: "a merge queue role", mergeQueue: "merge-queue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			pending := 4
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U", ReadyPendingVersion: &pending})
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})

			if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "approval", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 4}, readyEngine(tc.mergeQueue), admissionStub{}); err != nil {
				t.Fatalf("ApplyFact approval: %v", err)
			}
			var approved *int
			if err := pool.QueryRow(t.Context(), "select approved_version from design_gates where issue = 'LEGION-208'").Scan(&approved); err != nil || approved == nil || *approved != 4 {
				t.Fatalf("approved version = %v, %v; want the approval of 4 recorded", approved, err)
			}
			assertPhase(t, pool, phase.Merging)
			var stillPending *int
			if err := pool.QueryRow(t.Context(), "select ready_pending_version from issues where key = 'LEGION-208'").Scan(&stillPending); err != nil || stillPending != nil {
				t.Fatalf("ready_pending_version = %v, %v; want the packetless READY void", stillPending, err)
			}
			if got := messageBodies(t, pool); len(got) != 0 {
				t.Fatalf("Dispatch messages = %q, want none for a READY with no packet", got)
			}
			if got := mergeQueuePublishes(t, pool); len(got) != 0 {
				t.Fatalf("merge queue publishes = %v, want none for a READY with no packet", got)
			}
			if got := architectNotices(t, pool); len(got) != 1 || got[0].Kind != "ready-refused" || !strings.HasPrefix(got[0].Reason, "READY_PACKET_MISSING: design version 4 is approved") {
				t.Fatalf("architect notices = %+v, want one ready-refused naming READY_PACKET_MISSING", got)
			}
		})
	}
}

// A READY refused with no design gate registered at all names no pending version — the issues
// table takes only a positive one — but the merger's phase row still carries the refused
// completion's packet. Registering the gate is what the root architect does next; when that
// registration itself opens the gate (gates.design: off, or Dispatch already shows the version
// approved), the kept READY is released the moment it is registered, with no second READY from
// the merger.
func TestAREADYRefusedAtVersionZeroIsPostedWhenRegistrationOpensTheGate(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})
	engine := testEngine(config.DesignGateOff, nil)
	apply := applyFacts(t, pool, engine)

	result := apply("ready", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true, Summary: readyPacket, Commit: "head"})
	if result.Refusal == nil || result.Refusal.Code != "DESIGN_GATE_CLOSED" {
		t.Fatalf("ApplyFact READY = %#v, want the version-0 refusal", result)
	}
	var pending *int
	if err := pool.QueryRow(t.Context(), "select ready_pending_version from issues where key = 'LEGION-208'").Scan(&pending); err != nil || pending != nil {
		t.Fatalf("ready_pending_version after the refusal = %v, %v; want none: the issues table takes only a positive version", pending, err)
	}
	if got := messageBodies(t, pool); len(got) != 0 {
		t.Fatalf("Dispatch messages after the refusal = %q, want none", got)
	}

	apply("register", intake.GateRegistered{Issue: "LEGION-208", ArtifactID: "artifact-208", Version: 1})
	assertPhase(t, pool, phase.AwaitingMerge)
	if got := messageBodies(t, pool); len(got) != 1 || got[0] != readyPacket {
		t.Fatalf("Dispatch messages after registration = %q, want the kept READY packet once", got)
	}
}

// The same READY refused at version 0, but under an armed gate: registering the spec closes the
// gate at its own version (nobody has approved it yet), so the kept READY still waits. Approving
// that registered version is what releases it, the same way an approval releases a refusal that
// named a known version.
func TestAREADYRefusedAtVersionZeroIsPostedWhenTheRegisteredVersionIsApproved(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})
	apply := applyFacts(t, pool, readyEngine("merge-queue"))

	result := apply("ready", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true, Summary: readyPacket, Commit: "head"})
	if result.Refusal == nil || result.Refusal.Code != "DESIGN_GATE_CLOSED" {
		t.Fatalf("ApplyFact READY = %#v, want the version-0 refusal", result)
	}

	apply("register", intake.GateRegistered{Issue: "LEGION-208", ArtifactID: "artifact-208", Version: 1})
	assertPhase(t, pool, phase.Merging)
	if got := messageBodies(t, pool); len(got) != 0 {
		t.Fatalf("Dispatch messages after registration = %q, want none: version 1 is not approved yet", got)
	}

	apply("approval", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 1})
	assertPhase(t, pool, phase.AwaitingMerge)
	if got := messageBodies(t, pool); len(got) != 1 || got[0] != readyPacket {
		t.Fatalf("Dispatch messages after the approval = %q, want the kept READY packet once", got)
	}
	if got := mergeQueuePublishes(t, pool); len(got) != 1 || got[0].Packet != readyPacket {
		t.Fatalf("merge queue publishes = %v, want the kept packet once", got)
	}
}

// The real-world trigger for a version-0 refusal: a tree is re-admitted after it finished or
// lingered (admit.Admission.readmit), which clears the tree's one design-gate row with
// record.ClearTreeGeneration while a member a previous run left in phase.Merging keeps its own
// generation and phase. A merger that completes a READY before the new architect registers the
// gate again is refused at version 0. Registering the gate this time does not open it — nobody has
// approved the newly registered version yet — but the human already approved it before, so the
// gate-seed outbox's read of Dispatch (daemon/outbox.go's seedGate) reports that approval as an
// ordinary DispatchArtifact approval event, not registration opening the gate inline, and that
// approval event is what releases the kept READY.
func TestAREADYRefusedAtVersionZeroAfterReAdmissionIsPostedWhenDispatchReportsTheAlreadyApprovedVersion(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 1, ApprovedVersion: new(1)})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})
	apply := applyFacts(t, pool, readyEngine("merge-queue"))

	// The root is re-admitted (set back to todo after it finished or lingered): the tree's gate
	// is cleared exactly as admit.Admission.readmit clears it, leaving the merger's own phase and
	// generation untouched.
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().ClearTreeGeneration(context.Background(), tx, "LEGION-208")
	})
	var gateGone bool
	if err := pool.QueryRow(t.Context(), "select not exists(select 1 from design_gates where issue = 'LEGION-208')").Scan(&gateGone); err != nil || !gateGone {
		t.Fatalf("design_gates row after re-admission = gone:%v, %v; want it cleared", gateGone, err)
	}

	result := apply("ready", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true, Summary: readyPacket, Commit: "head"})
	if result.Refusal == nil || result.Refusal.Code != "DESIGN_GATE_CLOSED" {
		t.Fatalf("ApplyFact READY after re-admission = %#v, want the version-0 refusal", result)
	}

	apply("register", intake.GateRegistered{Issue: "LEGION-208", ArtifactID: "artifact-208", Version: 2})
	assertPhase(t, pool, phase.Merging)
	if got := messageBodies(t, pool); len(got) != 0 {
		t.Fatalf("Dispatch messages after registration = %q, want none: version 2 is not approved yet", got)
	}

	apply("gate-seed-approval", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 2})
	assertPhase(t, pool, phase.AwaitingMerge)
	if got := messageBodies(t, pool); len(got) != 1 || got[0] != readyPacket {
		t.Fatalf("Dispatch messages after the already-approved version is reported = %q, want the kept READY packet once", got)
	}
}

// A refusal that names a known version still releases exactly as before: registering a later
// document version at the root changes nothing for it until that later version is itself
// approved, and only the approval of the version the refusal is waiting on (or later) releases it.
func TestAREADYRefusedAtAKnownVersionStillReleasesOnlyOnItsApproval(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})
	apply := applyFacts(t, pool, readyEngine("merge-queue"))

	result := apply("ready", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true, Summary: readyPacket, Commit: "head"})
	if result.Refusal == nil || result.Refusal.Code != "DESIGN_GATE_CLOSED" {
		t.Fatalf("ApplyFact READY = %#v, want the refusal naming version 4", result)
	}
	var pending *int
	if err := pool.QueryRow(t.Context(), "select ready_pending_version from issues where key = 'LEGION-208'").Scan(&pending); err != nil || pending == nil || *pending != 4 {
		t.Fatalf("ready_pending_version after the refusal = %v, %v; want 4", pending, err)
	}

	apply("version-5", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactVersion, Version: 5})
	assertPhase(t, pool, phase.Merging)
	if got := messageBodies(t, pool); len(got) != 0 {
		t.Fatalf("Dispatch messages after a newer unapproved version = %q, want none", got)
	}

	apply("approval-5", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 5})
	assertPhase(t, pool, phase.AwaitingMerge)
	if got := messageBodies(t, pool); len(got) != 1 || got[0] != readyPacket {
		t.Fatalf("Dispatch messages after the approval = %q, want the kept READY packet once", got)
	}
}

// A READY the gate refused belongs to that merging phase. When the merger sends the issue back
// (its head must return to review), the refusal is void at once: the approval that arrives while
// the next merger verifies does not move the issue on, posts nothing, and has no void READY to
// report to the architect.
func TestABackwardMoveOutOfMergingVoidsTheRefusedREADY(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implementer-claim"})
	apply := applyFacts(t, pool, readyEngine(""))

	if result := apply("ready", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true, Summary: readyPacket, Commit: "head"}); result.Refusal == nil {
		t.Fatal("READY with the gate closed was not refused")
	}
	apply("back-to-retro", intake.BackwardMove{Issue: "LEGION-208", Requester: claim.RoleMerger, To: phase.Retro, Reason: "a commit above the approved head changes product code"})
	assertPhase(t, pool, phase.Retro)
	var pending int
	if err := pool.QueryRow(t.Context(), "select coalesce(ready_pending_version, 0) from issues where key = 'LEGION-208'").Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("ready_pending_version after the backward move = %d, %v; want the refused READY void (none)", pending, err)
	}
	apply("retro-done", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implementer-claim", Generation: 1, Summary: "retro recorded", Commit: "retro"})
	assertPhase(t, pool, phase.Merging)
	apply("approval", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 4})
	assertPhase(t, pool, phase.Merging)
	if got := messageBodies(t, pool); len(got) != 0 {
		t.Fatalf("Dispatch messages = %q, want the void READY never posted", got)
	}
	if got := architectNotices(t, pool); len(got) != 0 {
		t.Fatalf("architect notices = %+v, want none: the READY was voided when the issue left merging", got)
	}
}

// A tree that lingers has left the workflow, and linger holds every member where it stood. An
// approval that still reaches the lingering root's gate advances no member: no planner starts for
// an admitted child, and a child whose READY that gate refused is neither moved on nor has its
// packet posted or published to the merge queue.
func TestAnApprovalOnALingeringTreeAdvancesNoMember(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "V"})
	seedIssue(t, pool, record.Issue{Key: "LEGION-210", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "waiting child", Parent: &parent, Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "W"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 5, ApprovedVersion: new(4)})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleMerger, Claim: "merger-claim"})
	apply := applyFacts(t, pool, readyEngine("merge-queue"))

	if result := apply("ready", intake.HandoffComplete{Issue: "LEGION-209", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true, Summary: readyPacket, Commit: "head"}); result.Refusal == nil || result.Refusal.Code != "DESIGN_GATE_CLOSED" {
		t.Fatalf("the child's READY with the tree's gate closed = %#v, want DESIGN_GATE_CLOSED", result.Refusal)
	}
	apply("close-root", intake.DispatchIssue{Key: "LEGION-208", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.closed", Status: "done", Title: "root", Rank: "U"})
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

// A child's merger still in its turn when the root closes can send its READY after the tree
// lingers, with the gate open. Linger holds every member where it stood, so that completion is
// refused and moves nothing: the child stays in merging, its packet is not kept, and nothing is
// posted on Dispatch or published to the merge queue for a closed tree.
func TestAMergersREADYOnALingeringTreeIsRefused(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "V"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4, ApprovedVersion: new(4)})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleMerger, Claim: "merger-claim"})
	apply := applyFacts(t, pool, readyEngine("merge-queue"))

	apply("close-root", intake.DispatchIssue{Key: "LEGION-208", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.closed", Status: "done", Title: "root", Rank: "U"})
	if result := apply("ready", intake.HandoffComplete{Issue: "LEGION-209", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true, Summary: readyPacket, Commit: "head"}); result.Refusal == nil || result.Refusal.Code != "TREE_LINGERING" {
		t.Fatalf("the child's READY on the lingering tree = %#v, want TREE_LINGERING", result.Refusal)
	}
	var got phase.Phase
	var summary string
	if err := pool.QueryRow(t.Context(), "select i.phase, p.summary from issues i join phases p on p.issue = i.key and p.role = 'merger' where i.key = 'LEGION-209'").Scan(&got, &summary); err != nil || got != phase.Merging || summary != "" {
		t.Fatalf("the lingering tree's LEGION-209 = %q with a %d-byte packet, %v; want it held in merging with nothing kept", got, len(summary), err)
	}
	if got := messageBodies(t, pool); len(got) != 0 {
		t.Fatalf("Dispatch messages = %q, want no READY for a closed tree", got)
	}
	if got := mergeQueuePublishes(t, pool); len(got) != 0 {
		t.Fatalf("merge queue publishes = %v, want none for a closed tree", got)
	}
}

// Linger holds every member of a closed tree where it stood, whichever path a fact takes: no
// approval, green checks, red checks that send a head back or exhaust max_fix_attempts, a
// conflicting head awaiting merge, changes requested, reviewer's comment on a round no review
// decided, backward move, retry of a held phase or failed claim moves a member, counts a review
// round, posts or notifies, or starts a worker, whose start would resume a suspended claim inside
// a tree that has left the workflow. A worker's own request is refused, so it is told nothing
// moved. A merge is the one fact GitHub never sends again: it moves the child on to its production
// check, and still starts nobody.
func TestNoFactMovesAMemberOfALingeringTree(t *testing.T) {
	hold := record.Hold{From: phase.Testing}
	for _, tc := range []struct {
		name     string
		at, want phase.Phase
		hold     *record.Hold
		pr       record.PullRequest
		decision *record.ReviewDecision
		fact     intake.Fact
		refusal  string
	}{
		{name: "a merge at awaiting_merge", at: phase.AwaitingMerge, want: phase.ProductionCheck,
			fact: intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"}},
		{name: "an approval of a green head", at: phase.Reviewing, pr: record.PullRequest{CheckedHead: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}},
			fact: intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "head", HeadSHA: "head", Author: testReviewApp}},
		{name: "green checks on an approved head", at: phase.Reviewing, decision: &record.ReviewDecision{State: "approved", Head: "head"},
			fact: intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, Snapshot: "green-1", Failing: []string{}}},
		{name: "green checks on an approval the head does not carry", at: phase.Reviewing, decision: &record.ReviewDecision{State: "approved", Head: "older"},
			fact: intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, Snapshot: "green-1", Failing: []string{}}},
		{name: "the reviewer's comment on a round no review decided", at: phase.Reviewing, pr: record.PullRequest{CheckedHead: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}},
			fact: intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "commented", CommitID: "head", HeadSHA: "head", Author: testReviewApp, Body: "a thought"}},
		{name: "red checks at max_fix_attempts", at: phase.Testing, pr: record.PullRequest{FixAttempts: 3},
			fact: intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 2}}, Generation: 1, Snapshot: "red-1", Failing: []string{"ci"}}},
		{name: "red checks that send a code head back", at: phase.Testing,
			fact: intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 2}}, Generation: 1, Snapshot: "red-1", Failing: []string{"ci"}}},
		{name: "a conflicting head at awaiting_merge", at: phase.AwaitingMerge,
			fact: intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", Mergeable: record.MergeabilityConflicting}},
		{name: "changes requested at the round cap", at: phase.Reviewing,
			fact: intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "changes_requested", CommitID: "head", HeadSHA: "head", Author: testReviewApp, Body: "fix it"}},
		{name: "the worker's backward move", at: phase.Testing, refusal: "TREE_LINGERING",
			fact: intake.BackwardMove{Issue: "LEGION-209", Requester: claim.RoleTester, To: phase.Implementing, Reason: "the head changed"}},
		{name: "the architect's retry of a held phase", at: phase.Held, hold: &hold,
			fact: intake.RetryOrEscalate{Issue: "LEGION-209", Decision: intake.RetryDecision}},
		{name: "a failed claim", at: phase.Testing,
			fact: intake.ClaimFailed{Issue: "LEGION-209", Role: claim.RoleTester}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
			parent := "LEGION-208"
			seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: tc.at, Hold: tc.hold, Generation: 1, Status: "in_progress", Rank: "V"})
			pr := tc.pr
			pr.State, pr.Issue, pr.Repo, pr.Number, pr.Branch, pr.HeadSHA = record.PullRequestOpen, "LEGION-209", "sjawhar/legion", 42, "legion/LEGION-209", "head"
			// The base requires ci, the check every settlement here names, so a green or red head is
			// one the workflow would act on if the linger did not hold it.
			pr.Failing, pr.Required = []string{}, []string{"ci"}
			seedPR(t, pool, pr)
			// The implementer is one round short of the review round cap, so a counted round would
			// post the cap message and notify the architect. A review ends when both of its halves
			// are in (reviewRound), so the reviewer of a child in reviewing has completed its round,
			// and the review decides the rest.
			reviewer := record.PhaseRow{Role: claim.RoleReviewer, Decision: tc.decision}
			if tc.at == phase.Reviewing {
				reviewer.HandoffCommit = "review-handoff"
			}
			for _, row := range []record.PhaseRow{{Role: claim.RoleImplementer, Rounds: 2}, {Role: claim.RoleTester}, reviewer} {
				row.Issue, row.Claim = "LEGION-209", claim.Token(string(row.Role)+"-claim")
				seedPhase(t, pool, row)
			}
			apply := applyFacts(t, pool, readyEngine(""))

			apply("close-root", intake.DispatchIssue{Key: "LEGION-208", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.closed", Status: "done", Title: "root", Rank: "U"})
			result := apply("fact", tc.fact)
			if code := refusalCode(result); code != tc.refusal {
				t.Fatalf("refusal = %q, want %q", code, tc.refusal)
			}
			want := tc.want
			if want == "" {
				want = tc.at
			}
			var got phase.Phase
			var starts, rounds, told int
			if err := pool.QueryRow(t.Context(), `select phase,
				(select count(*) from outbox where issue = 'LEGION-209' and kind = 'supervise' and payload->>'op' = 'start'),
				(select rounds from phases where issue = 'LEGION-209' and role = 'implementer'),
				(select count(*) from outbox where issue = 'LEGION-209' and kind in ('dispatch_message', 'notice', 'controller_notice'))
				from issues where key = 'LEGION-209'`).Scan(&got, &starts, &rounds, &told); err != nil || got != want || starts != 0 || rounds != 2 || told != 0 {
				t.Fatalf("the lingering tree's LEGION-209 = %q, %d worker starts, %d rounds, %d messages and notices (the controller's included), %v; want %s, none, 2, none", got, starts, rounds, told, err, want)
			}
		})
	}
}

// A handed-over child carries a copy of its old root's gate, and the document it names still
// belongs to the old root, so every approval of it arrives keyed by the old root. A copy carried
// closed (a spec version bumped while the old tree lingered) refuses the carried merger's READY,
// and the approval of that version opens every gate on the document, the old root's and the
// copy, and releases the READY the copy refused. The old root's architect is told design-approved;
// the carried root's is told carried-gate-opened, which starts nothing of section 2's design
// approval, since its tree's design was approved before the carry.
func TestAnApprovalOfTheOldRootsDocumentReleasesACarriedChildsREADY(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "old root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U"})
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-209", Project: "LEGION", Title: "carried child", Parent: &parent, Phase: phase.Merging, Generation: 2, Status: "retro", Rank: "V"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2, ApprovedVersion: new(1)})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-209", ArtifactID: "artifact-208", LatestVersion: 2, ApprovedVersion: new(1)})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleMerger, Claim: "merger-claim"})
	engine := readyEngine("")
	packet := strings.ReplaceAll(readyPacket, "LEGION-208", "LEGION-209")

	result, err := intake.ApplyFact(ctx, pool, "api", "ready", intake.HandoffComplete{Issue: "LEGION-209", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 2, Ready: true, Summary: packet, Commit: "head"}, engine, admissionStub{})
	if err != nil || refusalCode(result) != "DESIGN_GATE_CLOSED" {
		t.Fatalf("carried READY = %#v, %v; want the closed copy's refusal", result, err)
	}
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "approval", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 2}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact approval: %v", err)
	}
	var got string
	if err := pool.QueryRow(ctx, "select phase from issues where key = 'LEGION-209'").Scan(&got); err != nil || got != string(phase.AwaitingMerge) {
		t.Fatalf("carried child after the old document's approval = %q, %v; want awaiting_merge", got, err)
	}
	if bodies := messageBodies(t, pool); len(bodies) != 1 || bodies[0] != packet {
		t.Fatalf("Dispatch messages = %q, want the carried READY once", bodies)
	}
	for _, key := range []string{"LEGION-208", "LEGION-209"} {
		var approved *int
		if err := pool.QueryRow(ctx, "select approved_version from design_gates where issue = $1", key).Scan(&approved); err != nil || approved == nil || *approved != 2 {
			t.Fatalf("%s's gate approved version = %v, %v; want 2", key, approved, err)
		}
	}
	var notices []string
	rows, err := pool.Query(ctx, `select issue || ' ' || (payload->>'kind') from outbox where kind = 'notice'
		and payload->>'kind' in ('design-approved', 'carried-gate-opened') order by id`)
	if err != nil {
		t.Fatalf("read notices: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var notice string
		if err := rows.Scan(&notice); err != nil {
			t.Fatalf("scan notice: %v", err)
		}
		notices = append(notices, notice)
	}
	if want := []string{"LEGION-208 design-approved", "LEGION-209 carried-gate-opened"}; !slices.Equal(notices, want) {
		t.Fatalf("notices = %v, want %v", notices, want)
	}
}

// A carried copy of the old root's gate takes only approvals: its work was approved under the
// design it carries, and the old document belongs to another tree now. After the carry, a new
// version of the old document, a changes request on it, or an approval of an older version leaves
// an open copy open, tells the carried architect nothing, and its merger's READY goes through, while
// the old root's own gate takes every event as before.
func TestTheOldRootsDocumentChangingAfterTheCarryLeavesAnOpenCopyOpen(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "old root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U"})
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-209", Project: "LEGION", Title: "carried child", Parent: &parent, Phase: phase.Merging, Generation: 2, Status: "retro", Rank: "V"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2, ApprovedVersion: new(2)})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-209", ArtifactID: "artifact-208", LatestVersion: 2, ApprovedVersion: new(2)})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleMerger, Claim: "merger-claim"})
	engine := readyEngine("")

	for _, event := range []intake.DispatchArtifact{
		{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactVersion, Version: 3},
		{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactChangesRequested, Version: 3, Reason: "rework the old design"},
		{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 1},
	} {
		if _, err := intake.ApplyFact(ctx, pool, "dispatch", string(event.Kind), event, engine, admissionStub{}); err != nil {
			t.Fatalf("ApplyFact %s: %v", event.Kind, err)
		}
	}
	var latest, approved int
	if err := pool.QueryRow(ctx, "select latest_version, coalesce(approved_version, 0) from design_gates where issue = 'LEGION-208'").Scan(&latest, &approved); err != nil || latest != 3 || approved != 1 {
		t.Fatalf("old root's gate = latest %d, approved %d, %v; want every event applied (latest 3, approved 1)", latest, approved, err)
	}
	var notices int
	if err := pool.QueryRow(ctx, "select count(*) from outbox where kind = 'notice' and issue = 'LEGION-209'").Scan(&notices); err != nil || notices != 0 {
		t.Fatalf("notices to the carried root = %d, %v; want none", notices, err)
	}
	packet := strings.ReplaceAll(readyPacket, "LEGION-208", "LEGION-209")
	result, err := intake.ApplyFact(ctx, pool, "api", "ready", intake.HandoffComplete{Issue: "LEGION-209", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 2, Ready: true, Summary: packet, Commit: "head"}, engine, admissionStub{})
	if err != nil || result.Refusal != nil {
		t.Fatalf("carried READY after the old document changed = %#v, %v; want it accepted", result, err)
	}
	var got string
	if err := pool.QueryRow(ctx, "select phase from issues where key = 'LEGION-209'").Scan(&got); err != nil || got != string(phase.AwaitingMerge) {
		t.Fatalf("carried child = %q, %v; want awaiting_merge", got, err)
	}
}

// refusalCode is the code of a fact's refusal, or "" when it was not refused.
func refusalCode(result intake.Result) string {
	if result.Refusal == nil {
		return ""
	}
	return result.Refusal.Code
}

func readyEngine(mergeQueue string) *Engine {
	return New(record.NewStore(), Config{
		Project: "LEGION", DesignGate: config.DesignGateRootIssues, MergeQueueRole: mergeQueue, Linger: time.Hour, ReviewAppLogin: testReviewApp, Repositories: testRepositories,
		Clock: func() time.Time { return time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC) },
	}, nil)
}

// applyFacts applies each fact to pool through engine, failing the test on an error, and returns
// the fact's result.
func applyFacts(t *testing.T, pool *pgxpool.Pool, engine *Engine) func(id string, fact intake.Fact) intake.Result {
	return func(id string, fact intake.Fact) intake.Result {
		t.Helper()
		result, err := intake.ApplyFact(context.Background(), pool, "api", id, fact, engine, admissionStub{})
		if err != nil {
			t.Fatalf("ApplyFact %s: %v", id, err)
		}
		return result
	}
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
