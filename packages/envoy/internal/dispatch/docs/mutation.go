package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

type versionPending struct {
	generation uint64
	authors    map[string]model.Actor
}

type versionWrite struct {
	named            bool
	summary          *string
	authors          []model.Actor
	capture          *versionPending
	docUpdateVersion *int64
}

// applyLive runs mutate against artifactID's live document and credits actor with the content it
// changes. Outside a transaction it writes the room directly, and the room's update observer
// credits actor with it (recordConnectedActors). Joined to a transaction it writes that
// transaction's fork of the room (see liveWrite), appends the update inside the transaction,
// and leaves the room to PublishLiveWrites and the credit to CreditLiveWrites, so a transaction
// that does not commit never reaches the room, a browser or a version's authors.
func (s *Service) applyLive(ctx context.Context, artifactID string, actor model.Actor, mutate func(*crdt.Doc, func(func(*crdt.Transaction))) (bool, error)) error {
	if s.shuttingDown(artifactID) {
		return ErrServiceUnavailable
	}
	if tx, joined := txFromContext(ctx); joined {
		return s.applyJoined(ctx, tx, artifactID, actor, mutate)
	}
	var mutateErr error
	err := s.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		transact, release := s.serviceTransact(transact, &actor)
		defer release()
		// ygo re-panics callback failures after unregistering its update observer; that
		// unregister needs the same document mutex and masks the originating failure.
		defer func() {
			if recovered := recover(); recovered != nil {
				mutateErr = fmt.Errorf("document mutation panicked: %v\n%s", recovered, debug.Stack())
				slog.Error("dispatch: document mutation panicked", "room", artifactID, "error", mutateErr)
			}
		}()
		_, mutateErr = mutate(doc, transact)
	})
	if mutateErr != nil {
		return mutateErr
	}
	return err
}

// applyJoined is applyLive joined to tx. It returns websocket.ErrNoChanges when mutate wrote
// nothing, as the room's Apply does.
func (s *Service) applyJoined(ctx context.Context, tx pgx.Tx, artifactID string, actor model.Actor, mutate func(*crdt.Doc, func(func(*crdt.Transaction))) (bool, error)) error {
	collector := eventCollector(ctx)
	if collector == nil {
		return errLiveWriteNeedsCollector
	}
	write, err := s.joinLiveWrite(ctx, tx, collector, artifactID)
	if err != nil {
		return err
	}
	fork, err := s.forkLive(ctx, write)
	if err != nil {
		return err
	}
	before, err := treeOf(fork)
	if err != nil {
		return err
	}
	var updates [][]byte
	unsubscribe := fork.OnUpdate(func(update []byte, _ any) {
		updates = append(updates, append([]byte(nil), update...))
	})
	changed, mutateErr := func() (changed bool, err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("document mutation panicked: %v\n%s", recovered, debug.Stack())
				slog.Error("dispatch: document mutation panicked", "room", artifactID, "error", err)
			}
		}()
		return mutate(fork, func(inner func(*crdt.Transaction)) { fork.Transact(inner) })
	}()
	unsubscribe()
	if mutateErr != nil {
		return mutateErr
	}
	if len(updates) == 0 {
		return websocket.ErrNoChanges
	}
	if !changed {
		return nil
	}
	tree, err := treeOf(fork)
	if err != nil {
		return err
	}
	markdown, err := renderTree(tree)
	if err != nil {
		return err
	}
	update, err := mergeUpdates(updates)
	if err != nil {
		return err
	}
	// The same measure the room's update observer classifies a live update by: a write that only
	// adds or moves anchor marks, or projects a mark record, leaves the content - and so the
	// settled version - alone.
	contentChanged := !pmdoc.StripAnchorMarks(before).Equal(pmdoc.StripAnchorMarks(tree))
	if _, err := s.persistence.AppendUpdateTx(ctx, tx, artifactID, update, contentChanged); err != nil {
		return fmt.Errorf("append transactional live document update: %w", err)
	}
	if _, err := s.writeVersionTx(ctx, tx, artifactID, markdown, tree, actor, nil); err != nil {
		return fmt.Errorf("refresh transactional document anchors: %w", err)
	}
	write.updates = append(write.updates, update)
	if contentChanged {
		s.creditLiveWrite(write, actor)
	}
	return nil
}

