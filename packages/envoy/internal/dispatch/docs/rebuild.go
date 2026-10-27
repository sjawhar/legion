package docs

import (
	"context"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// RebuildReport describes the durable history a rebuild replaced. SourceVersion is the artifact
// version whose markdown the fresh document state holds - the latest saved one, with a closed
// issue's ask states written into it (RebuildDocument), or the one a rebuild from supplied
// markdown that changed the document wrote; Head is the new monotonic update version.
type RebuildReport struct {
	Head               int64  `json:"head"`
	RemovedCheckpoints int64  `json:"removed_checkpoints"`
	RemovedSnapshots   int64  `json:"removed_snapshots"`
	RemovedUpdates     int64  `json:"removed_updates"`
	SourceVersion      int    `json:"source_version"`
	ValidationError    string `json:"validation_error"`
}

// RebuildDocument replaces a history ygo cannot load with one fresh update built from its latest
// saved markdown, or supplied markdown, inside the transaction the context's ledger joined. It is
// deliberately never a load fallback: a history that loads can contain unsettled edits newer than
// its latest version and is refused unchanged.
//
// Every refusal comes before the first write: a room resident in this server, a history that
// loads, a closed issue (for supplied markdown, which changes the document), and markdown that is
// not a Proof document or drops an open ask block. Supplied markdown is caller text: it is parsed
// as an upload is, and its rendering is weighed as a new document's is (SeedText). The latest
// version is the server's own rendering of the document, read back as every stored rendering is
// (pmdoc.ParseRendering), so a document whose version is past what one write may make - stored
// before that bound, or grown past it by browser edits - still rebuilds. Supplied markdown that
// differs from the latest version is a document change, so the same transaction writes its
// version, which moves the open approval request to it (writeVersionTx, MoveApprovalAsk); the
// VersionResult is what the caller's artifact.version event names. A rebuild of a closed issue's
// document from its latest version writes the close's ask states into the tree it seeds, as the
// close's final sweep writes them into the issue's other documents (closedIssueAskState), and
// marks the document owing the settlement those writes leave owed, so no ask block the close
// withdrew reads open again once the rebuild commits. A failure anywhere rolls the whole rebuild
// back with the transaction. The room refuses loads until that transaction ends
// (Ledger.holdRebuild): a load before the commit would read the old history, and a second rebuild
// would preflight against it. That is also why the close's states go into the seed rather than
// through a live write after it (WriteIssueAskStates): such a write's fork is refused while the
// rebuild holds the room, and would read the old history through the pool that owns loads.
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
	validationErr := validateUpdate(loaded.Update)
	if validationErr == nil {
		return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: document %s", ErrDocumentLoads, artifactID)
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
	if markdown == nil {
		tree, err = uploadedInput(pmdoc.ParseRendering(latest.markdown))
	} else {
		if !open {
			return RebuildReport{}, VersionResult{}, ErrIssueClosed
		}
		tree, err = parseInput(*markdown)
	}
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	if err := pmdoc.AskContentError(tree); err != nil {
		return RebuildReport{}, VersionResult{}, &ErrInvalidAskBlock{Reason: err}
	}
	if err := s.refuseDroppedAskBlocks(ctx, artifactID, tree); err != nil {
		return RebuildReport{}, VersionResult{}, &ErrInvalidAskBlock{Reason: err}
	}
	// A closed issue's document shows its asks as the close left them, and the tree read from the
	// latest version need not: settlement, which versions a document, runs on no closed issue, so
	// that version can predate the close, and the close's live writes to the document either never
	// reached it, its history not loading then, or are in the history this replaces. So the
	// close's final sweep is written into the tree here (closedIssueAskState): every resolved ask's
	// block and copy, and every answered ask's copy, from the rows as they stand under the issue's
	// row this transaction holds, and every readable ask block with no ask row marked resolved. The
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
	}
	seed, err := encodeDocumentTree(tree)
	if err != nil {
		return RebuildReport{}, VersionResult{}, fmt.Errorf("seed rebuilt document tree: %w", err)
	}

	report, err := s.persistence.RebuildTx(ctx, tx, artifactID, seed)
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	report.SourceVersion = latest.Number
	report.ValidationError = validationErr.Error()
	if closedStates != nil {
		// markdown is nil here: a closed issue refuses supplied markdown above. The seed differs
		// from the latest version by the close's states, as each of the issue's documents the
		// close wrote differs from its own, so it owes the settlement those writes leave owed
		// (markUpdateOwed): none runs while the issue is closed, and the one its reopen runs
		// versions the document and indexes, as new open asks, the blocks marked resolved here
		// with no ask row. RebuildTx holds the document's advisory lock, which the mark is ordered
		// by. The writes credit no one, as the close's do: no pending author is added, and the
		// latest edit source the row names is kept.
		if err := markUpdateOwed(ctx, tx, artifactID); err != nil {
			return RebuildReport{}, VersionResult{}, err
		}
		LogUnindexedAskBlocks(*owner.IssueKey, closedStates.marked)
	}
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
