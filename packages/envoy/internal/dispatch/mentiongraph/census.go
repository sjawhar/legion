// Package mentiongraph compares identity mention edges with their recorded resolutions.
package mentiongraph

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Edge is a resolved identity mention, independent of any graph view.
type Edge struct{ FromKind, FromID, ToKind, ToID string }

// SourcesSQL lists every current source in the window, including ones with no mention.
const SourcesSQL = `select 'comment' as kind, id::text as id from comments where created_at >= $1
 union all select 'message', id::text from messages where created_at >= $1
 union all select 'block', node_id from document_blocks where changed_at >= $1`

// Recorded reads the existing writers' records, not the graph or a re-parse of source text.
// Routes are decoded from their recorded address, and session ownership from verified aliases.
func Recorded(ctx context.Context, q refs.Queryer, since time.Time) ([]Edge, error) {
	set := map[Edge]struct{}{}
	add := func(kind, id, targetKind, targetID string) {
		if targetID != "" {
			set[Edge{kind, id, targetKind, targetID}] = struct{}{}
		}
	}
	rows, err := q.Query(ctx, `select r.from_kind, r.from_id, r.to_kind, r.to_id from refs r
 join (`+SourcesSQL+`) s on s.kind=r.from_kind and s.id=r.from_id
 where r.to_kind in ('person_mention','session_mention','role_mention')
 union all select 'block', r.from_node, r.to_kind, r.to_id from block_refs r
 join document_blocks d on d.artifact_id=r.artifact_id and d.block_id=r.block_id
 where d.changed_at >= $1 and r.kind='mentions' and r.to_kind in ('person_mention','session_mention','role_mention')`, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.FromKind, &e.FromID, &e.ToKind, &e.ToID); err != nil {
			rows.Close()
			return nil, err
		}
		e.ToKind = strings.TrimSuffix(e.ToKind, "_mention")
		set[e] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	rows, err = q.Query(ctx, `select m.comment_id::text,m.target,m.resolved_session_id from comment_mentions m join comments c on c.id=m.comment_id where c.created_at >= $1`, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, target string
		var session *string
		if err := rows.Scan(&id, &target, &session); err != nil {
			rows.Close()
			return nil, err
		}
		if route, err := model.ParseRoute(target); err == nil {
			add("comment", id, route.Kind, route.ID)
		}
		if session != nil {
			add("comment", id, "session", *session)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	rows, err = q.Query(ctx, `select 'comment',d.comment_id::text,d.session_id,d.resolve_error from comment_deliveries d join comments c on c.id=d.comment_id where c.created_at >= $1 and d.session_id <> ''
 union all select 'message',d.message_id::text,d.session_id,null::text from message_deliveries d join messages m on m.id=d.message_id where m.created_at >= $1 and d.session_id <> ''`, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var kind, id, session string
		var failure *string
		if err := rows.Scan(&kind, &id, &session, &failure); err != nil {
			rows.Close()
			return nil, err
		}
		if failure == nil || !strings.HasPrefix(*failure, "no live session ") {
			add(kind, id, "session", session)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	rows, err = q.Query(ctx, `select id::text,target from messages where created_at >= $1 and target is not null`, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, target string
		if err := rows.Scan(&id, &target); err != nil {
			rows.Close()
			return nil, err
		}
		if route, err := model.ParseRoute(target); err == nil {
			add("message", id, route.Kind, route.ID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	rows, err = q.Query(ctx, `select alias,person from person_aliases where alias_kind='session'`)
	if err != nil {
		return nil, err
	}
	owners := map[string][]string{}
	for rows.Next() {
		var session, person string
		if err := rows.Scan(&session, &person); err != nil {
			rows.Close()
			return nil, err
		}
		owners[session] = append(owners[session], person)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// Add to a separate slice: inserting into a map while ranging need not visit new entries.
	edges := make([]Edge, 0, len(set))
	for e := range set {
		edges = append(edges, e)
	}
	for _, e := range edges {
		if e.ToKind == "session" {
			for _, person := range owners[e.ToID] {
				add(e.FromKind, e.FromID, "person", person)
			}
		}
	}
	edges = edges[:0]
	for e := range set {
		edges = append(edges, e)
	}
	return edges, nil
}

type Report struct {
	Since      time.Time      `json:"since"`
	Checked    int            `json:"checked"`
	ByKind     map[string]int `json:"by_kind"`
	Recorded   int            `json:"recorded"`
	Mismatched int            `json:"mismatched"`
	Missing    map[string]int `json:"missing"`
	Unexpected map[string]int `json:"unexpected"`
}

// Census takes one read-only repeatable-read snapshot and compares whole source sets.
func Census(ctx context.Context, pool *store.Pool, since time.Time) (Report, error) {
	report := Report{Since: since.UTC(), ByKind: map[string]int{}, Missing: map[string]int{}, Unexpected: map[string]int{}}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return report, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, SourcesSQL, since)
	if err != nil {
		return report, err
	}
	for rows.Next() {
		var kind, id string
		if err := rows.Scan(&kind, &id); err != nil {
			rows.Close()
			return report, err
		}
		report.Checked++
		report.ByKind[kind]++
	}
	if err := rows.Err(); err != nil {
		return report, err
	}
	rows.Close()
	recorded, err := Recorded(ctx, tx, since)
	if err != nil {
		return report, fmt.Errorf("recorded mention resolutions: %w", err)
	}
	expected := make(map[Edge]bool, len(recorded))
	for _, e := range recorded {
		expected[e] = true
	}
	report.Recorded = len(expected)
	rows, err = tx.Query(ctx, `select g.from_kind,g.from_id,g.to_kind,g.to_id from `+refs.AllEdgesSQL+` g join (`+SourcesSQL+`) s on s.kind=g.from_kind and s.id=g.from_id where g.kind='mentions' and g.to_kind in ('person','session','role')`, since)
	if err != nil {
		return report, err
	}
	actual := map[Edge]bool{}
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.FromKind, &e.FromID, &e.ToKind, &e.ToID); err != nil {
			rows.Close()
			return report, err
		}
		actual[e] = true
	}
	if err := rows.Err(); err != nil {
		return report, err
	}
	rows.Close()
	mismatched := map[[2]string]bool{}
	for e := range expected {
		if !actual[e] {
			report.Missing[e.FromKind+":"+e.ToKind]++
			mismatched[[2]string{e.FromKind, e.FromID}] = true
		}
	}
	for e := range actual {
		if !expected[e] {
			report.Unexpected[e.FromKind+":"+e.ToKind]++
			mismatched[[2]string{e.FromKind, e.FromID}] = true
		}
	}
	report.Mismatched = len(mismatched)
	return report, tx.Commit(ctx)
}
