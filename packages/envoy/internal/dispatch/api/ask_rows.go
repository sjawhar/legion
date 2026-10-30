package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// askRowColumns are docs.AskColumns plus the documents a block ask, a quoted anchor or an
// approval request names; every ask read carries whichever document applies. Queries selecting
// them read from askRowFrom.
const askRowColumns = docs.AskColumns + `, ba.id::text, ba.slug, ba.is_primary,
	aa.project_key, aa.slug, aa.name, aa.is_primary,
	pa.project_key, pa.slug, pa.name, pa.is_primary`

const askRowFrom = `from asks a
	left join artifacts ba on ba.id = a.block_artifact_id
	left join artifacts aa on aa.id = (a.anchor->>'artifact_id')::uuid
	left join artifacts pa on pa.id = (a.approval->>'artifact_id')::uuid`

// newestReply selects the newest comment in the thread of the ask aliased a: the one that
// committed last, since a comment's created_at is when its insert ran (migration 0065) and every
// comment insert first takes the owner row. lastReplyJoin reads it for every ask read, and a
// hand-back records its id, so both agree on which reply is newest.
const newestReply = `select c.id, c.author, c.created_at, c.turn from comments c
			where c.ask_id = a.id
			order by c.created_at desc, c.id desc
			limit 1`

// lastReplyJoin attaches the newest comment in the ask's thread as lr; the ask must be
// aliased a. askReadFrom and listOpenAsks both read the reply through it.
const lastReplyJoin = `
		left join lateral (` + newestReply + `) lr on true`

// approvalMoved says the ask aliased a is an approval request a version moved past the version its
// agent last handed to the human. waitingOnExpression and describeAsk, which reads it for a reply's
// waiting_on (commentThreadTarget.waitingOnAfter), share it.
const approvalMoved = `(a.kind = 'approval'
		and (a.approval->>'requested_version')::bigint < (a.approval->>'version')::bigint)`

// waitingOnExpression is the one turn rule every open-ask read, the Inbox order, a document's
// awaiting approval and the approval route's hand-back decision share; a reply's own waiting_on
// applies it to the reply it just inserted (commentThreadTarget.waitingOnAfter). A moved approval
// request waits on its agent until that agent hands its current version back. A hand-back records
// the reply that was newest when it ran (handed_back_reply_id), so the request waits on the human
// until a newer reply's turn decides it. Every other ask follows its newest reply's turn.
const waitingOnExpression = `case
	when ` + approvalMoved + `
	then 'agent'
	when a.kind = 'approval' and lr.id = a.handed_back_reply_id
	then 'human'
	else coalesce(lr.turn, 'human')
end`

// askItemRefExpression is the SPA item route of the ask aliased a, whose issue is aliased i and
// whose project document is aliased ar: an issue ask's `/issues/<key>/asks/<id>`, which lands an
// anchored ask on its document and an unanchored one on its Conversation turn (the bare issue page
// reads no `?ask=`), or a document ask's `?ask=` link. listOpenAsks and listMyAnswers share it.
const askItemRefExpression = `case
	when i.key is not null then '/issues/' || i.key || '/asks/' || a.id::text
	else '/projects/' || ar.project_key || '/documents/' || ar.slug || '?ask=' || a.id::text
end`

// askWaitingOn reads whom the open ask askID waits on, for the approval route's hand-back decision.
func askWaitingOn(ctx context.Context, q queryer, askID string) (string, error) {
	var waitingOn string
	if err := q.QueryRow(ctx, `select `+waitingOnExpression+`
		from asks a`+lastReplyJoin+`
		where a.id = $1 and a.state = 'open'`, askID).Scan(&waitingOn); err != nil {
		return "", fmt.Errorf("read ask waiting_on: %w", err)
	}
	return waitingOn, nil
}

// askReadColumns are askRowColumns plus the newest comment in the ask's thread. WaitingOn derives
// from that reply with the moved and newly handed-back approval-request overrides in
// waitingOnExpression, aliased waiting_on so a query ordering by it (the Inbox) names the column
// rather than evaluating the rule a second time; LastReply still names the newest comment. Queries
// selecting them read from askReadFrom. Event payloads are built from askRowColumns instead: they
// never carry WaitingOn.
const askReadColumns = askRowColumns + `, lr.author, lr.created_at, ` + waitingOnExpression + ` as waiting_on`

