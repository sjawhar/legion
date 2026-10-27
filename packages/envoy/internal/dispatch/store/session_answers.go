package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Queryer is the read surface *Pool and pgx.Tx share.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// sessionAnswersQuery reads, in one statement, every item a person wrote that Dispatch delivered to
// the session $1 since $2, $3 being the session's own topic (contracts.AgentSubject). Its three
// sources:
//
//   - S1 (reached): events a person wrote of a type that carries a person's words to a session -
//     ask.answered, comment.created, message.created and message.answered (a reply to a message) -
//     created since the window's start, whose published_destinations hold the session's topic: the
//     outbox's own record of each topic it published the event to (outbox.publishDestination), so
//     the list holds what the follower, author and session routes delivered, and never what an
//     issue's role: route delivered to a role topic, which names no session. It reads the partial
//     index events_person_words (0121) for the window and filters the array on those rows.
//   - S2 (message_ids' second arm): the session's sent message deliveries (message_deliveries_session):
//     targeted messages, direct or on an issue, and broadcast copies.
//   - S3 (mentioned): the session's sent comment deliveries, a mention's (comment_deliveries_session,
//     0122). A delivery row's created_at is its transaction's now(), which for a comment's first
//     attempt precedes the comment's own clock_timestamp() created_at, so the window is applied to
//     the comment and not to its delivery.
//
// Each source names a row by its id, and each kind's ids are a set, so an item reached several ways
// is one row. Every row is then read at its current words: an ask's current answer (asks.answer)
// when that answer's own time is in the window, a comment's current body, a message's body. A
// comment or message an agent or the system wrote is left out by its author; an answer is a
// person's by construction. The ids are compared as text, through the asks_id_text, comments_id_text
// and messages_id_text indexes, so a payload id that is not a uuid cannot fail the read.
//
// replaced counts an ask's real answers as api's answerEventsQuery does: the ask.answered events
// whose actor is the user the answer names, one per answer time (a settlement that restores an
// answered block writes the same answer again under its editor). answer_events reads the listed
// asks' ask.answered events by their payload id alone (events_ask_payload_id) and is materialized,
// so the actor check runs on those few rows: with it inside the scan, the planner ANDs in
// events_person_words, whose predicate the check implies, and reads every entry of that index for
// each ask.
const sessionAnswersQuery = `
	with reached as (
		select e.type, e.payload->>'id' as id
		from events e
		where e.actor->>'kind' = 'user'
		  and e.type in ('ask.answered', 'comment.created', 'message.created', 'message.answered')
		  and e.created_at >= $2
		  and $3 = any(e.published_destinations)
	), mentioned as (
		select cd.comment_id::text as id
		from comment_deliveries cd
		where cd.session_id = $1 and cd.state = 'sent'
	), answer_ids as (
		select distinct id from reached where type = 'ask.answered'
	), answer_events as materialized (
		select e.payload->>'id' as id, e.actor, e.payload->'answer' as answer
		from events e
		where e.type = 'ask.answered' and e.payload->>'id' in (select id from answer_ids)
	), comment_ids as (
		select id from reached where type = 'comment.created'
		union
		select id from mentioned
	), message_ids as (
		select id from reached where type in ('message.created', 'message.answered')
		union
		select md.message_id::text
		from message_deliveries md
		where md.session_id = $1 and md.state = 'sent'
	)
	select 'answer' as kind, a.id::text as id, (a.answer->>'at')::timestamptz as at,
	       null::timestamptz as edited_at, null::jsonb as author, null::text as body, a.answer,
	       (select count(distinct ae.answer->>'at') from answer_events ae
	         where ae.id = x.id and ae.actor->>'kind' = 'user' and ae.actor->>'id' = ae.answer->>'user') > 1 as replaced,
	       a.id::text as ask_id, a.question as ask_question,
	       i.key as issue_key, i.title as issue_title, ar.project_key, ar.slug, ar.name,
	       false as mentioned, false as in_ask_thread, false as in_comment_thread, false as on_document,
	       false as replies_to_session
	from answer_ids x
	join asks a on a.id::text = x.id
	left join issues i on i.key = a.issue_key
	left join artifacts ar on ar.id = a.artifact_id
	where a.answer is not null and (a.answer->>'at')::timestamptz >= $2
	union all
	select 'comment', c.id::text, c.created_at, c.edited_at, c.author, c.body, null, false,
	       q.id::text, q.question,
	       i.key, i.title, ar.project_key, ar.slug, ar.name,
	       exists (select 1 from mentioned m where m.id = x.id),
	       c.ask_id is not null, c.reply_to is not null,
	       c.artifact_id is not null or c.anchor->>'artifact_id' is not null,
	       false
	from comment_ids x
	join comments c on c.id::text = x.id
	left join asks q on q.id = c.ask_id
	left join issues i on i.key = c.issue_key
	left join artifacts ar on ar.id = c.artifact_id
	where c.author->>'kind' = 'user' and c.created_at >= $2
	union all
	select 'message', m.id::text, m.created_at, null, m.author, m.body, null, false,
	       null, null,
	       i.key, i.title, null, null, null,
	       false, false, false, false,
	       exists (select 1 from messages p where p.id = m.in_reply_to
	               and p.author->>'kind' = 'session' and p.author->>'id' = $1)
	from message_ids x
	join messages m on m.id::text = x.id
	left join issues i on i.key = m.issue_key
	where m.author->>'kind' = 'user' and m.created_at >= $2
	order by at desc, id desc`

