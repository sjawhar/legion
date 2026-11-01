package docs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

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
// loads, a closed issue (for supplied markdown, which changes the document), a latest version
// holding an element this build does not know (ErrDocNewerContent, as every read of it answers),
// and markdown that is not a Proof document, drops an open ask block, or introduces a block
// pointer to a block of a type its attribute does not allow (refuseBlockPointers, against the
// latest version). Supplied markdown is caller text: it is parsed as an upload is, and its
// rendering is weighed as a new document's is (SeedText). The latest version is the server's own
// rendering of the document, read back as every stored rendering is (pmdoc.ParseRendering), so a
// document whose version is past what one write may make - stored before that bound, or grown
// past it by browser edits - still rebuilds, and so does one holding an element this build reads
// but may not create yet, which the seed puts back rather than creates
// (encodeDocumentTreeHolding). Supplied markdown that differs from the latest version is a
// document change, so the same transaction writes its
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
		latest, err := savedVersionNumber(ctx, tx, artifactID)
		if err != nil {
			return RebuildReport{}, VersionResult{}, err
		}
		return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: document %s is open in this Dispatch process, or its last editor left it less than two minutes ago, so Dispatch refused the rebuild. A rebuild discards every edit made after saved version %d. Stop. Ask a person whether to rebuild the document after two minutes", ErrDocumentLive, artifactID, latest)
	}
	// The pre-load is only how a history that loads, which this refuses, is told from one that
	// does not: the rebuild's source is the latest saved version or the supplied markdown, never
	// the stored updates. So a load that fails because the history does not load is that same
	// answer, and the rebuild goes on, as it does for a history that loads into nothing a reader
	// can use. That is every ErrDocumentUnloadable: a state that does not decode, and one ygo
	// refuses from its clocks, which will not even merge, so Load itself fails rather than
	// returning an update validateUpdate can refuse (mergedThrough). The load error is then what
	// the report names as the history's fault. Any other load failure is the store's, not the
	// document's, and the caller may retry past it.
	var validationErr error
	loaded, err := s.persistence.Load(ctx, artifactID)
	switch {
	case errors.Is(err, ErrDocumentUnloadable):
		validationErr = err
	case err != nil:
		return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: load document before rebuild: %v", ErrServiceUnavailable, err)
	default:
		validationErr = validateUpdate(loaded.Update)
	}
	if validationErr == nil {
		latest, err := savedVersionNumber(ctx, tx, artifactID)
		if err != nil {
			return RebuildReport{}, VersionResult{}, err
		}
		return RebuildReport{}, VersionResult{}, fmt.Errorf("%w (document %s, latest saved version %d)", ErrDocumentLoads, artifactID, latest)
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
	// stored is the latest version's own tree, as the version holds it, when the rebuild seeds from
	// that version: the seed puts back what the version holds, so an element this build reads and
	// may not create yet is held already, not created (pmdoc.UpdateHolding). It is nil for supplied
	// markdown, which creates every element it holds.
	var tree, stored *pmdoc.Node
	if markdown == nil {
		// The version's markdown is the server's own, not the caller's, so an element this build
		// does not know in it is newer content, as every read of the document answers it, not
		// invalid markdown.
		parsed, parseErr := pmdoc.ParseRendering(latest.markdown)
		if newer := documentSchemaError(parseErr); errors.Is(newer, ErrDocNewerContent) {
			return RebuildReport{}, VersionResult{}, newer
		}
		// uploadedInput copies parsed, so the close's states written into tree below leave
		// stored as the version holds it.
		tree, err = uploadedInput(parsed, parseErr)
		stored = parsed
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
	seed, err := encodeDocumentTreeHolding(tree, stored)
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

// savedVersionNumber is the number of artifactID's latest saved version, which a refused rebuild
// names: a rebuild discards every edit made after it.
func savedVersionNumber(ctx context.Context, tx pgx.Tx, artifactID string) (int, error) {
	var number int
	if err := tx.QueryRow(ctx, `
		select coalesce(max(number), 0) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&number); err != nil {
		return 0, fmt.Errorf("read latest document version number: %w", err)
	}
	return number, nil
}