func mergeUpdates(updates [][]byte) ([]byte, error) {
	switch len(updates) {
	case 0:
		return nil, websocket.ErrNoChanges
	case 1:
		return updates[0], nil
	default:
		update, err := crdt.MergeUpdatesV1(updates...)
		if err != nil {
			return nil, fmt.Errorf("merge live document updates: %w", err)
		}
		return update, nil
	}
}

// SeedText writes a new room's first Yjs update inside the caller's artifact
// creation transaction. A new room is loaded from this update on first use.
func (s *Service) SeedText(ctx context.Context, tx pgx.Tx, artifactID, markdown string, actor model.Actor) (string, error) {
	// The seeding actor is the caller's own first version author (written directly by the
	// caller, never through writeVersionTx), so it must not join `pending` - only the
	// settlement that indexes the seeded ask blocks needs to know who wrote them.
	s.recordLastActor(artifactID, actor)
	tree, err := parseInput(markdown)
	if err != nil {
		return "", err
	}
	canonical, err := renderTree(tree)
	if err != nil {
		return "", err
	}
	doc := crdt.New()
	fragment := doc.GetXmlFragment(fragmentName)
	doc.GetMap(marksMapName)
	if err := doc.TransactE(func(transaction *crdt.Transaction) error {
		return pmdoc.Update(transaction, fragment, tree)
	}); err != nil {
		return "", fmt.Errorf("seed live document tree: %w", err)
	}
	if _, err := s.persistence.AppendUpdateTx(ctx, tx, artifactID, crdt.EncodeStateAsUpdateV1(doc, nil), true); err != nil {
		return "", fmt.Errorf("seed live document: %w", err)
	}
	return canonical, nil
}

// ReplaceText replaces the entire live document tree so connected clients
// receive document uploads as a regular server-side transaction.
func (s *Service) ReplaceText(ctx context.Context, artifactID, markdown string, actor model.Actor) (string, error) {
	anchors, err := s.openAnchoredMarks(ctx, artifactID)
	if err != nil {
		return "", err
	}
	var canonical string
	var unchanged bool
	err = s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) (bool, error) {
		fragment := doc.GetXmlFragment(fragmentName)
		current, err := treeOf(doc)
		if err != nil {
			return false, err
		}
		target, err := parseInput(markdown)
		if err != nil {
			return false, err
		}
		currentMarkdown, err := renderTree(current)
		if err != nil {
			return false, err
		}
		canonical, err = renderTree(target)
		if err != nil {
			return false, err
		}
		if currentMarkdown == canonical {
			unchanged = true
			return false, nil
		}
		type reanchor struct {
			mark   anchoredMark
			range_ pmdoc.Range
			attrs  pmdoc.Attrs
		}
		reanchors := make([]reanchor, 0, len(anchors))
		for _, mark := range anchors {
			range_, _, found := pmdoc.FindMark(current, mark.markType, mark.anchor.MarkID)
			if !found {
				continue
			}
			attrs, found := pmdoc.MarkAttrs(current, mark.markType, mark.anchor.MarkID)
			if !found {
				return false, fmt.Errorf("%w: mark %q is missing attributes", ErrDocSchema, mark.anchor.MarkID)
			}
			reanchors = append(reanchors, reanchor{mark: mark, range_: range_, attrs: attrs})
		}
		var updateErr error
		transact(func(transaction *crdt.Transaction) {
			if updateErr = pmdoc.Update(transaction, fragment, target); updateErr != nil {
				return
			}
			for _, reanchor := range reanchors {
				if _, updateErr = markQuoteInTxn(transaction, fragment, target, reanchor.mark.anchor.Quote, nil, &reanchor.range_.From, MarkSpec{
					Kind:  MarkKind(reanchor.mark.markType),
					ID:    reanchor.mark.anchor.MarkID,
					Attrs: reanchor.attrs,
				}); errors.Is(updateErr, pmdoc.ErrTargetNotFound) {
					updateErr = nil
					continue
				} else if updateErr != nil {
					return
				}
			}
		})
		if updateErr != nil {
			return false, updateErr
		}
		return true, nil
	})
	if unchanged && errors.Is(err, websocket.ErrNoChanges) {
		return canonical, nil
	}
	if err != nil {
		return "", fmt.Errorf("replace live document: %w", err)
	}
	return canonical, nil
}

