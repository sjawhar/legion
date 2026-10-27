package docs

import (
	"context"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// RebuildReport describes the durable history a rebuild replaced. SourceVersion is the artifact
// version whose markdown the fresh document state holds - the latest saved one, or the one a
// rebuild from supplied markdown that changed the document wrote; Head is the new monotonic update
// version.
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
// Every refusal comes before the first write: a room resident in this server or in another
// envoy-dispatch process (a holds query, roomQueries.ask), a history that loads, a history another
// rebuild or an append moved past the one this rebuild loaded (RebuildTx), a closed issue (for
// supplied markdown, which changes the document), and markdown that is not a Proof document or
// drops an open ask block. Supplied markdown is caller text: it is parsed
// as an upload is, and its rendering is weighed as a new document's is (SeedText). The latest
// version is the server's own rendering of the document, read back as every stored rendering is
// (pmdoc.ParseRendering), so a document whose version is past what one write may make - stored
// before that bound, or grown past it by browser edits - still rebuilds. Supplied markdown that
// differs from the latest version is a document change, so the same transaction writes its
// version, which moves the open approval request to it (writeVersionTx, MoveApprovalAsk); the
// VersionResult is what the caller's artifact.version event names. A failure anywhere rolls the
// whole rebuild back with the transaction. The room refuses loads until that transaction ends
// (Ledger.holdRebuild): a load before the commit would read the old history, and a second rebuild
// would preflight against it.
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
	// The other envoy-dispatch processes' rooms are asked the same through a holds query: a member
	// that answers it holds a room of the document refuses the rebuild as this server's room does.
	// A member that does not answer within the query's wait, or a room that loads there after it
	// answered, is covered by room sync instead. RebuildTx replaces the whole history with one
	// update at head+1 and announces it, so an instance over there finds its stored history
	// replaced (historyReplaced): its catch-up closes and reloads the room
	// (roomCatchUps.closeReplaced), and until then its settlement and its browser's append are
	// refused (ErrRoomBehindHead) and lead to the same close, and the reload reads the rebuilt
	// document.
	if s.srv.GetDoc(artifactID) != nil {
		return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: document %s is live in this server; close its editors and retry two minutes later, or replace it from markdown instead", ErrDocumentLive, artifactID)
	}
	answers, _, err := s.roomQueries.ask(ctx, tx, artifactID, roomQueryHolds)
	if err != nil {
		return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: ask the other processes whether they hold the document: %v", ErrServiceUnavailable, err)
	}
	for _, answer := range answers {
		if answer.holds {
			return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: document %s is live in another server process; close its editors and retry two minutes later, or replace it from markdown instead", ErrDocumentLive, artifactID)
		}
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
	_, open, err := lockArtifactOwner(ctx, tx, artifactID)
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

	// RebuildTx replaces the history only if its head is still the one loaded above, which is the
	// history validated as one that cannot load: a second rebuild of the document on another
	// process that loaded the same history waits on the owner row above until this one commits,
	// then finds the head moved and is refused.
	report, err := s.persistence.RebuildTx(ctx, tx, artifactID, loaded.Version, seed)
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	report.SourceVersion = latest.Number
	report.ValidationError = validationErr.Error()
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
