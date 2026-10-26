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
// not yet written. Head is the new monotonic update version. ValidationError says why the history
// was rebuilt: ygo's refusal of it, or how near the decode cap it stood.
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

// RebuildDocument replaces a history no write can change with one fresh update, inside the
// transaction the context's ledger joined: a history ygo cannot load, built afresh from its latest
// saved markdown or supplied markdown; and a history that loads but stands so near the decode cap
// that every write to it is refused, built from its own text or supplied markdown. It is
// deliberately never a load fallback: a history that loads can contain unsettled edits newer than
// its latest version, so one further from the cap than that is refused unchanged, and one that is
// rebuilt keeps those edits, since what it is built from is its own tree.
//
// Near the cap means a write's smallest replacement would carry the history past it: its struct
// total as a load decodes it (countRoomStructs of the decoded history) and smallestReplacementAdds
// pass maxUpdateItems, so refuseUndecodable refuses every write that changes the text, as it
// refuses the write of a document no room holds by the count of its fork, a decode of the same
// history (leavesRoomPastDecodeCap). Such a document cannot be trimmed by anyone: a write is
// refused, a browser's edit is refused by the room's struct total, and only a rebuild leaves it
// small. Supplied markdown does not widen this: a history further from the cap is refused with or
// without it, and is trimmed by a write.
//
// Every refusal comes before the first write: a room resident in this server, a history that
// loads and is not near the cap, a closed issue (for supplied markdown, which changes the
// document), and markdown that is not a Proof document or drops an open ask block. Supplied
// markdown is caller text: it is parsed as an upload is, and its rendering is weighed as a new
// document's is (SeedText). The latest version is the server's own rendering of the document, read
// back as every stored rendering is (pmdoc.ParseRendering), so a document whose version is past
// what one write may make - stored before that bound, or grown past it by browser edits - still
// rebuilds. Supplied markdown that differs from the latest version is a document change, so the
// same transaction writes its version, which moves the open approval request to it
// (writeVersionTx, MoveApprovalAsk); the VersionResult is what the caller's artifact.version event
// names. A failure anywhere rolls the whole rebuild back with the transaction. The room refuses
// loads until that transaction ends (Ledger.holdRebuild): a load before the commit would read the
// old history, and a second rebuild would preflight against it.
//
// The fresh history lists the clients of the history it replaces, and the clock each had reached,
// under retiredMapName (rebuiltSeed), and the room refuses an update carrying any of their structs
// below that clock (weighPeerUpdate). A browser that held the document before the rebuild still
// holds the old history, and when it reconnects, the room holding none of it, its whole-state
// message hands all of it back: applied, the old document would sit beside the rebuilt one, every
// block twice, and the old history's structs would carry the room back toward the cap. Refused
// with DocumentTooLargeCloseCode, the editor drops what it sent and builds its document afresh
// from the rebuilt room, and drops its saved rows typed against the old history as rows of a
// rebuilt document (pending-sync.ts, addsParking). Before this list a rebuild of a history that
// cannot load had no such guard: the hook took the old history back unless the struct total
// refused it, which a browser's own copy, whose deleted runs Yjs merges, need not pass.
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
	// This refusal is this server's rooms alone, and that is all it can be: the other
	// envoy-dispatch process's rooms are not in this server's. What covers them is the stale-room
	// gate. RebuildTx replaces the whole history with one update at head+1, so an instance over
	// there no longer stands at the head, and its next durable write is refused
	// (ErrRoomBehindHead): its settlement closes the room (reloadRoomBehindHead), and its
	// browser's append fails it outright, and either way the reload reads the rebuilt document.
	if s.srv.GetDoc(artifactID) != nil {
		return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: document %s is live in this server; close its editors and retry two minutes later, or replace it from markdown instead", ErrDocumentLive, artifactID)
	}
	loaded, err := s.persistence.Load(ctx, artifactID)
	if err != nil {
		return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: load document before rebuild: %v", ErrServiceUnavailable, err)
	}
	history, validationErr := decodeHistory(loaded.Update)
	var why string
	var retired map[crdt.ClientID]uint64
	if validationErr == nil {
		structs := countRoomStructs(history)
		if structs+smallestReplacementAdds <= maxUpdateItems {
			return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: document %s", ErrDocumentLoads, artifactID)
		}
		why = fmt.Sprintf("the document's live state holds %d structs, and a write's smallest replacement, %d more, would carry it past the %d ygo decodes in one update",
			structs, smallestReplacementAdds, maxUpdateItems)
		retired = historyClients(history)
	} else {
		why = validationErr.Error()
		retired = undecodableHistoryClients(loaded.Update)
	}
	// The owner row before the room lock the write below takes, as every writer that takes both
	// does (lockDocumentRoom). Held, it also keeps the open asks the replacement must keep, and
	// the approval request the version moves, from changing under this transaction.
	_, open, err := lockArtifactOwner(ctx, tx, artifactID)
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
	canonical, offsets, err := renderTreeWithOffsets(tree)
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	if markdown != nil {
		if err := weighRendering("", canonical); err != nil {
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
	if markdown == nil || canonical == latest.markdown {
		return report, VersionResult{Version: latest.Version}, nil
	}
	// RebuildTx holds the document's advisory lock until this transaction ends, so the pending
	// authors read here are the ones the version's commit deletes.
	capture, err := s.capturePendingAuthors(ctx, tx, artifactID, nil, &actor)
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
	report.SourceVersion = written.version.Number
	return report, VersionResult{Version: written.version, Wrote: true, Changes: written.changes}, nil
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
