package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// eventTypes are the event types the seed writes, in the proportions a type that is not read in
// bulk needs: none of the measured statements filters on the type, so what matters is that the
// column holds the types a reader knows rather than one made-up value.
var eventTypes = []string{
	"issue.created", "issue.updated", "comment.created", "ask.opened", "ask.answered",
	"message.created", "artifact.version.created",
}

// eventOwnerShares is how the events are split over the three owners an event can have. An event
// has at most one (events_one_owner), and `last_seq` on that owner's row must equal the highest
// seq it carries or the next write to it fails the unique (owner, seq) - which is exactly the
// five write routes the measurement times (the plan's answer 2).
const (
	issueEventShare    = 80
	artifactEventShare = 16
	projectEventShare  = 4
)

// eventRow is one planned event: its owner, its sequence under that owner and its time.
type eventRow struct {
	owner     string // "issue", "artifact" or "project"
	index     int
	seq       int
	createdAt time.Time
}

// planEvents lays the events out over the owners: each owner takes its share of its kind's
// budget, numbered from one, so the highest seq of an owner is the count of its events.
func (r *seedRun) planEvents() []eventRow {
	total := r.sizes.Events
	budgets := []struct {
		owner string
		count int
		over  int
	}{
		{"issue", total * issueEventShare / 100, len(r.plan.issues)},
		{"artifact", total * artifactEventShare / 100, len(r.plan.documents)},
		{"project", total - total*issueEventShare/100 - total*artifactEventShare/100, len(r.plan.projects)},
	}
	rows := make([]eventRow, 0, total)
	for _, budget := range budgets {
		for index := range budget.over {
			count := share(index, budget.over, budget.count)
			for seq := 1; seq <= count; seq++ {
				rows = append(rows, eventRow{owner: budget.owner, index: index, seq: seq})
			}
		}
	}
	for position := range rows {
		rows[position].createdAt = timeAt(position, len(rows))
	}
	return rows
}

// writeEvents writes the event log. Every event is published (`notify` false and `published_at`
// set), so a server with NATS publishes no backlog of a million events on the first boot after
// the seed.
func (r *seedRun) writeEvents(ctx context.Context) error {
	events := r.planEvents()
	sessions := r.plan.sessions
	if err := r.inBatches(ctx, "events", len(events), 10*batchRows,
		func(ctx context.Context, tx pgx.Tx, from, to int) error {
			rows := make([][]any, 0, to-from)
			for index := from; index < to; index++ {
				event := events[index]
				var issueKey, projectKey, artifactID any
				switch event.owner {
				case "issue":
					issueKey = r.plan.issues[event.index].Key
				case "artifact":
					artifactID = r.plan.documents[event.index].ID
				default:
					projectKey = r.plan.projects[event.index].Key
				}
				// Three events in five carry a session actor, which is what
				// events_session_actor_created_at_idx is partial on and what
				// `agentLastActivity` groups. The session is chosen by how many session events
				// came before this one, not by the event's own index, so every session is an
				// actor rather than the three in five the index step would reach.
				actor := seedActor
				if index%5 < 3 {
					before := index/5*3 + min(index%5, 3)
					actor = sessionActor(sessions[before%len(sessions)])
				}
				rows = append(rows, []any{
					int64(index + 1), issueKey, artifactID, projectKey, event.seq,
					eventTypes[index%len(eventTypes)], actor, `{"seeded":true}`,
					false, event.createdAt, event.createdAt,
				})
			}
			_, err := tx.CopyFrom(ctx, pgx.Identifier{"events"}, []string{
				"id", "issue_key", "artifact_id", "project_key", "seq", "type", "actor",
				"payload", "notify", "published_at", "created_at",
			}, pgx.CopyFromRows(rows))
			return err
		}); err != nil {
		return err
	}
	// The sequence is set past the ids the seed wrote, so a server appending its first event
	// after the seed does not collide with one of them.
	_, err := r.database.Pool.Exec(ctx, `select setval('events_id_seq', $1, true)`, int64(len(events)))
	if err != nil {
		return fmt.Errorf("set the events sequence past the seeded ids: %w", err)
	}
	return nil
}

// settleEventCounters brings each owner's `last_seq` to the highest seq it carries and each
// project's `next_number` past its highest issue number. events/broker.go's Append reads
// `last_seq` off the owner row (lockEventOwner) and inserts seq = last_seq + 1, so a seeded issue
// left at 0 fails the unique (issue_key, seq) on the first comment, ask, message, upload or title
// edit written to it.
func (r *seedRun) settleEventCounters(ctx context.Context) error {
	for _, statement := range []string{
		`update issues i set last_seq = coalesce(e.top, 0)
		   from (select issue_key, max(seq) as top from events where issue_key is not null group by issue_key) e
		  where e.issue_key = i.key`,
		`update artifacts a set last_seq = coalesce(e.top, 0)
		   from (select artifact_id, max(seq) as top from events where artifact_id is not null group by artifact_id) e
		  where e.artifact_id = a.id`,
		`update projects p set last_seq = coalesce(e.top, 0)
		   from (select project_key, max(seq) as top from events where project_key is not null group by project_key) e
		  where e.project_key = p.key`,
		`update projects p set next_number = coalesce(i.top, 0) + 1
		   from (select project_key, max(number) as top from issues group by project_key) i
		  where i.project_key = p.key`,
	} {
		if _, err := r.database.Pool.Exec(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}
