package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"time"

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

// logUnindexedAskBlocks logs each ask block with no ask row that a write to issueKey's
// documents marked resolved: it is not an ask yet, so it has no record to show, and settlement
// indexes it as a new open ask once the issue reopens. WriteIssueAskStates and RebuildDocument
// each log the blocks they mark themselves, so every caller sees them logged once, whatever it
// does with them.
func logUnindexedAskBlocks(issueKey string, blocks []BlockRef) {
	for _, block := range blocks {
		slog.Warn("dispatch: mark resolved an ask block no settlement indexed before its issue closed",
			"issue", issueKey, "artifact", block.ArtifactID, "block", block.BlockID)
	}
}

// errSettleJoined refuses SettleIssueDocuments inside a transaction: each settlement opens a
// transaction of its own and takes the issue's row, which a caller's open transaction can hold.
var errSettleJoined = errors.New("docs: an issue's documents settle outside every transaction")

// issueDocumentIDs lists the documents of the issue $1, in id order: the order WriteIssueAskStates
// writes them and SettleIssueDocuments settles them.
const issueDocumentIDs = `select id::text from artifacts where issue_key = $1 and kind = 'doc' order by id`

type lifecycleWriteContextKey struct{}

// withLifecycleWrite marks the live writes made with ctx as server lifecycle writes
// (WriteIssueAskStates): the server writing into an ask's blocks the state an issue's close or
// reopen gave it. Such a write is no edit of the document, so it credits no pending author and
// leaves the document's latest edit source (roomState.lastActor) as it was (applyLive). A
// settlement then names the person who last edited the document, as the author of an ask block it
// indexes with no author of its own and on its derived events, never the closer or the reopener,
// however the room is released and loaded between the close and the reopen.
func withLifecycleWrite(ctx context.Context) context.Context {
	return context.WithValue(ctx, lifecycleWriteContextKey{}, true)
}

func isLifecycleWrite(ctx context.Context) bool {
	lifecycle, _ := ctx.Value(lifecycleWriteContextKey{}).(bool)
	return lifecycle
}

