package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// eventLogDispatch is chainDispatch with each issue's Dispatch event log, as EventsAfter serves it,
// in logs; a key it holds no log of is served an empty log, and every read fails with Dispatch's
// trouble while logErr is set. logReads counts each key's EventsAfter calls.
type eventLogDispatch struct {
	chainDispatch
	logs     map[string][]json.RawMessage
	logErr   error
	logReads map[string]int
}

func (d *eventLogDispatch) EventsAfter(_ context.Context, key string, _ int64) ([]json.RawMessage, error) {
	if d.logReads == nil {
		d.logReads = map[string]int{}
	}
	d.logReads[key]++
	if d.logErr != nil {
		return nil, d.logErr
	}
	return d.logs[key], nil
}

// issueLogEvent is one issue.created or issue.updated event of key's Dispatch event log, as
// EventsAfter serves it and the durable stream carries it: at seq, written by an actor of kind,
// with the issue in triage carrying labels.
func issueLogEvent(key string, seq int, typ, kind string, labels ...string) json.RawMessage {
	quoted := make([]string, len(labels))
	for i, label := range labels {
		quoted[i] = fmt.Sprintf("%q", label)
	}
	return json.RawMessage(fmt.Sprintf(`{"id":%d,"issue_key":%q,"seq":%d,"type":%q,"actor":{"kind":%q,"id":"%s-1"},"notify":true,"payload":{"key":%q,"status":"triage","title":%q,"labels":[%s]}}`,
		seq, key, seq, typ, kind, kind, key, key, strings.Join(quoted, ",")))
}

// labelledBy is the event log of an issue a person created unlabelled and an actor of kind then
// labelled.
func labelledBy(key, kind string) []json.RawMessage {
	return []json.RawMessage{issueLogEvent(key, 1, "issue.created", "user"), issueLogEvent(key, 2, "issue.updated", kind, "legion")}
}

// An event of an unrecorded root in triage that carries the label is prepared with who set the
// label (intake.DispatchIssue.LabelSetter), read from the issue's whole Dispatch event log in one
// read: a person's label reads "user", a session's "session". Nothing else reads the log — an
// unlabelled triage root, a labelled triage child, a labelled todo root, and a root the daemon
// records that a person moved back to triage, which is never queued again — and a read that fails,
// or a log that does not decode, fails the preparation naming the issue, so the event is delivered
// again rather than applied with a setter nobody read.
func TestPrepareIssueReadsWhoSetTheLabelOfAnUnrecordedTriageRoot(t *testing.T) {
	const key = "LEGION-755"
	triageRoot := intake.DispatchIssue{Key: key, Seq: 2, Type: "issue.updated", Status: "triage", Title: key, Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets"}
	pool := isolatedOutboxPool(t)
	for _, tc := range []struct {
		name     string
		event    func(issue intake.DispatchIssue) intake.DispatchIssue
		recorded bool
		logs     map[string][]json.RawMessage
		logErr   error
		reads    int
		want     string
		fails    string
	}{
		{name: "a person's label on an unrecorded triage root", event: func(i intake.DispatchIssue) intake.DispatchIssue { return i },
			logs: map[string][]json.RawMessage{key: labelledBy(key, "user")}, reads: 1, want: "user"},
		{name: "a session's label on an unrecorded triage root", event: func(i intake.DispatchIssue) intake.DispatchIssue { return i },
			logs: map[string][]json.RawMessage{key: labelledBy(key, "session")}, reads: 1, want: "session"},
		{name: "an unlabelled triage root", event: func(i intake.DispatchIssue) intake.DispatchIssue { i.HandedOver = false; return i },
			logs: map[string][]json.RawMessage{key: labelledBy(key, "user")}},
		{name: "a labelled triage child", event: func(i intake.DispatchIssue) intake.DispatchIssue { i.Parent = "LEGION-700"; return i },
			logs: map[string][]json.RawMessage{key: labelledBy(key, "user")}},
		{name: "a labelled todo root", event: func(i intake.DispatchIssue) intake.DispatchIssue { i.Status = "todo"; return i },
			logs: map[string][]json.RawMessage{key: labelledBy(key, "user")}},
		{name: "a recorded root a person moved back to triage", event: func(i intake.DispatchIssue) intake.DispatchIssue { return i }, recorded: true,
			logs: map[string][]json.RawMessage{key: labelledBy(key, "user")}},
		{name: "a failed read of the log", event: func(i intake.DispatchIssue) intake.DispatchIssue { return i },
			logErr: errors.New("Dispatch is restarting"), reads: 1, fails: "Dispatch is restarting"},
		{name: "a log that does not decode", event: func(i intake.DispatchIssue) intake.DispatchIssue { return i },
			logs: map[string][]json.RawMessage{key: {json.RawMessage(`{not json`)}}, reads: 1, fails: "decode Dispatch event 1 of 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The preparation only reads, so the cases share one migrated database and each starts
			// from no recorded issue.
			if _, err := pool.Exec(context.Background(), "delete from issues"); err != nil {
				t.Fatalf("clear the recorded issues: %v", err)
			}
			records := record.NewStore()
			if tc.recorded {
				putOutboxIssue(t, pool, records, recordedOn(key, key, "triage"))
			}
			// acme/widgets is served at main, so the labelled todo root is prepared for admission
			// as today, with no wait for a read of the mapping.
			repos := newRepositories("LEGION")
			repos.set([]mappedRepository{{Repo: ghrepo.MustParse("acme/widgets"), Default: true}}, nil)
			branches := newDefaultBranches(roleTokens{}, "", defaultBranchTTL, time.Now)
			branches.read["acme/widgets"] = defaultBranchRead{repository: ghbranch.Repository{Name: ghrepo.MustParse("acme/widgets"), DefaultBranch: "main"}, at: time.Now()}
			client := &eventLogDispatch{logs: tc.logs, logErr: tc.logErr}
			client.issues = map[string]dispatch.Issue{key: shownUnder(key, "todo", "", true)}
			w := &workflowRuntime{pool: pool, records: records, dispatch: client, repositories: repos, defaultBranches: branches, log: quietLogger()}

			got, err := w.prepareRepository(context.Background(), tc.event(triageRoot))
			if tc.fails != "" {
				if err == nil || !strings.Contains(err.Error(), tc.fails) || !strings.Contains(err.Error(), key) {
					t.Fatalf("prepareRepository = %#v, %v; want the failed read to fail the preparation, naming %s and %q", got, err, key, tc.fails)
				}
				return
			}
			if err != nil {
				t.Fatalf("prepareRepository: %v", err)
			}
			if got.LabelSetter != tc.want {
				t.Fatalf("LabelSetter = %q, want %q", got.LabelSetter, tc.want)
			}
			if client.logReads[key] != tc.reads {
				t.Fatalf("the event log of %s was read %d times, want %d", key, client.logReads[key], tc.reads)
			}
		})
	}
}

