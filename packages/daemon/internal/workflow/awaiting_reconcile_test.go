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
			{Issue: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, Open: true, WaitingOn: "human", ByTree: true, Seq: 5},
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
		{"nothing left with the person", func(v *awaitView) { v.Asks[0].WaitingOn = "agent" }, actStay, "still with the person"},
		{"a question the root's tree did not open", func(v *awaitView) { v.Asks[0].ByTree = false }, actStay, "still with the person"},
		{"an approval request rather than a question", func(v *awaitView) { v.Asks[0].Kind = record.AskApproval }, actStay, "still with the person"},
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
		{"a wait on the oldest question still with the person", func(v *awaitView) {
			v.Asks = append(v.Asks, record.RootAsk{Issue: "LEGION-208", Ask: "ask-older", Kind: record.AskQuestion,
				Open: true, WaitingOn: "human", ByTree: true, Seq: 3})
		}, actEnter, "question ask-older"},
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
			WaitingOn: "human", ByTree: true, Seq: 5, PersonSeq: 7}, record.AnsweredByAsk, "ask-q"},
		{"on another ask of the root", record.RootAsk{Issue: "LEGION-208", Ask: "ask-other", Kind: record.AskQuestion, Open: false,
			Seq: 6, PersonSeq: 7}, record.AnsweredByReply, "ask-other"},
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
