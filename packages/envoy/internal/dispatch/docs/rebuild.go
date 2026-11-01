package docs

import (
	"context"
	"fmt"
	"strconv"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// RebuildReport describes the durable history a rebuild replaced. SourceVersion is the artifact
// version the fresh document state was built from - the latest saved one, or the one a rebuild
// from supplied markdown that changed the document wrote; a rebuild of a history that loads,
// which is built from that history's own text, can hold edits newer than it that settlement has
// not yet written, and a closed issue's rebuild holds the close's ask states (RebuildDocument).
// Head is the new monotonic update version. ValidationError says why the history was rebuilt:
// ygo's refusal of it, or how near a cap it stood.
type RebuildReport struct {
	Head               int64  `json:"head"`
	RemovedCheckpoints int64  `json:"removed_checkpoints"`
	RemovedSnapshots   int64  `json:"removed_snapshots"`
	RemovedUpdates     int64  `json:"removed_updates"`
	SourceVersion      int    `json:"source_version"`
	ValidationError    string `json:"validation_error"`
}

// smallestReplacementAdds is what the smallest replacement a write can make adds to a document's
// struct total, as refuseUndecodable bounds a write's updates (updatesAdd): one character replaced
// by another, a new struct with the splits its integration can cause and a delete range with the
// splits it can cause.
const smallestReplacementAdds = 1 + splitsAStructCanCause + splitsAStructCanCause

// historyScans is how many items a re-scan of state, a document's encoded Yjs state, scans - the
// state applied into a fresh document in one remote transaction (rescan), ygo's count of that
// transaction's scans (Transaction.IntegrateScanned, sjawhar/ygo v1.51.3-sami.7,
// crdt/transaction.go:95-108) - and whether it could be counted: a state the decode refused is
// not. It is the count a room's load starts the room's scan total at (roomScans), from the same
// decode of the same state.
func historyScans(state []byte) (uint64, bool) {
	scans, err := rescan(state)
	return scans, err == nil
}

// nearACap reports whether doc, a document's decoded history or its live room, stands so near a
// cap that it must be rebuilt, and why. Near the decode cap means a write's smallest replacement
// would carry it past the cap: its struct total as a load decodes it (countRoomStructs) and
// smallestReplacementAdds pass maxUpdateItems, so refuseUndecodable refuses every write that adds
// a struct, as it refuses the write of a document no room holds by the count of its fork, a
// decode of the same history (leavesRoomPastDecodeCap). Near the scan total means a re-scan of
// its state (historyScans) scans at least roomScanTotal less honestHandoffScans items. Both tests
// read the one encode of doc's state below, taken after its parked structs as countRoomStructs
// takes them, so neither pays an encode of its own and both answer for the same state.
func nearACap(doc *crdt.Doc) (why string, near bool) {
	pending := doc.PendingStats()
	state := crdt.EncodeStateAsUpdateV1(doc, nil)
	tally, err := walkUpdate(state, fromRoom, nil)
	if structs := structTotalOf(tally, err, pending); structs+smallestReplacementAdds > maxUpdateItems {
		return fmt.Sprintf("the document's live state holds %d structs, and a write's smallest replacement, %d more, would carry it past the %d ygo decodes in one update",
			structs, smallestReplacementAdds, maxUpdateItems), true
	}
	if scans, counted := historyScans(state); counted && scans+honestHandoffScans >= roomScanTotal {
		return fmt.Sprintf("a re-scan of the document's live state scans %d items, within %d of the %d a room's history may scan",
			scans, honestHandoffScans, roomScanTotal), true
	}
	return "", false
}

// RebuildDocument replaces a history no write can change with one fresh update, inside the
// transaction the context's ledger joined: a history ygo cannot load, built afresh from its latest
// saved markdown or supplied markdown; and a history that loads but stands so near the decode cap
// or the scan total that every write to it is refused (nearACap), built from its own text or
// supplied markdown. It is deliberately never a load fallback: a history that loads can contain
// unsettled edits newer than its latest version, so one further from both caps than that is
// refused unchanged, and one that is rebuilt keeps those edits, since what it is built from is its
// own tree.
//
// A document near a cap cannot be grown by anyone: a write that adds a struct is refused, a
// browser's edit is refused by the room's totals, and only a rebuild leaves it small. A write
// that only deletes can land, since a delete range splits an item only where one of its ends
// falls inside that item, so a range covering whole items splits none of them and adds no struct
// (sjawhar/ygo v1.51.3-sami.7, crdt/yarray.go:827-844), and it leaves the state as large as it
// was: the document's own text is what a trim shortens, not its history. Supplied markdown does
// not widen this: a history further from both caps is refused with or without it, and is trimmed
// by a write.
//
// Every refusal comes before the first write: a room resident in this server whose document is far
// from both caps, a history that loads and is not near a cap, a closed issue (for supplied
// markdown, which changes the document), and markdown that is not a Proof document, drops an open
// ask block, or introduces a block pointer to a block of a type its attribute does not allow
// (refuseBlockPointers, against the latest version). Supplied markdown is caller text: it is
// parsed as an upload is, and its rendering is weighed as a new document's is (SeedText). The
// latest version is the server's own rendering of the document, read back as every stored
// rendering is (pmdoc.ParseRendering), so a document whose version is past what one write may
// make - stored before that bound, or grown past it by browser edits - still rebuilds. Supplied
// markdown that differs from the latest version is a document change, so the same transaction
// writes its version, which moves the open approval request to it (writeVersionTx,
// MoveApprovalAsk); the VersionResult is what the caller's artifact.version event names. A
// failure anywhere rolls the whole rebuild back with the transaction. The room refuses loads
// until that transaction ends (Ledger.holdRebuild): a load before the commit would read the old
// history, and a second rebuild would preflight against it.
//
// A document near a cap is one people are typing into when they are refused, so its room is
// usually resident, and a refusal of every resident room would leave no way to rebuild it. Its
// room is closed instead, through evictBeforeRebuild, which flushes what the room holds into the
// stored history before it closes; the history this rebuild then reads holds every edit the room
// took. A room a Server.Apply is inside its callback on is left open and the rebuild refused, so
// no commit of the server's own lands beside the history this replaces.
// The browsers connected to it reconnect, and once the rebuild commits they load the rebuilt
// document and are closed as holders of the replaced history (below), dropping their unsent
// edits. A resident room of a document far from both caps is refused with ErrDocumentLive, as
// before: its document can be trimmed by a write.
//
// The fresh history lists the clients of the history it replaces, and the clock each had reached,
// under retiredMapName (rebuiltSeed), and the room refuses an update carrying any of their structs
// below that clock (weighPeerUpdate). A browser that held the document before the rebuild still
// holds the old history, and when it reconnects, the room holding none of it, its whole-state
// message hands all of it back: applied, the old document would sit beside the rebuilt one, every
// block twice, and the old history's structs would carry the room back toward the cap. Refused
// with DocumentTooLargeCloseCode and DocumentNeedsRebuildCloseReason, the editor drops what it
// sent and builds its document afresh from the rebuilt room, and drops its saved rows typed
// against the old history as rows of a rebuilt document (pending-sync.ts, addsParking). Before this
// list a rebuild of a history that cannot load had no such guard: the hook took the old history
// back unless the struct total refused it, which a browser's own copy, whose deleted runs Yjs
// merges, need not pass.
//
// A rebuild of a closed issue's document writes the close's ask states into the tree it seeds, as
// the close's final sweep writes them into the issue's other documents (closedIssueAskState), and
// marks the document owing the settlement those writes leave owed, so no ask block the close
// withdrew reads open again once the rebuild commits. That is also why the close's states go into
// the seed rather than through a live write after it (WriteIssueAskStates): such a write's fork is
// refused while the rebuild holds the room, and would read the old history through the pool that
// owns loads.
func (s *Service) RebuildDocument(ctx context.Context, artifactID string, markdown *string, actor model.Actor) (RebuildReport, VersionResult, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return RebuildReport{}, VersionResult{}, errUnjoined
	}
	if _, loaded := s.rebuilding.LoadOrStore(artifactID, struct{}{}); loaded {
		return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: document %s is already being rebuilt", ErrDocumentLive, artifactID)
	}
	ledgerFrom(ctx).holdRebuild(artifactID)
	// A room still loading is not resident yet. It reads the same history the check below reads,
	// so it can only fail on a history that cannot load, and its successor load is refused above.
	// A room stays resident for roomIdleTimeout after its last editor leaves, and ygo's idle
	// sweep that then evicts it runs every 30 seconds, so the refusal names two minutes. A load
	// that starts after this check reads the history this transaction has not yet replaced and
	// fails on it, since it cannot load, or, once the rebuild commits, reads the rebuilt head:
	// versions only increase, so its room holds the rebuilt update and none it would mistake for it.
	//
	// This check is this server's rooms alone, and that is all it can be: the other
	// envoy-dispatch process's rooms are not in this server's. What covers them is the stale-room
	// gate. RebuildTx replaces the whole history with one update at head+1, so an instance over
	// there no longer stands at the head, and its next durable write is refused
	// (ErrRoomBehindHead): its settlement closes the room (reloadRoomBehindHead), and its
	// browser's append fails it outright, and either way the reload reads the rebuilt document.
	if live := s.srv.GetDoc(artifactID); live != nil {
		if _, near := nearACap(live); !near {
			return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: document %s is live in this server; close its editors and retry two minutes later, or replace it from markdown instead", ErrDocumentLive, artifactID)
		}
		// The loads its browsers make as they reconnect are refused until this transaction
		// ends (refuseIfRebuilding), so the room stays closed while the history is replaced. A
		// Server.Apply already inside its callback on the room is the one thing that close gives
		// way to, since its commit would reach the store beside the history this rebuild
		// replaces (evictBeforeRebuild); the rebuild is refused, and the caller retries.
		closed, err := s.evictBeforeRebuild(artifactID)
		if err != nil {
			return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: close document %s before its rebuild: %v", ErrServiceUnavailable, artifactID, err)
		}
		if !closed {
			return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: document %s is being written in this server; retry", ErrServiceUnavailable, artifactID)
		}
	}
	history, why, retired, err := s.rebuildSource(ctx, artifactID)
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	// The owner row before the room lock the write below takes, as every writer that takes both
	// does (lockDocumentRoom). Held, it also keeps the open asks the replacement must keep, and
	// the approval request the version moves, from changing under this transaction.
	owner, open, err := lockArtifactOwner(ctx, tx, artifactID)
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	latest, err := latestVersion(ctx, tx, artifactID)
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	var tree *pmdoc.Node
	switch {
	case markdown != nil:
		if !open {
			return RebuildReport{}, VersionResult{}, ErrIssueClosed
		}
		tree, err = parseInput(*markdown)
	case history != nil:
		// The history loads, so its own tree is the document, with every edit settlement has not
		// yet written into a version, its anchor marks and its typed blocks' state.
		tree, err = treeOf(history)
	default:
		tree, err = uploadedInput(pmdoc.ParseRendering(latest.markdown))
	}
	history = nil
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	if err := pmdoc.AskContentError(tree); err != nil {
		return RebuildReport{}, VersionResult{}, &ErrInvalidAskBlock{Reason: err}
	}
	if err := s.refuseDroppedAskBlocks(ctx, artifactID, tree); err != nil {
		return RebuildReport{}, VersionResult{}, &ErrInvalidAskBlock{Reason: err}
	}
	// A closed issue's document shows its asks as the close left them, and the tree this rebuild
	// was built from - the history's own, or the latest version's - need not: settlement, which
	// versions a document, runs on no closed issue, so that version can predate the close, and the
	// close's live writes to the document either never reached it, its history not loading or
	// standing at a cap then, or are in the history this replaces. So the close's final sweep is
	// written into the tree here (closedIssueAskState): every resolved ask's block and copy, and
	// every answered ask's copy, from the rows as they stand under the issue's row this
	// transaction holds, and every readable ask block with no ask row marked resolved. The
	// ask rows are taken before RebuildTx takes the document's advisory lock, the order the close's
	// own write takes them in (WriteIssueAskStates). Once this transaction commits, the rebuilt
	// document reads as the issue's other documents do.
	var closedStates *issueAskStateWrites
	if !open {
		closedStates, err = readIssueAskStateWrites(ctx, tx, artifactID, owner, tree, closedIssueAskState, true)
		if err != nil {
			return RebuildReport{}, VersionResult{}, err
		}
		if !closedStates.apply(tree) {
			closedStates = nil
		}
	}
	canonical, offsets, err := renderTreeWithOffsets(tree)
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	if markdown != nil {
		if err := weighRendering("", canonical); err != nil {
			return RebuildReport{}, VersionResult{}, err
		}
		// Supplied markdown is a caller's write, so a block pointer it introduces over the
		// latest version is checked. That version is read back only when the schema declares a
		// pointer attribute; one that does not read back holds no pointer.
		var prior *pmdoc.Node
		if pmdoc.HasLinkKinds() {
			if latestTree, err := pmdoc.ParseRendering(latest.markdown); err == nil {
				prior = latestTree
			}
		}
		if err := refuseBlockPointers(ctx, tx, artifactID, tree, prior, offsets); err != nil {
			return RebuildReport{}, VersionResult{}, err
		}
	}
	seed, err := rebuiltSeed(tree, retired)
	if err != nil {
		return RebuildReport{}, VersionResult{}, fmt.Errorf("seed rebuilt document tree: %w", err)
	}

	report, err := s.persistence.RebuildTx(ctx, tx, artifactID, seed)
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	report.SourceVersion = latest.Number
	report.ValidationError = why
	if closedStates != nil {
		// markdown is nil here: a closed issue refuses supplied markdown above. The seed differs
		// from the tree it was built from by the close's states, as each of the issue's documents
		// the close wrote differs from its own, so it owes the settlement those writes leave owed
		// (markUpdateOwed): none runs while the issue is closed, and the one its reopen runs
		// versions the document and indexes, as new open asks, the blocks marked resolved here
		// with no ask row. RebuildTx holds the document's advisory lock, which the mark is ordered
		// by. The writes credit no one, as the close's do: no pending author is added, and the
		// latest edit source the row names is kept.
		if err := markUpdateOwed(ctx, tx, artifactID); err != nil {
			return RebuildReport{}, VersionResult{}, err
		}
		logUnindexedAskBlocks(*owner.IssueKey, closedStates.marked)
	}
	if markdown == nil || canonical == latest.markdown {
		return report, VersionResult{Version: latest.Version}, nil
	}
	// RebuildTx holds the document's advisory lock until this transaction ends, so the pending
	// authors read here are the ones the version's commit deletes.
	capture, err := s.capturePendingAuthors(ctx, tx, artifactID, nil, &actor, everyCredit)
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	written, err := s.writeVersionTx(ctx, tx, artifactID, renderedTree{tree: tree, markdown: canonical, offsets: offsets}, actor, &versionWrite{
		authors: actorSlice(capture.authors),
		capture: &capture,
	})
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	// A rebuild replaces the document's content with markdown its own actor wrote, so that actor
	// made the document's latest change, whatever edit the broken history still had pending. It is
	// recorded as the latest edit source for the versions that follow, and names this version's
	// edit notice (recordEditNotice), which goes when that actor is a person.
	if err := recordLatestEditSource(ctx, tx, artifactID, &actor); err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	if err := s.recordEditNotice(ctx, tx, artifactID, written.version.Number, actorSlice(capture.authors), &actor); err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	report.SourceVersion = written.version.Number
	return report, VersionResult{Version: written.version, Wrote: true, Changes: written.changes}, nil
}