const askReadFrom = askRowFrom + lastReplyJoin

// nullableAskArtifactRef holds the nullable columns of one optional document an ask names - a
// quoted anchor's or an approval request's artifact_id, both resolved through a left join that
// may find no row. artifact is nil exactly when the join found none.
type nullableAskArtifactRef struct {
	project, slug, name *string
	primary             *bool
}

func (ref nullableAskArtifactRef) artifact() *model.AskAnchorArtifact {
	if ref.project == nil {
		return nil
	}
	return &model.AskAnchorArtifact{
		Project: *ref.project, Slug: *ref.slug, Name: *ref.name, Primary: *ref.primary,
	}
}

// scanAskRow decodes one askRowColumns row; extra receives the columns after them.
func scanAskRow(row pgx.Row, extra ...any) (model.Ask, error) {
	var blockArtifactID, blockArtifactSlug *string
	var blockArtifactPrimary *bool
	var anchor, approval nullableAskArtifactRef
	ask, err := docs.ScanAsk(
		row,
		append(
			[]any{
				&blockArtifactID,
				&blockArtifactSlug,
				&blockArtifactPrimary,
				&anchor.project,
				&anchor.slug,
				&anchor.name,
				&anchor.primary,
				&approval.project,
				&approval.slug,
				&approval.name,
				&approval.primary,
			},
			extra...,
		)...,
	)
	if err != nil {
		return model.Ask{}, err
	}
	if blockArtifactID != nil {
		ask.BlockArtifact = &model.AskBlockArtifact{
			ID: *blockArtifactID, Slug: *blockArtifactSlug, Primary: *blockArtifactPrimary,
		}
	}
	ask.AnchorArtifact = anchor.artifact()
	ask.ApprovalArtifact = approval.artifact()
	return ask, nil
}

// attachAskBacklinkCounts reads every listed ask's inbound graph-edge count in one query. The
// count is nullable on model.Ask because mutation responses do not pay for it; list and detail
// readers always set it, including zero. An ask's own clarification replies are inbound edges
// too (`graph_edges`' `replies_to` arm), and the card renders that thread inline directly
// below the count, so they are not what "referenced by" counts.
func attachAskBacklinkCounts(ctx context.Context, q queryer, asks []*model.Ask) error {
	if len(asks) == 0 {
		return nil
	}
	ids := make([]string, 0, len(asks))
	for _, ask := range asks {
		ids = append(ids, ask.ID)
	}
	counts, err := refs.BacklinkCounts(ctx, q, "ask", ids, refs.AskBacklinkExclusions)
	if err != nil {
		return err
	}
	for _, ask := range asks {
		count := counts[ask.ID]
		ask.ReferencedByCount = &count
	}
	return nil
}

// attachAskPartIDs reads, in one query over `ask_blocks`, the part id of every listed ask's
// options and answer and the ask's current part revision, so every read that shows an ask's
// options shows the reference of each: `dispatch://<key>/ask/<id>#b-<part id>`, pinned to the
// ask's `revision` with `@v<N>`. It is read beside attachAskBacklinkCounts, which every list and
// detail read of an ask already pays for, and never by a write, whose answer carries no id.
//
// A part is matched to the option it stands for by its stored position and type, never by
// hashing the option's label a second time: the index owns the identity of a part, and a reader
// of this id must get back exactly the part the index wrote. An ask whose parts are not indexed
// yet - owed by migration 0149's trigger and never drained, so no build with the index has
// written them - has no state row or no parts, and carries no id and revision 0: a read of an ask
// never depends on the index being current. So does an ask whose stored parts stand for other
// options than the row now holds, where the index is behind the row and positions no longer line
// up; the revision it has reached is still its own.
func attachAskPartIDs(ctx context.Context, q queryer, asks []*model.Ask) error {
	if len(asks) == 0 {
		return nil
	}
	ids := make([]string, 0, len(asks))
	for _, ask := range asks {
		ids = append(ids, ask.ID)
	}
	parts, err := readAskParts(ctx, q, ids)
	if err != nil {
		return err
	}
	for _, ask := range asks {
		stored, indexed := parts[ask.ID]
		if !indexed {
			continue
		}
		ask.Revision = stored.revision
		optionIDs := make([]string, 0, len(stored.ids))
		answerID := ""
		for index, partType := range stored.types {
			switch partType {
			case refs.AskOptionPart:
				optionIDs = append(optionIDs, stored.ids[index])
			case refs.AskAnswerPart:
				answerID = stored.ids[index]
			}
		}
		if len(optionIDs) != len(ask.Options) || (answerID != "") != (ask.Answer != nil) {
			continue
		}
		for index := range ask.Options {
			ask.Options[index].ID = optionIDs[index]
		}
		if ask.Answer != nil {
			ask.Answer.ID = answerID
		}
	}
	return nil
}

