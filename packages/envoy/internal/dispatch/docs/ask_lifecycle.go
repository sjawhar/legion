package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// BlockRef names one block of one document: the document's artifact id and the block's id.
type BlockRef struct {
	ArtifactID string
	BlockID    string
}

// errSettleJoined refuses SettleIssueDocuments inside a transaction: each settlement opens a
// transaction of its own and takes the issue's row, which a caller's open transaction can hold.
var errSettleJoined = errors.New("docs: an issue's documents settle outside every transaction")

// WriteIssueAskStates writes the state of each of asks into the blocks that show it in every
// document of issueKey, in the documents' id order: an ask's own block on its document, and every
// block copiedAskSources classifies as a copy of one of asks on another, which keeps naming its
// source (copied_from, copied_from_document). Each block gets the server attributes settlement
// writes from the ask (setAskServerAttributes), so asks carry the state the caller wants shown:
// resolved for an issue's close, open for its reopen. Every other block is left as it is - the
// blocks of other asks, their copies, a block under one of asks' block ids that asks something
// else - and so is a block settlement could not read. Classification reads the database as it
// stands now: the caller writes the asks' rows after this for a close, and before it for a reopen.
//
// With markUnindexed it also writes state="resolved" into every readable ask block that neither
// has an ask of its own on its document nor is a copy of another document's ask, and returns each
// one it changed for the caller to log: a block no settlement has indexed yet, which no longer
// reads open once its issue has closed, and which settlement indexes as a new open ask once the
// issue reopens.
//
// It runs in the transaction ctx joined (errUnjoined otherwise), and its caller holds the issue's
// row, as settlement holds it, so the writes are owner-verified: they reach a document whose room
// is flagged closed, and one no room holds, while every other write to a closed issue's document
// is refused. A write is credited to actor as any live write is (applyLive), never clearing the
// authors already pending, and writes no version: the settlement it leaves owed versions it, and
// none whose markdown repeats the latest version is written. A document it changes nothing in is
// written nothing.
func (s *Service) WriteIssueAskStates(ctx context.Context, issueKey string, asks []model.Ask, actor model.Actor, markUnindexed bool) ([]BlockRef, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return nil, errUnjoined
	}
	if len(asks) == 0 && !markUnindexed {
		return nil, nil
	}
	given := make(map[string]model.Ask, len(asks))
	for _, ask := range asks {
		given[ask.ID] = ask
	}
	rows, err := tx.Query(ctx, `select id::text from artifacts where issue_key = $1 and kind = 'doc' order by id`, issueKey)
	if err != nil {
		return nil, fmt.Errorf("list the issue's documents: %w", err)
	}
	documents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list the issue's documents: %w", err)
	}
	ctx = withOwnerVerified(ctx)
	owner := artifactOwner{IssueKey: &issueKey}
	var unindexed []BlockRef
	for _, document := range documents {
		marked, err := s.writeDocumentAskStates(ctx, tx, document, owner, given, actor, markUnindexed)
		if err != nil {
			return nil, fmt.Errorf("write the ask states of document %s: %w", document, err)
		}
		unindexed = append(unindexed, marked...)
	}
	return unindexed, nil
}

// writeDocumentAskStates is WriteIssueAskStates for one document, artifactID, in one live write,
// and returns the unindexed blocks it marked resolved.
func (s *Service) writeDocumentAskStates(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	owner artifactOwner,
	asks map[string]model.Ask,
	actor model.Actor,
	markUnindexed bool,
) ([]BlockRef, error) {
	var marked []BlockRef
	err := s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, current *pmdoc.Node, _ func(func(*crdt.Transaction))) error {
		if current == nil {
			// Settlement reads none of its ask blocks either (settleRoomWithin).
			slog.Warn("dispatch: leave the ask blocks of a document outside Proof schema as they are", "artifact", artifactID)
			return nil
		}
		blocks := readableAskBlocks(current)
		if len(blocks) == 0 {
			return nil
		}
		rows, err := loadAskBlocks(ctx, tx, artifactID)
		if err != nil {
			return err
		}
		sources, err := copiedAskSources(ctx, tx, artifactID, owner, blocks, rows)
		if err != nil {
			return err
		}
		// The writes are made on the fork as it stands (writeLive), each on the block carrying its
		// id, as settlement makes its repairs.
		var writes settlementReconciliation
		for _, block := range blocks {
			if row, indexed := rows[block.id]; indexed {
				if ask, wanted := asks[row.ID]; wanted {
					writes.repairs = append(writes.repairs, askRepair{blockID: block.id, set: askServerAttributes(ask, "", false)})
				}
				continue
			}
			if source, copied := sources[block.id]; copied {
				if ask, wanted := asks[source.ask.ID]; wanted {
					writes.repairs = append(writes.repairs, askRepair{blockID: block.id, set: askServerAttributes(ask, source.document, false)})
				}
				continue
			}
			if markUnindexed {
				ref := BlockRef{ArtifactID: artifactID, BlockID: block.id}
				writes.repairs = append(writes.repairs, askRepair{blockID: block.id, set: func(node *pmdoc.Node) bool {
					if node.Attrs["state"] == "resolved" {
						return false
					}
					node.Attrs["state"] = "resolved"
					marked = append(marked, ref)
					return true
				}})
			}
		}
		if len(writes.repairs) == 0 {
			return nil
		}
		_, err = writes.writeLive(doc, nil)
		return err
	})
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return nil, err
	}
	return marked, nil
}

