package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// awaitingRoot is askRoot waiting on question ask-q, from the hand-back at since.
func awaitingRoot(since int64) record.Issue {
	root := askRoot()
	root.Phase = phase.AwaitingPerson
	root.Awaiting = &record.Awaiting{Ask: "ask-q", Since: record.EventMark{Seq: since}}
	return root
}

// seedQuestion records the mirror row of a tree question of LEGION-208 at seq, with the person.
func seedQuestion(t *testing.T, pool *pgxpool.Pool, ask string, seq int64) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutRootAsk(context.Background(), tx, record.RootAsk{
			Issue: "LEGION-208", Ask: ask, Kind: record.AskQuestion, Open: true, Seq: seq, WaitingOn: "human", WaitingOnSeq: seq, ByTree: true})
	})
}

// declaredSeq is what LEGION-208's record holds as its architect's newest hand-back.
func declaredSeq(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	return recordedRoot(t, pool).DeclaredSeq
}

// A question a session of the root's own tree opens is the architect handing work to the person,
// and is recorded as the hand-back the next admission pass decides on. A question anyone else
// opens, and an approval request, hand back nothing: the root is not waiting on either.
func TestOnlyATreeSessionsQuestionDeclaresAHandBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		fact intake.DispatchAskOpened
		want int64
	}{
		{"a tree session's question", intake.DispatchAskOpened{Key: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion,
			AuthorSession: askArchitect.ID, Mark: askSeq(5)}, 5},
		{"a question opened outside the tree", intake.DispatchAskOpened{Key: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion,
			AuthorSession: askOutsider.ID, Mark: askSeq(5)}, 0},
		{"the gate's approval request", intake.DispatchAskOpened{Key: "LEGION-208", Ask: "ask-approval", Kind: record.AskApproval,
			AuthorSession: askArchitect.ID, Mark: askSeq(5)}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, apply := askRig(t)
			apply("opened", tc.fact)
			if got := declaredSeq(t, pool); got != tc.want {
				t.Fatalf("declared_seq = %d, want %d", got, tc.want)
			}
			if got := recordedRoot(t, pool).Phase; got != phase.Admitted {
				t.Fatalf("phase = %s, want the root left admitted: the mirror decides nothing", got)
			}
		})
	}
}

// A hand-back is the newest one the architect made since the daemon last gave it work: the wait
// that follows begins there, so a question answered before it does not answer the wait the next
// question starts (round 22's declared_seq finding).
func TestTheNewestHandBackIsTheOneTheWaitBeginsAt(t *testing.T) {
	pool, apply := askRig(t)
	apply("q1", intake.DispatchAskOpened{Key: "LEGION-208", Ask: "ask-q1", Kind: record.AskQuestion, AuthorSession: askArchitect.ID, Mark: askSeq(5)})
	apply("q2", intake.DispatchAskOpened{Key: "LEGION-208", Ask: "ask-q2", Kind: record.AskQuestion, AuthorSession: askArchitect.ID, Mark: askSeq(9)})
	if got := declaredSeq(t, pool); got != 9 {
		t.Fatalf("declared_seq = %d, want the newest hand-back 9", got)
	}
}

// A hand-back at or below the root's last handout belongs to work the daemon already handed out
// again — a redelivered event of the generation before, or of the turn the handout started — and
// declares nothing.
func TestALateHandBackAtOrBelowTheHandoutDeclaresNothing(t *testing.T) {
	pool, apply := askRig(t)
	seedRecord(t, pool, func(tx pgx.Tx) error { return record.NewStore().StartHandout(context.Background(), tx, "LEGION-208") })
	// The root's record holds sequence 1, so the handout stands there.
	apply("old", intake.DispatchAskOpened{Key: "LEGION-208", Ask: "ask-old", Kind: record.AskQuestion, AuthorSession: askArchitect.ID, Mark: askSeq(1)})
	if got := declaredSeq(t, pool); got != 0 {
		t.Fatalf("declared_seq = %d, want nothing declared at or below the handout", got)
	}
	apply("fresh", intake.DispatchAskOpened{Key: "LEGION-208", Ask: "ask-new", Kind: record.AskQuestion, AuthorSession: askArchitect.ID, Mark: askSeq(2)})
	if got := declaredSeq(t, pool); got != 2 {
		t.Fatalf("declared_seq = %d, want the hand-back past the handout", got)
	}
}

