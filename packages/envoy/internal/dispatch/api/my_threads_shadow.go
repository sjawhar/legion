package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type threadMembership struct{ Cause, Kind, ID string }
type threadSource struct{ Kind, ID string }
type participationCount struct {
	Expected   int `json:"expected"`
	Actual     int `json:"actual"`
	Missing    int `json:"missing"`
	Unexpected int `json:"unexpected"`
	Expanded   int `json:"expanded"`
}
type participationShadow struct {
	Person         string                         `json:"person"`
	CheckedAt      time.Time                      `json:"checked_at"`
	Mismatched     int                            `json:"mismatched"`
	ByCause        map[string]*participationCount `json:"by_cause"`
	AuthoredDirect int                            `json:"authored_direct"`
}

// These are raw addressing records, not graph_inbox_edges or participation. Read
// marks do not remove membership. Direct message roots retain the existing lower
// (not trimmed) author convention; stored delivery aliases require verified owners.
const participationDeliverySources = `
 select 'ask'::text, a.id::text from asks a join issues i on i.key = a.issue_key where i.assignee = $1
 union
 select case when u.kind = 'message' then 'message' when u.kind = 'ask' and u.last_reply_id = u.thread_id then 'ask' else 'comment' end,
        u.last_reply_id::text from user_threads u where u.login = $1 and u.last_reply_id is not null
 union
 select 'message', m.id::text from messages m
 cross join lateral dispatch_thread_ancestors(array['message'], array[m.id]) walk
 join messages root on walk.kind = 'message' and root.id = walk.id and walk.parent_id is null
 where m.author->>'kind' = 'session' and m.issue_key is null
   and root.in_reply_to is null and root.issue_key is null and root.target like 'session:%'
   and root.author->>'kind' = 'user' and lower(root.author->>'id') = $1
 union
 select case e.type when 'comment.created' then 'comment' when 'ask.answered' then 'ask' else 'message' end,
        e.payload->>'id'
 from events e cross join lateral unnest(e.published_destinations) d(topic)
 join person_aliases alias on alias.alias_kind = 'session' and d.topic = 'notifications.agent.' || alias.alias
 where alias.person = $1 and e.type in ('comment.created','message.created','message.answered','ask.answered')
 union
 select 'message', d.message_id::text from message_deliveries d join person_aliases a on a.alias_kind = 'session' and a.alias = d.session_id
 where a.person = $1 and d.state = 'sent'
 union
 select 'comment', d.comment_id::text from comment_deliveries d join person_aliases a on a.alias_kind = 'session' and a.alias = d.session_id
 where a.person = $1 and d.state = 'sent'`

const participationAuthoredSources = `
 select g.from_kind, g.from_id from graph_person_edges g
 where g.kind = 'authored_by' and g.to_kind = 'person' and g.to_id = $1
 union
 select 'ask', a.id::text from asks a where user_threads_login(a.answer->>'user') = $1
 union
 select 'ask', a.id::text from events e join asks a on a.id::text = e.payload->>'id'
 where e.type = 'ask.answered' and user_threads_login(e.payload->'answer'->>'user') = $1`

// Read only this person's recorded mention targets and verified session aliases.
// No graph view is used here, so an omitted participation arm remains observable.
const participationMentionSources = `
 with sessions as materialized (
   select alias from person_aliases where alias_kind = 'session' and person = $1
 )
 select r.from_kind, r.from_id from refs r
 where r.to_kind = 'person_mention' and r.to_id = $1 and r.kind = 'mentions'
   and r.from_kind in ('comment', 'message', 'ask')
 union
 select r.from_kind, r.from_id from refs r join sessions s on s.alias = r.to_id
 where r.to_kind = 'session_mention' and r.kind = 'mentions'
   and r.from_kind in ('comment', 'message', 'ask')
 union
 select 'ask', r.ask_id::text from ask_block_refs r
 join ask_blocks b on b.ask_id = r.ask_id and b.block_id = r.block_id
 where r.to_kind = 'person_mention' and r.to_id = $1 and r.kind = 'mentions' and b.position is not null
 union
 select 'ask', r.ask_id::text from ask_block_refs r
 join ask_blocks b on b.ask_id = r.ask_id and b.block_id = r.block_id
 join sessions s on s.alias = r.to_id
 where r.to_kind = 'session_mention' and r.kind = 'mentions' and b.position is not null
 union
 select 'comment', m.comment_id::text from comment_mentions m join sessions s on m.target = 'session:' || s.alias
 union
 select 'comment', m.comment_id::text from comment_mentions m join sessions s on m.resolved_session_id = s.alias
 union
 select 'comment', d.comment_id::text from comment_deliveries d join sessions s on d.session_id = s.alias
 where d.resolve_error is null or d.resolve_error not like 'no live session %'
 union
 select 'message', m.id::text from messages m join sessions s on m.target = 'session:' || s.alias
 union
 select 'message', d.message_id::text from message_deliveries d join sessions s on d.session_id = s.alias`