// Text returns the rendered document, loading and rendering persisted state
// when the document room is not resident.
func (s *Service) Text(ctx context.Context, artifactID string) (string, error) {
	if err := s.awaitRoomRecovery(ctx, artifactID); err != nil {
		return "", err
	}
	if fork, err := s.joinedFork(ctx, artifactID); err != nil || fork != nil {
		if err != nil {
			return "", err
		}
		tree, err := treeOf(fork)
		if err != nil {
			return "", err
		}
		return renderTree(tree)
	}
	if doc := s.srv.GetDoc(artifactID); doc != nil {
		tree, err := treeOf(doc)
		if err != nil {
			return "", err
		}
		return renderTree(tree)
	}
	loaded, err := s.persistence.Load(ctx, artifactID)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		s.failRoom(artifactID, err)
		return "", fmt.Errorf("%w: %w", ErrServiceUnavailable, err)
	}
	if len(loaded.Update) == 0 {
		return "", nil
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		s.failRoom(artifactID, fmt.Errorf("decode live document: %w", err))
		return "", fmt.Errorf("%w: decode live document: %w", ErrServiceUnavailable, err)
	}
	tree, err := treeOf(doc)
	if err != nil {
		return "", err
	}
	return renderTree(tree)
}

// TextWithToken returns canonical markdown and a token over its full Proof tree,
// including inline marks that canonical Markdown does not render.
func (s *Service) TextWithToken(ctx context.Context, artifactID string) (string, string, error) {
	if err := s.awaitRoomRecovery(ctx, artifactID); err != nil {
		return "", "", err
	}
	if fork, err := s.joinedFork(ctx, artifactID); err != nil || fork != nil {
		if err != nil {
			return "", "", err
		}
		return renderTokenTree(fork)
	}
	if s.srv.GetDoc(artifactID) == nil {
		loaded, err := s.persistence.Load(ctx, artifactID)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return "", "", err
			}
			s.failRoom(artifactID, err)
			return "", "", fmt.Errorf("%w: %w", ErrServiceUnavailable, err)
		}
		if len(loaded.Update) == 0 {
			return "", "", nil
		}
		doc := crdt.New()
		if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
			s.failRoom(artifactID, fmt.Errorf("decode live document: %w", err))
			return "", "", fmt.Errorf("%w: decode live document: %w", ErrServiceUnavailable, err)
		}
		return renderTokenTree(doc)
	}

	var markdown, token string
	var readErr error
	err := s.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		markdown, token, readErr = renderTokenTree(doc)
	})
	if readErr != nil {
		return "", "", readErr
	}
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return "", "", err
	}
	return markdown, token, nil
}

func renderTokenTree(doc *crdt.Doc) (string, string, error) {
	tree, err := treeOf(doc)
	if err != nil {
		return "", "", err
	}
	markdown, err := renderTree(tree)
	if err != nil {
		return "", "", err
	}
	token, err := nodeToken(tree)
	if err != nil {
		return "", "", err
	}
	return markdown, token, nil
}

// Blocks returns each stamped block and its byte range in canonical markdown.
func (s *Service) Blocks(ctx context.Context, artifactID string) ([]model.ArtifactBlock, error) {
	_, blocks, err := s.TextWithBlocks(ctx, artifactID)
	return blocks, err
}

// TextWithBlocks renders the live tree once and returns its canonical markdown beside the
// blocks whose byte ranges index into it.
func (s *Service) TextWithBlocks(ctx context.Context, artifactID string) (string, []model.ArtifactBlock, error) {
	if err := s.awaitRoomRecovery(ctx, artifactID); err != nil {
		return "", nil, err
	}
	var markdown string
	var blocks []model.ArtifactBlock
	var readErr error
	err := s.docView(ctx, artifactID, func(doc *crdt.Doc) {
		tree, err := treeOf(doc)
		if err != nil {
			readErr = err
			return
		}
		tableDescendants, err := pmdoc.TableDescendantIDs(tree)
		if err != nil {
			readErr = err
			return
		}
		tokens, err := blockTokens(tree)
		if err != nil {
			readErr = err
			return
		}
		rendered, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
		if err != nil {
			readErr = err
			return
		}
		markdown = rendered
		blocks = make([]model.ArtifactBlock, len(offsets))
		for index, offset := range offsets {
			blocks[index] = model.ArtifactBlock{
				ID:            offset.ID,
				Type:          offset.Type,
				From:          offset.From,
				To:            offset.To,
				Token:         tokens[offset.ID],
				DescendantIDs: tableDescendants[offset.ID],
			}
		}
	})
	if readErr != nil {
		return "", nil, readErr
	}
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", nil, err
		}
		s.failRoom(artifactID, err)
		return "", nil, fmt.Errorf("%w: %w", ErrServiceUnavailable, err)
	}
	return markdown, blocks, nil
}

