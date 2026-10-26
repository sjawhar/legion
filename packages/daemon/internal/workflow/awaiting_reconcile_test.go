package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// awaitRootView is a slotted admitted root whose architect handed a question back at 5, with its
// architect's claim working and its task taken: the view decideAwait would enter on.
func awaitRootView() awaitView {
	root := askRoot()
	root.DeclaredSeq = 5
	return awaitView{
		Root:    root,
		Slotted: true,
		Run:     record.RoleRun{Claim: &record.RoleClaim{State: supervise.StateWorking}},
		Asks: []record.RootAsk{
			{Issue: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, Open: true, Seq: 5, WaitingOn: "human", WaitingOnSeq: 5, ByTree: true},
		},
	}
}

// decideAwait is the whole rule for whether a root waits on a person, checked in one order and
// from the record alone. Each row below is one reason it does not, and the last is the wait.
func TestDecideAwaitsTable(t *testing.T) {
	confirmed := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		view func(*awaitView)
		want act
		why  string
	}{
		{"a root in a phase of its own", func(v *awaitView) { v.Root.Phase = phase.Planning }, actStay, "not an admitted root"},
		{"a root holding no slot", func(v *awaitView) { v.Slotted = false }, actStay, "not an admitted root"},
		{"a root that left the workflow", func(v *awaitView) { v.Root.Status = "backlog" }, actStay, "not an admitted root"},
		{"a child", func(v *awaitView) { v.Root.Tree = "LEGION-1" }, actStay, "not an admitted root"},
		{"an architect that handed nothing back", func(v *awaitView) { v.Root.DeclaredSeq = 0 }, actStay, "handed nothing"},
		{"nothing left with the person", func(v *awaitView) { v.Asks[0].WaitingOn = "agent" }, actTakeIn, "still with the person"},
		{"a question the root's tree did not open", func(v *awaitView) { v.Asks[0].ByTree = false }, actTakeIn, "still with the person"},
		{"an approval request rather than a question", func(v *awaitView) { v.Asks[0].Kind = record.AskApproval }, actTakeIn, "still with the person"},
		{"nothing left with the person while a member is in a phase", func(v *awaitView) {
			v.Asks[0].WaitingOn = "agent"
			v.Members = []record.Issue{{Key: "LEGION-209", Tree: "LEGION-208", Phase: phase.Implementing, Status: "in_progress"}}
		}, actTakeIn, "still with the person"},
		{"a member of its tree in a phase", func(v *awaitView) {
			v.Members = []record.Issue{{Key: "LEGION-209", Tree: "LEGION-208", Phase: phase.Implementing, Status: "in_progress"}}
		}, actStay, "of its tree is in implementing"},
		{"a member out of the workflow", func(v *awaitView) {
			v.Members = []record.Issue{{Key: "LEGION-209", Tree: "LEGION-208", Phase: phase.Implementing, Status: "done"}}
		}, actEnter, "awaiting an answer"},
		{"a root that could not resume", func(v *awaitView) { v.Unresumable = "its Dispatch claim is held by user:alice" }, actStay, "could not resume"},
		{"an architect whose start is still queued", func(v *awaitView) {
			v.Run = record.RoleRun{Queued: []record.QueuedSupervise{{Request: record.SuperviseRequest{Op: "start"}}},
				Claim: &record.RoleClaim{State: supervise.StateWorking}}
		}, actStay, "start still queued"},
		{"an architect with no claim yet", func(v *awaitView) { v.Run = record.RoleRun{} }, actStay, "no claim yet"},
		{"an architect still starting", func(v *awaitView) {
			v.Run = record.RoleRun{Claim: &record.RoleClaim{State: supervise.StateLaunching}}
		}, actStay, "still starting"},
		{"an architect that has not taken its task", func(v *awaitView) {
			v.Run = record.RoleRun{Claim: &record.RoleClaim{State: supervise.StateWorking, Pending: &supervise.Delivery{}}}
		}, actStay, "not taken its task"},
		{"an architect working the task it took", func(v *awaitView) {
			v.Run = record.RoleRun{Claim: &record.RoleClaim{State: supervise.StateWorking, Pending: &supervise.Delivery{ConfirmedAt: confirmed}}}
		}, actEnter, "awaiting an answer"},
		{"a person who already acted after the hand-back", func(v *awaitView) { v.Asks[0].PersonSeq = 7 }, actTellInPlace, "already acted"},
		{"a person who acted before it", func(v *awaitView) { v.Asks[0].PersonSeq = 4 }, actEnter, "awaiting an answer"},
		{"a person's answer of the gate's approval request after it", func(v *awaitView) {
			v.Gate = &record.DesignGate{Issue: "LEGION-208", ArtifactID: gateDoc, LatestVersion: 1}
			v.Asks = append(v.Asks, record.RootAsk{Issue: "LEGION-208", Ask: "ask-approval", Kind: record.AskApproval, Artifact: gateDoc, Seq: 7,
				WaitingOn: "human", WaitingOnSeq: 4, ClosedBy: record.ClosedByPerson, ClosedSeq: 7, PersonAnswerSeq: 7})
		}, actEnter, "awaiting an answer"},
		{"a person's answer of another document's approval request after it", func(v *awaitView) {
			v.Gate = &record.DesignGate{Issue: "LEGION-208", ArtifactID: gateDoc, LatestVersion: 1}
			v.Asks = append(v.Asks, record.RootAsk{Issue: "LEGION-208", Ask: "ask-plan", Kind: record.AskApproval, Artifact: "artifact-plan", Seq: 7,
				WaitingOn: "human", WaitingOnSeq: 4, ClosedBy: record.ClosedByPerson, ClosedSeq: 7, PersonAnswerSeq: 7})
		}, actTellInPlace, "already acted"},
		{"a wait on the oldest question still with the person", func(v *awaitView) {
			v.Asks = append(v.Asks, record.RootAsk{Issue: "LEGION-208", Ask: "ask-older", Kind: record.AskQuestion,
				Open: true, Seq: 3, WaitingOn: "human", WaitingOnSeq: 3, ByTree: true})
		}, actEnter, "question ask-older"},
		{"a wait on the question longest on the person's turn, whatever reopened it since", func(v *awaitView) {
			v.Asks = append(v.Asks, record.RootAsk{Issue: "LEGION-208", Ask: "ask-restored", Kind: record.AskQuestion,
				Open: true, Seq: 9, WaitingOn: "human", WaitingOnSeq: 3, ClosedBy: record.ClosedBySettlement, ClosedSeq: 4, ByTree: true})
		}, actEnter, "question ask-restored"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := awaitRootView()
			tc.view(&view)
			got := decideAwait(view)
			if got.Act != tc.want || !strings.Contains(got.Why, tc.why) {
				t.Fatalf("decideAwait = %+v, want act %v because %q", got, tc.want, tc.why)
			}
			if got.Act == actEnter && got.Waiting.Since.Seq != view.Root.DeclaredSeq {
				t.Fatalf("the wait begins at %d, want the newest hand-back %d", got.Waiting.Since.Seq, view.Root.DeclaredSeq)
			}
		})
	}
}

