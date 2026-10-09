package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// copiedAskSources finds the ask each block of blocks was copied from, keyed by block id, among the
// blocks no ask of this document indexes (rows). Block ids are unique per document only
// (asks_block_id_unique is on block_artifact_id and block_id), so a document copied from another
// carries its ask blocks' ids, and an id an author chose, such as `decision`, can name unrelated
// questions on two documents. A block is therefore a copy only of an ask on another document of
// the same owner - the same issue, or for a project document the same project's other documents -
// that indexes the same block id and asks exactly what the block asks (its question, options,
// multiple and urgency, which a copy carries unchanged), now or before an edit: the copy can be of
// an earlier version (askContentsAsked). A block whose text the copy changes is a new question and
// opens an ask. An ask settlement retracted because its block left its document is no source: a
// block cut from one document and pasted into another is the only one left, and opens an ask as any
// new block does. Of several sources, the earliest asked is the original, since a copy that opened
// asks of its own before copies were recognised came after it; two unrelated asks of one owner
// under one id asking the same thing are therefore one question to a copy, the earlier. The copy
// shows its source's state as of this settlement; answering the source later changes the copy at
// the copy's next settlement. The rows are read, never locked or written: settlement holds this
// document's owner row and room lock, and a source's answer route takes the source's ask row
// before its own document's room.
func copiedAskSources(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	owner artifactOwner,
	blocks []askBlock,
	rows map[string]model.Ask,
) (map[string]model.Ask, error) {
	var unindexed []string
	for _, block := range blocks {
		if _, indexed := rows[block.id]; !indexed {
			unindexed = append(unindexed, block.id)
		}
	}
	if len(unindexed) == 0 {
		return nil, nil
	}
	query, ownerKey := copiedAskSourcesQuery(owner)
	found, err := tx.Query(ctx, query, unindexed, artifactID, ownerKey)
	if err != nil {
		return nil, fmt.Errorf("load copied ask sources: %w", err)
	}
	defer found.Close()
	candidates := map[string][]model.Ask{}
	for found.Next() {
		ask, err := ScanAsk(found)
		if err != nil {
			return nil, fmt.Errorf("scan copied ask source: %w", err)
		}
		if settlementRetracted(ask) {
			continue
		}
		candidates[*ask.BlockID] = append(candidates[*ask.BlockID], ask)
	}
	if err := found.Err(); err != nil {
		return nil, fmt.Errorf("iterate copied ask sources: %w", err)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	sources := map[string]model.Ask{}
	var earlier []string
	for _, block := range blocks {
		for _, ask := range candidates[block.id] {
			if block.asks(askContentOf(ask)) {
				sources[block.id] = ask
				break
			}
		}
		if _, matched := sources[block.id]; !matched {
			for _, ask := range candidates[block.id] {
				earlier = append(earlier, ask.ID)
			}
		}
	}
	if len(earlier) == 0 {
		return sources, nil
	}
	asked, err := askContentsAsked(ctx, tx, earlier)
	if err != nil {
		return nil, err
	}
	for _, block := range blocks {
		if _, matched := sources[block.id]; matched {
			continue
		}
		for _, ask := range candidates[block.id] {
			if slices.ContainsFunc(asked[ask.ID], block.asks) {
				sources[block.id] = ask
				break
			}
		}
	}
	return sources, nil
}

// ownerDocuments is the predicate on artifacts d that selects owner's documents, and the value it
// binds as $3: the issue's documents (artifacts_issue_key), or for a project document the
// project's other unlinked documents (artifacts_project_documents, migration 0083); an agent
// conversation's artifacts belong to neither.
func ownerDocuments(owner artifactOwner) (string, string) {
	if owner.IssueKey != nil {
		return `d.issue_key = $3`, *owner.IssueKey
	}
	return `d.issue_key is null and d.session_id is null and d.project_key = $3`, owner.Project
}

// copiedAskSourcesQuery reads the asks of owner's other documents under the block ids $1, the
// document settled being $2, oldest first, and returns the value it binds as $3: the owner's
// documents through their index, then each one's asks through asks_block_id_unique.
func copiedAskSourcesQuery(owner artifactOwner) (string, string) {
	documents, ownerKey := ownerDocuments(owner)
	return `
		select ` + AskColumns + `
		from artifacts d
		join asks a on a.block_artifact_id = d.id and a.block_id = any($1)
		where ` + documents + ` and d.id <> $2
		order by a.created_at, a.id
	`, ownerKey
}

// owedCopiesQuery reads owner's other documents, the document settled being $2, whose latest
// version holds an ask block opener of the pairs ($1 block id, $4 LIKE pattern for its opener)
// with no ask of its own under that id, and returns the value it binds as $3.
func owedCopiesQuery(owner artifactOwner) (string, string) {
	documents, ownerKey := ownerDocuments(owner)
	return `
		select d.id::text
		from artifacts d
		join lateral (
			select v.markdown from artifact_versions v
			where v.artifact_id = d.id order by v.number desc limit 1
		) latest on true
		where ` + documents + ` and d.id <> $2 and exists (
			select 1 from unnest($1::text[], $4::text[]) as copied(block_id, pattern)
			where latest.markdown like copied.pattern and not exists (
				select 1 from asks a where a.block_artifact_id = d.id and a.block_id = copied.block_id
			)
		)
		order by d.id
	`, ownerKey
}

// owedCopiesOfRetracted marks a settlement owed, in tx, for every other document of owner whose
// latest version holds the block of an ask settlement is retracting with no ask of its own under
// that block's id, and returns them for the caller to arm once tx commits. Such a block is a copy
// of the retracted ask, or one its document has not settled yet. A copy has no ask row and shows
// its source as of its own last settlement, so without this it would go on showing an open
// question no ask holds until something else settled it: its next settlement skips the retracted
// source and opens the block's own ask. A document's latest version is read for the block's
// opener (`:::ask{#<id> …}`), so one that quotes the opener elsewhere, in code, is settled for
// nothing. The pending rows (markSettlementPending) keep the debt across a restart, for the
// resumption to arm.
func owedCopiesOfRetracted(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	owner artifactOwner,
	retracted []model.Ask,
) ([]string, error) {
	var blockIDs, patterns []string
	for _, ask := range retracted {
		if ask.BlockID == nil {
			continue
		}
		id := likeLiteral(*ask.BlockID)
		blockIDs = append(blockIDs, *ask.BlockID, *ask.BlockID)
		patterns = append(patterns, `%ask{#`+id+` %`, `%ask{#`+id+`}%`)
	}
	if len(blockIDs) == 0 {
		return nil, nil
	}
	query, ownerKey := owedCopiesQuery(owner)
	rows, err := tx.Query(ctx, query, blockIDs, artifactID, ownerKey, patterns)
	if err != nil {
		return nil, fmt.Errorf("find copies of retracted asks: %w", err)
	}
	var copies []string
	for rows.Next() {
		var copy string
		if err := rows.Scan(&copy); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan copy of a retracted ask: %w", err)
		}
		copies = append(copies, copy)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate copies of retracted asks: %w", err)
	}
	for _, copy := range copies {
		if err := markSettlementPending(ctx, tx, copy); err != nil {
			return nil, err
		}
	}
	return copies, nil
}

