package admit

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
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
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// scrum is a session outside every tree; alice, the person, is awaiting_person_answers_test.go's.
var scrum = dispatch.Actor{Kind: "session", ID: "scrum"}

// parkedRoot records LEGION-ROOT as a root a person parked: out of the workflow, lingering.
func parkedRoot(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	until := fixedNow.Add(72 * time.Hour)
	putIssue(t, pool, record.Issue{Key: "LEGION-ROOT", Tree: "LEGION-ROOT", Project: testProject, Title: "root", Phase: phase.Done,
		Generation: 1, Status: "backlog", Rank: "A", HandedOver: true, LingerUntil: &until, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"})
}

// parkedDecisionNotices is every parked-decision controller notice in the outbox, oldest first.
func parkedDecisionNotices(t *testing.T, pool *pgxpool.Pool) []effect {
	t.Helper()
	var got []effect
	for _, row := range controllerNotices(t, pool) {
		if row.payload.(record.ControllerNotice).Kind == record.ParkedDecisionNotice {
			got = append(got, row)
		}
	}
	return got
}

// architectStartRows is every architect start in the outbox, oldest first.
func architectStartRows(t *testing.T, pool *pgxpool.Pool) []record.SuperviseRequest {
	t.Helper()
	var starts []record.SuperviseRequest
	for _, effect := range effects(t, pool) {
		if start, ok := effect.payload.(record.SuperviseRequest); ok && start.Op == "start" && start.Role == claim.RoleArchitect {
			starts = append(starts, start)
		}
	}
	return starts
}

// architectStartTasks is the task of every architect start in the outbox, oldest first.
func architectStartTasks(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var tasks []string
	for _, start := range architectStartRows(t, pool) {
		tasks = append(tasks, start.Task)
	}
	return tasks
}

// readmitRoot sets the parked LEGION-ROOT back to todo, as a person or the controller does.
func readmitRoot(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
	t.Helper()
	apply(t, pool, admission, "readmitted", intake.DispatchIssue{Key: "LEGION-ROOT", ResolvedRepo: "acme/widgets", Seq: 5, Type: "issue.updated",
		Status: "todo", Title: "root", Rank: "A", HandedOver: true}, admission.engine)
}

// A person's answer on an ask of a parked tree goes to the controller, once however often its
// event is replayed; a session's reply sends nothing and is logged, on the agent's turn and under
// a closed ask alike, a reply that leaves the turn with the human sends nothing, and an answer to
// an approval ask (the gate relays that decision) sends nothing.
func TestAPersonsAnswerOnAParkedTreeGoesToTheController(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	var logged bytes.Buffer
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(&logged, nil)))
	parkedRoot(t, pool)
	for _, step := range []struct {
		id   string
		fact intake.Fact
	}{
		{"dispatch-41", intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-1", Kind: "question", State: "answered", Actor: alice}},
		{"dispatch-41", intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-1", Kind: "question", State: "answered", Actor: alice}},
		{"dispatch-42", intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-1", WaitingOn: "agent", Actor: scrum, Comment: "c-1"}},
		{"dispatch-43", intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-1", WaitingOn: "human", Actor: alice, Comment: "c-2"}},
		{"dispatch-44", intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-approval", Kind: "approval", State: "answered", Actor: alice}},
		{"dispatch-45", intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-2", Kind: "", State: "answered", Actor: alice}},
		{"dispatch-46", intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-3", Kind: "question", State: "resolved", Actor: scrum}},
		{"dispatch-47", intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-3", WaitingOn: "", Actor: scrum, Comment: "c-3"}},
	} {
		if _, err := intake.ApplyFact(ctx, pool, "dispatch", step.id, step.fact, admission.engine, admission); err != nil {
			t.Fatalf("apply %s: %v", step.id, err)
		}
	}
	want := []effect{{kind: record.OutboxKindControllerNotice, issue: "LEGION-ROOT", payload: record.ControllerNotice{
		Kind: record.ParkedDecisionNotice, Decision: record.DecisionAskAnswered, Root: "LEGION-ROOT", Ref: "dispatch://LEGION-ROOT/ask/ask-1", Actor: "user:alice"}}}
	if got := parkedDecisionNotices(t, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("parked-decision notices = %#v, want %#v", got, want)
	}
	if n := strings.Count(logged.String(), `msg="workflow: a session's reply on a parked tree is not relayed"`); n != 2 || !strings.Contains(logged.String(), "actor=session:scrum") ||
		!strings.Contains(logged.String(), "ask=ask-1") || !strings.Contains(logged.String(), "ask=ask-3") {
		t.Fatalf("log %q; want both session replies logged, naming session:scrum, ask-1 and ask-3", logged.String())
	}
	if n := strings.Count(logged.String(), `msg="workflow: a person's decision on a parked tree goes to the controller"`); n != 1 ||
		!strings.Contains(logged.String(), "decision=ask-answered ref=dispatch://LEGION-ROOT/ask/ask-1 actor=user:alice") {
		t.Fatalf("log %q; want the relay logged once with its decision, ref and actor", logged.String())
	}
}

// A person's resolution of a question on a parked tree is relayed as ask-resolved, and a person's
// agent-turn reply, or reply under a closed ask, as ask-reply naming the comment.
func TestAPersonsResolutionAndReplyAreRelayed(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	parkedRoot(t, pool)
	for id, fact := range map[string]intake.Fact{
		"dispatch-51": intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-1", Kind: "question", State: "resolved", Actor: alice},
		"dispatch-52": intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-2", WaitingOn: "agent", Actor: alice, Comment: "c-2"},
		"dispatch-53": intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-3", WaitingOn: "agent", Actor: alice},
		"dispatch-54": intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-4", WaitingOn: "", Actor: alice, Comment: "c-4"},
	} {
		if _, err := intake.ApplyFact(ctx, pool, "dispatch", id, fact, admission.engine, admission); err != nil {
			t.Fatalf("apply %s: %v", id, err)
		}
	}
	got := map[string]record.DecisionKind{}
	for _, row := range parkedDecisionNotices(t, pool) {
		notice := row.payload.(record.ControllerNotice)
		got[notice.Ref] = notice.Decision
	}
	want := map[string]record.DecisionKind{
		"dispatch://LEGION-ROOT/ask/ask-1":   record.DecisionAskResolved,
		"dispatch://LEGION-ROOT/comment/c-2": record.DecisionAskReply,
		"dispatch://LEGION-ROOT/ask/ask-3":   record.DecisionAskReply,
		"dispatch://LEGION-ROOT/comment/c-4": record.DecisionAskReply,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("relayed = %v, want %v: a reply naming no comment falls back to its ask", got, want)
	}
}

// A person's reply on an ask of a parked child goes to the controller on the child's issue, naming
// the tree's root.
func TestAPersonsReplyOnAParkedChildNamesItsRoot(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	parkedRoot(t, pool)
	root := "LEGION-ROOT"
	putIssue(t, pool, record.Issue{Key: "LEGION-KID", Tree: root, Parent: &root, Project: testProject, Title: "kid", Phase: phase.Planning,
		Generation: 1, Status: "in_progress", Rank: "B", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"})
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "dispatch-61",
		intake.DispatchAskReply{Key: "LEGION-KID", Ask: "ask-9", WaitingOn: "agent", Actor: alice, Comment: "c-9"}, admission.engine, admission); err != nil {
		t.Fatalf("apply the reply: %v", err)
	}
	want := []effect{{kind: record.OutboxKindControllerNotice, issue: "LEGION-KID", payload: record.ControllerNotice{
		Kind: record.ParkedDecisionNotice, Decision: record.DecisionAskReply, Root: "LEGION-ROOT", Ref: "dispatch://LEGION-KID/comment/c-9", Actor: "user:alice"}}}
	if got := parkedDecisionNotices(t, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("parked-decision notices = %#v, want %#v", got, want)
	}
}

// On a running tree its architect hears every answer and reply, so nothing goes to the controller,
// and a session's reply there is no parked tree's to log.
func TestAnAnswerOnARunningTreeSendsNothing(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	var logged bytes.Buffer
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(&logged, nil)))
	seedSlotted(t, pool, "LEGION-ROOT", "A")
	for id, fact := range map[string]intake.Fact{
		"dispatch-71": intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-1", Kind: "question", State: "answered", Actor: alice},
		"dispatch-72": intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-1", WaitingOn: "agent", Actor: scrum, Comment: "c-1"},
		"dispatch-73": intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-1", WaitingOn: "agent", Actor: alice, Comment: "c-2"},
	} {
		if _, err := intake.ApplyFact(ctx, pool, "dispatch", id, fact, admission.engine, admission); err != nil {
			t.Fatalf("apply %s: %v", id, err)
		}
	}
	if got := parkedDecisionNotices(t, pool); len(got) != 0 {
		t.Fatalf("parked-decision notices = %#v, want none on a running tree", got)
	}
	if strings.Contains(logged.String(), "parked tree") {
		t.Fatalf("log %q; want no parked-tree line on a running tree", logged.String())
	}
}

// A root re-admitted after the daemon relayed a person's decision on it starts its architect with
// a task naming that decision's ref, and the start row names the row it read, which its delivery
// forgets (the outbox's start): until then the decision stays kept, so a start that never reaches
// its architect is not the last word on it.
func TestARootReadmittedAfterARelayedDecisionStartsItsArchitectNamingIt(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	parkedRoot(t, pool)
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "dispatch-41",
		intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-1", Kind: "question", State: "answered", Actor: alice}, admission.engine, admission); err != nil {
		t.Fatalf("apply the answer: %v", err)
	}
	readmitRoot(t, pool, admission)
	starts := architectStartRows(t, pool)
	if len(starts) != 1 || starts[0].Task != "While nobody in LEGION-ROOT's tree could hear it, a person decided on the tree: "+
		"dispatch://LEGION-ROOT/ask/ask-1 (ask-answered by user:alice). Read each before you continue." {
		t.Fatalf("architect starts %#v, want one naming dispatch://LEGION-ROOT/ask/ask-1", starts)
	}
	if kept := starts[0].Kept; kept == nil || len(kept.Decisions) != 1 || kept.Decisions[0].Ref != "dispatch://LEGION-ROOT/ask/ask-1" ||
		!kept.Decisions[0].At.Equal(fixedNow) || kept.Approved != 0 {
		t.Fatalf("the start's kept record %#v, want the one decision it named, at %s", kept, fixedNow)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		if kept, err := record.NewStore().ParkedDecisions(ctx, tx, "LEGION-ROOT"); err != nil || len(kept) != 1 {
			t.Fatalf("kept after the start %#v, %v; want the decision still kept until the start is delivered", kept, err)
		}
	})
}

// A re-admitted root that waits for its slot has no architect yet, so a person's answer or reply on
// its asks is kept for its start, not sent to the controller: the tree already runs again. A
// session's reply there keeps nothing. When the slot frees, the architect's start task names it.
func TestADecisionWhileARootWaitsForItsSlotIsNamedAtItsStart(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	var logged bytes.Buffer
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(&logged, nil)))
	seedSlotted(t, pool, "LEGION-BUSY", "A")
	parkedRoot(t, pool)
	readmitRoot(t, pool, admission)
	if tasks := architectStartTasks(t, pool); len(tasks) != 0 {
		t.Fatalf("architect starts' tasks %q, want none while the root waits for its slot", tasks)
	}
	for id, fact := range map[string]intake.Fact{
		"dispatch-81": intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-1", WaitingOn: "agent", Actor: alice, Comment: "c-1"},
		"dispatch-82": intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-1", WaitingOn: "agent", Actor: scrum, Comment: "c-2"},
	} {
		if _, err := intake.ApplyFact(ctx, pool, "dispatch", id, fact, admission.engine, admission); err != nil {
			t.Fatalf("apply %s: %v", id, err)
		}
	}
	if got := parkedDecisionNotices(t, pool); len(got) != 0 {
		t.Fatalf("parked-decision notices = %#v, want none for a tree that already runs again", got)
	}
	if strings.Contains(logged.String(), "parked tree") || strings.Count(logged.String(),
		`msg="workflow: a person's decision on a tree waiting for its slot is kept for its architect's start"`) != 1 {
		t.Fatalf("log %q; want the person's reply logged as kept once and no parked-tree line", logged.String())
	}
	apply(t, pool, admission, "busy-parked", intake.DispatchIssue{Key: "LEGION-BUSY", ResolvedRepo: "acme/widgets", Seq: 7, Type: "issue.updated",
		Status: "backlog", Title: "LEGION-BUSY", Rank: "A", HandedOver: true}, admission.engine)
	tasks := architectStartTasks(t, pool)
	if len(tasks) != 1 || tasks[0] != "While nobody in LEGION-ROOT's tree could hear it, a person decided on the tree: "+
		"dispatch://LEGION-ROOT/comment/c-1 (ask-reply by user:alice). Read each before you continue." {
		t.Fatalf("architect starts' tasks %q, want one naming dispatch://LEGION-ROOT/comment/c-1", tasks)
	}
}

// Two roots stand at todo, handed over, with nobody in their tree parked: one waiting on a person,
// whose answer resumes it with a task of its own (workflow's ResumeAwaitedRoot), and one that holds
// its slot and runs. Neither waits for a slot (record.WaitsForSlot), so a person's reply and a
// person's answer on either keep nothing and tell the controller nothing — the first fails if the
// wait on a person stops being a term of the rule, the second if the slot it holds does.
func TestADecisionOnARootThatWaitsForNoSlotKeepsNothing(t *testing.T) {
	root := record.Issue{Key: "LEGION-ROOT", Tree: "LEGION-ROOT", Project: testProject, Title: "root", Generation: 1,
		Status: "todo", Rank: "A", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	awaiting := root
	awaiting.Phase, awaiting.Awaiting = phase.AwaitingPerson, &record.Awaiting{Ask: "ask-q", Since: record.EventMark{Seq: 3}}
	slotted := root
	slotted.Phase = phase.Admitted
	for _, tc := range []struct {
		name  string
		issue record.Issue
		slot  bool
	}{
		{"a root waiting on a person that a person set to todo", awaiting, false},
		{"a slotted root still at todo", slotted, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			putIssue(t, pool, tc.issue)
			if tc.slot {
				inTx(t, pool, func(tx pgx.Tx) {
					if err := record.NewStore().PutSlot(ctx, tx, record.Slot{Issue: tc.issue.Key, Index: 0, AdmittedAt: fixedNow}); err != nil {
						t.Fatalf("give the root its slot: %v", err)
					}
				})
			}
			for _, step := range []struct {
				id   string
				fact intake.Fact
			}{
				{"dispatch-91", intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-1", WaitingOn: "agent", Actor: alice, Comment: "c-1"}},
				{"dispatch-92", intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-2", Kind: "question", State: "answered", Actor: alice}},
			} {
				if _, err := intake.ApplyFact(ctx, pool, "dispatch", step.id, step.fact, admission.engine, admission); err != nil {
					t.Fatalf("apply %s: %v", step.id, err)
				}
			}
			if got := parkedDecisionNotices(t, pool); len(got) != 0 {
				t.Fatalf("parked-decision notices = %#v, want none: the tree hears the person itself", got)
			}
			inTx(t, pool, func(tx pgx.Tx) {
				kept, err := record.NewStore().ParkedDecisions(ctx, tx, "LEGION-ROOT")
				if err != nil || len(kept) != 0 {
					t.Fatalf("kept %#v, %v; want nothing kept for a root that waits for no slot", kept, err)
				}
			})
		})
	}
}

// A root first admitted keeps nothing, so its architect starts with no task, as before.
func TestAFirstAdmissionStartsItsArchitectWithNoTask(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	apply(t, pool, admission, "first", intake.DispatchIssue{Key: "LEGION-NEW", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.created",
		Status: "todo", Title: "new", Rank: "A", HandedOver: true}, admission.engine)
	if tasks := architectStartTasks(t, pool); len(tasks) != 1 || tasks[0] != "" {
		t.Fatalf("architect starts' tasks %q, want one start with no task", tasks)
	}
}

// A person who approved a version of the spec that it has since moved past gives the tree a next
// step: re-admitted, its architect is told both versions, so it requests approval of the latest.
// The re-admission clears the old generation's gate, so the pair is kept first and named by the
// start, which leaves it kept until it is delivered. With the gate open at its latest version
// there is nothing to request, and under gates.design off no person approves anything.
func TestARootReadmittedOnAnOlderApprovalIsToldBothVersions(t *testing.T) {
	const versions = " The spec is approved at version 3 and is now at version 4: request approval of version 4, " +
		"as skill://legion-architect/references/design-gate.md (\"The armed gate\") says."
	const decided = "While nobody in LEGION-ROOT's tree could hear it, a person decided on the tree: dispatch://LEGION-ROOT/spec (design-approved by user:alice). Read each before you continue."
	for _, tc := range []struct {
		name     string
		approved int
		gate     config.DesignGate
		decision bool
		want     string
	}{
		{"an approval of an older version, relayed", 3, config.DesignGateRootIssues, true, decided + versions},
		{"an approval of an older version, found by the walk", 3, config.DesignGateRootIssues, false, strings.TrimPrefix(versions, " ")},
		{"an approval of the latest version", 4, config.DesignGateRootIssues, true, decided},
		{"gates.design off", 3, config.DesignGateOff, true, decided},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := migratedPool(t)
			engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
				Clock: func() time.Time { return fixedNow }, DesignGate: tc.gate}, nil)
			admission := New(record.NewStore(), engine, 1, testProject, nil)
			admission.now = func() time.Time { return fixedNow }
			parkedRoot(t, pool)
			inTx(t, pool, func(tx pgx.Tx) {
				records := record.NewStore()
				if err := records.PutGate(ctx, tx, record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 4, ApprovedVersion: new(tc.approved)}); err != nil {
					t.Fatalf("put the gate: %v", err)
				}
				if tc.decision {
					if err := records.AddParkedDecision(ctx, tx, record.ParkedDecision{Issue: "LEGION-ROOT", Ref: "dispatch://LEGION-ROOT/spec",
						Decision: record.DecisionDesignApproved, Actor: "user:alice", At: fixedNow}); err != nil {
						t.Fatalf("keep the relayed decision: %v", err)
					}
				}
			})
			readmitRoot(t, pool, admission)
			starts := architectStartRows(t, pool)
			if len(starts) != 1 || starts[0].Task != tc.want {
				t.Fatalf("architect starts %#v, want one whose task is %q", starts, tc.want)
			}
			named := 0
			if strings.Contains(tc.want, "request approval") {
				named = 3
			}
			if kept := starts[0].Kept; kept == nil || kept.Approved != named {
				t.Fatalf("the start's kept record %#v, want the approval it named (%d)", kept, named)
			}
			inTx(t, pool, func(tx pgx.Tx) {
				records := record.NewStore()
				if gate, err := records.Gate(ctx, tx, "LEGION-ROOT"); err != nil || gate != nil {
					t.Fatalf("gate after the re-admission = %#v, %v; want it cleared with the old generation", gate, err)
				}
				wantApproved, wantLatest := 3, 4
				if tc.approved == 4 || tc.gate == config.DesignGateOff {
					wantApproved, wantLatest = 0, 0
				}
				if approved, latest, err := records.ReadmittedApproval(ctx, tx, "LEGION-ROOT"); err != nil || approved != wantApproved || latest != wantLatest {
					t.Fatalf("kept approval after the start = %d, %d, %v; want %d, %d, which only the start's delivery forgets",
						approved, latest, err, wantApproved, wantLatest)
				}
			})
		})
	}
}