// A review event is prepared with Dispatch's view of the issue it is on
// (intake.DispatchArtifact.Issue) only for an approval a person made that is not Dispatch's carry
// of an earlier one, in one read of the issue: a session's approval, a carried approval, a changes
// request and a version event read nothing and carry no view, and a read that fails fails the
// preparation, so the event is delivered again.
func TestPrepareArtifactReadsTheIssueOnlyForAPersonsOwnApproval(t *testing.T) {
	const key = "LEGION-755"
	person := dispatch.Actor{Kind: "user", ID: "sami"}
	approval := intake.DispatchArtifact{Key: key, ArtifactID: "art-1", Kind: intake.DispatchArtifactApproved, Version: 9, Actor: person}
	shown := dispatch.Issue{Key: key, Status: "backlog", Title: key, Rank: "A", HandedOver: true, PrimaryArtifactID: "art-1", LastSeq: 40}
	for _, tc := range []struct {
		name    string
		fact    func(fact intake.DispatchArtifact) intake.DispatchArtifact
		failing bool
		reads   int
		fails   bool
	}{
		{name: "a person's approval", fact: func(a intake.DispatchArtifact) intake.DispatchArtifact { return a }, reads: 1},
		{name: "a person's approval with a reason of their own", fact: func(a intake.DispatchArtifact) intake.DispatchArtifact { a.Reason = "Ship it."; return a }, reads: 1},
		{name: "a session's approval", fact: func(a intake.DispatchArtifact) intake.DispatchArtifact {
			a.Actor = dispatch.Actor{Kind: "session", ID: "architect-1"}
			return a
		}},
		{name: "a carried approval", fact: func(a intake.DispatchArtifact) intake.DispatchArtifact {
			a.Reason = intake.CarriedApprovalReasonPrefix + " 2: the answer to the open ask was the whole change"
			return a
		}},
		{name: "a person's changes request", fact: func(a intake.DispatchArtifact) intake.DispatchArtifact {
			a.Kind, a.Reason = intake.DispatchArtifactChangesRequested, "Name the limit."
			return a
		}},
		{name: "a version", fact: func(a intake.DispatchArtifact) intake.DispatchArtifact {
			a.Kind, a.Actor = intake.DispatchArtifactVersion, dispatch.Actor{}
			return a
		}},
		{name: "a failed read of the issue", fact: func(a intake.DispatchArtifact) intake.DispatchArtifact { return a }, failing: true, reads: 1, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &chainDispatch{issues: map[string]dispatch.Issue{key: shown}}
			if tc.failing {
				client.failing = key
			}
			w := &workflowRuntime{dispatch: client, log: quietLogger()}
			fact := tc.fact(approval)

			got, err := w.prepareArtifact(context.Background(), fact)
			if tc.fails {
				if err == nil || !strings.Contains(err.Error(), key) {
					t.Fatalf("prepareArtifact = %#v, %v; want the failed read to fail the preparation, naming %s", got, err, key)
				}
				return
			}
			if err != nil {
				t.Fatalf("prepareArtifact: %v", err)
			}
			if client.reads[key] != tc.reads {
				t.Fatalf("%s was read %d times, want %d", key, client.reads[key], tc.reads)
			}
			if tc.reads == 0 {
				if got.Issue != nil {
					t.Fatalf("Issue = %#v, want no view for a review that queues nothing", *got.Issue)
				}
				got.Issue = nil
				if got != fact {
					t.Fatalf("prepareArtifact = %#v, want the fact unchanged, %#v", got, fact)
				}
				return
			}
			if got.Issue == nil || got.Issue.Key != key || got.Issue.Status != shown.Status || got.Issue.PrimaryArtifactID != shown.PrimaryArtifactID || !got.Issue.HandedOver {
				t.Fatalf("Issue = %#v, want Dispatch's view of %s", got.Issue, key)
			}
			got.Issue = nil
			if got != fact {
				t.Fatalf("prepareArtifact = %#v, want the fact as decoded apart from its view, %#v", got, fact)
			}
		})
	}
}