// likeLiteral escapes text for a LIKE pattern, under its default escape character.
func likeLiteral(text string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(text)
}

// askContentOf is the text ask asks as it stands.
func askContentOf(ask model.Ask) model.AskEditPrevious {
	return model.AskEditPrevious{Question: ask.Question, Options: ask.Options, Multiple: ask.Multiple, Urgency: ask.Urgency}
}

// asks reports whether block asks exactly what content does.
func (block askBlock) asks(content model.AskEditPrevious) bool {
	return block.question == content.Question && slices.Equal(block.options, content.Options) &&
		block.multiple == content.Multiple && block.urgency == content.Urgency
}

// copiedAskContentsQuery reads what each ask asked: an ask.opened event its text when it opened, an
// ask.edited event its text after the edit and before it (previous). Its type list is a subset of
// events_ask_payload_id's, which the planner needs to use that index (migration 0030).
const copiedAskContentsQuery = `
	select payload->>'id', payload
	from events
	where type in ('ask.opened', 'ask.edited') and payload->>'id' = any($1)
`

// askContentsAsked is every text each of the asks ids names has asked, keyed by ask id, read from
// the ask.opened and ask.edited events that record them (copiedAskContentsQuery).
func askContentsAsked(ctx context.Context, tx pgx.Tx, ids []string) (map[string][]model.AskEditPrevious, error) {
	rows, err := tx.Query(ctx, copiedAskContentsQuery, ids)
	if err != nil {
		return nil, fmt.Errorf("load what copied asks asked: %w", err)
	}
	defer rows.Close()
	asked := map[string][]model.AskEditPrevious{}
	for rows.Next() {
		var id string
		var payload []byte
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, fmt.Errorf("scan what a copied ask asked: %w", err)
		}
		var event struct {
			model.AskEditPrevious
			Previous *model.AskEditPrevious `json:"previous"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			return nil, fmt.Errorf("decode what copied ask %s asked: %w", id, err)
		}
		asked[id] = append(asked[id], event.AskEditPrevious)
		if event.Previous != nil {
			asked[id] = append(asked[id], *event.Previous)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate what copied asks asked: %w", err)
	}
	return asked, nil
}
