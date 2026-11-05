package refs

import (
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
)

// AllEdgesSQL is the complete derived graph relation for new named reads. graph_edges retains
// the relations old builds read; graph_person_edges adds identity and recorded delivery. Keeping
// the union here leaves old readers' result sets and query plans unchanged, including rollback.
const AllEdgesSQL = `(
	select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from graph_edges
	union all
	select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from graph_person_edges
)`

// sessionReceipts is the shared derivation of the `delivered_to` edges that reached a session,
// from the records their writers own: the recorded destinations of the four word-carrying event
// types, and the sent delivery attempts of messages and comments. It is the same relation
// graph_person_edges derives for the whole graph, narrowed to a named set of sessions and a
// lower bound so a page-sized read of it is index-sized too, which the whole-graph form cannot
// be: that form reaches a session only after unnesting every recorded destination.
//
// The topic arm tests `published_destinations && <topics>`, which migration 20261104074751's
// partial GIN index answers, and only then unnests the matched row to name which session it
// reached, so the expansion runs over the rows the index already chose and never over the log.
// The attempt arms read `comment_deliveries_session` (0146) and `message_deliveries_session`
// (0147). A failed or pending attempt is not a receipt, and neither is a role topic: a role is
// an address, and the session it resolved to is what the attempt rows record.
const sessionReceipts = `(
	select case e.type when 'comment.created' then 'comment' when 'ask.answered' then 'ask' else 'message' end as from_kind,
	       e.payload->>'id' as from_id, substring(t.topic from {offset}) as session_id
	from events e cross join lateral unnest(e.published_destinations) t(topic)
	where e.type in ('ask.answered', 'comment.created', 'message.created', 'message.answered')
	  and e.payload->>'id' <> '' and e.created_at >= {since}
	  and e.published_destinations && {topics} and t.topic = any ({topics})
	union all
	select 'message', d.message_id::text, d.session_id
	from message_deliveries d
	where d.session_id = any ({sessions}::text[]) and d.state = 'sent' and d.created_at >= {since}
	union all
	select 'comment', d.comment_id::text, d.session_id
	from comment_deliveries d
	where d.session_id = any ({sessions}::text[]) and d.state = 'sent' and d.created_at >= {since}
)`

// SessionReceiptsSQL is sessionReceipts with the calling statement's own placeholders put in:
// sessions names its `text[]` of session ids and since its lower bound. It yields one row per
// record that reached one of those sessions, as (from_kind, from_id, session_id); a caller that
// only asks whether a node was reached selects distinct from_kind, from_id.
//
// Both the person page's read and the delivery census run this one text, so the census compares
// the page's own receipts with the recorded destinations and attempts it decodes in Go, and no
// second derivation of a receipt exists to drift from it.
func SessionReceiptsSQL(sessions, since string) string {
	topics := "array(select '" + contracts.AgentTopicPrefix + "' || unnest(" + sessions + "::text[]))"
	return strings.NewReplacer(
		"{sessions}", sessions,
		"{since}", since,
		"{topics}", topics,
		"{offset}", strconv.Itoa(len(contracts.AgentTopicPrefix)+1),
	).Replace(sessionReceipts)
}