// The boot listing reads who set the label of a labelled triage root the daemon does not record,
// in one read of its event log, as an event's preparation does, since the listing carries no actor.
// It reads nothing for a labelled triage root it records (one a person moved back to triage, which
// is never queued again), an unlabelled triage root, or a labelled triage child; and a read that
// fails fails the reconcile naming the issue.
func TestBootReconcileReadsWhoSetTheLabelOfAnUnrecordedTriageRoot(t *testing.T) {
	const unrecorded, recorded, unlabelled, child = "CAPTURE-755", "CAPTURE-589", "CAPTURE-526", "CAPTURE-756"
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	conn, err := nats.Connect(workflowNATS(t), nats.Timeout(time.Second))
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	log := quietLogger()
	consumers, err := intake.OpenConsumers(ctx, js, pool, intake.ConsumerSpec{
		Project: "CAPTURE", Repositories: []ghrepo.Repository{ghrepo.MustParse("acme/widgets")}, AckWait: time.Second, NakDelay: time.Millisecond, Logger: log,
	})
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	records := record.NewStore()
	back := recordedOn(recorded, recorded, "triage")
	back.Project, back.LastDispatchSeq = "CAPTURE", 5
	putOutboxIssue(t, pool, records, back)
	under := func(key string) *string { return &key }
	client := &eventLogDispatch{logs: map[string][]json.RawMessage{
		unrecorded: labelledBy(unrecorded, "user"),
		recorded:   labelledBy(recorded, "user"),
		unlabelled: labelledBy(unlabelled, "user"),
		child:      labelledBy(child, "user"),
	}}
	client.listed = []dispatch.IssueSummary{
		{Key: unrecorded, ResolvedRepo: "acme/widgets", Title: unrecorded, Status: "triage", Rank: "A", HandedOver: true, LastSeq: 2},
		{Key: recorded, ResolvedRepo: "acme/widgets", Title: recorded, Status: "triage", Rank: "B", HandedOver: true, LastSeq: 5},
		{Key: unlabelled, ResolvedRepo: "acme/widgets", Title: unlabelled, Status: "triage", Rank: "C", LastSeq: 2},
		{Key: child, ResolvedRepo: "acme/widgets", Title: child, Status: "triage", Rank: "D", Parent: under(unrecorded), HandedOver: true, LastSeq: 2},
	}
	engine := workflow.New(records, workflow.Config{Project: "CAPTURE", Repositories: testRepositories}, log)
	admission := admit.New(records, engine, 2, "CAPTURE", log)
	w := &workflowRuntime{
		pool: pool, records: records, admission: admission, engine: engine, handlers: []intake.Handler{engine, admission}, repositories: newRepositories("CAPTURE"),
		dispatch: client, dispatchProject: "CAPTURE", bootID: "handover-boot", log: log, consumers: consumers,
	}

	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(client.logReads) != 1 || client.logReads[unrecorded] != 1 {
		t.Fatalf("the boot read the event logs %v, want one read of %s alone", client.logReads, unrecorded)
	}
	if len(client.reads) != 0 {
		t.Fatalf("the boot read %v from Dispatch, want no issue detail: its listing shows every row it prepares", client.reads)
	}

	client.logErr = errors.New("Dispatch is restarting")
	err = w.reconcile(ctx)
	if err == nil || !strings.Contains(err.Error(), unrecorded) || !strings.Contains(err.Error(), "Dispatch is restarting") {
		t.Fatalf("reconcile with the log unreadable = %v; want the failed read to fail the reconcile, naming %s", err, unrecorded)
	}
}