// WriteIssueAskStates writes the state of each of asks into the blocks that show it in every
// document of issueKey, in the documents' id order: an ask's own block on its document, and every
// block readCopiedAskSources classifies as a copy of one of asks on another, which keeps naming its
// source (copied_from, copied_from_document). Each block gets the server attributes settlement
// writes from the ask (setAskServerAttributes), so asks carry the state the caller wants shown:
// resolved for an issue's close, open for its reopen. Every other block is left as it is - the
// blocks of other asks, their copies, a block under one of asks' block ids that asks something
// else - and so is a block settlement could not read. Classification reads the database as it
// stands now: the caller writes the asks' rows after this for a close, and before it for a reopen.
//
// A document outside the Proof schema is left as it is, as settlement leaves it, and so is one
// whose stored history does not load (ErrDocumentUnloadable): nothing can read or open it, so no
// reader sees its blocks open, and the history a retry would meet is the same. Each is logged,
// naming the issue and the document, and the other documents are written as if it were not
// there, so the close, reopen or repair writing them is not refused for good. While the issue is
// closed, a rebuild from the latest version is the one way such a document loads again, and that
// rebuild writes the close's states into the document it seeds (RebuildDocument), so the blocks
// this left as they were do not come back open on the closed issue. Every other error, a failed or
// rebuilding room's ErrServiceUnavailable and the database's included, is returned.
//
// An answered ask, which the close's final sweep passes, is written into its copies alone, never
// into its own block: the answer route writes that block itself (SetBlockAttributes), and a write
// from the row could put back an answer it left out for size. Each copy gets the ask's state,
// answerer and time with the answer left out, as settlement writes a block whose document has no
// room for its answer (withholdAnswers): no write here is weighed for size, so none adds an
// answer's bytes to a document. A copy that already holds the answer in full is left as it is
// (answeredCopyAttributes), and the settlement an issue's reopen leaves owed writes the answer into
// the rest.
//
// With markUnindexed it also writes state="resolved" into every readable ask block that neither
// has an ask of its own on its document nor is a copy of another document's ask: a block no
// settlement has indexed yet, which no longer reads open once its issue has closed, and which
// settlement indexes as a new open ask once the issue reopens. Once every document is written, it
// logs each such block it changed (logUnindexedAskBlocks), so no caller logs them again, and
// returns them for a caller that counts them. Among these is every readable ask block that repeats
// the id an earlier ask block of its document holds and does not read state="resolved" - a
// browser's paste copies a block with its id - which a write addressed by id cannot reach, since
// such a write lands on the id's first holder (repairLive). It is written by its place among the
// document's ask blocks instead (markRepeatedAskBlocks), and logged and returned under the id it
// repeats. Its id is left as it is: stamping it an id of its own is settlement's job, and the
// reopen's settlement stamps it and indexes it as a new open ask.
//
// It runs in the transaction ctx joined (errUnjoined otherwise), and its caller holds the issue's
// row, as settlement holds it, so the writes are owner-verified: they reach a document whose room
// is flagged closed, and one no room holds, while every other write to a closed issue's document
// is refused. A write credits no one (withLifecycleWrite): it adds no pending author, leaves the
// authors already pending and the document's latest edit source as they were, and writes no
// version: the settlement it leaves owed versions it, and none whose markdown repeats the latest
// version is written. A document it changes nothing in is written nothing.
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
	rows, err := tx.Query(ctx, issueDocumentIDs, issueKey)
	if err != nil {
		return nil, fmt.Errorf("list the issue's documents: %w", err)
	}
	documents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list the issue's documents: %w", err)
	}
	ctx = withLifecycleWrite(withOwnerVerified(ctx))
	owner := artifactOwner{IssueKey: &issueKey}
	var unindexed []BlockRef
	for _, document := range documents {
		marked, err := s.writeDocumentAskStates(ctx, tx, document, owner, given, actor, markUnindexed)
		if err != nil {
			return nil, fmt.Errorf("write the ask states of document %s: %w", document, err)
		}
		unindexed = append(unindexed, marked...)
	}
	logUnindexedAskBlocks(issueKey, unindexed)
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
	given := func(row model.Ask) (model.Ask, bool) {
		ask, wanted := asks[row.ID]
		return ask, wanted
	}
	var writes *issueAskStateWrites
	err := s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, current *pmdoc.Node, _ func(func(*crdt.Transaction))) error {
		if current == nil {
			// Settlement reads none of its ask blocks either (settleRoomWithin).
			slog.Warn("dispatch: leave the ask blocks of a document outside Proof schema as they are", "artifact", artifactID)
			return nil
		}
		var err error
		writes, err = readIssueAskStateWrites(ctx, tx, artifactID, owner, current, given, markUnindexed)
		if err != nil || writes.empty() {
			return err
		}
		// The writes are made on the fork as it stands (rewriteLive), each on the block carrying its
		// id, as settlement makes its repairs (repairLive), and each repeat on the block at its place
		// (markRepeatedAskBlocks), all in one rewrite of the fork, whose tree is the one current read.
		// A document none of them changes is left as it was, and applyLive writes nothing for it.
		_, _, err = rewriteLive(doc, nil, writes.apply)
		return err
	})
	switch {
	case err == nil, errors.Is(err, websocket.ErrNoChanges):
	case errors.Is(err, ErrDocumentUnloadable) && !errors.Is(err, ErrServiceUnavailable):
		// Nothing can load the document, so no reader sees its blocks open, and a retry meets the
		// same history: the close, reopen or repair writing it goes on without it. The rebuild
		// that makes it load again writes its closed issue's states itself (RebuildDocument).
		slog.Warn("dispatch: leave the ask blocks of a document whose history cannot load as they are",
			"issue", *owner.IssueKey, "artifact", artifactID, "error", err)
		return nil, nil
	default:
		return nil, err
	}
	if writes == nil {
		return nil, nil
	}
	return writes.marked, nil
}

// issueAskStateWrites is what WriteIssueAskStates writes into one document of an issue, read off
// one tree of it (readIssueAskStateWrites) and made on a tree by apply.
type issueAskStateWrites struct {
	artifactID string
	// writes are the attribute writes to the blocks carrying their ids (repairLive).
	writes settlementReconciliation
	// repeats are the blocks repeating an earlier ask block's id, each marked resolved at its
	// place (markRepeatedAskBlocks).
	repeats []repeatedAskBlock
	// marked are the unindexed blocks apply changed to read state="resolved".
	marked []BlockRef
}

