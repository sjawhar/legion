package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A root that runs at admitted when the daemon goes down may have handed work to the person
// meanwhile: the boot catch-up reads its event log from its last handout, so the question its
// architect opened is mirrored and declared as the live event would have left it.
func TestCatchUpReadsAnAdmittedRootsAsksFromItsLastHandout(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	root := record.Issue{Key: "CAPTURE-100", Tree: "CAPTURE-100", Project: "CAPTURE", Title: "root", Phase: phase.Admitted,
		Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		LastDispatchSeq: catchUpHandout}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, root); err != nil {
			return err
		}
		if err := records.StartHandout(ctx, tx, root.Key); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ('legion-capture-capture-100-architect', 'CAPTURE', 'CAPTURE-100', 'CAPTURE-100', 'architect', 1, 'session-architect', '', 'working', 0, 0, 0, 0)`)
		return err
	}); err != nil {
		t.Fatalf("seed the admitted root: %v", err)
	}
	opened := askEvent(9, "ask.opened", architectSession, map[string]any{"id": "ask-q", "kind": "question",
		"author": map[string]any{"kind": architectSession.Kind, "id": architectSession.ID}})
	client := &awaitingDispatch{issue: dispatch.Issue{Key: root.Key, LastSeq: 9}, events: []json.RawMessage{opened}}
	engine := workflow.New(records, workflow.Config{Project: "CAPTURE", Repositories: testRepositories, Clock: func() time.Time { return time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC) }}, quietLogger())
	admission := admit.New(records, engine, 0, "CAPTURE", quietLogger())
	w := &workflowRuntime{pool: pool, records: records, dispatch: client, dispatchProject: "CAPTURE", handlers: []intake.Handler{engine, admission}, log: quietLogger()}
	if err := w.catchUpAwaiting(ctx); err != nil {
		t.Fatalf("catch up: %v", err)
	}
	if len(client.afters) != 1 || client.afters[0] != catchUpHandout {
		t.Fatalf("event log read after %v, want one read after the last handout %d", client.afters, catchUpHandout)
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		got, err := records.Issue(ctx, tx, root.Key)
		if err != nil {
			return err
		}
		ask, err := records.RootAsk(ctx, tx, root.Key, "ask-q")
		if err != nil {
			return err
		}
		if got.DeclaredSeq != 9 || ask == nil || !ask.WithHuman() || !ask.ByTree {
			t.Fatalf("root after the catch-up = %#v with ask %#v, want the question mirrored with the person and declared at 9", got, ask)
		}
		return nil
	}); err != nil {
		t.Fatalf("read the caught-up root: %v", err)
	}
}

// catchUpRefuser answers every read of CAPTURE-1 with status, and CAPTURE-2's normally.
type catchUpRefuser struct {
	awaitingDispatch
	status int
	reads  int
}

func (d *catchUpRefuser) GetIssue(ctx context.Context, key string) (dispatch.Issue, error) {
	if key == "CAPTURE-1" {
		d.reads++
		return dispatch.Issue{}, &dispatch.Error{Status: d.status, Message: http.StatusText(d.status)}
	}
	return dispatch.Issue{Key: key, LastSeq: 7}, nil
}

// A root Dispatch keeps refusing does not hold the boot: an error Dispatch may stop giving (a 5xx)
// is read again a bounded number of times, a credential refusal (401, 403) not at all, and the root
// is then skipped, so the next root is caught up and the catch-up finishes.
func TestCatchUpSkipsARootDispatchKeepsRefusing(t *testing.T) {
	for _, tc := range []struct {
		status int
		reads  int
	}{
		{status: http.StatusBadGateway, reads: catchUpAttempts},
		{status: http.StatusServiceUnavailable, reads: catchUpAttempts},
		{status: http.StatusUnauthorized, reads: 1},
		{status: http.StatusForbidden, reads: 1},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			ctx := context.Background()
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				for _, key := range []string{"CAPTURE-1", "CAPTURE-2"} {
					if err := records.PutIssue(ctx, tx, record.Issue{Key: key, Tree: key, Project: "CAPTURE", Title: key, Phase: phase.AwaitingPerson,
						Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
						Awaiting: &record.Awaiting{Ask: "ask-" + key}}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatalf("seed the roots: %v", err)
			}
			answered, err := json.Marshal(map[string]any{"id": 1009, "issue_key": "CAPTURE-2", "seq": 9, "type": "ask.answered", "notify": true,
				"actor": map[string]any{"kind": alice.Kind, "id": alice.ID}, "created_at": time.Date(2026, 10, 23, 17, 0, 9, 0, time.UTC),
				"payload": map[string]any{"id": "ask-CAPTURE-2", "kind": "question"}})
			if err != nil {
				t.Fatalf("encode the answer: %v", err)
			}
			client := &catchUpRefuser{status: tc.status, awaitingDispatch: awaitingDispatch{events: []json.RawMessage{answered}}}
			engine := workflow.New(records, workflow.Config{Project: "CAPTURE", Repositories: testRepositories, Clock: func() time.Time { return time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC) }}, quietLogger())
			admission := admit.New(records, engine, 0, "CAPTURE", quietLogger())
			w := &workflowRuntime{pool: pool, records: records, dispatch: client, dispatchProject: "CAPTURE", handlers: []intake.Handler{engine, admission},
				log: quietLogger(), catchUpRetryDelay: time.Millisecond}
			if err := w.catchUpAwaiting(ctx); err != nil {
				t.Fatalf("catch up with a root Dispatch refuses = %v, want the root skipped", err)
			}
			if client.reads != tc.reads {
				t.Fatalf("reads of the refused root = %d, want %d", client.reads, tc.reads)
			}
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				got, err := records.Issue(ctx, tx, "CAPTURE-2")
				if err != nil {
					return err
				}
				if got == nil || !got.Answered() {
					t.Fatalf("the root after the refused one = %#v, want its answer caught up", got)
				}
				return nil
			}); err != nil {
				t.Fatalf("read the second root: %v", err)
			}
		})
	}
}