// A pair an earlier re-admission kept is dropped by a later one when no start could ever name it
// (gates.design off) or the gate holds no stale approval any more, so a later start never names a
// pair the gate has outgrown; under an armed gate with no gate row at all (an earlier re-admission
// cleared it and nothing registered since), the earlier pair stays for the start to name.
func TestAReadmissionKeepsOrDropsTheApprovalAnEarlierKept(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy config.DesignGate
		gate   *record.DesignGate
		keeps  bool
	}{
		{"an armed gate approved at its latest version", config.DesignGateRootIssues,
			&record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 4, ApprovedVersion: new(4)}, false},
		{"an armed gate approved at no version", config.DesignGateRootIssues, &record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 4}, false},
		{"an armed policy and no gate", config.DesignGateRootIssues, nil, true},
		{"gates.design off and no gate", config.DesignGateOff, nil, false},
		{"gates.design off and a stale approval", config.DesignGateOff,
			&record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 4, ApprovedVersion: new(3)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := migratedPool(t)
			engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
				Clock: func() time.Time { return fixedNow }, DesignGate: tc.policy}, nil)
			admission := New(record.NewStore(), engine, 1, testProject, nil)
			admission.now = func() time.Time { return fixedNow }
			parkedRoot(t, pool)
			inTx(t, pool, func(tx pgx.Tx) {
				records := record.NewStore()
				if err := records.KeepReadmittedApproval(ctx, tx, "LEGION-ROOT", 2, 3); err != nil {
					t.Fatalf("keep the earlier re-admission's pair: %v", err)
				}
				if tc.gate != nil {
					if err := records.PutGate(ctx, tx, *tc.gate); err != nil {
						t.Fatalf("put the gate: %v", err)
					}
				}
			})
			readmitRoot(t, pool, admission)
			want := 0
			if tc.keeps {
				want = 2
			}
			inTx(t, pool, func(tx pgx.Tx) {
				if approved, _, err := record.NewStore().ReadmittedApproval(ctx, tx, "LEGION-ROOT"); err != nil || approved != want {
					t.Fatalf("kept approval after the re-admission = %d, %v; want %d", approved, err, want)
				}
			})
		})
	}
}