// SessionAnswers lists what people wrote that Dispatch delivered to session since since, newest
// first by time, then by id descending: an ask's current answer, a comment at its current body, or
// a message, each (kind, id) once (sessionAnswersQuery). It is GET
// /api/v1/agents/{session_id}/answers' read, which the outbox's tests read back through too, so
// the route reads what the publisher records.
//
// An item reached several ways carries one relation, the most specific to the session, in this
// order of precedence: a comment is mentioned_you when a mention delivered it to the session (it
// named this session in particular), else replied_on_your_ask in an ask's thread, else
// replied_in_a_comment_thread when it replies to another comment, else commented_on_a_document when
// it is on a document (anchored on one, or on a project document), else commented_on_an_issue. A
// message is replied_to_your_message when it replies to a message the session wrote, else
// sent_you_a_message. An answer is always answered_your_ask: an ask's answer reaches its asker and
// its followers, and the session route reaches the session an issue is routed to.
func SessionAnswers(ctx context.Context, q Queryer, session string, since time.Time) ([]model.SessionAnswer, error) {
	rows, err := q.Query(ctx, sessionAnswersQuery, session, since, contracts.AgentSubject(session))
	if err != nil {
		return nil, fmt.Errorf("read session %q answers: %w", session, err)
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.SessionAnswer, error) {
		return scanSessionAnswer(row, session)
	})
	if err != nil {
		return nil, fmt.Errorf("read session %q answers: %w", session, err)
	}
	if items == nil {
		items = []model.SessionAnswer{}
	}
	return items, nil
}