const participationShadowTimeout = 5 * time.Second

func loadParticipationSources(ctx context.Context, tx pgx.Tx, query, login string) ([]threadSource, error) {
	rows, err := tx.Query(ctx, query, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sources []threadSource
	for rows.Next() {
		var source threadSource
		if err := rows.Scan(&source.Kind, &source.ID); err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

// Root mapping is shared with normal writes, not reimplemented in the reference.
// The source loaders above are independent of graph_thread_participation.
func rootParticipationSources(ctx context.Context, tx pgx.Tx, cause string, sources []threadSource, into map[threadMembership]bool) (int, error) {
	if len(sources) == 0 {
		return 0, nil
	}
	kinds := make([]string, len(sources))
	ids := make([]string, len(sources))
	for i, source := range sources {
		kinds[i], ids[i] = source.Kind, source.ID
	}
	rows, err := tx.Query(ctx, `
  select distinct r.kind, r.id::text, r.issue_key is null and r.artifact_id is null as direct
  from unnest($1::text[],$2::text[]) s(kind,id)
  cross join lateral (
    select id from comments where s.kind = 'comment' and id::text = s.id
    union all select id from messages where s.kind = 'message' and id::text = s.id
    union all select id from asks where s.kind = 'ask' and id::text = s.id
  ) source
  cross join lateral dispatch_thread_ancestors(array[s.kind],array[source.id]) r
  left join artifacts a on a.id = r.artifact_id
  left join messages m on r.kind = 'message' and m.id = r.id
  where r.parent_id is null and (r.issue_key is not null or a.project_key is not null or m.target like 'session:%')
 `, kinds, ids)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	directCount := 0
	for rows.Next() {
		var kind, id string
		var direct bool
		if err := rows.Scan(&kind, &id, &direct); err != nil {
			return 0, err
		}
		into[threadMembership{cause, kind, id}] = true
		if direct {
			directCount++
		}
	}
	return directCount, rows.Err()
}

func (s *server) myThreadsShadow(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireOwnThreads(w, r, false)
	if !ok {
		return
	}
	login := model.CanonicalLogin(caller.ID)
	ctx, cancel := context.WithTimeout(r.Context(), participationShadowTimeout)
	defer cancel()
	tx, err := s.deps.Store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	report, err := readParticipationShadow(ctx, tx, login)
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("shadow my threads: %w", err))
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, report)
}

func readParticipationShadow(ctx context.Context, tx pgx.Tx, login string) (participationShadow, error) {
	report := participationShadow{Person: login, CheckedAt: time.Now().UTC(), ByCause: map[string]*participationCount{"authored": {}, "mentioned": {}, "addressed": {}}}
	expected := map[threadMembership]bool{}
	for _, source := range []struct{ cause, sql string }{{"authored", participationAuthoredSources}, {"addressed", participationDeliverySources}, {"mentioned", participationMentionSources}} {
		records, err := loadParticipationSources(ctx, tx, source.sql, login)
		if err != nil {
			return report, err
		}
		direct, err := rootParticipationSources(ctx, tx, source.cause, records, expected)
		if err != nil {
			return report, err
		}
		if source.cause == "authored" {
			report.AuthoredDirect = direct
		}
	}
	actual := map[threadMembership]bool{}
	rows, err := tx.Query(ctx, `select distinct cause,thread_kind,thread_id::text from graph_thread_participation where person=$1`, login)
	if err != nil {
		return report, err
	}
	for rows.Next() {
		var member threadMembership
		if err := rows.Scan(&member.Cause, &member.Kind, &member.ID); err != nil {
			rows.Close()
			return report, err
		}
		actual[member] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return report, err
	}
	for member := range expected {
		report.ByCause[member.Cause].Expected++
		if !actual[member] {
			report.ByCause[member.Cause].Missing++
			report.Mismatched++
		}
	}
	for member := range actual {
		count := report.ByCause[member.Cause]
		count.Actual++
		if !expected[member] {
			count.Unexpected++
			report.Mismatched++
		}
	}
	// Compare membership expansion to the untouched per-login index, not to unread
	// rows. A mentioned/addressed thread can have several causes but is never a mismatch.
	rows, err = tx.Query(ctx, `select distinct p.cause,p.thread_kind,p.thread_id::text from graph_thread_participation p where p.person=$1 and not exists(select 1 from user_threads u where u.login=$1 and u.thread_id=p.thread_id)`, login)
	if err != nil {
		return report, err
	}
	defer rows.Close()
	for rows.Next() {
		var cause, kind, id string
		if err := rows.Scan(&cause, &kind, &id); err != nil {
			return report, err
		}
		report.ByCause[cause].Expanded++
	}
	return report, rows.Err()
}
