package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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
// new block does. Of several sources, the earliest asked is the original, whichever wording it
// matched on, since a copy that opened asks of its own before copies were recognised came after it;
// two unrelated asks of one owner under one id asking the same thing are therefore one question to
// a copy, the earlier. The copy shows its source's state as of its own last settlement, and an
// answer or a resolution of the source settles it again (SettleCopiesOf). The rows are read, never
// locked or written: settlement holds this document's owner row and room lock, and a source's
// answer route takes the source's ask row before its own document's room. A project document's
// settlement also takes its project's copy lock first (lockProjectCopies).
func copiedAskSources(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	owner artifactOwner,
	blocks []askBlock,
	rows map[string]model.Ask,
) (map[string]copiedSource, error) {
	var unindexed []string
	for _, block := range blocks {
		if _, indexed := rows[block.id]; !indexed {
			unindexed = append(unindexed, block.id)
		}
	}
	if len(unindexed) == 0 {
		return nil, nil
	}
	if err := lockProjectCopies(ctx, tx, owner); err != nil {
		return nil, err
	}
	query, ownerKey := copiedAskSourcesQuery(owner)
	found, err := tx.Query(ctx, query, unindexed, artifactID, ownerKey)
	if err != nil {
		return nil, fmt.Errorf("load copied ask sources: %w", err)
	}
	defer found.Close()
	candidates := map[string][]copiedSource{}
	for found.Next() {
		var issueKey *string
		var project, slug, kind string
		var primary bool
		ask, err := ScanAsk(found, &issueKey, &project, &slug, &kind, &primary)
		if err != nil {
			return nil, fmt.Errorf("scan copied ask source: %w", err)
		}
		if settlementRetracted(ask) {
			continue
		}
		document := refs.ArtifactRef(issueKey, project, nil, slug, kind, primary)
		candidates[*ask.BlockID] = append(candidates[*ask.BlockID], copiedSource{ask: ask, document: document})
	}
	if err := found.Err(); err != nil {
		return nil, fmt.Errorf("iterate copied ask sources: %w", err)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	// The earliest asked match wins whichever wording it matched on, so an ask's earlier wordings
	// are read only for the candidates older than a block's first match on its current wording.
	var earlier []string
	for _, block := range blocks {
		for _, candidate := range candidates[block.id] {
			if block.asks(model.NewAskEditPrevious(candidate.ask)) {
				break
			}
			earlier = append(earlier, candidate.ask.ID)
		}
	}
	var asked map[string][]model.AskEditPrevious
	if len(earlier) > 0 {
		if asked, err = askContentsAsked(ctx, tx, earlier); err != nil {
			return nil, err
		}
	}
	sources := map[string]copiedSource{}
	for _, block := range blocks {
		for _, candidate := range candidates[block.id] {
			if block.asks(model.NewAskEditPrevious(candidate.ask)) || slices.ContainsFunc(asked[candidate.ask.ID], block.asks) {
				sources[block.id] = candidate
				break
			}
		}
	}
	return sources, nil
}

// copiedSource is the ask a copied block was copied from and the address of the document it is
// on (refs.ArtifactRef), which settlement writes into the copy as copied_from and
// copied_from_document, so a reader names the source and where it is answered without asking.
type copiedSource struct {
	ask      model.Ask
	document string
}

// lockProjectCopies serialises, for a project document, its project's copy bookkeeping: a
// settlement's copy-source read (copiedAskSources), and the owed copies a settlement's retraction
// or a source's answer or resolution marks (owedCopiesOf). Without it two copies of one retracted
// ask settling at once would each find no source and each open an ask. An issue's documents need
// no lock of their own, since every one of those transactions already holds their issue's row
// (lockArtifactOwner, requireOpenOwner); a project document's owner row is the document itself,
// which no sibling takes. The lock is a transaction advisory lock keyed on the project, as
// lockProjectRankAllocation's is, so read marks, event appends and issue creation, which lock the
// project's row, never wait on a settlement. hashtext is 32 bits wide, so two projects can share a
// key: they then serialise their copy bookkeeping, and nothing else. Every holder takes it before
// any doc_settlements_pending row and before the events' commit-order lock: a settlement right
// after its owner row and room lock, an answer or a resolution after its owner row and ask row. The
// copy-source read is the next statement, so under READ COMMITTED its snapshot holds every ask a
// sibling's settlement committed while this one waited, and the ask createAskBlock opens commits in
// this transaction, which holds the lock until then. A settlement whose blocks all have asks of
// their own and that retracts none takes none.
func lockProjectCopies(ctx context.Context, tx pgx.Tx, owner artifactOwner) error {
	if owner.IssueKey != nil || owner.Project == "" {
		return nil
	}
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('copied-asks:' || $1))`, owner.Project); err != nil {
		return fmt.Errorf("lock the project's copied asks: %w", err)
	}
	return nil
}

// ownerDocuments is the predicate on artifacts d that selects owner's documents, and the value it
// binds as $3: the issue's documents (artifacts_issue_key), or for a project document the
// project's other unlinked documents (artifacts_project_documents, migration 0085); an agent
// conversation's artifacts belong to neither.
func ownerDocuments(owner artifactOwner) (string, string) {
	if owner.IssueKey != nil {
		return `d.issue_key = $3`, *owner.IssueKey
	}
	return `d.issue_key is null and d.session_id is null and d.project_key = $3`, owner.Project
}

// copiedAskSourcesQuery reads the asks of owner's other documents under the block ids $1, the
// document settled being $2, oldest first, each followed by its document's issue key, project,
// slug, kind and whether it is the issue's spec, and returns the value it binds as $3: the owner's
// documents through their index, then each one's asks through asks_block_id_unique.
func copiedAskSourcesQuery(owner artifactOwner) (string, string) {
	documents, ownerKey := ownerDocuments(owner)
	return `
		select ` + AskColumns + `, d.issue_key, coalesce(d.project_key, ''), d.slug, d.kind, d.is_primary
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

// owedCopiesOf marks a settlement owed, in tx, for every other document of owner whose latest
// version holds the block of one of asks with no ask of its own under that block's id, and returns
// them for the caller to arm once tx commits. Such a block is a copy of the ask, or one its
// document has not settled yet, and shows the ask as of its own last settlement. Settlement calls
// this for the asks it retracts, whose copies then open the block's own ask, and an answer or a
// resolution for the ask it closes (SettleCopiesOf), whose copies then show it. A document's latest
// version is read for the block's opener (`:::ask{#<id> …}`), so one that quotes the opener
// elsewhere, in code, is settled for nothing. The pending rows (markSettlementPending, one
// statement however many copies there are) keep the debt across a restart, for the resumption to
// arm. They are taken under the project's copy lock (lockProjectCopies), and the caller has taken
// no other pending row and not the events' commit-order lock: a copy's settlement deletes its own
// row before it appends an event, so the two can wait on each other in only one direction.
func owedCopiesOf(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	owner artifactOwner,
	asks []model.Ask,
) ([]string, error) {
	var blockIDs, patterns []string
	for _, ask := range asks {
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
	if err := lockProjectCopies(ctx, tx, owner); err != nil {
		return nil, err
	}
	query, ownerKey := owedCopiesQuery(owner)
	rows, err := tx.Query(ctx, query, blockIDs, artifactID, ownerKey, patterns)
	if err != nil {
		return nil, fmt.Errorf("find copies of asks: %w", err)
	}
	var copies []string
	for rows.Next() {
		var copy string
		if err := rows.Scan(&copy); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan a copy of an ask: %w", err)
		}
		copies = append(copies, copy)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate copies of asks: %w", err)
	}
	if len(copies) == 0 {
		return nil, nil
	}
	if err := markSettlementPending(ctx, tx, copies, nil, false, true); err != nil {
		return nil, err
	}
	return copies, nil
}

// SettleCopiesOf marks owed, in the transaction ctx joined, the settlement of every other document
// of ask's owner showing a copy of it (owedCopiesOf), and arms each once that transaction commits
// (Ledger.Commit), so a copy shows an answer or a resolution of its source a settlement delay after
// it, rather than at its own next edit. ask is a block ask, its BlockID and BlockArtifactID set.
// The caller holds ask's owner row and ask row, and calls this before it writes the source's block
// or appends an event: the copy lock and the copies' pending rows come before the source's room
// lock, its own pending row and the events' commit-order lock.
func (s *Service) SettleCopiesOf(ctx context.Context, ask model.Ask) error {
	tx, joined := txFromContext(ctx)
	if !joined {
		return errUnjoined
	}
	if ask.BlockID == nil || ask.BlockArtifactID == nil {
		return fmt.Errorf("ask %q is not a block ask", ask.ID)
	}
	var owner artifactOwner
	if err := tx.QueryRow(ctx, `
		select issue_key, coalesce(project_key, '') from artifacts where id = $1
	`, *ask.BlockArtifactID).Scan(&owner.IssueKey, &owner.Project); err != nil {
		return fmt.Errorf("load the ask's document owner: %w", err)
	}
	copies, err := owedCopiesOf(ctx, tx, *ask.BlockArtifactID, owner, []model.Ask{ask})
	if err != nil {
		return err
	}
	ledger := ledgerFrom(ctx)
	ledger.copies = append(ledger.copies, copies...)
	return nil
}

// likeLiteral escapes text for a LIKE pattern, under its default escape character.
func likeLiteral(text string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(text)
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
