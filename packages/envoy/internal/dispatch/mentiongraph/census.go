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
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Edge is a resolved identity mention, independent of any graph view.
type Edge struct{ FromKind, FromID, ToKind, ToID string }

// SourcesSQL lists every current source in the window, including ones with no mention.
const SourcesSQL = `select 'comment' as kind, c.id::text as id from comments c
 where c.created_at >= $1 or c.edited_at >= $1
    or exists (select 1 from comment_deliveries d where d.comment_id=c.id and d.created_at >= $1)
 union all select 'message', m.id::text from messages m
 where m.created_at >= $1 or exists (select 1 from message_deliveries d where d.message_id=m.id and d.created_at >= $1)
 union all select 'block', d.node_id from document_blocks d
 where d.changed_at >= $1 or exists (select 1 from artifact_versions v where v.artifact_id=d.artifact_id and v.created_at >= $1)`

// Recorded reparses current source bodies and document versions independently of the
// reference indexes. Explicit comment mention resolutions and verified session aliases
// supplement those bodies; direct-message destinations are deliveries, not mentions.
func Recorded(ctx context.Context, q refs.Queryer, since time.Time) ([]Edge, error) {
	set := map[Edge]struct{}{}
	add := func(kind, id, targetKind, targetID string) {
		if targetID != "" {
			set[Edge{kind, id, targetKind, targetID}] = struct{}{}
		}
	}
	rows, err := q.Query(ctx, `select s.kind,s.id,c.body from (`+SourcesSQL+`) s join comments c on s.kind='comment' and c.id::text=s.id
	 union all select s.kind,s.id,m.body from (`+SourcesSQL+`) s join messages m on s.kind='message' and m.id::text=s.id`, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var kind, id, body string
		if err := rows.Scan(&kind, &id, &body); err != nil {
			rows.Close()
			return nil, err
		}
		for _, ref := range text.Extract(body, "") {
			if refs.IsIdentity(ref.Kind) {
				add(kind, id, ref.Kind, text.TargetID(ref))
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err := documentMentions(ctx, q, since, add); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `select m.comment_id::text,m.target,m.resolved_session_id from comment_mentions m
 join (`+SourcesSQL+`) s on s.kind='comment' and s.id=m.comment_id::text`, since)
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
	rows, err = q.Query(ctx, `select 'comment',d.comment_id::text,d.session_id,d.resolve_error from comment_deliveries d
	 join (`+SourcesSQL+`) s on s.kind='comment' and s.id=d.comment_id::text where d.session_id <> ''`, since)
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
	// Expansion only inserts person targets; newly inserted entries need no further expansion.
	for e := range set {
		if e.ToKind == "session" {
			for _, person := range owners[e.ToID] {
				add(e.FromKind, e.FromID, "person", person)
			}
		}
	}
	edges := make([]Edge, 0, len(set))
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
