package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A finished root's hand-over of a child it leaves mid-run is one write to Dispatch, so the one
// event Dispatch makes of it carries both the todo and the legion label, under the daemon's own
// actor: that event is what admission takes the child in by as an orphan (LEGION-624). The labels
// the child already carried are kept.
func TestAHandOverReachesScratchDispatchAsOneEventCarryingTheTodoAndTheLabel(t *testing.T) {
	baseURL, token := startScratchDispatch(t)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := createScratchDispatchIssue(t, baseURL)
	client := &http.Client{Timeout: 10 * time.Second}
	patch, err := json.Marshal(map[string]any{"status": "in_progress", "labels": []string{"repo:x"}})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPatch, baseURL+"/api/v1/issues/"+issue, bytes.NewReader(patch))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "smoke")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("set the child mid-run: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("set the child mid-run = %s", response.Status)
	}
	daemonDispatch := dispatch.New(baseURL, token)
	before, err := daemonDispatch.GetIssue(context.Background(), issue)
	if err != nil {
		t.Fatalf("read the mid-run child: %v", err)
	}

	// The closed tree, as the record holds it once its root's linger ended with the root done.
	const root = "LEGION-900"
	until := time.Now().UTC()
	parent := root
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(context.Background(), tx, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION", Title: root, Phase: phase.Done, Generation: 2, Status: "done", LingerUntil: &until}); err != nil {
			return err
		}
		if err := records.PutIssue(context.Background(), tx, record.Issue{Key: issue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION", Title: issue, Parent: &parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress"}); err != nil {
			return err
		}
		return records.PutHandover(context.Background(), tx, record.Handover{Issue: issue, Tree: root, Generation: 2})
	}); err != nil {
		t.Fatalf("seed the closed tree: %v", err)
	}

	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue, record.StatusWrite{Status: "todo", ObservedStatus: "in_progress", HandOver: true, HandOverTree: root, HandOverLinger: 2}, now))
	runner := &outbox{dispatchProject: "LEGION", pool: pool, records: records, dispatch: daemonDispatch, now: func() time.Time { return now }, log: quietLogger()}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the hand-over: %v", err)
	}
	if remaining := outboxRows(t, pool); remaining != 0 {
		t.Fatalf("outbox rows after the hand-over = %d, want it finished", remaining)
	}

	after, err := daemonDispatch.GetIssue(context.Background(), issue)
	if err != nil {
		t.Fatalf("read the handed-over child: %v", err)
	}
	if after.Status != "todo" || !after.HandedOver || !slices.Contains(after.Labels, "repo:x") || after.LastSeq != before.LastSeq+1 {
		t.Fatalf("the handed-over child = %#v (it was at sequence %d), want todo with repo:x and legion, one event later", after, before.LastSeq)
	}
	eventsRequest, err := http.NewRequest(http.MethodGet, baseURL+"/api/v1/issues/"+issue+"/events?order=desc&limit=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	eventsRequest.Header.Set("Authorization", "Bearer "+token)
	eventsResponse, err := client.Do(eventsRequest)
	if err != nil {
		t.Fatalf("read the hand-over's event: %v", err)
	}
	defer eventsResponse.Body.Close()
	var events []struct {
		Seq   int64  `json:"seq"`
		Type  string `json:"type"`
		Actor struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"actor"`
		Payload struct {
			Status string   `json:"status"`
			Labels []string `json:"labels"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(eventsResponse.Body).Decode(&events); err != nil {
		t.Fatalf("decode the hand-over's event: %v", err)
	}
	if len(events) != 1 || events[0].Seq != after.LastSeq || events[0].Type != "issue.updated" || events[0].Actor.Kind != "session" ||
		events[0].Actor.ID != dispatch.DaemonSession(issue) || events[0].Payload.Status != "todo" || !dispatch.CarriesLegionLabel(events[0].Payload.Labels) {
		t.Fatalf("the hand-over's event = %+v, want one issue.updated by %s carrying todo and the legion label", events, dispatch.DaemonSession(issue))
	}
}