// rebuildSource is what a rebuild of artifactID starts from: the stored history decoded (nil for
// one that does not decode), why it must be rebuilt - how near a cap it stands (nearACap), or the
// decode's refusal - and the clients it retires. A history that loads and stands near no cap is
// ErrDocumentLoads. The read's fold, its decode and the check's re-scan of it run under one grant
// of the copy-heap bound, whose wait ends with ctx, given back once the history is the rebuild's.
func (s *Service) rebuildSource(ctx context.Context, artifactID string) (*crdt.Doc, string, map[crdt.ClientID]uint64, error) {
	ctx, grant, done := withCopyGrant(ctx)
	defer done()
	loaded, err := s.persistence.Load(ctx, artifactID)
	if err == nil {
		err = grant.cover(ctx, len(loaded.Update))
	}
	if err != nil {
		return nil, "", nil, fmt.Errorf("%w: load document before rebuild: %v", ErrServiceUnavailable, err)
	}
	history, validationErr := decodeHistory(loaded.Update)
	if validationErr != nil {
		return nil, validationErr.Error(), undecodableHistoryClients(loaded.Update), nil
	}
	why, near := nearACap(history)
	if !near {
		return nil, "", nil, fmt.Errorf("%w: document %s", ErrDocumentLoads, artifactID)
	}
	return history, why, historyClients(history), nil
}