// readIssueAskStateWrites reads, in tx, the writes WriteIssueAskStates makes to tree, the document
// artifactID of owner, an issue whose row the caller holds. For each readable ask block that an
// ask of the document indexes, or that readCopiedAskSources classifies as a copy of an ask on
// another document, wanted is handed that ask's row and answers the ask whose state the block is
// to show and whether it is to be written at all. An answered ask's own block is never written,
// and its copies are written with the answer left out (answeredCopyAttributes). With
// markUnindexed, every other readable ask block, and every one repeating an earlier ask block's id
// that does not read resolved, is marked resolved. It changes nothing in tree.
func readIssueAskStateWrites(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	owner artifactOwner,
	tree *pmdoc.Node,
	wanted func(row model.Ask) (model.Ask, bool),
	markUnindexed bool,
) (*issueAskStateWrites, error) {
	found := &issueAskStateWrites{artifactID: artifactID}
	blocks, repeats := readableAskBlocks(tree)
	if markUnindexed {
		found.repeats = repeats
	}
	if len(blocks) == 0 {
		return found, nil
	}
	rows, err := loadAskBlocks(ctx, tx, artifactID)
	if err != nil {
		return nil, err
	}
	// An issue's documents take no copy lock (lockProjectCopies): the caller holds the issue's
	// row, as every transaction reading or marking the copies among its documents does.
	sources, err := readCopiedAskSources(ctx, tx, artifactID, owner, blocks, unindexedAskBlockIDs(blocks, rows))
	if err != nil {
		return nil, err
	}
	for _, block := range blocks {
		if row, indexed := rows[block.id]; indexed {
			if ask, write := wanted(row); write && ask.State != "answered" {
				found.writes.repairs = append(found.writes.repairs, askRepair{blockID: block.id, set: askServerAttributes(ask, "", false)})
			}
			continue
		}
		if source, copied := sources[block.id]; copied {
			if ask, write := wanted(source.ask); write {
				set := askServerAttributes(ask, source.document, false)
				if ask.State == "answered" {
					set = answeredCopyAttributes(ask, source.document)
				}
				found.writes.repairs = append(found.writes.repairs, askRepair{blockID: block.id, set: set})
			}
			continue
		}
		if markUnindexed {
			ref := BlockRef{ArtifactID: artifactID, BlockID: block.id}
			found.writes.repairs = append(found.writes.repairs, askRepair{blockID: block.id, set: func(node *pmdoc.Node) bool {
				if node.Attrs["state"] == "resolved" {
					return false
				}
				node.Attrs["state"] = "resolved"
				found.marked = append(found.marked, ref)
				return true
			}})
		}
	}
	return found, nil
}

// empty reports whether w has nothing to write.
func (w *issueAskStateWrites) empty() bool {
	return len(w.writes.repairs) == 0 && len(w.repeats) == 0
}

// apply makes w's writes on live, a tree of w's document, recording each unindexed block it marks
// resolved, and reports whether any changed it.
func (w *issueAskStateWrites) apply(live *pmdoc.Node) bool {
	repaired := w.writes.repairLive(live)
	repeated := markRepeatedAskBlocks(live, w.artifactID, w.repeats)
	w.marked = append(w.marked, repeated...)
	return repaired || len(repeated) > 0
}

// closedIssueAskState is the ask a block showing row shows once row's issue has closed, as the
// close's final sweep writes it: the row as it stands, for a resolved ask - among them every ask
// the close withdrew - and for an answered one. An open row is none: no close leaves one, and the
// repair that withdraws the asks a close by an older release left open writes their blocks.
func closedIssueAskState(row model.Ask) (model.Ask, bool) {
	return row, row.State == "resolved" || row.State == "answered"
}

// answeredCopyAttributes is the write WriteIssueAskStates makes to a copy of ask, an answered ask,
// whose source document's address is copied (copied_from_document): the copy's server attributes
// are made to agree with ask with the answer left out (setAskServerAttributes with withholdAnswer).
// A copy whose attributes already agree with ask, answer and all, as a settlement before the
// issue's close wrote them, is left as it is: leaving its answer out would take from the closed
// issue's document an answer its row still holds.
func answeredCopyAttributes(ask model.Ask, copied string) func(*pmdoc.Node) bool {
	return func(node *pmdoc.Node) bool {
		if !changesAskBlock(node, askServerAttributes(ask, copied, false)) {
			return false
		}
		changed, _ := setAskServerAttributes(node, ask, copied, true)
		return changed
	}
}

