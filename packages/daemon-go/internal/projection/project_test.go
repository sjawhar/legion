package projection

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

type projectionStore struct {
	record.Store
	issues []record.Issue
	slots  []record.Slot
}

func (s projectionStore) Issues(context.Context, pgx.Tx) ([]record.Issue, error) { return s.issues, nil }
func (s projectionStore) Slots(context.Context, pgx.Tx) ([]record.Slot, error)   { return s.slots, nil }
func (projectionStore) PendingStatusWrites(context.Context, pgx.Tx) ([]record.OutboxRow, error) {
	return nil, nil
}
func (projectionStore) Phases(context.Context, pgx.Tx, string) ([]record.PhaseRow, error) {
	return nil, nil
}
func (projectionStore) PullRequest(context.Context, pgx.Tx, string) (*record.PullRequest, error) {
	return nil, nil
}
func (projectionStore) Gate(context.Context, pgx.Tx, string) (*record.DesignGate, error) {
	return nil, nil
}

func TestProjectShowsOnlySlotlessTodoIssuesInDispatchRankOrder(t *testing.T) {
	now := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	store := projectionStore{
		issues: []record.Issue{
			{Key: "LEGION-210", Status: "done", Rank: "00000"},
			{Key: "LEGION-211", Status: "backlog", Rank: "00001"},
			{Key: "LEGION-212", Status: "todo", Rank: "00004", LastDispatchSeq: 1},
			{Key: "LEGION-213", Status: "todo", Rank: "00003", LastDispatchSeq: 10, Phase: phase.Implementing},
			{Key: "LEGION-214", Status: "todo", Rank: "00002"},
		},
		slots: []record.Slot{{Issue: "LEGION-214", Index: 0, AdmittedAt: now}},
	}
	got, err := Project(context.Background(), nil, store, []supervise.Claim{})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if !reflect.DeepEqual(got.Admission.Active, []string{"LEGION-214"}) {
		t.Fatalf("active = %#v, want the slotted issue", got.Admission.Active)
	}
	if !reflect.DeepEqual(got.Admission.Waiting, []string{"LEGION-213", "LEGION-212"}) {
		t.Fatalf("waiting = %#v, want slotless todo issues in rank order", got.Admission.Waiting)
	}
	if got.Issues["LEGION-213"].Phase != phase.Implementing {
		t.Fatalf("projected phase = %q, want %q", got.Issues["LEGION-213"].Phase, phase.Implementing)
	}
	var _ api.State = got
}
