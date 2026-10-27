package workflow

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

var (
	askPerson    = dispatch.Actor{Kind: "user", ID: "alice"}
	askArchitect = dispatch.Actor{Kind: "session", ID: "session-architect"}
	askOutsider  = dispatch.Actor{Kind: "session", ID: "session-elsewhere"}
)

func askSeq(seq int64) record.EventMark { return record.EventMark{Seq: seq} }

// askRoot is LEGION-208, an admitted root whose architect's session holds a claim in its tree.
func askRoot() record.Issue {
	return record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
}

// askRig is a migrated database holding askRoot and its architect's claim, and the facts applied
// to it through the engine.
func askRig(t *testing.T) (*pgxpool.Pool, func(id string, fact intake.Fact)) {
	t.Helper()
	pool := migratedPool(t)
	seedIssue(t, pool, askRoot())
	seedClaimSession(t, pool, "architect-claim", askArchitect.ID)
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
	return pool, func(id string, fact intake.Fact) {
		t.Helper()
		if result := apply(id, fact); result.Refusal != nil {
			t.Fatalf("%s refused: %#v", id, result.Refusal)
		}
	}
}

// mirroredAsk is the mirror row of ask on LEGION-208, nil when it has none.
func mirroredAsk(t *testing.T, pool *pgxpool.Pool, ask string) *record.RootAsk {
	t.Helper()
	var row *record.RootAsk
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		row, err = record.NewStore().RootAsk(context.Background(), tx, "LEGION-208", ask)
		return err
	})
	return row
}

func recordedGate(t *testing.T, pool *pgxpool.Pool) *record.DesignGate {
	t.Helper()
	var gate *record.DesignGate
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		gate, err = record.NewStore().Gate(context.Background(), tx, "LEGION-208")
		return err
	})
	return gate
}

func outboxRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox`).Scan(&rows); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	return rows
}

func recordedRoot(t *testing.T, pool *pgxpool.Pool) record.Issue {
	t.Helper()
	var issue *record.Issue
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		issue, err = record.NewStore().Issue(context.Background(), tx, "LEGION-208")
		return err
	})
	return *issue
}

// An ask's events are mirrored whatever order they arrive in: its kind and whether the root's tree
// opened it are kept from its opening, the person's act on it from every event that carries one,
// its state and its newest close are the close's, and its turn is the newest turn any event gave
// it. The mirror starts nothing by itself: the only part of the root's record it moves is the
// hand-back its architect made (DeclaredSeq, which admission decides on), and the outbox is as it
// was. The architect's reply leaving the question with the person at 7 is that hand-back whenever
// the question's opening, which says the root's tree asked it, arrived before it: its turn is the
// newest the row holds, though the close at 8 is newer. A reply that arrives before the opening
// cannot tell its question is the tree's, and the opening, older than the reply's turn, hands
// nothing back.
func TestAsksOfARootAreMirroredWhateverTheirOrder(t *testing.T) {
	events := []intake.Fact{
		intake.DispatchAskOpened{Key: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, AuthorSession: askArchitect.ID, Mark: askSeq(5)},
		intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-q", WaitingOn: "agent", Actor: askPerson, Mark: askSeq(6)},
		intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-q", WaitingOn: "human", Actor: askArchitect, Mark: askSeq(7)},
		intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-q", State: "resolved", Actor: askArchitect, Mark: askSeq(8)},
	}
	want := record.RootAsk{Issue: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, Open: false, Seq: 8, WaitingOn: "human", WaitingOnSeq: 7,
		ClosedBy: record.ClosedByTree, ClosedSeq: 8, ByTree: true, PersonSeq: 6}
	for _, tc := range []struct {
		order    []int
		declared int64
	}{{[]int{0, 1, 2, 3}, 7}, {[]int{3, 2, 1, 0}, 0}, {[]int{1, 3, 0, 2}, 7}} {
		order := tc.order
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			pool, apply := askRig(t)
			before := recordedRoot(t, pool)
			for _, i := range order {
				apply(fmt.Sprintf("event-%d", i), events[i])
			}
			got := mirroredAsk(t, pool, "ask-q")
			if got == nil {
				t.Fatal("the ask has no mirror row")
			}
			if *got != want {
				t.Fatalf("mirror row = %#v, want %#v", *got, want)
			}
			after := recordedRoot(t, pool)
			if after.DeclaredSeq != tc.declared {
				t.Fatalf("declared_seq = %d, want %d", after.DeclaredSeq, tc.declared)
			}
			after.DeclaredSeq = before.DeclaredSeq
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("the root's record moved on ask events beyond its hand-back: %#v, want %#v", after, before)
			}
			if rows := outboxRows(t, pool); rows != 0 {
				t.Fatalf("ask events queued %d outbox rows, want none", rows)
			}
		})
	}
}

// A closed ask keeps the turn it last had, and settlement's restore reopens it on that turn, or on
// the person's when no event named one, keeping the close it reopened from. Nobody acted, so the
// restore records no person's act.
func TestAClosedAskKeepsItsTurnAndARestoredAskReopensOnIt(t *testing.T) {
	pool, apply := askRig(t)
	apply("opened", intake.DispatchAskOpened{Key: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, AuthorSession: askArchitect.ID, Mark: askSeq(5)})
	apply("note", intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-q", WaitingOn: "agent", Actor: askArchitect, Mark: askSeq(6)})
	apply("retracted", intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-q", State: "resolved", Actor: dispatch.SettlementActor, Mark: askSeq(7)})
	if got := mirroredAsk(t, pool, "ask-q"); got.Open || got.WaitingOn != "agent" || got.ClosedBy != record.ClosedBySettlement {
		t.Fatalf("retracted ask = %#v, want it closed by settlement on the agent's turn it last had", got)
	}
	apply("restored", intake.DispatchAskRestored{Key: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, Open: true, Mark: askSeq(8)})
	want := record.RootAsk{Issue: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, Open: true, Seq: 8, WaitingOn: "agent", WaitingOnSeq: 6,
		ClosedBy: record.ClosedBySettlement, ClosedSeq: 7, ByTree: true}
	if got := mirroredAsk(t, pool, "ask-q"); *got != want {
		t.Fatalf("restored ask = %#v, want %#v", *got, want)
	}

	// An ask the mirror saw only closed has no turn: restored, it is with the person.
	apply("closed-only", intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-r", State: "resolved", Actor: dispatch.SettlementActor, Mark: askSeq(9)})
	apply("restored-r", intake.DispatchAskRestored{Key: "LEGION-208", Ask: "ask-r", Open: true, Mark: askSeq(10)})
	if got := mirroredAsk(t, pool, "ask-r"); !got.WithHuman() || got.PersonActSeq("") != 0 {
		t.Fatalf("restored ask with no turn = %#v, want it open on the person's turn with no person's act", got)
	}
	// A restore of the answer closes it again and keeps the turn.
	apply("re-answered", intake.DispatchAskRestored{Key: "LEGION-208", Ask: "ask-q", Mark: askSeq(11)})
	if got := mirroredAsk(t, pool, "ask-q"); got.Open || got.WaitingOn != "agent" || got.ClosedBy != record.ClosedByOther || got.PersonActSeq("") != 0 {
		t.Fatalf("ask closed again by a restore = %#v, want it closed on its turn with no person's act", got)
	}
}

// gateDoc is the document LEGION-208's design gate is registered on in these tests.
const gateDoc = "artifact-208"

// Every act of a person that would answer a root waiting on them is recorded on the ask's row, by
// the one rule (personAct): an answer or a resolution, of a question or of an approval request of
// another document than the gate's, a resolution of a request of the gate's document, and a reply
// that does not leave the ask with the person, a comment on a closed ask among them. A person's
// answer of a request of the gate's document is a review, recorded through its own artifact event;
// a person's reply on their own turn and a session's act are no person's act. The live answer of a
// root waiting on the person reads the same rule against the gate's document: the act answers the
// wait exactly when the mirror records it.
func TestAPersonsActIsRecordedWhereverItWouldAnswer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		act   intake.Fact
		acted bool
	}{
		{name: "an answer of a question", act: intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-x", Kind: record.AskQuestion, State: "answered", Actor: askPerson, Mark: askSeq(20)}, acted: true},
		{name: "a resolution of a question", act: intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-x", Kind: record.AskQuestion, State: "resolved", Actor: askPerson, Mark: askSeq(20)}, acted: true},
		{name: "an answer of another document's request", act: intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-x", Kind: record.AskApproval, Artifact: "artifact-plan", State: "answered", Actor: askPerson, Mark: askSeq(20)}, acted: true},
		{name: "a resolution of the gate's request", act: intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-x", Kind: record.AskApproval, Artifact: gateDoc, State: "resolved", Actor: askPerson, Mark: askSeq(20)}, acted: true},
		{name: "a comment on a closed ask", act: intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-x", Actor: askPerson, Mark: askSeq(20)}, acted: true},
		{name: "a reply handing the ask to the agent", act: intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-x", WaitingOn: "agent", Actor: askPerson, Mark: askSeq(20)}, acted: true},
		{name: "an answer of the gate's request", act: intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-x", Kind: record.AskApproval, Artifact: gateDoc, State: "answered", Actor: askPerson, Mark: askSeq(20)}},
		{name: "a person's reply on their own turn", act: intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-x", WaitingOn: "human", Actor: askPerson, Mark: askSeq(20)}},
		{name: "a session's answer", act: intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-x", Kind: record.AskQuestion, State: "answered", Actor: askOutsider, Mark: askSeq(20)}},
		{name: "settlement's retraction", act: intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-x", Kind: record.AskQuestion, State: "resolved", Actor: dispatch.SettlementActor, Mark: askSeq(20)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, apply := askRig(t)
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: gateDoc, LatestVersion: 2})
			seedIssue(t, pool, awaitingRoot(15))
			apply("act", tc.act)
			got := mirroredAsk(t, pool, "ask-x")
			if got == nil {
				t.Fatal("the act left no mirror row")
			}
			if acted := got.PersonActSeq(gateDoc) == 20; acted != tc.acted {
				t.Fatalf("%s left the newest person's act at %d, want a person's act recorded: %t", tc.name, got.PersonActSeq(gateDoc), tc.acted)
			}
			if answered := recordedRoot(t, pool).Awaiting.AnsweredAt != nil; answered != tc.acted {
				t.Fatalf("%s answered the root's wait: %t, want %t", tc.name, answered, tc.acted)
			}
		})
	}
}

// An ask event whose kind the daemon does not know is still mirrored, with no kind: a kind the
// table refuses must never fail the event's transaction.
func TestAnAskOfUnknownKindIsMirroredWithNoKind(t *testing.T) {
	pool, apply := askRig(t)
	apply("opened", intake.DispatchAskOpened{Key: "LEGION-208", Ask: "ask-old", Kind: "action", Mark: askSeq(5)})
	apply("answered", intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-old", Kind: "action", State: "answered", Actor: askPerson, Mark: askSeq(6)})
	want := record.RootAsk{Issue: "LEGION-208", Ask: "ask-old", Seq: 6, WaitingOn: "human", WaitingOnSeq: 5, ClosedBy: record.ClosedByPerson,
		ClosedSeq: 6, PersonAnswerSeq: 6}
	if got := mirroredAsk(t, pool, "ask-old"); got == nil || *got != want {
		t.Fatalf("ask of an unknown kind = %#v, want %#v", got, want)
	}
}

// Only a root's asks are mirrored: an ask on a child of its tree, and on an issue the daemon does
// not record, writes no row.
func TestOnlyARootsAsksAreMirrored(t *testing.T) {
	pool, apply := askRig(t)
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "V"})
	apply("child-ask", intake.DispatchAskOpened{Key: "LEGION-209", Ask: "ask-child", Kind: record.AskQuestion, AuthorSession: askArchitect.ID, Mark: askSeq(5)})
	apply("unrecorded-ask", intake.DispatchAskOpened{Key: "LEGION-300", Ask: "ask-elsewhere", Kind: record.AskQuestion, Mark: askSeq(5)})
	apply("child-move", intake.DispatchApprovalMoved{Key: "LEGION-209", Ask: "ask-child-approval", Artifact: "artifact-209", Mark: askSeq(6)})
	var rows int
	if err := pool.QueryRow(context.Background(), `select count(*) from root_asks`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("mirror rows = %d, %v; want none", rows, err)
	}
}

// A registration writes the mirror row of the approval request its read found open, as the read
// found it, at the sequence the read began at: an approval request of the registered document,
// open, on the turn the read gave. A hand-back at or below that sequence, delivered after, is
// older than the row and changes nothing, and a later note moves it. The registration binds
// nothing on the gate: the row is the request's own.
func TestARegistrationMirrorsTheRequestItReadAtItsSequence(t *testing.T) {
	pool, apply := askRig(t)
	apply("register", intake.GateRegistered{Issue: "LEGION-208", ArtifactID: gateDoc, Version: 2, DispatchVersion: 2,
		RequestAsk: "ask-approval", Since: askSeq(10)})
	want := record.RootAsk{Issue: "LEGION-208", Ask: "ask-approval", Kind: record.AskApproval, Artifact: gateDoc, Open: true, Seq: 10, WaitingOn: "human",
		WaitingOnSeq: 10}
	if got := mirroredAsk(t, pool, "ask-approval"); got == nil || *got != want {
		t.Fatalf("request row = %#v, want %#v", got, want)
	}
	apply("late-hand-back", intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-approval", Kind: record.AskApproval, Artifact: gateDoc, WaitingOn: "human",
		Actor: askArchitect, Mark: askSeq(9)})
	if got := mirroredAsk(t, pool, "ask-approval"); *got != want {
		t.Fatalf("request row after an event the read covered = %#v, want %#v", *got, want)
	}
	apply("note", intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-approval", WaitingOn: "agent", Actor: askArchitect, Mark: askSeq(11)})
	if got := mirroredAsk(t, pool, "ask-approval"); got.WaitingOn != "agent" || got.WaitingOnSeq != 11 || got.Seq != 11 {
		t.Fatalf("request row after a later note = %#v, want it on the agent's turn at 11", got)
	}

	other, applyOther := askRig(t)
	applyOther("register", intake.GateRegistered{Issue: "LEGION-208", ArtifactID: gateDoc, Version: 2, DispatchVersion: 2,
		RequestAsk: "ask-approval", RequestWaitsOnAgent: true, Since: askSeq(10)})
	if got := mirroredAsk(t, other, "ask-approval"); got == nil || got.WaitingOn != "agent" || !got.Open {
		t.Fatalf("request row read waiting on the agent = %#v, want it open on the agent's turn", got)
	}
}

// A registration that changes no gate (one older than the gate's newest version, or one whose read
// predates the review the gate holds) still mirrors the request its read found open: the row is
// the request's, and the read's write is fenced by the read's own sequence.
func TestARegistrationThatChangesNoGateStillMirrorsItsRequest(t *testing.T) {
	want := record.RootAsk{Issue: "LEGION-208", Ask: "ask-r", Kind: record.AskApproval, Artifact: gateDoc, Open: true, Seq: 10, WaitingOn: "human", WaitingOnSeq: 10}
	for name, first := range map[string]intake.Fact{
		"older than the gate's version": intake.GateRegistered{Issue: "LEGION-208", ArtifactID: gateDoc, Version: 3, DispatchVersion: 3, Since: askSeq(12)},
		"before the gate's review":      intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: gateDoc, Kind: intake.DispatchArtifactChangesRequested, Version: 2, Reason: "no", Mark: askSeq(11)},
	} {
		pool, apply := askRig(t)
		if _, ok := first.(intake.DispatchArtifact); ok {
			apply("register-first", intake.GateRegistered{Issue: "LEGION-208", ArtifactID: gateDoc, Version: 2, DispatchVersion: 2, Since: askSeq(5)})
		}
		apply("first", first)
		apply("register", intake.GateRegistered{Issue: "LEGION-208", ArtifactID: gateDoc, Version: 2, DispatchVersion: 2, RequestAsk: "ask-r", Since: askSeq(10)})
		if got := mirroredAsk(t, pool, "ask-r"); got == nil || *got != want {
			t.Errorf("%s: request row = %#v, want %#v", name, got, want)
		}
	}
}

// A version that moves an approval request past the version handed to the person moves the
// request to its agent's turn through the request's own ask.edited event, whichever request it is:
// an older move than the turn the row holds changes nothing, and the document's artifact.version
// event moves no ask at all.
func TestAVersionMovesTheRequestThroughItsOwnEdit(t *testing.T) {
	pool, apply := askRig(t)
	apply("register", intake.GateRegistered{Issue: "LEGION-208", ArtifactID: gateDoc, Version: 2, DispatchVersion: 2, RequestAsk: "ask-approval", Since: askSeq(10)})
	apply("old-move", intake.DispatchApprovalMoved{Key: "LEGION-208", Ask: "ask-approval", Artifact: gateDoc, Mark: askSeq(9)})
	apply("version", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: gateDoc, Kind: intake.DispatchArtifactVersion, Version: 3, Mark: askSeq(12)})
	if got := mirroredAsk(t, pool, "ask-approval"); !got.WithHuman() || got.WaitingOnSeq != 10 {
		t.Fatalf("request row after an older move and the document's version event = %#v, want it with the person since 10", got)
	}
	apply("move", intake.DispatchApprovalMoved{Key: "LEGION-208", Ask: "ask-approval", Artifact: gateDoc, Mark: askSeq(12)})
	if got := mirroredAsk(t, pool, "ask-approval"); !got.Open || got.Seq != 10 || got.WaitingOn != "agent" || got.WaitingOnSeq != 12 {
		t.Fatalf("request row after its move = %#v, want it open since 10 and on the agent's turn since 12", got)
	}
}

// After a changes request the architect requests approval again without registering, and
// Dispatch opens a second request on the same document. Each request is tracked by its own events:
// a person's edit moves the second request (not the first, which is closed), and a person's
// answer of either is the same review, a person's act on neither.
func TestASecondRequestAfterAChangesRequestIsTrackedByItsOwnEvents(t *testing.T) {
	pool, apply := askRig(t)
	apply("register", intake.GateRegistered{Issue: "LEGION-208", ArtifactID: gateDoc, Version: 2, DispatchVersion: 2, RequestAsk: "ask-1", Since: askSeq(10)})
	apply("answer-1", intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-1", Kind: record.AskApproval, Artifact: gateDoc, State: "answered", Actor: askPerson, Mark: askSeq(11)})
	apply("changes", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: gateDoc, Kind: intake.DispatchArtifactChangesRequested, Version: 2, Reason: "no", Mark: askSeq(12)})
	apply("v3", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: gateDoc, Kind: intake.DispatchArtifactVersion, Version: 3, Mark: askSeq(13)})
	apply("open-2", intake.DispatchAskOpened{Key: "LEGION-208", Ask: "ask-2", Kind: record.AskApproval, Artifact: gateDoc, AuthorSession: askArchitect.ID, Mark: askSeq(14)})
	apply("v4", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: gateDoc, Kind: intake.DispatchArtifactVersion, Version: 4, Mark: askSeq(15)})
	apply("move-2", intake.DispatchApprovalMoved{Key: "LEGION-208", Ask: "ask-2", Artifact: gateDoc, Mark: askSeq(15)})
	if got := mirroredAsk(t, pool, "ask-2"); got.WithHuman() || got.WaitingOn != "agent" || got.WaitingOnSeq != 15 {
		t.Errorf("ask-2 after version 4 = %#v, want it on the agent's turn since 15, as Dispatch shows it", *got)
	}
	if got := mirroredAsk(t, pool, "ask-1"); got.Open || got.WaitingOn != "human" || got.WaitingOnSeq != 10 {
		t.Errorf("ask-1 after version 4 = %#v, want it closed with the turn the registration read", *got)
	}
	apply("answer-2", intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-2", Kind: record.AskApproval, Artifact: gateDoc, State: "answered", Actor: askPerson, Mark: askSeq(16)})
	gate := recordedGate(t, pool)
	one, two := mirroredAsk(t, pool, "ask-1"), mirroredAsk(t, pool, "ask-2")
	if one.PersonActSeq(gate.ArtifactID) != 0 || two.PersonActSeq(gate.ArtifactID) != 0 || !one.GateRequestOf(gate.ArtifactID) || !two.GateRequestOf(gate.ArtifactID) {
		t.Errorf("the same review counts %d on ask-1 and %d on ask-2, want neither a person's act", one.PersonActSeq(gate.ArtifactID), two.PersonActSeq(gate.ArtifactID))
	}
}

// A version that moves the request after register_gate's read but is applied before the
// registration leaves the request on its agent's turn: the registration's older read never rolls
// the move back, whichever order the two arrive in.
func TestAMoveAppliedBeforeTheRegistrationKeepsTheAgentsTurn(t *testing.T) {
	register := intake.GateRegistered{Issue: "LEGION-208", ArtifactID: gateDoc, Version: 2, DispatchVersion: 2, RequestAsk: "ask-1", Since: askSeq(11)}
	move := intake.DispatchApprovalMoved{Key: "LEGION-208", Ask: "ask-1", Artifact: gateDoc, Mark: askSeq(12)}
	want := record.RootAsk{Issue: "LEGION-208", Ask: "ask-1", Kind: record.AskApproval, Artifact: gateDoc, Open: true, Seq: 11, WaitingOn: "agent", WaitingOnSeq: 12}
	for name, facts := range map[string][]intake.Fact{"move-first": {move, register}, "register-first": {register, move}} {
		pool, apply := askRig(t)
		for i, fact := range facts {
			apply(fmt.Sprintf("fact-%d", i), fact)
		}
		if got := mirroredAsk(t, pool, "ask-1"); got == nil || *got != want {
			t.Errorf("%s: request row = %#v, want %#v", name, got, want)
		}
	}
}

// A move can reach a request the mirror has no row of yet. It records the turn alone and says
// nothing of whether the request is open: the row is a valid one, the fact applies, and a later
// opening of the request, older than the move, still opens it and leaves the move's turn.
func TestAMoveReachingARequestWithNoRowRecordsOnlyItsTurn(t *testing.T) {
	move := intake.DispatchApprovalMoved{Key: "LEGION-208", Ask: "ask-approval", Artifact: gateDoc, Mark: askSeq(12)}
	opened := intake.DispatchAskOpened{Key: "LEGION-208", Ask: "ask-approval", Kind: record.AskApproval, Artifact: gateDoc, AuthorSession: askArchitect.ID, Mark: askSeq(10)}
	want := record.RootAsk{Issue: "LEGION-208", Ask: "ask-approval", Kind: record.AskApproval, Artifact: gateDoc, Open: true, Seq: 10, WaitingOn: "agent",
		WaitingOnSeq: 12, ByTree: true}
	for name, facts := range map[string][]intake.Fact{"move-first": {move, opened}, "opened-first": {opened, move}} {
		pool, apply := askRig(t)
		apply(fmt.Sprintf("%s-0", name), facts[0])
		if name == "move-first" {
			if got := mirroredAsk(t, pool, "ask-approval"); got == nil || got.Open || got.Seq != 0 || got.WaitingOn != "agent" || got.WithHuman() {
				t.Fatalf("request row after the move alone = %#v, want only the agent's turn at 12", got)
			}
		}
		apply(fmt.Sprintf("%s-1", name), facts[1])
		if got := mirroredAsk(t, pool, "ask-approval"); got == nil || *got != want {
			t.Fatalf("%s: request row = %#v, want %#v", name, got, want)
		}
	}
}

// Settlement restoring a retracted question moves its mirror row, and nothing else: the root keeps
// its phase and record, and nothing is queued. The reopened row keeps the close it reopened from.
func TestARestoredAskIsMirroredButDeclaresNothing(t *testing.T) {
	pool, apply := askRig(t)
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutRootAsk(context.Background(), tx, record.RootAsk{Issue: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion,
			Seq: 6, WaitingOn: "human", WaitingOnSeq: 5, ClosedBy: record.ClosedBySettlement, ClosedSeq: 6, ByTree: true})
	})
	before := recordedRoot(t, pool)
	apply("restored", intake.DispatchAskRestored{Key: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, Open: true, Mark: askSeq(15)})
	if got := mirroredAsk(t, pool, "ask-q"); !got.WithHuman() || got.Seq != 15 || got.ClosedBy != record.ClosedBySettlement || got.ClosedSeq != 6 ||
		got.PersonActSeq("") != 0 {
		t.Fatalf("restored ask = %#v, want it open with the person at 15, its close at 6 kept, and no person's act", got)
	}
	if after := recordedRoot(t, pool); !reflect.DeepEqual(after, before) || outboxRows(t, pool) != 0 {
		t.Fatalf("the restore moved the root (%#v, want %#v) or queued work", after, before)
	}
}

// A comment on an ask a person has answered, delivered before the answer, says only that the ask
// is closed: the answer, delivered after it, still records who closed the ask and where, so every
// order leaves the same row.
func TestAReplyDeliveredBeforeItsAsksCloseLeavesTheCloseToIt(t *testing.T) {
	events := []intake.Fact{
		intake.DispatchAskOpened{Key: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, AuthorSession: askArchitect.ID, Mark: askSeq(10)},
		intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, State: "answered", Actor: askPerson, Mark: askSeq(11)},
		intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-q", Actor: askArchitect, Mark: askSeq(12)},
	}
	rows := map[string]record.RootAsk{}
	for _, order := range [][]int{{0, 1, 2}, {0, 2, 1}, {2, 1, 0}} {
		pool, apply := askRig(t)
		for _, i := range order {
			apply(fmt.Sprintf("event-%d", i), events[i])
		}
		row := mirroredAsk(t, pool, "ask-q")
		if row.Open || row.ClosedBy != record.ClosedByPerson || row.ClosedSeq != 11 {
			t.Fatalf("order %v: row = %#v, want it closed by the person at 11", order, *row)
		}
		rows[fmt.Sprint(order)] = *row
	}
	if rows["[0 1 2]"] != rows["[0 2 1]"] || rows["[0 1 2]"] != rows["[2 1 0]"] {
		t.Fatalf("the same three events leave different rows: %#v", rows)
	}
}

// A restored ask reopens on the newest turn Dispatch gave it, whatever order the turn, the close
// and the restore arrive in: a reply older than the close but newer than the turn the row holds
// still sets the turn.
func TestARestoredAskReopensOnItsNewestTurnWhateverTheOrder(t *testing.T) {
	events := []intake.Fact{
		intake.DispatchAskOpened{Key: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, AuthorSession: askArchitect.ID, Mark: askSeq(5)},
		intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-q", WaitingOn: "agent", Actor: askPerson, Mark: askSeq(6)},
		intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-q", State: "resolved", Actor: dispatch.SettlementActor, Mark: askSeq(7)},
		intake.DispatchAskRestored{Key: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, Open: true, Mark: askSeq(8)},
	}
	for _, order := range [][]int{{0, 1, 2, 3}, {0, 2, 1, 3}, {2, 3, 0, 1}, {3, 2, 1, 0}} {
		pool, apply := askRig(t)
		for _, i := range order {
			apply(fmt.Sprintf("event-%d", i), events[i])
		}
		if row := mirroredAsk(t, pool, "ask-q"); !row.Open || row.WaitingOn != "agent" || row.WithHuman() {
			t.Fatalf("order %v: restored row = %#v, want it open on the agent's turn", order, *row)
		}
	}
}

// A reply on a retracted question is its thread's newest reply and records no turn, so Dispatch
// shows the question waiting on the person once settlement restores it: the mirror reopens it with
// the person too, whatever order the five events arrive in.
func TestARestoredAskAfterAReplyOnItsCloseReopensWithThePerson(t *testing.T) {
	events := []intake.Fact{
		intake.DispatchAskOpened{Key: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, AuthorSession: askArchitect.ID, Mark: askSeq(5)},
		intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-q", WaitingOn: "agent", Actor: askPerson, Mark: askSeq(6)},
		intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-q", State: "resolved", Actor: dispatch.SettlementActor, Mark: askSeq(7)},
		intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-q", Actor: askArchitect, Mark: askSeq(8)},
		intake.DispatchAskRestored{Key: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, Open: true, Mark: askSeq(9)},
	}
	for _, order := range [][]int{{0, 1, 2, 3, 4}, {0, 1, 2, 4, 3}, {4, 3, 2, 1, 0}, {3, 0, 4, 1, 2}} {
		pool, apply := askRig(t)
		for _, i := range order {
			apply(fmt.Sprintf("event-%d", i), events[i])
		}
		if row := mirroredAsk(t, pool, "ask-q"); !row.WithHuman() || row.WaitingOnSeq != 8 || row.Seq != 9 {
			t.Errorf("order %v: restored row = %#v, want it open since 9 with the person since 8", order, *row)
		}
	}
}

// A person's answer of an approval request of the gate's document is a review, never a person's
// act on the ask, whether the registration is applied before the answer or after: a person may
// approve before the architect registers, and the row carries its document from the answer itself.
func TestTheGatesRequestIsNoPersonsActWhateverTheOrder(t *testing.T) {
	register := intake.GateRegistered{Issue: "LEGION-208", ArtifactID: gateDoc, Version: 2, DispatchVersion: 2, RequestAsk: "ask-approval", Since: askSeq(10)}
	answer := intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-approval", Kind: record.AskApproval, Artifact: gateDoc, State: "answered", Actor: askPerson, Mark: askSeq(11)}
	for name, facts := range map[string][]intake.Fact{"register-first": {register, answer}, "answer-first": {answer, register}} {
		pool, apply := askRig(t)
		for i, fact := range facts {
			apply(fmt.Sprintf("fact-%d", i), fact)
		}
		row, gate := mirroredAsk(t, pool, "ask-approval"), recordedGate(t, pool)
		if row.PersonActSeq(gate.ArtifactID) != 0 || row.PersonAnswerSeq != 11 || row.Artifact != gateDoc {
			t.Fatalf("%s: the gate's request row = %#v, want its answer at 11 kept and no person's act on it", name, *row)
		}
	}
}