// A reply of a tree session that leaves a question of its own tree with the person hands it back.
// A reply on an ask the root could never wait on — one another tree opened, or an approval
// request — declares nothing, whoever leaves it with the person.
func TestAReplyOnAnAskTheRootCannotWaitOnDeclaresNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed record.RootAsk
		want int64
	}{
		{"a question of the root's own tree", record.RootAsk{Issue: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion,
			Open: true, WaitingOn: "agent", WaitingOnSeq: 4, ByTree: true, Seq: 4}, 6},
		{"a question the root's tree did not open", record.RootAsk{Issue: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion,
			Open: true, WaitingOn: "agent", WaitingOnSeq: 4, Seq: 4}, 0},
		{"the gate's approval request", record.RootAsk{Issue: "LEGION-208", Ask: "ask-q", Kind: record.AskApproval,
			Open: true, WaitingOn: "agent", WaitingOnSeq: 4, ByTree: true, Seq: 4}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, apply := askRig(t)
			row := tc.seed
			seedRecord(t, pool, func(tx pgx.Tx) error { return record.NewStore().PutRootAsk(context.Background(), tx, row) })
			apply("hand-back", intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-q", WaitingOn: "human", Actor: askArchitect, Mark: askSeq(6)})
			if got := declaredSeq(t, pool); got != tc.want {
				t.Fatalf("declared_seq = %d, want %d", got, tc.want)
			}
		})
	}
}

// Dispatch's settlement restoring an ask is nobody's act: it declares no hand-back. A restoration
// that leaves the root's awaited question closed takes it away from the person, and with nothing
// else with them the wait is answered by the retraction it restored.
func TestARestoredAskDeclaresNothingAndCanEndTheWait(t *testing.T) {
	pool, apply := askRig(t)
	seedIssue(t, pool, awaitingRoot(3))
	seedQuestion(t, pool, "ask-q", 4)
	apply("restored", intake.DispatchAskRestored{Key: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, Open: false, Mark: askSeq(7)})
	root := recordedRoot(t, pool)
	if root.DeclaredSeq != 0 {
		t.Fatalf("declared_seq = %d, want a restoration to declare nothing", root.DeclaredSeq)
	}
	if root.Awaiting == nil || root.Awaiting.AnsweredAt == nil || root.Awaiting.AnsweredBy != record.AnsweredByClosed {
		t.Fatalf("awaiting = %+v, want the wait answered by the ask being closed", root.Awaiting)
	}
}

// A person's act on any ask of the root answers its wait the moment it arrives, and the act is
// named in the record: the architect may have asked the question it answers in another thread.
// An act Dispatch recorded at or before the wait began is one the person gave before the work was
// handed back to them, and answers nothing.
func TestOnlyAPersonsActAfterTheWaitBeganAnswersIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ask    string
		mark   record.EventMark
		answer record.Answer
	}{
		{"on the awaited question", "ask-q", askSeq(8), record.AnsweredByAsk},
		{"on another ask of the root", "ask-other", askSeq(8), record.AnsweredByReply},
		{"recorded before the wait began", "ask-q", askSeq(2), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, apply := askRig(t)
			seedIssue(t, pool, awaitingRoot(3))
			seedQuestion(t, pool, "ask-q", 4)
			seedQuestion(t, pool, "ask-other", 4)
			apply("answered", intake.DispatchAskClosed{Key: "LEGION-208", Ask: tc.ask, Kind: record.AskQuestion, State: "answered",
				Actor: askPerson, Mark: tc.mark})
			awaiting := recordedRoot(t, pool).Awaiting
			if tc.answer == "" {
				if awaiting == nil || awaiting.AnsweredAt != nil {
					t.Fatalf("awaiting = %+v, want the root still waiting", awaiting)
				}
				return
			}
			if awaiting == nil || awaiting.AnsweredAt == nil || awaiting.AnsweredBy != tc.answer || awaiting.AnsweredDetail != tc.ask {
				t.Fatalf("awaiting = %+v, want it answered by %s on %s", awaiting, tc.answer, tc.ask)
			}
		})
	}
}