// changesAskBlock reports whether set, a write to an ask block's attributes, would change node,
// by running it on a copy of node's attributes, which leaves node as it is.
func changesAskBlock(node *pmdoc.Node, set func(*pmdoc.Node) bool) bool {
	clone := &pmdoc.Node{Type: node.Type, Attrs: make(pmdoc.Attrs, len(node.Attrs)+len(askServerAttributeNames))}
	maps.Copy(clone.Attrs, node.Attrs)
	return set(clone)
}

// readableAskBlocks is every ask block of tree settlement reads (parseAskBlock), each id's first
// holder alone: a write addressed by id lands on the first block carrying it (repairLive), and a
// later block repeating it is no ask until settlement stamps it an id of its own. Unlike
// collectAskBlocksForSettlement, a repeated id leaves the rest of the document readable, so a
// browser's paste does not stop an issue from closing.
//
// It returns apart, as repeats, every later holder settlement reads that carries a non-empty id an
// earlier ask block of tree already holds and does not read state="resolved", each with its place
// among tree's ask blocks in document order: the address a write to it takes, which its id is not
// (markRepeatedAskBlocks).
func readableAskBlocks(tree *pmdoc.Node) ([]askBlock, []repeatedAskBlock) {
	var blocks []askBlock
	var repeats []repeatedAskBlock
	seen := map[string]struct{}{}
	index := -1
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		index++
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if _, repeated := seen[id]; repeated {
			if id != "" && node.Attrs["state"] != "resolved" {
				if _, err := parseAskBlock(node); err == nil {
					repeats = append(repeats, repeatedAskBlock{index: index, id: id})
				}
			}
			return true
		}
		seen[id] = struct{}{}
		if block, err := parseAskBlock(node); err == nil {
			blocks = append(blocks, block)
		}
		return true
	})
	return blocks, repeats
}

// repeatedAskBlock is an ask block that repeats the id an earlier ask block of its document holds
// (readableAskBlocks): index is its place among the document's ask blocks in document order, and
// id the id it repeats.
type repeatedAskBlock struct {
	index int
	id    string
}

// markRepeatedAskBlocks writes state="resolved" into each of repeats in live, the tree of the
// document artifactID that readableAskBlocks read them from: into the ask block at the repeat's
// place among live's ask blocks, while that block still carries the id the repeat names and does
// not read resolved already. It returns a BlockRef, under the repeated id, for each block it
// changed. repeats are in document order, as readableAskBlocks returns them.
func markRepeatedAskBlocks(live *pmdoc.Node, artifactID string, repeats []repeatedAskBlock) []BlockRef {
	if len(repeats) == 0 {
		return nil
	}
	var marked []BlockRef
	next, index := 0, -1
	pmdoc.Walk(live, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		index++
		if index < repeats[next].index {
			return true
		}
		repeat := repeats[next]
		next++
		if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); id == repeat.id && node.Attrs["state"] != "resolved" {
			node.Attrs["state"] = "resolved"
			marked = append(marked, BlockRef{ArtifactID: artifactID, BlockID: repeat.id})
		}
		return next < len(repeats)
	})
	return marked
}

// The bounds on SettleIssueDocuments' retries. Every settlement it runs is one the close of an
// issue waits for before it opens its transaction, and the request that closes the issue waits
// with it, so the retries are few and short: a settlement that stopped short because the room's
// updates had not landed yet, or because the room had to be reloaded at its durable head, settles
// on the next attempt, and one that has not settled in three is not going to settle while a person
// waits on the close. The budget is for the whole issue, not each document. It bounds the waits for
// the rooms' durable appends and whether a retry starts, not a settlement already running, and
// every document gets its first attempt whatever is left of it. The pause is a few times the
// millisecond the room's own waits poll at, long enough for a persistence worker to store the
// update an attempt met.
const (
	issueSettleAttempts   = 3
	issueSettleBudget     = 3 * time.Second
	issueSettleRetryPause = 20 * time.Millisecond
)

