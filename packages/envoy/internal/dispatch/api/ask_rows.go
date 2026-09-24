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

// lastReplyJoin attaches the newest comment in the ask's thread as lr; the ask must be
// aliased a. askReadFrom and listOpenAsks both read the reply through it.
const lastReplyJoin = `
		left join lateral (
			select c.author, c.created_at, c.turn from comments c
			where c.ask_id = a.id
			order by c.created_at desc, c.id desc
			limit 1
		) lr on true`

// askReadColumns are askRowColumns plus the newest comment in the ask's thread, which is
// WaitingOn for an open ask and LastReply where a read carries one. Queries selecting them
// read from askReadFrom. Event payloads are built from askRowColumns instead: they never
// carry WaitingOn.
const askReadColumns = askRowColumns + `, lr.author, lr.created_at, coalesce(lr.turn, 'human')`

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

// scanAskRead decodes one askReadColumns row; extra receives the columns after them. An
// open ask gets WaitingOn from its newest reply ("human" when nobody has replied); the
// reply itself is returned, nil when the thread is empty.
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