// SnapshotVersion returns the current immutable version, adding an unnamed
// version only when the live text has diverged since the previous one.
func (s *Service) SnapshotVersion(ctx context.Context, tx pgx.Tx, artifactID string, actor model.Actor) (VersionResult, error) {
	tree, markdown, capture, authors, err := s.captureLiveTextAndAuthors(ctx, artifactID, nil)
	if err != nil {
		return VersionResult{}, err
	}
	latest, err := latestVersion(ctx, tx, artifactID)
	if err != nil {
		return VersionResult{}, err
	}
	if latest.markdown == markdown {
		return VersionResult{Version: latest.Version}, nil
	}
	capture.authors[actorKey(actor)] = actor
	authors = actorSlice(capture.authors)
	result, err := s.writeVersionTx(ctx, tx, artifactID, markdown, tree, actor, &versionWrite{
		authors: authors,
		capture: &capture,
	})
	if err != nil {
		return VersionResult{}, err
	}
	return VersionResult{Version: result.version, Wrote: true, Changes: result.changes}, nil
}

// CommitVersion clears authors consumed by a version only after its enclosing
// transaction has committed.
func (s *Service) CommitVersion(artifactID string, version model.Version) {
	state := s.room(artifactID)
	state.mu.Lock()
	defer state.mu.Unlock()
	capture, ok := state.pendingVersions[version.Number]
	if !ok {
		return
	}
	delete(state.pendingVersions, version.Number)
	if state.gen != capture.generation {
		return
	}
	for key := range capture.authors {
		delete(state.pending, key)
	}
}

func (s *Service) discardPendingVersion(room string, version model.Version) {
	state := s.room(room)
	state.mu.Lock()
	delete(state.pendingVersions, version.Number)
	state.mu.Unlock()
}

// prevalidateLiveOperations performs database-backed table-anchor checks
// before the Yjs transaction, retaining the table-mark snapshots the
// transaction re-derives before it writes.
func (s *Service) prevalidateLiveOperations(ctx context.Context, artifactID string, ops []model.EditOp) ([]tableAnchorSnapshot, error) {
	var (
		snapshots []tableAnchorSnapshot
		planErr   error
	)
	err := s.docView(ctx, artifactID, func(doc *crdt.Doc) {
		tree, err := treeOf(doc)
		if err != nil {
			planErr = err
			return
		}
		snapshots, planErr = s.prevalidateOperations(ctx, artifactID, tree, ops)
	})
	if planErr != nil {
		return nil, planErr
	}
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return nil, err
	}
	return snapshots, nil
}

const maxQueuedConditionalEdits = 32

type conditionalEditGate struct {
	turn   chan struct{}
	mu     sync.Mutex
	queued int
}

// AcquireConditionalEdit queues one conditional edit in memory before it can
// acquire a database connection, keeping stale retries from exhausting pgx.
func (s *Service) AcquireConditionalEdit(ctx context.Context, artifactID string) (func(), error) {
	for {
		created := &conditionalEditGate{turn: make(chan struct{}, 1)}
		created.turn <- struct{}{}
		value, _ := s.conditionalGates.LoadOrStore(artifactID, created)
		gate := value.(*conditionalEditGate)
		gate.mu.Lock()
		current, present := s.conditionalGates.Load(artifactID)
		if !present || current != gate {
			gate.mu.Unlock()
			continue
		}
		if gate.queued >= maxQueuedConditionalEdits {
			gate.mu.Unlock()
			return nil, ErrPreconditionBusy
		}
		gate.queued++
		gate.mu.Unlock()

		select {
		case <-gate.turn:
			return func() {
				gate.mu.Lock()
				gate.queued--
				last := gate.queued == 0
				if last {
					s.conditionalGates.CompareAndDelete(artifactID, gate)
				}
				gate.mu.Unlock()
				if !last {
					gate.turn <- struct{}{}
				}
			}, nil
		case <-ctx.Done():
			gate.mu.Lock()
			gate.queued--
			last := gate.queued == 0
			if last {
				s.conditionalGates.CompareAndDelete(artifactID, gate)
			}
			gate.mu.Unlock()
			return nil, ctx.Err()
		}
	}
}