// decodeHistory decodes a stored history as validateUpdate does, as a room's load will, and returns
// the decoded document, or ygo's refusal of it and no document.
func decodeHistory(update []byte) (*crdt.Doc, error) {
	history := newDocumentCopy()
	if len(update) == 0 {
		return history, nil
	}
	if err := crdt.ApplyUpdateV1(history, update, nil); err != nil {
		return nil, err
	}
	return history, nil
}

// rebuiltSeed writes tree into a fresh document, as encodeDocumentTree does, with the retired
// clients listed under retiredMapName, each its client id in decimal against the clock it had
// reached, and returns the document's first durable update.
func rebuiltSeed(tree *pmdoc.Node, retired map[crdt.ClientID]uint64) ([]byte, error) {
	doc := crdt.New()
	fragment := doc.GetXmlFragment(fragmentName)
	doc.GetMap(marksMapName)
	list := doc.GetMap(retiredMapName)
	if err := doc.TransactE(func(transaction *crdt.Transaction) error {
		for client, clock := range retired {
			list.Set(transaction, strconv.FormatUint(uint64(client), 10), int64(clock))
		}
		return pmdoc.Update(transaction, fragment, tree)
	}); err != nil {
		return nil, err
	}
	return crdt.EncodeStateAsUpdateV1(doc, nil), nil
}

