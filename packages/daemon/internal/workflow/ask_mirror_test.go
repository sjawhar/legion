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
// and its state is the newest event's. The mirror starts nothing by itself: the only part of the
// root's record it moves is the hand-back its architect made (DeclaredSeq, which admission decides
// on), and the outbox is as it was.
func TestAsksOfARootAreMirroredWhateverTheirOrder(t *testing.T) {
	events := []intake.Fact{
		intake.DispatchAskOpened{Key: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, AuthorSession: askArchitect.ID, Mark: askSeq(5)},
		intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-q", WaitingOn: "agent", Actor: askPerson, Mark: askSeq(6)},
		intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-q", WaitingOn: "human", Actor: askArchitect, Mark: askSeq(7)},
		intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-q", State: "resolved", Actor: askArchitect, Mark: askSeq(8)},
	}
	want := record.RootAsk{Issue: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, Open: false, Seq: 8, ByTree: true, PersonSeq: 6,
		ClosedSeq: 8, ClosedBy: record.ClosedByTree}
	for _, order := range [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {1, 3, 0, 2}} {
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
			// The turn is the newest event's state: a closed row keeps the turn it last had, which
			// depends on which open events the row saw before its close.
			got.WaitingOn = ""
			if *got != want {
				t.Fatalf("mirror row = %#v, want %#v", *got, want)
			}
			after := recordedRoot(t, pool)
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
// the person's when no event named one. Nobody acted, so the restore records no person's act.
func TestAClosedAskKeepsItsTurnAndARestoredAskReopensOnIt(t *testing.T) {
	pool, apply := askRig(t)
	apply("opened", intake.DispatchAskOpened{Key: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, AuthorSession: askArchitect.ID, Mark: askSeq(5)})
	apply("note", intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-q", WaitingOn: "agent", Actor: askArchitect, Mark: askSeq(6)})
	apply("retracted", intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-q", State: "resolved", Actor: dispatch.SettlementActor, Mark: askSeq(7)})
	if got := mirroredAsk(t, pool, "ask-q"); got.Open || got.WaitingOn != "agent" || got.ClosedBy != record.ClosedBySettlement {
		t.Fatalf("retracted ask = %#v, want it closed by settlement on the agent's turn it last had", got)
	}
	apply("restored", intake.DispatchAskRestored{Key: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, Open: true, Mark: askSeq(8)})
	want := record.RootAsk{Issue: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, Open: true, WaitingOn: "agent", Seq: 8, ByTree: true, ClosedSeq: 7}
	if got := mirroredAsk(t, pool, "ask-q"); *got != want {
		t.Fatalf("restored ask = %#v, want %#v", *got, want)
	}

	// An ask the mirror saw only closed has no turn: restored, it is with the person.
	apply("closed-only", intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-r", State: "resolved", Actor: dispatch.SettlementActor, Mark: askSeq(9)})
	apply("restored-r", intake.DispatchAskRestored{Key: "LEGION-208", Ask: "ask-r", Open: true, Mark: askSeq(10)})
	if got := mirroredAsk(t, pool, "ask-r"); !got.WithHuman() || got.PersonSeq != 0 {
		t.Fatalf("restored ask with no turn = %#v, want it open on the person's turn with no person's act", got)
	}
	// A restore of the answer closes it again and keeps the turn.
	apply("re-answered", intake.DispatchAskRestored{Key: "LEGION-208", Ask: "ask-q", Mark: askSeq(11)})
	if got := mirroredAsk(t, pool, "ask-q"); got.Open || got.WaitingOn != "agent" || got.ClosedBy != record.ClosedByOther || got.PersonSeq != 0 {
		t.Fatalf("ask closed again by a restore = %#v, want it closed on its turn with no person's act", got)
	}
}

// Every act of a person that would answer a root waiting on them is recorded on the ask's row, by
// the one rule (personAnswers): an answer or a resolution, of a question or of an approval ask
// that is not the gate's request, a resolution of the gate's request, and a reply that does not
// leave the ask with the person, a comment on a closed ask among them. A person's answer of the
// gate's request is a review, recorded through its own artifact event; a person's reply on their
// own turn and a session's act are no person's act.
func TestAPersonsActIsRecordedWhereverItWouldAnswer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		act   intake.Fact
		acted bool
	}{
		{name: "an answer of a question", act: intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-x", Kind: record.AskQuestion, State: "answered", Actor: askPerson, Mark: askSeq(20)}, acted: true},
		{name: "a resolution of a question", act: intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-x", Kind: record.AskQuestion, State: "resolved", Actor: askPerson, Mark: askSeq(20)}, acted: true},
		{name: "an answer of another approval ask", act: intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-x", Kind: record.AskApproval, State: "answered", Actor: askPerson, Mark: askSeq(20)}, acted: true},
		{name: "a resolution of the gate's request", act: intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-approval", Kind: record.AskApproval, State: "resolved", Actor: askPerson, Mark: askSeq(20)}, acted: true},
		{name: "a comment on a closed ask", act: intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-x", Actor: askPerson, Mark: askSeq(20)}, acted: true},
		{name: "a reply handing the ask to the agent", act: intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-x", WaitingOn: "agent", Actor: askPerson, Mark: askSeq(20)}, acted: true},
		{name: "an answer of the gate's request", act: intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-approval", Kind: record.AskApproval, State: "answered", Actor: askPerson, Mark: askSeq(20)}},
		{name: "a person's reply on their own turn", act: intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-x", WaitingOn: "human", Actor: askPerson, Mark: askSeq(20)}},
		{name: "a session's answer", act: intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-x", Kind: record.AskQuestion, State: "answered", Actor: askOutsider, Mark: askSeq(20)}},
		{name: "settlement's retraction", act: intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-x", Kind: record.AskQuestion, State: "resolved", Actor: dispatch.SettlementActor, Mark: askSeq(20)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, apply := askRig(t)
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2, RequestAsk: "ask-approval"})
			apply("act", tc.act)
			ask := "ask-x"
			if closed, ok := tc.act.(intake.DispatchAskClosed); ok {
				ask = closed.Ask
			}
			got := mirroredAsk(t, pool, ask)
			if got == nil {
				t.Fatal("the act left no mirror row")
			}
			if acted := got.PersonSeq == 20; acted != tc.acted {
				t.Fatalf("%s recorded person_seq %d, want a person's act recorded: %t", tc.name, got.PersonSeq, tc.acted)
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
	want := record.RootAsk{Issue: "LEGION-208", Ask: "ask-old", Seq: 6, PersonSeq: 6, ClosedSeq: 6, ClosedBy: record.ClosedByPerson, WaitingOn: "human"}
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
	var rows int
	if err := pool.QueryRow(context.Background(), `select count(*) from root_asks`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("mirror rows = %d, %v; want none", rows, err)
	}
}

// A registration binds the gate to the approval request its read found open and writes that
// request's mirror row as the read found it, at the sequence the read began at: a hand-back at or
// below it, delivered after, is older than the row and changes nothing, and a later note moves it.
// A read that found the request waiting on the agent mirrors that turn.
func TestARegistrationMirrorsTheRequestItReadAtItsSequence(t *testing.T) {
	pool, apply := askRig(t)
	apply("register", intake.GateRegistered{Issue: "LEGION-208", ArtifactID: "artifact-208", Version: 2, DispatchVersion: 2,
		RequestAsk: "ask-approval", Since: askSeq(10)})
	if gate := recordedGate(t, pool); gate == nil || gate.RequestAsk != "ask-approval" {
		t.Fatalf("gate = %#v, want it bound to ask-approval", gate)
	}
	want := record.RootAsk{Issue: "LEGION-208", Ask: "ask-approval", Kind: record.AskApproval, Open: true, WaitingOn: "human", Seq: 10}
	if got := mirroredAsk(t, pool, "ask-approval"); got == nil || *got != want {
		t.Fatalf("request row = %#v, want %#v", got, want)
	}
	apply("late-hand-back", intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-approval", Kind: record.AskApproval, WaitingOn: "agent", Actor: askArchitect, Mark: askSeq(9)})
	if got := mirroredAsk(t, pool, "ask-approval"); *got != want {
		t.Fatalf("request row after an event the read covered = %#v, want %#v", *got, want)
	}
	apply("note", intake.DispatchAskReply{Key: "LEGION-208", Ask: "ask-approval", WaitingOn: "agent", Actor: askArchitect, Mark: askSeq(11)})
	if got := mirroredAsk(t, pool, "ask-approval"); got.WaitingOn != "agent" || got.Seq != 11 {
		t.Fatalf("request row after a later note = %#v, want it on the agent's turn at 11", got)
	}

	other, applyOther := askRig(t)
	applyOther("register", intake.GateRegistered{Issue: "LEGION-208", ArtifactID: "artifact-208", Version: 2, DispatchVersion: 2,
		RequestAsk: "ask-approval", RequestWaitsOnAgent: true, Since: askSeq(10)})
	if got := mirroredAsk(t, other, "ask-approval"); got == nil || got.WaitingOn != "agent" || !got.Open {
		t.Fatalf("request row read waiting on the agent = %#v, want it open on the agent's turn", got)
	}
}

// A registration whose read found no request open keeps the request the gate names for the same
// document, since the request may be answered or closed rather than gone; only a registration of
// another document rebinds it, to that document's open request or to none.
func TestARegistrationThatFindsNoOpenRequestKeepsTheSameDocumentsRequest(t *testing.T) {
	pool, apply := askRig(t)
	apply("register-a", intake.GateRegistered{Issue: "LEGION-208", ArtifactID: "artifact-a", Version: 2, DispatchVersion: 2, RequestAsk: "ask-a", Since: askSeq(10)})
	apply("register-a-again", intake.GateRegistered{Issue: "LEGION-208", ArtifactID: "artifact-a", Version: 3, DispatchVersion: 3, Since: askSeq(14)})
	if gate := recordedGate(t, pool); gate.RequestAsk != "ask-a" || gate.LatestVersion != 3 {
		t.Fatalf("gate after a registration of the same document finding no request = %#v, want ask-a kept at version 3", gate)
	}
	apply("register-b", intake.GateRegistered{Issue: "LEGION-208", ArtifactID: "artifact-b", Version: 1, DispatchVersion: 1, Since: askSeq(16)})
	if gate := recordedGate(t, pool); gate.RequestAsk != "" || gate.ArtifactID != "artifact-b" {
		t.Fatalf("gate after a registration of another document finding no request = %#v, want no request", gate)
	}
}

// A registration of another document rebinds the gate to that document's open request and writes
// its row; the earlier request's row stays as Dispatch last showed it.
func TestARegistrationOfAnotherDocumentRebindsTheRequest(t *testing.T) {
	pool, apply := askRig(t)
	apply("register-a", intake.GateRegistered{Issue: "LEGION-208", ArtifactID: "artifact-a", Version: 2, DispatchVersion: 2, RequestAsk: "ask-a", Since: askSeq(10)})
	apply("register-b", intake.GateRegistered{Issue: "LEGION-208", ArtifactID: "artifact-b", Version: 1, DispatchVersion: 1, RequestAsk: "ask-b", Since: askSeq(12)})
	if gate := recordedGate(t, pool); gate.RequestAsk != "ask-b" {
		t.Fatalf("gate = %#v, want it rebound to ask-b", gate)
	}
	if got := mirroredAsk(t, pool, "ask-b"); got == nil || !got.WithHuman() || got.Seq != 12 {
		t.Fatalf("ask-b row = %#v, want it open with the person at 12", got)
	}
	if got := mirroredAsk(t, pool, "ask-a"); got == nil || !got.WithHuman() || got.Seq != 10 {
		t.Fatalf("ask-a row = %#v, want it as the first registration read it", got)
	}
}

// A new version of the gate's document moves its open request to the agent's turn, as Dispatch
// does; an older event than the row's, and a version with no sequence, change nothing.
func TestAVersionMovesTheRequestToTheAgentsTurn(t *testing.T) {
	pool, apply := askRig(t)
	apply("register", intake.GateRegistered{Issue: "LEGION-208", ArtifactID: "artifact-208", Version: 2, DispatchVersion: 2, RequestAsk: "ask-approval", Since: askSeq(10)})
	apply("old-version", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactVersion, Version: 2, Mark: askSeq(9)})
	apply("seed-version", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactVersion, Version: 3})
	if got := mirroredAsk(t, pool, "ask-approval"); !got.WithHuman() || got.Seq != 10 {
		t.Fatalf("request row after an older and an unsequenced version = %#v, want it with the person at 10", got)
	}
	apply("version", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactVersion, Version: 4, Mark: askSeq(12)})
	if got := mirroredAsk(t, pool, "ask-approval"); !got.Open || got.WaitingOn != "agent" || got.Seq != 12 {
		t.Fatalf("request row after a new version = %#v, want it open on the agent's turn at 12", got)
	}
}

// A gate reset to the version Dispatch holds (the gate seed found it recorded above that version)
// is still the same document's gate, so it keeps the approval request its registration bound.
func TestAGateResetToDispatchsVersionKeepsItsRequest(t *testing.T) {
	pool, apply := askRig(t)
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 5, RequestAsk: "ask-approval"})
	apply("reset", intake.GateAboveDispatch{Generation: 1, Above: 5, Read: intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208",
		Kind: intake.DispatchArtifactVersion, Version: 3}})
	if gate := recordedGate(t, pool); gate == nil || gate.LatestVersion != 3 || gate.RequestAsk != "ask-approval" {
		t.Fatalf("gate after the reset = %#v, want version 3 still bound to ask-approval", gate)
	}
}

// Settlement restoring a retracted question moves its mirror row, and nothing else: the root keeps
// its phase and record, and nothing is queued.
func TestARestoredAskIsMirroredButDeclaresNothing(t *testing.T) {
	pool, apply := askRig(t)
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutRootAsk(context.Background(), tx, record.RootAsk{Issue: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion,
			WaitingOn: "human", Seq: 6, ByTree: true, ClosedSeq: 6, ClosedBy: record.ClosedBySettlement})
	})
	before := recordedRoot(t, pool)
	apply("restored", intake.DispatchAskRestored{Key: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, Open: true, Mark: askSeq(15)})
	if got := mirroredAsk(t, pool, "ask-q"); !got.WithHuman() || got.Seq != 15 || got.ClosedBy != "" || got.PersonSeq != 0 {
		t.Fatalf("restored ask = %#v, want it open with the person at 15 and no person's act", got)
	}
	if after := recordedRoot(t, pool); !reflect.DeepEqual(after, before) || outboxRows(t, pool) != 0 {
		t.Fatalf("the restore moved the root (%#v, want %#v) or queued work", after, before)
	}
}