// A root waits on its design gate when the gate is registered, armed and closed and the approval
// request it names is with the person: the wait is on the gate at its latest version, on its
// document, ahead of any question, and begins at the newest hand-back. A person's act on the
// request is told in place like any act; a review the gate holds is not an act told here (the
// daemon's own notice told it, a handout), and an open gate, a gate off, or a request on the
// agent's turn asks the person for nothing.
func TestDecideAwaitsOnTheGate(t *testing.T) {
	approved := 4
	gateView := func() awaitView {
		view := awaitRootView()
		view.Asks = []record.RootAsk{{Issue: "LEGION-208", Ask: "ask-approval", Kind: record.AskApproval, Artifact: "artifact-208", Open: true, WaitingOn: "human", Seq: 5}}
		view.Gate = &record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4}
		view.Armed = true
		return view
	}
	for _, tc := range []struct {
		name string
		view func(*awaitView)
		want act
		why  string
	}{
		{"the request with the person", func(*awaitView) {}, actEnter, "spec version 4 is awaiting a person's review"},
		{"the request beside an older question", func(v *awaitView) {
			v.Asks = append(v.Asks, record.RootAsk{Issue: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, Open: true, WaitingOn: "human", ByTree: true, Seq: 3})
		}, actEnter, "spec version 4"},
		{"the request on the agent's turn", func(v *awaitView) { v.Asks[0].WaitingOn = "agent" }, actTakeIn, "still with the person"},
		{"the request of a gate already open", func(v *awaitView) { v.Gate.ApprovedVersion = &approved }, actTakeIn, "still with the person"},
		{"the request with the design gate off", func(v *awaitView) { v.Armed = false }, actTakeIn, "still with the person"},
		{"an approval request of another document", func(v *awaitView) { v.Asks[0].Artifact = "artifact-other" }, actTakeIn, "still with the person"},
		{"a person's reply on the request after the hand-back", func(v *awaitView) { v.Asks[0].PersonSeq = 7 }, actTellInPlace, "already acted"},
		{"a changes request the gate already told after the hand-back", func(v *awaitView) {
			reason := "say more"
			v.Gate.ToldChangesVersion, v.Gate.ToldChangesReason, v.Gate.ToldChangesMark = &approved, reason, record.EventMark{Seq: 7}
		}, actEnter, "spec version 4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := gateView()
			tc.view(&view)
			got := decideAwait(view)
			if got.Act != tc.want || !strings.Contains(got.Why, tc.why) {
				t.Fatalf("decideAwait = %+v, want act %v because %q", got, tc.want, tc.why)
			}
			if got.Act == actEnter && (got.Waiting.GateVersion != 4 || got.Waiting.Artifact != "artifact-208" || got.Waiting.Ask != "" ||
				got.Waiting.Since.Seq != view.Root.DeclaredSeq) {
				t.Fatalf("the wait = %+v, want the gate at version 4 of artifact-208 from the newest hand-back %d", got.Waiting, view.Root.DeclaredSeq)
			}
		})
	}
}

