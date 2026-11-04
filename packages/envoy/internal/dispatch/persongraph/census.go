// Package persongraph reads the derived person graph and checks recorded delivery against it.
package persongraph

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// DeliveryCensus compares recipient sets, not edge rows: retries and multiple delivery paths
// can legitimately give the graph several rows for one recipient. Counts contain no content.
type DeliveryCensus struct {
	Since      time.Time      `json:"since"`
	Checked    int            `json:"checked"`
	ByKind     map[string]int `json:"by_kind"`
	Mismatched int            `json:"mismatched"`
	Missing    map[string]int `json:"missing"`
	Unexpected map[string]int `json:"unexpected"`
}

type node struct{ kind, id string }
type recipient struct{ kind, id string }

const censusSources = `with sources as (
	select 'comment' as kind, id::text as id from comments where created_at >= $1
	union all select 'message', id::text from messages where created_at >= $1
	union all select 'ask', id::text from asks where created_at >= $1
)`

// Census checks every comment, message and ask created since since, including an item's empty
// recipient set, in one read-only repeatable-read snapshot. The reference is decoded in Go from
// the outbox's recorded destinations and sent attempts, independently of the view's SQL. Event
// types are the word-carrying types sessionanswers reads; unlike that human-words projection,
// delivery edges retain session-authored items too. Role topics prove a role, not its holder.
func Census(ctx context.Context, pool *store.Pool, since time.Time) (DeliveryCensus, error) {
	report := DeliveryCensus{Since: since.UTC(), ByKind: map[string]int{"comment": 0, "message": 0, "ask": 0}, Missing: map[string]int{}, Unexpected: map[string]int{}}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return report, err
	}
	defer tx.Rollback(ctx)
	expected := map[node]map[recipient]string{}
	rows, err := tx.Query(ctx, censusSources+` select kind, id from sources`, since)
	if err != nil {
		return report, fmt.Errorf("census sources: %w", err)
	}
	for rows.Next() {
		var source node
		if err := rows.Scan(&source.kind, &source.id); err != nil {
			rows.Close()
			return report, err
		}
		expected[source] = map[recipient]string{}
		report.Checked++
		report.ByKind[source.kind]++
	}
	if err := rows.Err(); err != nil {
		return report, err
	}
	rows.Close()
	rows, err = tx.Query(ctx, censusSources+`
		select e.type, e.payload->>'id', e.published_destinations from events e
		join sources s on s.id = e.payload->>'id' and s.kind = split_part(e.type, '.', 1)
		where e.type in ('ask.answered', 'comment.created', 'message.created', 'message.answered')`, since)
	if err != nil {
		return report, fmt.Errorf("census recorded topics: %w", err)
	}
	for rows.Next() {
		var eventType, id string
		var topics []string
		if err := rows.Scan(&eventType, &id, &topics); err != nil {
			rows.Close()
			return report, err
		}
		kind, _, _ := strings.Cut(eventType, ".")
		set := expected[node{kind, id}]
		for _, topic := range topics {
			if session, ok := strings.CutPrefix(topic, contracts.AgentTopicPrefix); ok && session != "" {
				set[recipient{"session", session}] = "session_topic"
			}
			if role, ok := strings.CutPrefix(topic, contracts.RoleTopicPrefix); ok && role != "" {
				set[recipient{"role", role}] = "role_topic"
			}
		}
	}
	if err := rows.Err(); err != nil {
		return report, err
	}
	rows.Close()
	rows, err = tx.Query(ctx, censusSources+`
		select 'message', d.message_id::text, d.session_id from message_deliveries d
		join sources s on s.kind = 'message' and s.id = d.message_id::text where d.state = 'sent' and d.session_id <> ''
		union all
		select 'comment', d.comment_id::text, d.session_id from comment_deliveries d
		join sources s on s.kind = 'comment' and s.id = d.comment_id::text where d.state = 'sent' and d.session_id <> ''`, since)
	if err != nil {
		return report, fmt.Errorf("census sent attempts: %w", err)
	}
	for rows.Next() {
		var source node
		var session string
		if err := rows.Scan(&source.kind, &source.id, &session); err != nil {
			rows.Close()
			return report, err
		}
		expected[source][recipient{"session", session}] = source.kind + "_delivery"
	}
	if err := rows.Err(); err != nil {
		return report, err
	}
	rows.Close()
	actual := map[node]map[recipient]bool{}
	rows, err = tx.Query(ctx, censusSources+`
		select g.from_kind, g.from_id, g.to_kind, g.to_id from `+refs.AllEdgesSQL+` g
		join sources s on s.kind = g.from_kind and s.id = g.from_id
		where g.kind = 'delivered_to' and g.to_kind in ('session', 'role')`, since)
	if err != nil {
		return report, fmt.Errorf("census graph: %w", err)
	}
	for rows.Next() {
		var source node
		var target recipient
		if err := rows.Scan(&source.kind, &source.id, &target.kind, &target.id); err != nil {
			rows.Close()
			return report, err
		}
		if actual[source] == nil {
			actual[source] = map[recipient]bool{}
		}
		actual[source][target] = true
	}
	if err := rows.Err(); err != nil {
		return report, err
	}
	rows.Close()
	for source, want := range expected {
		mismatch := false
		for target, cause := range want {
			if !actual[source][target] {
				report.Missing[cause]++
				mismatch = true
			}
		}
		for target := range actual[source] {
			if _, recorded := want[target]; !recorded {
				report.Unexpected["unrecorded"]++
				mismatch = true
			}
		}
		if mismatch {
			report.Mismatched++
		}
	}
	return report, tx.Commit(ctx)
}