// SettleIssueDocuments runs, on the calling goroutine, the settlement each document of issueKey
// owes, as Shutdown's drain does: it waits for the room's durable appends to land, reads whether
// the document owes a settlement, and settles it at its room's current generation, in place of the
// timer armed for it, if one is. Every new ask block the documents hold is then an ask, credited
// to its author as settlement credits one, which the close of an issue, which no settlement runs
// after, withdraws with the rest.
//
// A settlement that stops short (settleOutcome) is run again, up to issueSettleAttempts times for
// each document and within issueSettleBudget for the whole issue, no retry starting once the
// budget is spent (every document still gets its first attempt, as before the retries): a timer's
// settlement would retry it later (followUpSettlement), but the close does not wait for later, and
// a block left unindexed when it commits is no ask it can withdraw with a record. A settlement
// stops short on a room that holds updates its persistence has not stored or made durable yet,
// which the next attempt, after a pause of issueSettleRetryPause, finds stored; on one superseded
// by a write that landed while it ran, which the next attempt settles with that write; and on a
// room behind its durable head, because another envoy-dispatch process appended to the document
// after this one loaded it. The gate's refusal closes that room itself (reloadRoomBehindHead) and
// leaves the document owing its settlement, so the next attempt warms the room from the head and
// settles every update, this process's and the other's. A settlement still short after its last
// attempt, or once ctx ends or the service stops, which no further attempt would get past, is
// logged and returned, joined with the others: the document's pending-settlement row keeps the
// debt, for the room's next load or the resumption (RunSettlementResumption).
//
// It opens a transaction of its own for each settlement, which takes the issue's row, so it is
// refused inside a transaction: a joined context's, or, for a context the pool tracks, any
// (store.ErrNestedAcquire), as SetIssueClosed is.
func (s *Service) SettleIssueDocuments(ctx context.Context, issueKey string) error {
	if ledgerFrom(ctx).inTransaction() {
		return errSettleJoined
	}
	rooms, err := s.documentRooms(ctx, "the issue's documents", issueDocumentIDs, issueKey)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(issueSettleBudget)
	var failures []error
	for _, room := range rooms {
		var err error
		for attempt := 1; attempt <= issueSettleAttempts && (attempt == 1 || time.Now().Before(deadline)); attempt++ {
			if attempt > 1 && (ctx.Err() != nil || s.stopping.Load()) {
				break
			}
			if err = s.settleOwedNow(ctx, room, deadline); err == nil || attempt == issueSettleAttempts {
				break
			}
			pause := time.NewTimer(min(issueSettleRetryPause, time.Until(deadline)))
			select {
			case <-ctx.Done():
				pause.Stop()
			case <-pause.C:
			}
		}
		if err != nil {
			slog.Warn("dispatch: settle a document of an issue", "issue", issueKey, "room", room, "error", err)
			failures = append(failures, fmt.Errorf("settle document %s: %w", room, err))
		}
	}
	return errors.Join(failures...)
}

// settleOwedNow settles room, when its document owes a settlement, on the calling goroutine, and
// reports why the document still owes one afterwards: the reason a settlement that stopped short
// gives (settleOutcome), whatever stopped it, in place of the retry or re-arming a timer's
// settlement would do (followUpSettlement); or, for one that ended with no reason - among them one
// another write superseded and one the gate refused on a room behind its durable head, which leave
// the debt to the pending-settlement row - that the row still stands. Its caller decides whether to
// run it again (SettleIssueDocuments). It waits for the room's durable appends to land first, but
// not past deadline, so no attempt hangs past its caller's budget waiting for them; the settlement
// itself runs under ctx alone. It takes the settleWG count of the timer it runs in place of, or one
// of its own, so Shutdown joins it as it would the timer's settlement, and holds the room's state
// across it, so the state the settlement looks up is the one whose generation it runs at.
func (s *Service) settleOwedNow(ctx context.Context, room string, deadline time.Time) error {
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	err := s.waitForDurableAppends(waitCtx, room)
	cancel()
	if err != nil {
		return fmt.Errorf("wait for the room's appends to become durable: %w", err)
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
	outcome := s.settleRoomWithin(ctx, room, generation)
	state.mu.Lock()
	state.settling--
	s.unlockState(room, state)
	s.settleWG.Done()
	if outcome.err != nil {
		return outcome.err
	}
	if owes, err = settlementPending(ctx, s.store.Pool, room); err != nil {
		return err
	}
	if owes {
		return errors.New("the document still owes a settlement")
	}
	return nil
}
