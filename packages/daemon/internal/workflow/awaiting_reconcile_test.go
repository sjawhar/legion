package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// decideAwait's table, each row on its own: a root that handed a question back to the person,
// with its architect running a turn it took, enters awaiting_person; each guard of the table keeps
// it in its slot; and an act of a person the record already holds past the hand-back tells the
// architect where it stands.
func TestDecideAwaitsTable(t *testing.T) {
	ready := record.RoleRun{Claim: &record.RoleClaim{State: supervise.StateWorking, Tree: "LEGION-1"}}
	root := record.Issue{Key: "LEGION-1", Tree: "LEGION-1", Phase: phase.Admitted, Status: "in_progress", HandedOver: true, DeclaredSeq: 10}
	question := record.RootAsk{Issue: "LEGION-1", Ask: "ask-q", Kind: "question", Open: true, WaitingOn: "human", Seq: 10, ByTree: true}
	request := record.RootAsk{Issue: "LEGION-1", Ask: "ask-approval", Kind: "approval", Open: true, WaitingOn: "human", Seq: 12}
	closedGate := &record.DesignGate{Issue: "LEGION-1", ArtifactID: "artifact-1", LatestVersion: 4, RequestAsk: "ask-approval"}
	base := func() awaitView {
		return awaitView{Root: root, Slotted: true, Run: ready, Asks: []record.RootAsk{question}, Armed: true}
	}
	since := record.EventMark{Seq: 10}
	for _, tc := range []struct {
		name   string
		view   func(v *awaitView)
		act    act
		wait   record.Awaiting
		answer record.Answer
		detail string
	}{
		{name: "a question handed back enters", act: actEnter, wait: record.Awaiting{Ask: "ask-q", Since: since}},
		{name: "a closed gate's request with the person is the subject", act: actEnter,
			view: func(v *awaitView) { v.Gate, v.Asks = closedGate, append(v.Asks, request) },
			wait: record.Awaiting{GateVersion: 4, Artifact: "artifact-1", Since: since}},
		{name: "0: not admitted", view: func(v *awaitView) { v.Root.Phase = phase.Planning }},
		{name: "0: no slot", view: func(v *awaitView) { v.Slotted = false }},
		{name: "0: lingering", view: func(v *awaitView) { at := time.Now(); v.Root.LingerUntil = &at }},
		{name: "0: a child", view: func(v *awaitView) { v.Root.Tree = "LEGION-0" }},
		{name: "0: out of the workflow", view: func(v *awaitView) { v.Root.Status = "backlog" }},
		{name: "1: nothing handed back since the handout", view: func(v *awaitView) { v.Root.DeclaredSeq = 0 }},
		{name: "2: nothing with the person", view: func(v *awaitView) { v.Asks[0].WaitingOn = "agent" }},
		{name: "2: a question outside the tree", view: func(v *awaitView) { v.Asks[0].ByTree = false }},
		{name: "2: the request of an open gate", view: func(v *awaitView) {
			open := *closedGate
			open.ApprovedVersion = new(4)
			v.Gate, v.Asks = &open, []record.RootAsk{request}
		}},
		{name: "2: the request with the gate off", view: func(v *awaitView) { v.Gate, v.Asks, v.Armed = closedGate, []record.RootAsk{request}, false }},
		{name: "3: a member mid-phase", view: func(v *awaitView) {
			v.Members = []record.Issue{{Key: "LEGION-2", Tree: "LEGION-1", Phase: phase.Implementing, Status: "in_progress", HandedOver: true}}
		}},
		{name: "3: a member at admitted holds nothing", act: actEnter, wait: record.Awaiting{Ask: "ask-q", Since: since}, view: func(v *awaitView) {
			v.Members = []record.Issue{{Key: "LEGION-2", Tree: "LEGION-1", Phase: phase.Admitted, Status: "todo"}}
		}},
		{name: "4: could not resume", view: func(v *awaitView) { v.Unresumable = "its Dispatch claim is held by user:bob" }},
		{name: "5: a start still queued", view: func(v *awaitView) {
			v.Run.Queued = []record.QueuedSupervise{{ID: 7, Request: record.SuperviseRequest{Op: "start"}}}
		}},
		{name: "5: no claim yet", view: func(v *awaitView) { v.Run.Claim = nil }},
		{name: "5: a claim still launching", view: func(v *awaitView) { v.Run.Claim = &record.RoleClaim{State: supervise.StateLaunching} }},
		{name: "5: a claim registered", view: func(v *awaitView) { v.Run.Claim = &record.RoleClaim{State: supervise.StateRegistered} }},
		{name: "5: a task not yet taken", view: func(v *awaitView) {
			v.Run.Claim = &record.RoleClaim{State: supervise.StateReady, Pending: &supervise.Delivery{Task: "resume"}}
		}},
		{name: "5: a task taken is owed nothing", act: actEnter, wait: record.Awaiting{Ask: "ask-q", Since: since}, view: func(v *awaitView) {
			v.Run.Claim = &record.RoleClaim{State: supervise.StateWorking, Pending: &supervise.Delivery{Task: "resume", ConfirmedAt: time.Now()}}
		}},
		{name: "5: a suspended architect is owed nothing", act: actEnter, wait: record.Awaiting{Ask: "ask-q", Since: since}, view: func(v *awaitView) {
			v.Run.Claim = &record.RoleClaim{State: supervise.StateSuspended}
		}},
		{name: "6a: a person's act on the question since", act: actTellInPlace, answer: record.AnsweredByAsk, detail: "ask-q",
			wait: record.Awaiting{Ask: "ask-q", Since: since}, view: func(v *awaitView) { v.Asks[0].PersonSeq = 11 }},
		{name: "6a: a person's act on the question before the hand-back", act: actEnter,
			wait: record.Awaiting{Ask: "ask-q", Since: since}, view: func(v *awaitView) { v.Asks[0].PersonSeq = 10 }},
		{name: "6a: a person's act on another question", act: actTellInPlace, answer: record.AnsweredByReply, detail: "ask-other",
			wait: record.Awaiting{Ask: "ask-q", Since: since}, view: func(v *awaitView) {
				v.Asks = append(v.Asks, record.RootAsk{Issue: "LEGION-1", Ask: "ask-other", Kind: "question", Seq: 12, PersonSeq: 12, ClosedSeq: 12, ClosedBy: record.ClosedByPerson})
			}},
		{name: "6a: a person's reply on the gate's request", act: actTellInPlace, answer: record.AnsweredByReply, detail: "ask-approval",
			wait: record.Awaiting{Ask: "ask-q", Since: since}, view: func(v *awaitView) {
				replied := request
				replied.WaitingOn, replied.PersonSeq = "agent", 13
				v.Gate, v.Asks = closedGate, append(v.Asks, replied)
			}},
		{name: "6a: a review of the gate since", act: actTellInPlace, answer: record.AnsweredByChanges, detail: `version 4: "name the rollback path"`,
			wait: record.Awaiting{GateVersion: 4, Artifact: "artifact-1", Since: since}, view: func(v *awaitView) {
				reviewed := *closedGate
				reviewed.ToldChangesVersion, reviewed.ToldChangesReason, reviewed.ToldChangesMark = new(4), "name the rollback path", record.EventMark{Seq: 14}
				v.Gate, v.Asks = &reviewed, append(v.Asks, request)
			}},
		{name: "6a: an approval since, the question still with the person", act: actTellInPlace, answer: record.AnsweredByApproval, detail: "version 4",
			wait: record.Awaiting{Ask: "ask-q", Since: since}, view: func(v *awaitView) {
				approved := *closedGate
				approved.ApprovedVersion, approved.ApprovedMark = new(4), record.EventMark{Seq: 14}
				v.Gate = &approved
			}},
		{name: "6a: a review before the hand-back", act: actEnter,
			wait: record.Awaiting{GateVersion: 4, Artifact: "artifact-1", Since: since}, view: func(v *awaitView) {
				reviewed := *closedGate
				reviewed.ToldChangesVersion, reviewed.ToldChangesReason, reviewed.ToldChangesMark = new(4), "name the rollback path", record.EventMark{Seq: 9}
				v.Gate, v.Asks = &reviewed, append(v.Asks, request)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := base()
			view.Asks = append([]record.RootAsk(nil), view.Asks...)
			if tc.view != nil {
				tc.view(&view)
			}
			got := decideAwait(view)
			if got.Act != tc.act {
				t.Fatalf("decision = %#v, want act %d", got, tc.act)
			}
			if tc.act == actStay {
				if got.Why == "" {
					t.Fatalf("decision = %#v, want a reason it stays", got)
				}
				return
			}
			if got.Waiting != tc.wait || got.Answer != tc.answer || got.Detail != tc.detail {
				t.Fatalf("decision = %#v, want wait %#v answered %q %q", got, tc.wait, tc.answer, tc.detail)
			}
		})
	}
}