// historyClients is what a rebuild of history, a history that loads, retires: every client its
// state vector names, at the clock it reached, and every client the history itself listed as
// retired by an earlier rebuild, so a browser holding any history the document has had is refused.
func historyClients(history *crdt.Doc) map[crdt.ClientID]uint64 {
	retired := retiredClientsOf(history)
	if retired == nil {
		retired = make(map[crdt.ClientID]uint64)
	}
	for client, clock := range history.StateVector() {
		retired[client] = max(retired[client], clock)
	}
	return retired
}

// undecodableHistoryClients is what a rebuild of a history ygo cannot load retires: the clients
// a walk of its update reads before it refuses, each at the end of the last struct read, skips
// left out, since the holder of the history lacks those clocks too. A browser holding such a
// history holds every client a walk reaches first - each client's structs are encoded from its
// first clock - so one retired client is enough to refuse it; a client whose structs all follow
// the walk's refusal, and the clients an earlier rebuild listed, whose values the walk does not
// decode, are not retired.
func undecodableHistoryClients(update []byte) map[crdt.ClientID]uint64 {
	var read historyClientReader
	_, _ = walkUpdate(update, fromRoom, &read)
	return read.clients
}

// historyClientReader is undecodableHistoryClients's visitor.
type historyClientReader struct{ clients map[crdt.ClientID]uint64 }

func (r *historyClientReader) walkedStruct(walked updateStruct) {
	if walked.kind == structSkip {
		return
	}
	if r.clients == nil {
		r.clients = make(map[crdt.ClientID]uint64)
	}
	client := crdt.ClientID(walked.client)
	r.clients[client] = max(r.clients[client], walked.clock+walked.length)
}

func (*historyClientReader) walkedDelete(_, _, _ uint64) {}