// readableAskBlocks is every ask block of tree settlement reads (parseAskBlock), each id's first
// holder alone: a write lands on the first block carrying its id (repairLive), and a later block
// repeating it is no ask until settlement stamps it an id of its own. Unlike
// collectAskBlocksForSettlement, a repeated id leaves the rest of the document readable, so a
// browser's paste does not stop an issue from closing.
func readableAskBlocks(tree *pmdoc.Node) []askBlock {
	var blocks []askBlock
	seen := map[string]struct{}{}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if _, repeated := seen[id]; repeated {
			return true
		}
		seen[id] = struct{}{}
		if block, err := parseAskBlock(node); err == nil {
			blocks = append(blocks, block)
		}
		return true
	})
	return blocks
}

// SettleIssueDocuments runs, on the calling goroutine, the settlement each document of issueKey
// owes, as Shutdown's drain does: it waits for the room's durable appends to land, reads whether
// the document owes a settlement, and settles it at its room's current generation, in place of the
// timer armed for it, if one is. A settlement that fails is logged and returned, joined with the
// others, and is not retried: the document's pending-settlement row keeps the debt, for the room's
// next load or the resumption (RunSettlementResumption). Every new ask block the documents hold is
// then an ask, credited to its author as settlement credits one, which the close of an issue,
// which no settlement runs after, withdraws with the rest.
//
// It opens a transaction of its own for each settlement, which takes the issue's row, so it is
// refused inside a transaction: a joined context's, or, for a context the pool tracks, any
// (store.ErrNestedAcquire), as SetIssueClosed is.
func (s *Service) SettleIssueDocuments(ctx context.Context, issueKey string) error {
	if ledgerFrom(ctx).inTransaction() {
		return errSettleJoined
	}
	rooms, err := s.documentRooms(ctx, "the issue's documents", `
		select id::text from artifacts where issue_key = $1 and kind = 'doc' order by id
	`, issueKey)
	if err != nil {
		return err
	}
	var failures []error
	for _, room := range rooms {
		if err := s.settleOwedNow(ctx, room); err != nil {
			slog.Warn("dispatch: settle a document of an issue", "issue", issueKey, "room", room, "error", err)
			failures = append(failures, fmt.Errorf("settle document %s: %w", room, err))
		}
	}
	return errors.Join(failures...)
}

// settleOwedNow settles room, when its document owes a settlement, on the calling goroutine, and
// reports why the document still owes one afterwards. It takes the settleWG count of the timer it
// runs in place of, or one of its own, so Shutdown joins it as it would the timer's settlement,
// and holds the room's state across it, so the state the settlement looks up is the one whose
// generation it runs at.
func (s *Service) settleOwedNow(ctx context.Context, room string) error {
	if err := s.waitForDurableAppends(ctx, room); err != nil {
		return err
	}
	owes, err := settlementPending(ctx, s.store.Pool, room)
	if err != nil || !owes {
		return err
	}
	state := s.lockState(room)
	if !s.takeSettleTimer(state.settle) && !s.addUnlessStopping(&s.settleWG) {
		s.unlockState(room, state)
		return ErrServiceUnavailable
	}
	generation := state.roomGeneration
	state.settling++
	s.unlockState(room, state)
	var failure error
	s.settleRoomWithin(ctx, room, generation, func(err error) { failure = err })
	state.mu.Lock()
	state.settling--
	s.unlockState(room, state)
	s.settleWG.Done()
	if failure != nil {
		return failure
	}
	if owes, err = settlementPending(ctx, s.store.Pool, room); err != nil {
		return err
	}
	if owes {
		return errors.New("the document still owes a settlement")
	}
	return nil
}