// A person's act the record already holds, recorded after the hand-back, is told to the architect
// where it stands rather than starting a wait nothing would answer: the act on the subject
// question is named as that question's, one on any other ask as a reply in its thread.
func TestAnActAlreadyRecordedTellsTheArchitectInPlace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ask    record.RootAsk
		answer record.Answer
		detail string
	}{
		{"on the subject question", record.RootAsk{Issue: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, Open: true,
			Seq: 5, WaitingOn: "human", WaitingOnSeq: 5, ByTree: true, PersonSeq: 7}, record.AnsweredByAsk, "ask-q"},
		{"on another ask of the root", record.RootAsk{Issue: "LEGION-208", Ask: "ask-other", Kind: record.AskQuestion, Open: false,
			Seq: 6, PersonSeq: 7}, record.AnsweredByReply, "ask-other"},
		{"a person's answer of another ask", record.RootAsk{Issue: "LEGION-208", Ask: "ask-other", Kind: record.AskQuestion, Open: false,
			Seq: 7, ClosedBy: record.ClosedByPerson, ClosedSeq: 7, PersonAnswerSeq: 7}, record.AnsweredByReply, "ask-other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := awaitRootView()
			if tc.ask.Ask == "ask-q" {
				view.Asks[0] = tc.ask
			} else {
				view.Asks = append(view.Asks, tc.ask)
			}
			got := decideAwait(view)
			if got.Act != actTellInPlace || got.Answer != tc.answer || got.Detail != tc.detail {
				t.Fatalf("decideAwait = %+v, want it told in place as %s on %s", got, tc.answer, tc.detail)
			}
		})
	}
}

// The task an answered root's architect resumes with names what answered the wait, how its spec
// stands now, what waits on the architect and what is still with the person.
func TestTheResumeTaskNamesEveryItemStillWithThePerson(t *testing.T) {
	answered := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	version := 3
	asks := []record.RootAsk{
		{Issue: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, Open: false, ByTree: true, Seq: 8},
		{Issue: "LEGION-208", Ask: "ask-still", Kind: record.AskQuestion, Open: true, WaitingOn: "human", ByTree: true, Seq: 6},
		{Issue: "LEGION-208", Ask: "ask-yours", Kind: record.AskQuestion, Open: true, WaitingOn: "agent", ByTree: true, Seq: 7},
	}
	for _, tc := range []struct {
		name    string
		waiting record.Awaiting
		gate    *record.DesignGate
		want    []string
	}{
		{
			name:    "a person's answer with the spec approved",
			waiting: record.Awaiting{Ask: "ask-q", AnsweredAt: &answered, AnsweredBy: record.AnsweredByAsk, AnsweredDetail: "ask-q"},
			gate:    &record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: &version},
			want: []string{"A person answered your ask ask-q", "Your spec is approved at version 3",
				"Waiting on you: ask ask-yours", "Still with the person: ask ask-still", "Continue."},
		},
		{
			name:    "a changes request with the gate closed",
			waiting: record.Awaiting{Ask: "ask-q", AnsweredAt: &answered, AnsweredBy: record.AnsweredByChanges, AnsweredDetail: "version 4: \"say more\""},
			gate:    &record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4},
			want: []string{"A person requested changes to your spec on LEGION-208 at version 4: \"say more\"",
				"Your spec is not approved at version 4", "Hand each one back", "returns the slot"},
		},
		{
			name:    "a question nobody will answer any more",
			waiting: record.Awaiting{Ask: "ask-q", AnsweredAt: &answered, AnsweredBy: record.AnsweredByClosed, AnsweredDetail: "a session of your tree"},
			gate:    nil,
			want:    []string{"was closed by a session of your tree", "if you still need the decision, ask again"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := resumeTask("LEGION-208", tc.waiting, tc.gate, asks, true)
			for _, want := range tc.want {
				if !strings.Contains(task, want) {
					t.Fatalf("resume task %q, want it to name %q", task, want)
				}
			}
		})
	}
}