// Under an armed gate, a root re-admitted on a stale approval, whose start then finishes without
// delivering, and which is parked and re-admitted again with no gate registered between, still
// starts its architect naming both versions: the second re-admission finds no gate and keeps the
// pair the first kept, which the undelivered start released.
func TestARootReadmittedTwiceWithNoStartDeliveredIsStillToldBothVersions(t *testing.T) {
	const versions = "The spec is approved at version 3 and is now at version 4: request approval of version 4, " +
		"as skill://legion-architect/references/design-gate.md (\"The armed gate\") says."
	ctx := context.Background()
	pool := migratedPool(t)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Clock: func() time.Time { return fixedNow }, DesignGate: config.DesignGateRootIssues}, nil)
	admission := New(record.NewStore(), engine, 1, testProject, nil)
	admission.now = func() time.Time { return fixedNow }
	parkedRoot(t, pool)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutGate(ctx, tx, record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 4, ApprovedVersion: new(3)}); err != nil {
			t.Fatalf("put the gate: %v", err)
		}
	})
	readmitRoot(t, pool, admission)
	if starts := architectStartRows(t, pool); len(starts) != 1 || starts[0].Task != versions {
		t.Fatalf("first architect starts %#v, want one whose task is %q", starts, versions)
	}
	// The start finishes without delivering (its tree parked again first): its row goes, and with
	// it its claim on the pair.
	if _, err := pool.Exec(ctx, "delete from outbox"); err != nil {
		t.Fatalf("finish the first start without delivering: %v", err)
	}
	if _, err := pool.Exec(ctx, "delete from slots"); err != nil {
		t.Fatalf("free the slot: %v", err)
	}
	parkedRoot(t, pool)
	apply(t, pool, admission, "readmitted-again", intake.DispatchIssue{Key: "LEGION-ROOT", ResolvedRepo: "acme/widgets", Seq: 9, Type: "issue.updated",
		Status: "todo", Title: "root", Rank: "A", HandedOver: true}, admission.engine)
	if starts := architectStartRows(t, pool); len(starts) != 1 || starts[0].Task != versions {
		t.Fatalf("second architect starts %#v, want one whose task is %q", starts, versions)
	}
}