func lockTableAnchorRows(ctx context.Context, tx pgx.Tx, artifactID string, snapshots []tableAnchorSnapshot) error {
	byMark := make(map[string]tableAnchorSnapshot)
	for _, snapshot := range snapshots {
		for _, markID := range snapshot.markIDs {
			byMark[markID] = snapshot
		}
	}
	if len(byMark) == 0 {
		return nil
	}
	markIDs := make([]string, 0, len(byMark))
	for markID := range byMark {
		markIDs = append(markIDs, markID)
	}
	sort.Strings(markIDs)
	seen := make(map[string]struct{}, len(markIDs))
	var active []string
	rows, err := tx.Query(ctx, `
		select id::text, anchor->>'mark_id', state
		from asks
		where anchor->>'artifact_id' = $1 and anchor->>'mark_id' = any($2::text[])
		for share
	`, artifactID, markIDs)
	if err != nil {
		return fmt.Errorf("lock table ask anchors: %w", err)
	}
	for rows.Next() {
		var id, markID, state string
		if err := rows.Scan(&id, &markID, &state); err != nil {
			rows.Close()
			return fmt.Errorf("scan table ask anchor: %w", err)
		}
		seen[markID] = struct{}{}
		if state == "open" {
			active = append(active, "ask "+id+" (anchor "+markID+")")
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate table ask anchors: %w", err)
	}
	rows.Close()

	rows, err = tx.Query(ctx, `
		select id::text, anchor->>'mark_id', resolved
		from comments
		where anchor->>'artifact_id' = $1 and anchor->>'mark_id' = any($2::text[])
		for share
	`, artifactID, markIDs)
	if err != nil {
		return fmt.Errorf("lock table comment anchors: %w", err)
	}
	for rows.Next() {
		var id, markID string
		var resolved bool
		if err := rows.Scan(&id, &markID, &resolved); err != nil {
			rows.Close()
			return fmt.Errorf("scan table comment anchor: %w", err)
		}
		seen[markID] = struct{}{}
		if !resolved {
			active = append(active, "comment "+id+" (anchor "+markID+")")
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate table comment anchors: %w", err)
	}
	rows.Close()
	for _, markID := range markIDs {
		if _, found := seen[markID]; !found {
			snapshot := byMark[markID]
			return &ErrInvalidOp{Field: "index", Reason: fmt.Sprintf("%s index %d has unindexed anchor mark: %s", snapshot.axis, snapshot.index, markID)}
		}
	}
	if len(active) == 0 {
		return nil
	}
	sort.Strings(active)
	first := byMark[markIDs[0]]
	return &ErrInvalidOp{Field: "index", Reason: fmt.Sprintf("%s index %d would remove active anchors: %s", first.axis, first.index, strings.Join(active, ", "))}
}

func (s *Service) warmLiveDocument(ctx context.Context, artifactID string) error {
	err := s.srv.Apply(ctx, artifactID, func(_ *crdt.Doc, _ func(func(*crdt.Transaction))) {})
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return err
	}
	return nil
}

// ApplyOps resolves every requested operation against the document's one Yjs
// transaction. A conditional edit checks its precondition, resolves the batch,
// and writes the plan inside that transaction, so no live writer can enter the
// check-to-apply window.
func (s *Service) ApplyOps(ctx context.Context, artifactID string, ops []model.EditOp, actor model.Actor, precondition *model.EditPrecondition) (int, error) {
	if precondition == nil {
		return s.applyOpsUnconditional(ctx, artifactID, ops, actor)
	}
	tx, joined := txFromContext(ctx)
	if !joined {
		return 0, &ErrInvalidPrecondition{Reason: "requires an enclosing transaction"}
	}
	collector := eventCollector(ctx)
	if collector == nil {
		return 0, errLiveWriteNeedsCollector
	}
	// The live document's locks, in their order (see liveWrite): its owner row and, once the
	// room has recovered from any failure, its writer slot; then, with the room loaded, its
	// advisory lock, held from before the precondition is read.
	if _, err := s.joinLiveWrite(ctx, tx, collector, artifactID); err != nil {
		return 0, err
	}
	if err := s.warmLiveDocument(ctx, artifactID); err != nil {
		return 0, err
	}
	if err := lockDocumentRoom(ctx, tx, artifactID); err != nil {
		return 0, err
	}
	if len(ops) == 0 {
		var checkErr error
		err := s.docView(ctx, artifactID, func(doc *crdt.Doc) {
			tree, err := treeOf(doc)
			if err != nil {
				checkErr = err
				return
			}
			checkErr = checkEditPrecondition(tree, *precondition)
		})
		if checkErr != nil {
			return 0, checkErr
		}
		if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			return 0, fmt.Errorf("check empty document edit precondition: %w", err)
		}
		return 0, nil
	}
	var (
		err       error
		snapshots []tableAnchorSnapshot
	)
	if hasTableAnchorMutation(ops) {
		snapshots, err = s.prevalidateLiveOperations(ctx, artifactID, ops)
		if err != nil {
			return 0, fmt.Errorf("prevalidate live document operations: %w", err)
		}
	}
	if err := lockTableAnchorRows(ctx, tx, artifactID, snapshots); err != nil {
		return 0, err
	}
	err = s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) (bool, error) {
		fragment := doc.GetXmlFragment(fragmentName)
		var mutationErr error
		transact(func(transaction *crdt.Transaction) {
			tree, err := treeOfTransaction(transaction, fragment)
			if err != nil {
				mutationErr = err
				return
			}
			if err := checkEditPrecondition(tree, *precondition); err != nil {
				mutationErr = err
				return
			}
			if len(snapshots) > 0 {
				if err := verifyTableAnchorSnapshots(tree, ops, snapshots); err != nil {
					mutationErr = err
					return
				}
			}
			// Block addressing (delete/move by id, whole-text delete) needs every block
			// identified; a browser-authored block the closer has not yet stamped gets its id
			// here, and the same ids persist through the update below.
			pmdoc.EnsureBlockIDs(tree)
			next, err := applyOperations(tree, ops)
			if err != nil {
				mutationErr = err
				return
			}
			if err := requirePreconditionCoverage(tree, ops, *precondition); err != nil {
				mutationErr = err
				return
			}
			pmdoc.EnsureBlockIDs(next)
			if err := validateAskBlocks(next); err != nil {
				mutationErr = &ErrInvalidAskBlock{Reason: err}
				return
			}
			mutationErr = pmdoc.Update(transaction, fragment, next)
		})
		if mutationErr != nil {
			return false, mutationErr
		}
		return true, nil
	})
	if err != nil {
		var quoteNotFound *ErrQuoteNotFound
		if errors.As(err, &quoteNotFound) {
			return 0, err
		}
		return 0, fmt.Errorf("apply live document operations: %w", err)
	}
	return len(ops), nil
}