// With the design gate off, no approval is asked for, so the resume task never advises registering
// one.
func TestTheResumeTaskUnderNoDesignGateAdvisesNoApproval(t *testing.T) {
	answered := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	waiting := record.Awaiting{Ask: "ask-q", AnsweredAt: &answered, AnsweredBy: record.AnsweredByAsk, AnsweredDetail: "ask-q"}
	gate := &record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2}
	task := resumeTask("LEGION-208", waiting, gate, nil, false)
	if strings.Contains(task, "register_gate") {
		t.Fatalf("resume task %q, want no approval advice with the design gate off", task)
	}
}

// The task resuming a root that waited on its gate names a newer version that answered it, lists
// the closed gate's approval request with the person or on the architect's turn, and advises a
// hand-back — the request's own thread only while the request is open, re-requesting approval and
// registering it otherwise.
func TestTheResumeTaskOfAGateWait(t *testing.T) {
	answered := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	gate := &record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 5}
	for _, tc := range []struct {
		name    string
		waiting record.Awaiting
		asks    []record.RootAsk
		want    []string
		not     []string
	}{
		{
			name:    "a newer version, the request on the architect's turn",
			waiting: record.Awaiting{GateVersion: 5, Artifact: "artifact-208", AnsweredAt: &answered, AnsweredBy: record.AnsweredByVersion, AnsweredDetail: "version 5"},
			asks:    []record.RootAsk{{Issue: "LEGION-208", Ask: "ask-approval", Kind: record.AskApproval, Artifact: "artifact-208", Open: true, WaitingOn: "agent", Seq: 9}},
			want: []string{"A newer version of your spec on LEGION-208, version 5, handed the approval request back to you",
				"Waiting on you: ask ask-approval", "register that version with register_gate", "or answer in the request's thread", "returns the slot"},
		},
		{
			name:    "a person's answer of a question, the request still with the person",
			waiting: record.Awaiting{Ask: "ask-q", AnsweredAt: &answered, AnsweredBy: record.AnsweredByAsk, AnsweredDetail: "ask-q"},
			asks: []record.RootAsk{{Issue: "LEGION-208", Ask: "ask-approval", Kind: record.AskApproval, Artifact: "artifact-208", Open: true, WaitingOn: "human", Seq: 9},
				{Issue: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, ByTree: true, Seq: 10}},
			want: []string{"Still with the person: the approval request for spec version 5", "or request approval again and register that version with register_gate"},
		},
		{
			name:    "a changes request that closed the request",
			waiting: record.Awaiting{Ask: "ask-approval", AnsweredAt: &answered, AnsweredBy: record.AnsweredByChanges, AnsweredDetail: "version 5: \"say more\""},
			asks:    []record.RootAsk{{Issue: "LEGION-208", Ask: "ask-approval", Kind: record.AskApproval, Artifact: "artifact-208", Seq: 11, ClosedSeq: 11, ClosedBy: record.ClosedByPerson}},
			want:    []string{"requested changes", "request approval again and register that version with register_gate"},
			not:     []string{"request's thread", "Still with the person"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := resumeTask("LEGION-208", tc.waiting, gate, tc.asks, true)
			for _, want := range tc.want {
				if !strings.Contains(task, want) {
					t.Fatalf("resume task %q, want it to name %q", task, want)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(task, not) {
					t.Fatalf("resume task %q, want no %q", task, not)
				}
			}
		})
	}
}
