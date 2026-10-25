// The issue.deployed event (LEGION-781): the delivery timeline tells a pull request's issue that
// the pull request reached production, the moment the timeline first knows which run shipped it,
// by intake or by the reconcile. The Legion daemon consumes it on the issue's subject
// (notifications.dispatch.issue.<KEY>.issue.deployed) to wake the production check an issue's
// close opened.
package delivery

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// DeployedEventType is the event RecordDeployedEvents appends on a pull request's issue.
const DeployedEventType = "issue.deployed"

// DeployedEventActor writes every issue.deployed: a system actor, since no person or session caused
// the deploy the timeline observed. Broker.Notify is false for a system actor, which keeps the event
// out of inboxes and routed delivery only; the outbox publishes it on the issue's subject as it does
// every unpublished event, and the event feed and the event log carry it, whoever the actor is.
var DeployedEventActor = model.Actor{Kind: "system", ID: "delivery-timeline"}

// IssueDeployedEventPayload is an issue.deployed event's payload, mirrored by
// IssueDeployedEventPayload in packages/contracts/src/dispatch-api.ts and decoded by the Legion
// daemon's intake: the pull request (its repository and number), the production run that shipped
// it, and when that run's production job completed, in UTC.
type IssueDeployedEventPayload struct {
	Repo        string    `json:"repo"`
	Number      int       `json:"number"`
	RunID       int64     `json:"run_id"`
	CompletedAt time.Time `json:"completed_at"`
}

// shippedPullRequest is one row listUnrecordedShipped answers: a merged pull request of the deploy
// repository attributed to an issue, with the run that shipped it, whose event is not written yet.
type shippedPullRequest struct {
	Repo        string
	Number      int
	IssueKey    string
	RunID       int64
	CompletedAt time.Time
}

// RecordDeployedEvents appends issue.deployed on the issue of every merged pull request of the
// deploy repository that a production run has shipped and that carries no deployed_event_at yet,
// and sets that marker, each in a transaction of its own; it answers how many it wrote. Which run
// shipped a pull request is shippingRunJoin's rule, the one the timeline's waiting line reads, so
// the event names the run the Delivery page shows. Once per pull request: the marker is set in the
// event's transaction, under the row's lock and only while still null, so a second pass writes
// nothing, and of intake and the reconcile running at once, which both call this, the second finds
// the row marked and appends nothing. A pull request whose issue was deleted between the read and
// the write (its issue_key set null by the foreign key) is skipped the same way. A failed row ends
// the pass with its error: the rows before it stay written, the rest wait for the next call. The
// payload is IssueDeployedEventPayload; the actor DeployedEventActor.
func RecordDeployedEvents(ctx context.Context, pool *store.Pool, settings DeliverySettings, broker *events.Broker, now time.Time) (int, error) {
	shipped, err := listUnrecordedShipped(ctx, pool, settings.DeployRepo, settings.Production())
	if err != nil {
		return 0, err
	}
	written := 0
	for _, pr := range shipped {
		wrote, err := recordDeployedEvent(ctx, pool, broker, pr, now)
		if err != nil {
			return written, fmt.Errorf("record issue.deployed for %s#%d on %s: %w", pr.Repo, pr.Number, pr.IssueKey, err)
		}
		if wrote {
			written++
		}
	}
	return written, nil
}

// listUnrecordedShipped lists the deploy repository's merged pull requests that name an issue, carry
// no deployed_event_at, and that a production run has shipped (shippingRunJoin), oldest merge first
// and those merged in one instant by id, as ListPullRequestsWaitingAt orders them.
func listUnrecordedShipped(ctx context.Context, pool *store.Pool, deployRepo string, production Production) ([]shippedPullRequest, error) {
	rows, err := pool.Query(ctx, `
		select pr.repo, pr.number, pr.issue_key, ship.run_id, ship.completed_at
		from delivery_pull_requests pr
		`+shippingRunJoin("$1", "$2", "$3")+`
		where pr.repo = $1 and pr.merged_at is not null and pr.issue_key is not null
			and pr.deployed_event_at is null and ship.run_id is not null
		order by pr.merged_at, (pr.repo || '#' || pr.number) collate "C"
	`, deployRepo, production.JobNames, production.ApplyStep)
	if err != nil {
		return nil, fmt.Errorf("list shipped pull requests without their issue.deployed: %w", err)
	}
	defer rows.Close()
	var shipped []shippedPullRequest
	for rows.Next() {
		var pr shippedPullRequest
		if err := rows.Scan(&pr.Repo, &pr.Number, &pr.IssueKey, &pr.RunID, &pr.CompletedAt); err != nil {
			return nil, err
		}
		shipped = append(shipped, pr)
	}
	return shipped, rows.Err()
}

// recordDeployedEvent marks pr and appends its event in one transaction, and reports whether it did:
// false when another writer marked the row first, or the row no longer names pr's issue.
func recordDeployedEvent(ctx context.Context, pool *store.Pool, broker *events.Broker, pr shippedPullRequest, now time.Time) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		update delivery_pull_requests set deployed_event_at = $4
		where repo = $1 and number = $2 and issue_key = $3 and deployed_event_at is null
	`, pr.Repo, pr.Number, pr.IssueKey, now)
	if err != nil {
		return false, fmt.Errorf("mark the pull request: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	event, err := broker.Append(ctx, tx, model.Event{
		IssueKey: &pr.IssueKey,
		Type:     DeployedEventType,
		Actor:    DeployedEventActor,
		Payload: IssueDeployedEventPayload{
			Repo: pr.Repo, Number: pr.Number, RunID: pr.RunID, CompletedAt: pr.CompletedAt.UTC(),
		},
	})
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	broker.Publish(event)
	return true, nil
}
