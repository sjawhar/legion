package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// eventTypes are the event types the filler events carry, in the proportions a type that is not
// read in bulk needs: none of the measured statements filters on the type, so what matters is
// that the column holds the types a reader knows rather than one made-up value. No `ask.*` type
// is among them: a read of an event of one of those decodes its payload as an ask and fails the
// whole request when it names none (api.attachAskEventFields), so the only ask events the seed
// writes are the opening events writeAsks writes with their asks, each carrying its own ask.
var eventTypes = []string{
	"issue.created", "issue.updated", "comment.created", "message.created",
	"artifact.version.created",
}

// eventColumns are the columns of `events` the seed writes, shared by the two writers of that
// table: the filler events here and the opening event each ask is written with.
var eventColumns = []string{
	"id", "issue_key", "artifact_id", "project_key", "seq", "type", "actor",
	"payload", "notify", "published_at", "created_at",
}

// eventOwnerShares is how the events are split over the three owners an event can have. An event
// has at most one (events_one_owner), and `last_seq` on that owner's row must equal the highest
// seq it carries or the next write to it fails the unique (owner, seq) - and those next writes
// are the five write routes the measurement times, so a counter left behind would fail the
// first write of a measured run rather than time it.
const (
	issueEventShare    = 80
	artifactEventShare = 16
	projectEventShare  = 4
)

// eventRow is one planned filler event: its owner, its sequence under that owner and its time.
type eventRow struct {
	owner     string // "issue", "artifact" or "project"
	index     int
	seq       int
	createdAt time.Time
}

// eventBudget is one owner kind's share of the filler events: how many of them that kind takes
// and how many owners of it share them. planEvents lays the rows out over the budgets and
// askEventSeqs numbers each ask's opening event past the rows its owner was left, so both read
// one split of the log.
type eventBudget struct {
	owner string
	count int
	over  int
}

func (r *seedRun) eventBudgets() []eventBudget {
	total := r.sizes.Events
	issues := total * issueEventShare / 100
	artifacts := total * artifactEventShare / 100
	return []eventBudget{
		{"issue", issues, len(r.plan.issues)},
		{"artifact", artifacts, len(r.plan.documents)},
		{"project", total - issues - artifacts, len(r.plan.projects)},
	}
}

// planEvents lays the filler events out over the owners: each owner takes its share of its
// kind's budget, numbered from one, so the highest seq of an owner is the count of its filler
// events and an ask's opening event continues from there.
func (r *seedRun) planEvents() []eventRow {
	rows := make([]eventRow, 0, r.sizes.Events)
	for _, budget := range r.eventBudgets() {
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

// askEventSeqs is the seq each ask's opening event takes under the ask's owner, by ask index.
// The filler events hold 1..count on every owner, so an ask's event continues from count + 1 and
// an owner carrying several asks numbers theirs in ask order; settleEventCounters then brings
// `last_seq` to the highest of them, since it runs after both writers.
func (r *seedRun) askEventSeqs() []int {
	var issueNext, artifactNext []int
	for _, budget := range r.eventBudgets() {
		if budget.owner != "issue" && budget.owner != "artifact" {
			continue
		}
		next := make([]int, budget.over)
		for index := range next {
			next[index] = share(index, budget.over, budget.count) + 1
		}
		if budget.owner == "issue" {
			issueNext = next
		} else {
			artifactNext = next
		}
	}
	seqs := make([]int, len(r.plan.asks))
	for index, ask := range r.plan.asks {
		// An ask is owned by an issue or by an unlinked document (asks_one_owner) and its event
		// is owned by that same row, as api/asks.go appends it on the ask's owner. A block ask's
		// block_artifact_id names the document its block stands in, which does not own it.
		next, at := issueNext, ask.Issue
		if ask.Document >= 0 {
			next, at = artifactNext, ask.Document
		}
		seqs[index] = next[at]
		next[at]++
	}
	return seqs
}

// askEventIDBase is the id an ask's opening event is numbered from, by ask index. The opening
// events are written with their asks, before the filler events, and still take the higher ids:
// a real log numbers one owner's events in the order their seqs run (events/broker.go's Append
// draws both under the owner's row lock), so a reader paging an owner by id must not meet its
// seqs out of order. writeEvents holds the filler events to exactly this count.
func (r *seedRun) askEventIDBase() int64 {
	return int64(r.sizes.Events)
}

// askOpenedPayload is the payload of an ask's opening event, built by model.NewAskEventPayload,
// which is how a producer builds an ask payload (api/asks.go's create route builds the same
// value through events.AskEventPayload), so a reader finds on a seeded event the fields it finds
// on a written one. The ask is the ask as the write that opened it saw it - open, with no answer
// and no resolution, whatever state its row ends in - because that is what an opening event
// records. Its reference changes are empty, as they are on every transition that writes no text:
// they tell a live subscriber which backlink counts to refresh, a seeded run has no subscriber,
// and the edges themselves are the `refs` rows writeAsks writes.
func askOpenedPayload(ask model.Ask) (string, error) {
	encoded, err := json.Marshal(model.NewAskEventPayload(ask, model.ReferenceChanges{}))
	if err != nil {
		return "", fmt.Errorf("encode the opening event of ask %s: %w", ask.ID, err)
	}
	return string(encoded), nil
}

// writeEvents writes the filler events, the log beside the opening events the asks were written
// with. Every event is published (`notify` false and `published_at` set), so a server with NATS
// publishes no backlog of a million events on the first boot after the seed.
func (r *seedRun) writeEvents(ctx context.Context) error {
	events := r.planEvents()
	// The opening events are numbered past the filler events (askEventIDBase), which holds only
	// while the plan writes exactly the budget those ids were counted from.
	if len(events) != r.sizes.Events {
		return fmt.Errorf("the event plan holds %d rows, not the %d the asks' events were numbered past",
			len(events), r.sizes.Events)
	}
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
			_, err := tx.CopyFrom(ctx, pgx.Identifier{"events"}, eventColumns, pgx.CopyFromRows(rows))
			return err
		}); err != nil {
		return err
	}
	// The sequence is set past every id the seed wrote, the asks' opening events included, so a
	// server appending its first event after the seed does not collide with one of them.
	_, err := r.database.Pool.Exec(ctx, `select setval('events_id_seq', $1, true)`,
		r.askEventIDBase()+int64(len(r.plan.asks)))
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