// assertGateNotice fails unless key's one notice of kind names actor and ref.
func assertGateNotice(t *testing.T, pool *pgxpool.Pool, key string, kind record.NoticeKind, actor, ref string) record.Notice {
	t.Helper()
	got := gateNotices(t, pool, key, kind)
	if len(got) != 1 || got[0].Actor != actor || got[0].Ref != ref {
		t.Fatalf("%s notices of %s = %#v, want one naming actor %q and ref %q", kind, key, got, actor, ref)
	}
	return got[0]
}

// gates.design off opens the gate at registration with no person's review, so the design-approved
// notice names no person, though register_gate read Dispatch's approval and its reviewer.
func TestAGateOpenedByPolicyNamesNoPerson(t *testing.T) {
	pool := migratedPool(t)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Clock: func() time.Time { return fixedNow }, DesignGate: config.DesignGateOff}, nil)
	admission := New(record.NewStore(), engine, 1, testProject, nil)
	admission.now = func() time.Time { return fixedNow }
	seedSlotted(t, pool, "LEGION-ROOT", "A")
	if _, err := intake.ApplyFact(context.Background(), pool, "api", "registered-off",
		intake.GateRegistered{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 3, ApprovedVersion: 3, ReviewedBy: alice}, engine, admission); err != nil {
		t.Fatalf("register the gate: %v", err)
	}
	assertGateNotice(t, pool, "LEGION-ROOT", "design-approved", "", "dispatch://LEGION-ROOT/spec")
}

