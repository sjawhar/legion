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

// askRowColumns are docs.AskColumns plus the documents a block ask or quoted anchor names;
// every ask read carries either document when it applies. Queries selecting them read from
// askRowFrom.
const askRowColumns = docs.AskColumns + `, ba.id::text, ba.slug, ba.is_primary,
	aa.project_key, aa.slug, aa.name, aa.is_primary`

const askRowFrom = `from asks a
	left join artifacts ba on ba.id = a.block_artifact_id
	left join artifacts aa on aa.id = (a.anchor->>'artifact_id')::uuid`

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

// waitingOnExpression is the one turn rule every open-ask read, the Inbox order, a reply's
// waiting_on and the approval route's hand-back decision share. A moved approval request waits on
// its agent until that agent hands its current version back. A hand-back records the reply that
// was newest when it ran (handed_back_reply_id), so the request waits on the human until a newer
// reply's turn decides it. Every other ask follows its newest reply's turn.
const waitingOnExpression = `case
	when a.kind = 'approval'
		and (a.approval->>'requested_version')::bigint < (a.approval->>'version')::bigint
	then 'agent'
	when a.kind = 'approval' and lr.id = a.handed_back_reply_id
	then 'human'
	else coalesce(lr.turn, 'human')
end`

// askWaitingOnQuery reads whom the open ask $1 waits on. askWaitingOn runs it on its own, and a
// comment's insert sends it in the same round trip, right behind the insert
// (commentThreadTarget.insertComment).
const askWaitingOnQuery = `select ` + waitingOnExpression + `
		from asks a` + lastReplyJoin + `
		where a.id = $1 and a.state = 'open'`

func askWaitingOn(ctx context.Context, q queryer, askID string) (string, error) {
	return scanAskWaitingOn(q.QueryRow(ctx, askWaitingOnQuery, askID))
}

func scanAskWaitingOn(row pgx.Row) (string, error) {
	var waitingOn string
	if err := row.Scan(&waitingOn); err != nil {
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

// scanAskRow decodes one askRowColumns row; extra receives the columns after them.
func scanAskRow(row pgx.Row, extra ...any) (model.Ask, error) {
	var blockArtifactID, blockArtifactSlug *string
	var blockArtifactPrimary *bool
	var anchorProject, anchorSlug, anchorName *string
	var anchorPrimary *bool
	ask, err := docs.ScanAsk(
		row,
		append(
			[]any{
				&blockArtifactID,
				&blockArtifactSlug,
				&blockArtifactPrimary,
				&anchorProject,
				&anchorSlug,
				&anchorName,
				&anchorPrimary,
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
	if anchorProject != nil {
		ask.AnchorArtifact = &model.AskAnchorArtifact{
			Project: *anchorProject,
			Slug:    *anchorSlug,
			Name:    *anchorName,
			Primary: *anchorPrimary,
		}
	}
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

func (s *server) loadAskAnchorArtifact(
	ctx context.Context,
	q queryer,
	artifactID string,
) (*model.AskAnchorArtifact, error) {
	var artifact model.AskAnchorArtifact
	if err := q.QueryRow(ctx, `
		select project_key, slug, name, is_primary
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