// The oldest of the tree's questions with the person is a wait's subject when the gate's request is
// not with the person; the request is, whenever it is.
func TestTheWaitsSubjectIsTheRequestOrTheOldestQuestion(t *testing.T) {
	since := record.EventMark{Seq: 3}
	asks := []record.RootAsk{
		{Ask: "ask-late", Kind: "question", Open: true, WaitingOn: "human", Seq: 9, ByTree: true},
		{Ask: "ask-early", Kind: "question", Open: true, WaitingOn: "human", Seq: 4, ByTree: true},
		{Ask: "ask-approval", Kind: "approval", Open: true, WaitingOn: "human", Seq: 11},
	}
	gate := &record.DesignGate{ArtifactID: "artifact-1", LatestVersion: 2, RequestAsk: "ask-approval"}
	if got, ok := waitSubject(asks, nil, true, since); !ok || got != (record.Awaiting{Ask: "ask-early", Since: since}) {
		t.Fatalf("subject with no gate = %#v %t, want ask-early", got, ok)
	}
	if got, ok := waitSubject(asks, gate, true, since); !ok || got != (record.Awaiting{GateVersion: 2, Artifact: "artifact-1", Since: since}) {
		t.Fatalf("subject with the request with the person = %#v %t, want the gate at version 2", got, ok)
	}
	if _, ok := waitSubject(asks[2:], gate, false, since); ok {
		t.Fatal("subject with the gate off and no question = a wait, want none")
	}
}