// A registration Dispatch already shows reviewed names the reviewer: an approval of the latest
// version on its design-approved notice, a changes request on its design-changes-requested notice.
func TestARegistrationAlreadyReviewedNamesTheReviewer(t *testing.T) {
	for _, tc := range []struct {
		name string
		fact intake.GateRegistered
		kind record.NoticeKind
	}{
		{"approved", intake.GateRegistered{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 3, ApprovedVersion: 3, ReviewedBy: alice}, "design-approved"},
		{"changes requested", intake.GateRegistered{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 3, ChangesRequestedVersion: 3,
			ChangesReason: "name the rollback path", ReviewedBy: alice}, "design-changes-requested"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-ROOT", "A")
			if _, err := intake.ApplyFact(context.Background(), pool, "api", "registered", tc.fact, admission.engine, admission); err != nil {
				t.Fatalf("register the gate: %v", err)
			}
			assertGateNotice(t, pool, "LEGION-ROOT", tc.kind, "user:alice", "dispatch://LEGION-ROOT/spec")
		})
	}
}

// A person's review of a registered spec reaches the architect naming who made it and where: an
// approval on design-approved, a changes request on design-changes-requested with its reason.
func TestASpecDecisionNoticeNamesWhoMadeItAndWhere(t *testing.T) {
	for _, tc := range []struct {
		name string
		fact intake.DispatchArtifact
		kind record.NoticeKind
	}{
		{"approved", intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactApproved, Version: 3, Actor: alice}, "design-approved"},
		{"changes requested", intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested, Version: 3,
			Reason: "name the rollback path", Actor: alice}, "design-changes-requested"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-ROOT", "A")
			inTx(t, pool, func(tx pgx.Tx) {
				if err := record.NewStore().PutGate(context.Background(), tx, record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 3}); err != nil {
					t.Fatalf("put the gate: %v", err)
				}
			})
			if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "review", tc.fact, admission.engine, admission); err != nil {
				t.Fatalf("apply the review: %v", err)
			}
			notice := assertGateNotice(t, pool, "LEGION-ROOT", tc.kind, "user:alice", "dispatch://LEGION-ROOT/spec")
			if notice.Version != 3 || notice.Reason != tc.fact.Reason {
				t.Fatalf("notice %#v, want version 3 and reason %q", notice, tc.fact.Reason)
			}
		})
	}
}