// applyOpsUnconditional preserves the precondition-free edit path's existing
// validation and live-mutation behavior.
func (s *Service) applyOpsUnconditional(ctx context.Context, artifactID string, ops []model.EditOp, actor model.Actor) (int, error) {
	if len(ops) == 0 {
		return 0, nil
	}
	err := s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) (bool, error) {
		fragment := doc.GetXmlFragment(fragmentName)
		tree, err := treeOf(doc)
		if err != nil {
			return false, err
		}
		pmdoc.EnsureBlockIDs(tree)
		next, err := s.applyOperations(ctx, artifactID, tree, ops)
		if err != nil {
			return false, err
		}
		pmdoc.EnsureBlockIDs(next)
		if err := validateAskBlocks(next); err != nil {
			return false, &ErrInvalidAskBlock{Reason: err}
		}
		var updateErr error
		transact(func(transaction *crdt.Transaction) {
			updateErr = pmdoc.Update(transaction, fragment, next)
		})
		if updateErr != nil {
			return false, updateErr
		}
		return true, nil
	})
	if err != nil {
		var quoteNotFound *ErrQuoteNotFound
		if errors.As(err, &quoteNotFound) {
			return 0, err
		}
		return 0, fmt.Errorf("apply live document operations: %w", err)
	}
	return len(ops), nil
}

// SetBlockAttributes applies server-owned typed-block state through the
// transactional live-document mutation path.
func (s *Service) SetBlockAttributes(
	ctx context.Context,
	artifactID, blockID string,
	attributes map[string]any,
	actor model.Actor,
) error {
	err := s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) (bool, error) {
		fragment := doc.GetXmlFragment(fragmentName)
		tree, err := treeOf(doc)
		if err != nil {
			return false, err
		}
		next, err := pmdoc.SetBlockAttributes(tree, blockID, pmdoc.Attrs(attributes))
		if err != nil {
			return false, err
		}
		if next.EqualWithBlockIDs(tree) {
			return false, nil
		}
		var updateErr error
		transact(func(transaction *crdt.Transaction) {
			updateErr = pmdoc.Update(transaction, fragment, next)
		})
		if updateErr != nil {
			return false, updateErr
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("set live block attributes: %w", err)
	}
	return nil
}