// attachAskReadFields is what every list and detail read of an ask attaches beyond its row: its
// inbound graph-edge count (attachAskBacklinkCounts), then the part ids of its options and of its
// answer with its part revision (attachAskPartIDs). A write attaches neither. The two are always
// read together and in this order, so the reads a caller pays for, and the error it reports, are
// the same at every site.
func attachAskReadFields(ctx context.Context, q queryer, asks []*model.Ask) error {
	if err := attachAskBacklinkCounts(ctx, q, asks); err != nil {
		return err
	}
	return attachAskPartIDs(ctx, q, asks)
}

// askParts is one ask's indexed parts as attachAskPartIDs reads them: the revision its parts have
// reached, and the id and type of each current part in position order (AskParts' order: the
// question, then the options, then the answer the ask holds now).
type askParts struct {
	revision   int
	ids, types []string
}

// readAskParts reads the indexed parts of each ask, keyed by ask id; an ask with no state row,
// which no build with the part index has written, is absent. One statement for the whole list,
// with the parts of each ask as arrays in position order, as api/item_blocks.go reads a comment's
// blocks; an ask whose state row holds no part yet reads as an empty list.
func readAskParts(ctx context.Context, q queryer, askIDs []string) (map[string]askParts, error) {
	rows, err := q.Query(ctx, `
		select s.ask_id::text, s.revision,
		       coalesce(array_agg(b.block_id order by b.position) filter (where b.block_id is not null), '{}') as ids,
		       coalesce(array_agg(b.type order by b.position) filter (where b.block_id is not null), '{}') as types
		from ask_block_state s
		left join ask_blocks b on b.ask_id = s.ask_id and b.position is not null
		where s.ask_id = any($1::uuid[])
		group by s.ask_id, s.revision
	`, askIDs)
	if err != nil {
		return nil, fmt.Errorf("read ask parts: %w", err)
	}
	defer rows.Close()
	parts := make(map[string]askParts, len(askIDs))
	for rows.Next() {
		var askID string
		var stored askParts
		if err := rows.Scan(&askID, &stored.revision, &stored.ids, &stored.types); err != nil {
			return nil, fmt.Errorf("scan ask parts: %w", err)
		}
		parts[askID] = stored
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read ask parts: %w", err)
	}
	return parts, nil
}

func (s *server) loadAskAnchorArtifact(
	ctx context.Context,
	q queryer,
	artifactID string,
) (*model.AskAnchorArtifact, error) {
	var artifact model.AskAnchorArtifact
	if err := q.QueryRow(ctx, `
		select coalesce(project_key, ''), slug, name, is_primary
		from artifacts
		where id = $1
	`, artifactID).Scan(&artifact.Project, &artifact.Slug, &artifact.Name, &artifact.Primary); err != nil {
		return nil, err
	}
	return &artifact, nil
}

// scanAskRead decodes one askReadColumns row; an open ask gets WaitingOn from
// waitingOnExpression, and the newest reply is returned separately (nil when the thread is
// empty).
func scanAskRead(row pgx.Row, extra ...any) (model.Ask, *model.AskLastReply, error) {
	var replyAuthor []byte
	var repliedAt *time.Time
	var turn string
	ask, err := scanAskRow(row, append([]any{&replyAuthor, &repliedAt, &turn}, extra...)...)
	if err != nil {
		return model.Ask{}, nil, err
	}
	if ask.State == "open" {
		ask.WaitingOn = turn
	}
	if repliedAt == nil {
		return ask, nil, nil
	}
	var reply model.AskLastReply
	if err := json.Unmarshal(replyAuthor, &reply.Author); err != nil {
		return model.Ask{}, nil, fmt.Errorf("decode ask last reply author: %w", err)
	}
	reply.CreatedAt = timestampValue(*repliedAt)
	return ask, &reply, nil
}