// When an item of the wait leaves the person by an act that is nobody's answer, the wait moves to
// what is still with them, and is answered only once nothing is. Here a session of the tree hands
// the awaited question back to the agent while a second question is still with the person: the
// wait moves to that question, and the person's answer of it ends the wait.
func TestAnItemLeavingThePersonMovesTheWait(t *testing.T) {
	pool, apply := askRig(t)
	seedIssue(t, pool, awaitingRoot(3))
	seedQuestion(t, pool, "ask-q", 4)
	seedQuestion(t, pool, "ask-second", 5)
	apply("note", intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-q", WaitingOn: "agent", Actor: askArchitect, Mark: askSeq(8)})
	moved := recordedRoot(t, pool).Awaiting
	if moved == nil || moved.AnsweredAt != nil || moved.Ask != "ask-second" {
		t.Fatalf("awaiting = %+v, want the wait moved to ask-second and unanswered", moved)
	}
	apply("answered", intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-second", Kind: record.AskQuestion, State: "answered",
		Actor: askPerson, Mark: askSeq(9)})
	answered := recordedRoot(t, pool).Awaiting
	if answered == nil || answered.AnsweredAt == nil || answered.AnsweredBy != record.AnsweredByAsk {
		t.Fatalf("awaiting = %+v, want the moved wait answered", answered)
	}
}

// A session handing the last question of the wait to the agent's turn leaves the person asked
// nothing, so the wait is answered: the architect looks again rather than waiting for good.
func TestASessionHandingTheLastQuestionToTheAgentAnswersTheWait(t *testing.T) {
	pool, apply := askRig(t)
	seedIssue(t, pool, awaitingRoot(3))
	seedQuestion(t, pool, "ask-q", 4)
	apply("note", intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-q", WaitingOn: "agent", Actor: askArchitect, Mark: askSeq(8)})
	awaiting := recordedRoot(t, pool).Awaiting
	if awaiting == nil || awaiting.AnsweredAt == nil || awaiting.AnsweredBy != record.AnsweredByTurn {
		t.Fatalf("awaiting = %+v, want the wait answered by the turn moving to the agent", awaiting)
	}
}

// A close of the awaited question by anyone who is not a person ends the wait when nothing else is
// with the person, named by who closed it, so the resume task can say so.
func TestACloseOfTheAwaitedQuestionAnswersTheWaitWithItsCloser(t *testing.T) {
	for _, tc := range []struct {
		name   string
		actor  dispatch.Actor
		answer record.Answer
		detail string
	}{
		{"a session of the root's tree", askArchitect, record.AnsweredByClosed, "a session of your tree"},
		{"a session outside it", askOutsider, record.AnsweredByClosed, "someone outside your tree"},
		{"Dispatch's settlement", dispatch.SettlementActor, record.AnsweredByRetraction, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, apply := askRig(t)
			seedIssue(t, pool, awaitingRoot(3))
			seedQuestion(t, pool, "ask-q", 4)
			apply("closed", intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, State: "resolved",
				Actor: tc.actor, Mark: askSeq(8)})
			awaiting := recordedRoot(t, pool).Awaiting
			if awaiting == nil || awaiting.AnsweredAt == nil || awaiting.AnsweredBy != tc.answer || awaiting.AnsweredDetail != tc.detail {
				t.Fatalf("awaiting = %+v, want it answered by %s (%q)", awaiting, tc.answer, tc.detail)
			}
		})
	}
}

// A person's review of the root's spec answers a root waiting on its architect's question: they
// have decided what the work depends on, and the resume task says how the spec stands. A review
// Dispatch recorded before the wait began answers nothing.
func TestAPersonsReviewOfTheSpecAnswersAQuestionWait(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   intake.DispatchArtifactKind
		mark   record.EventMark
		answer record.Answer
	}{
		{"an approval", intake.DispatchArtifactApproved, askSeq(8), record.AnsweredByApproval},
		{"a changes request", intake.DispatchArtifactChangesRequested, askSeq(8), record.AnsweredByChanges},
		{"a review given before the wait began", intake.DispatchArtifactApproved, askSeq(2), ""},
		{"a new version of the spec", intake.DispatchArtifactVersion, askSeq(8), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, apply := askRig(t)
			seedIssue(t, pool, awaitingRoot(3))
			seedQuestion(t, pool, "ask-q", 4)
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2})
			apply("review", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: tc.kind, Version: 2,
				Reason: "say more", Mark: tc.mark})
			awaiting := recordedRoot(t, pool).Awaiting
			if tc.answer == "" {
				if awaiting == nil || awaiting.AnsweredAt != nil {
					t.Fatalf("awaiting = %+v, want the root still waiting", awaiting)
				}
				return
			}
			if awaiting == nil || awaiting.AnsweredAt == nil || awaiting.AnsweredBy != tc.answer {
				t.Fatalf("awaiting = %+v, want it answered by %s", awaiting, tc.answer)
			}
		})
	}
}