// NamedVersion records the live document as a deliberately named immutable version.
func (s *Service) NamedVersion(ctx context.Context, artifactID, summary string, actor model.Actor) (VersionResult, error) {
	if _, joined := txFromContext(ctx); !joined && eventCollector(ctx) == nil {
		ctx = WithEventCollector(ctx, NewEventCollector())
	}
	tree, markdown, capture, authors, err := s.captureLiveTextAndAuthors(ctx, artifactID, &actor)
	if err != nil {
		return VersionResult{}, err
	}
	_, joinedTransaction := txFromContext(ctx)
	written := VersionResult{Wrote: true}
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		_, open, err := lockArtifactOwner(ctx, tx, artifactID)
		if err != nil {
			return err
		}
		if !open {
			return ErrIssueClosed
		}

		result, writeErr := s.writeVersionTx(ctx, tx, artifactID, markdown, tree, actor, &versionWrite{
			named:   true,
			summary: new(summary),
			authors: authors,
			capture: &capture,
		})
		written.Version = result.version
		written.Changes = result.changes
		return writeErr
	})
	if err != nil {
		s.discardPendingVersion(artifactID, written.Version)
		return VersionResult{}, err
	}
	if !joinedTransaction {
		s.CommitVersion(artifactID, written.Version)
		for _, event := range eventCollector(ctx).Events() {
			s.events.Publish(event)
		}
	}
	return written, nil
}

// serviceTransact wraps Server.Apply's transact so the room's update observer can tell the
// service's own transactions from browser peers' edits, and credit the content they change to
// actor (nil credits no one): the Apply call's origin is registered in serviceOrigins on the
// first transaction and forgotten by release. Observers fire before a transaction returns, so
// release is safe once the Apply callback is done with transact.
func (s *Service) serviceTransact(transact func(func(*crdt.Transaction)), actor *model.Actor) (wrapped func(func(*crdt.Transaction)), release func()) {
	var origin any
	wrapped = func(inner func(*crdt.Transaction)) {
		transact(func(txn *crdt.Transaction) {
			if origin == nil {
				origin = txn.Origin
				s.serviceOrigins.Store(origin, actor)
			}
			inner(txn)
		})
	}
	release = func() {
		if origin != nil {
			s.serviceOrigins.Delete(origin)
		}
	}
	return wrapped, release
}

func (s *Service) recordLastActor(room string, actor model.Actor) {
	state := s.room(room)
	state.mu.Lock()
	state.lastActor = new(actor)
	state.mu.Unlock()
}

func (s *Service) captureLiveTextAndAuthors(ctx context.Context, room string, actor *model.Actor) (*pmdoc.Node, string, versionPending, []model.Actor, error) {
	fork, err := s.joinedFork(ctx, room)
	if err != nil {
		return nil, "", versionPending{}, nil, err
	}
	if fork == nil && s.srv.GetDoc(room) == nil {
		err := s.srv.Apply(ctx, room, func(_ *crdt.Doc, _ func(func(*crdt.Transaction))) {})
		if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			return nil, "", versionPending{}, nil, fmt.Errorf("warm live document: %w", err)
		}
	}

	state := s.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
	doc := fork
	if doc == nil {
		doc = s.srv.GetDoc(room)
	}
	if doc == nil {
		return nil, "", versionPending{}, nil, errors.New("warm live document did not retain room")
	}
	tree, err := treeOf(doc)
	if err != nil {
		return nil, "", versionPending{}, nil, err
	}
	markdown, err := renderTree(tree)
	if err != nil {
		return nil, "", versionPending{}, nil, err
	}
	capture, authors := captureAuthors(state, joinedLiveWrite(ctx, room), actor)
	return tree, markdown, capture, authors, nil
}

// captureAuthors is a version's authors: the room's pending actors, those the calling
// transaction's own write will credit once it commits, and actor.
func captureAuthors(state *roomState, write *liveWrite, actor *model.Actor) (versionPending, []model.Actor) {
	authors := make(map[string]model.Actor, len(state.pending)+1)
	for key, pendingActor := range state.pending {
		authors[key] = pendingActor
	}
	if write != nil {
		for key, credited := range write.credits {
			authors[key] = credited
		}
	}
	if actor != nil {
		authors[actorKey(*actor)] = *actor
	}
	capture := versionPending{generation: state.gen, authors: authors}
	return capture, actorSlice(authors)
}

