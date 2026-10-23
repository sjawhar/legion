package daemon

import (
	"context"
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

type awaitingDispatch struct {
	outboxDispatch
	issue    dispatch.Issue
	approval dispatch.Approval
	ask      dispatch.Ask
}

func (d *awaitingDispatch) GetIssue(context.Context, string) (dispatch.Issue, error) {
	return d.issue, nil
}
func (d *awaitingDispatch) Approval(context.Context, string) (dispatch.Approval, error) {
	return d.approval, nil
}
func (d *awaitingDispatch) Ask(context.Context, string) (dispatch.Ask, error) { return d.ask, nil }

func TestCatchUpAwaitingAppliesAnApprovalOrClosedAskMissedAtBoot(t *testing.T) {
	for _, tc := range []struct {
		name     string
		awaiting record.Awaiting
		approval dispatch.Approval
		ask      dispatch.Ask
		gate     *record.DesignGate
	}{
		{
			name:     "approved gate",
			awaiting: record.Awaiting{GateVersion: 3},
			approval: dispatch.Approval{State: "approved", LatestVersion: 3, Version: new(3)},
			gate:     &record.DesignGate{ArtifactID: "artifact-root", LatestVersion: 3},
		},
		{
			name:     "answered ask",
			awaiting: record.Awaiting{Ask: "ask-root"},
			ask:      dispatch.Ask{ID: "ask-root", IssueKey: "CAPTURE-ROOT", Kind: "question", State: "answered"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			root := record.Issue{Key: "CAPTURE-ROOT", Tree: "CAPTURE-ROOT", Project: "CAPTURE", Title: "root", Phase: phase.AwaitingPerson,
				Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Awaiting: &tc.awaiting}
			if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
				if err := records.PutIssue(context.Background(), tx, root); err != nil {
					return err
				}
				if tc.gate != nil {
					gate := *tc.gate
					gate.Issue = root.Key
					return records.PutGate(context.Background(), tx, gate)
				}
				return nil
			}); err != nil {
				t.Fatalf("seed awaiting root: %v", err)
			}
			engine := workflow.New(records, workflow.Config{Project: "CAPTURE", Repositories: testRepositories, Clock: func() time.Time { return time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC) }}, quietLogger())
			admission := admit.New(records, engine, 0, "CAPTURE", quietLogger())
			client := &awaitingDispatch{issue: dispatch.Issue{Key: root.Key, LastSeq: 7}, approval: tc.approval, ask: tc.ask}
			w := &workflowRuntime{pool: pool, records: records, dispatch: client, dispatchProject: "CAPTURE", handlers: []intake.Handler{engine, admission}}

			if err := w.catchUpAwaiting(context.Background()); err != nil {
				t.Fatalf("catch up awaiting root: %v", err)
			}
			var got *record.Issue
			if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
				var err error
				got, err = records.Issue(context.Background(), tx, root.Key)
				return err
			}); err != nil {
				t.Fatalf("read caught-up root: %v", err)
			}
			if got == nil || !got.Answered() || got.ClaimSeq != 7 {
				t.Fatalf("caught-up root = %#v, want answered root with claim sequence 7", got)
			}
		})
	}
}