// A changes request that answered a root's wait is named by the task its architect resumes with,
// so no design-changes-requested notice is sent for it as well.
func TestAChangesRequestThatAnswersAWaitIsNotAlsoToldAsANotice(t *testing.T) {
	pool, apply := askRig(t)
	seedIssue(t, pool, awaitingRoot(3))
	seedQuestion(t, pool, "ask-q", 4)
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2})
	before := outboxRows(t, pool)
	apply("changes", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactChangesRequested,
		Version: 2, Reason: "say more", Mark: askSeq(8)})
	if got := outboxRows(t, pool); got != before {
		t.Fatalf("outbox rows = %d, want no notice beside the answered wait (%d)", got, before)
	}
}

// A changes request told to a root that holds its slot is work the daemon handed its architect, so
// it is a handout: a hand-back of the turn before it declares nothing afterwards.
func TestAChangesRequestToldByNoticeIsAHandout(t *testing.T) {
	pool, apply := askRig(t)
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2})
	apply("q", intake.DispatchAskOpened{Key: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, AuthorSession: askArchitect.ID, Mark: askSeq(5)})
	if got := declaredSeq(t, pool); got != 5 {
		t.Fatalf("declared_seq = %d, want the hand-back recorded", got)
	}
	apply("changes", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactChangesRequested,
		Version: 2, Reason: "say more", Mark: askSeq(8)})
	root := recordedRoot(t, pool)
	if root.DeclaredSeq != 0 {
		t.Fatalf("declared_seq = %d, want the notice to clear what was handed back", root.DeclaredSeq)
	}
	if root.HandoutSeq < 8 {
		t.Fatalf("handout_seq = %d, want it at the notice's review (8) or past it", root.HandoutSeq)
	}
}

// A changes request a read gives no sequence to cannot be placed against a root's wait: it is
// neither told nor recorded as told, and its own sequenced event decides later.
func TestAnUnplaceableChangesRequestIsNeitherToldNorRecorded(t *testing.T) {
	pool, apply := askRig(t)
	seedIssue(t, pool, awaitingRoot(3))
	seedQuestion(t, pool, "ask-q", 4)
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2})
	before := outboxRows(t, pool)
	apply("changes", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactChangesRequested,
		Version: 2, Reason: "say more", Mark: record.EventMark{At: time.Time{}}})
	if got := outboxRows(t, pool); got != before {
		t.Fatalf("outbox rows = %d, want the unplaceable request told to nobody (%d)", got, before)
	}
	if gate := recordedGate(t, pool); gate == nil || gate.ToldChangesVersion != nil {
		t.Fatalf("gate = %+v, want the request not recorded as told", gate)
	}
}

// A changes request recorded as told because the resume task would name it is no longer recorded
// that way when the root leaves the workflow before it resumed: the next admission tells it.
func TestAChangesRequestARootNeverResumedForIsNotRecordedAsTold(t *testing.T) {
	pool, apply := askRig(t)
	root := awaitingRoot(3)
	answered := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	root.Awaiting.AnsweredAt, root.Awaiting.AnsweredBy = &answered, record.AnsweredByChanges
	root.Awaiting.AnsweredDetail = "version 2: \"say more\""
	seedIssue(t, pool, root)
	version := 2
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2,
		ToldChangesVersion: &version, ToldChangesReason: "say more"})
	apply("parked", intake.DispatchIssue{Key: "LEGION-208", Seq: 20, Type: "issue.updated", Status: "backlog", Title: "root", Rank: "U"})
	if gate := recordedGate(t, pool); gate == nil || gate.ToldChangesVersion != nil {
		t.Fatalf("gate = %+v, want the told record cleared with the root's leaving", gate)
	}
}