func (s *Service) rememberPendingVersion(room string, version model.Version, capture versionPending) {
	state := s.room(room)
	state.mu.Lock()
	state.pendingVersions[version.Number] = capture
	state.mu.Unlock()
}

func latestVersion(ctx context.Context, tx pgx.Tx, artifactID string) (struct {
	model.Version
	markdown         string
	docUpdateVersion int64
}, error) {
	var version struct {
		model.Version
		markdown         string
		docUpdateVersion int64
	}
	var authors []byte
	if err := tx.QueryRow(ctx, `
		select number, named, summary, authors, created_at, markdown, doc_update_version
		from artifact_versions where artifact_id = $1
		order by number desc limit 1
	`, artifactID).Scan(&version.Number, &version.Named, &version.Summary, &authors, &version.CreatedAt, &version.markdown, &version.docUpdateVersion); err != nil {
		return version, fmt.Errorf("read latest document version: %w", err)
	}
	if err := json.Unmarshal(authors, &version.Authors); err != nil {
		return version, fmt.Errorf("decode latest document version authors: %w", err)
	}
	return version, nil
}

func contentChangedSinceVersion(ctx context.Context, tx pgx.Tx, artifactID string, cursor int64) (bool, error) {
	var changed bool
	if err := tx.QueryRow(ctx, `
		select exists(
			select 1 from doc_updates
			where artifact_id = $1 and version > $2 and content_changed
		)
	`, artifactID, cursor).Scan(&changed); err != nil {
		return false, fmt.Errorf("check content updates since version: %w", err)
	}
	return changed, nil
}

// versionWriteResult is a written version together with the counted reference targets its body
// moved: the caller that appends the version event names them, so a reader holding those
// nodes' batched backlink counts refreshes exactly their rows.
type versionWriteResult struct {
	version model.Version
	changes model.ReferenceChanges
}

// writeVersionTx is the only path that changes the durable version protocol:
// version writes index references, refresh anchors, and retain its author
// capture until the enclosing transaction commits. A nil write records no
// version but keeps a transactional tree mutation's anchors in the same path.
func (s *Service) writeVersionTx(ctx context.Context, tx pgx.Tx, artifactID, markdown string, tree *pmdoc.Node, actor model.Actor, write *versionWrite) (versionWriteResult, error) {
	if write == nil {
		return versionWriteResult{}, s.refreshAnchors(ctx, tx, artifactID, tree, actor)
	}

	encodedAuthors, err := json.Marshal(write.authors)
	if err != nil {
		return versionWriteResult{}, fmt.Errorf("encode document version authors: %w", err)
	}
	var version model.Version
	var authorsRaw []byte
	if err := tx.QueryRow(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors, named, summary, doc_update_version)
		select $1, coalesce(max(number), 0) + 1, $2, $3, $4, $5,
			coalesce($6, (select max(version) from doc_updates where artifact_id = $1), 0)
		from artifact_versions where artifact_id = $1
		returning number, named, summary, authors, created_at
	`, artifactID, markdown, encodedAuthors, write.named, write.summary, write.docUpdateVersion).Scan(
		&version.Number, &version.Named, &version.Summary, &authorsRaw, &version.CreatedAt,
	); err != nil {
		return versionWriteResult{}, fmt.Errorf("write document version: %w", err)
	}
	if err := json.Unmarshal(authorsRaw, &version.Authors); err != nil {
		return versionWriteResult{}, fmt.Errorf("decode document version authors: %w", err)
	}
	changes, err := refs.ReplaceCounted(ctx, tx, "artifact", artifactID, markdown, s.serverURL)
	if err != nil {
		return versionWriteResult{}, err
	}
	if err := s.refreshAnchors(ctx, tx, artifactID, tree, actor); err != nil {
		return versionWriteResult{}, err
	}
	if write.capture != nil {
		s.rememberPendingVersion(artifactID, version, *write.capture)
	}
	return versionWriteResult{version: version, changes: changes}, nil
}

func actorKey(actor model.Actor) string {
	return actor.Kind + "\x00" + actor.ID
}

func actorSlice(actors map[string]model.Actor) []model.Actor {
	keys := make([]string, 0, len(actors))
	for key := range actors {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]model.Actor, 0, len(keys))
	for _, key := range keys {
		result = append(result, actors[key])
	}
	return result
}

func (s *Service) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	if tx, ok := txFromContext(ctx); ok {
		return fn(tx)
	}
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin document transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit document transaction: %w", err)
	}
	return nil
}