// scanSessionAnswer decodes one sessionAnswersQuery row into the item it is to session.
func scanSessionAnswer(row pgx.Row, session string) (model.SessionAnswer, error) {
	var (
		item                                                model.SessionAnswer
		author, answer                                      []byte
		body, askID, askQuestion                            *string
		issueKey, issueTitle, docProject, docSlug, docName  *string
		mentioned, inAskThread, inCommentThread, onDocument bool
		repliesToSession                                    bool
	)
	if err := row.Scan(
		&item.Kind, &item.ID, &item.At, &item.EditedAt, &author, &body, &answer, &item.Replaced,
		&askID, &askQuestion, &issueKey, &issueTitle, &docProject, &docSlug, &docName,
		&mentioned, &inAskThread, &inCommentThread, &onDocument, &repliesToSession,
	); err != nil {
		return model.SessionAnswer{}, err
	}
	switch item.Kind {
	case model.SessionAnswerKindAnswer:
		var current model.AskAnswer
		if err := json.Unmarshal(answer, &current); err != nil {
			return model.SessionAnswer{}, fmt.Errorf("decode ask %s answer: %w", item.ID, err)
		}
		item.Relation = model.SessionAnswerRelationAnsweredYourAsk
		item.At = current.At
		item.Author = model.Actor{Kind: "user", ID: current.User}
		if current.Text != nil {
			item.Text = *current.Text
		}
		item.Selected = current.Selected
	case model.SessionAnswerKindComment, model.SessionAnswerKindMessage:
		if err := json.Unmarshal(author, &item.Author); err != nil {
			return model.SessionAnswer{}, fmt.Errorf("decode %s %s author: %w", item.Kind, item.ID, err)
		}
		item.Text = *body
		switch {
		case item.Kind == model.SessionAnswerKindMessage && repliesToSession:
			item.Relation = model.SessionAnswerRelationRepliedToYourMessage
		case item.Kind == model.SessionAnswerKindMessage:
			item.Relation = model.SessionAnswerRelationSentYouMessage
		case mentioned:
			item.Relation = model.SessionAnswerRelationMentionedYou
		case inAskThread:
			item.Relation = model.SessionAnswerRelationRepliedOnYourAsk
		case inCommentThread:
			item.Relation = model.SessionAnswerRelationRepliedInCommentThread
		case onDocument:
			item.Relation = model.SessionAnswerRelationCommentedOnDocument
		default:
			item.Relation = model.SessionAnswerRelationCommentedOnIssue
		}
	default:
		return model.SessionAnswer{}, fmt.Errorf("session answer %s has kind %q", item.ID, item.Kind)
	}
	item.At = item.At.UTC()
	if item.EditedAt != nil {
		edited := item.EditedAt.UTC()
		item.EditedAt = &edited
	}
	if askID != nil {
		item.Ask = &model.SessionAnswerAsk{ID: *askID, Question: *askQuestion}
	}
	item.Owner, item.Ref, item.Href = sessionAnswerPlace(item.Kind, item.ID, session, issueKey, issueTitle, docProject, docSlug, docName)
	return item, nil
}

// sessionAnswerPlace is where an item lives: its owner, its dispatch:// address and its relative
// deep link, in the shapes DISPATCH_HREF_REFERENCES (packages/contracts/src/dispatch-href.ts)
// lists - under its issue, or under the project document holding it. A message on no issue is a
// direct message to the session: it has no owner and no address, and links to the session's
// conversation.
func sessionAnswerPlace(kind, id, session string, issueKey, issueTitle, docProject, docSlug, docName *string) (*model.OpenAskOwner, *string, string) {
	segment := map[string]string{
		model.SessionAnswerKindAnswer:  "ask",
		model.SessionAnswerKindComment: "comment",
		model.SessionAnswerKindMessage: "message",
	}[kind]
	switch {
	case issueKey != nil:
		ref := "dispatch://" + *issueKey + "/" + segment + "/" + id
		return &model.OpenAskOwner{Issue: &model.OpenAskIssue{Key: *issueKey, Title: *issueTitle}},
			&ref, "/issues/" + *issueKey + "/" + segment + "s/" + id
	case docProject != nil:
		ref := "dispatch://" + *docProject + "/artifact/" + *docSlug + "/" + segment + "/" + id
		return &model.OpenAskOwner{Document: &model.OpenAskDocument{Project: *docProject, Slug: *docSlug, Name: *docName}},
			&ref, "/projects/" + *docProject + "/documents/" + *docSlug + "?" + segment + "=" + id
	default:
		return nil, nil, "/agents/" + url.PathEscape(session) + "/live"
	}
}
