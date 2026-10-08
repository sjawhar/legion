package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"weak"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

type versionWrite struct {
	named            bool
	summary          *string
	authors          []model.Actor
	capture          *authorCapture
	docUpdateVersion *int64
}

// mergeInto copies src into *dst, allocating *dst if it is nil and src holds anything; a nil or
// empty src leaves *dst exactly as it was, nil included.
func mergeInto[K comparable, V any](dst *map[K]V, src map[K]V) {
	if len(src) == 0 {
		return
	}
	if *dst == nil {
		*dst = make(map[K]V, len(src))
	}
	maps.Copy(*dst, src)
}

// recoverMutation, deferred around a document mutation, turns its panic into *err and logs it.
func recoverMutation(room string, err *error) {
	if recovered := recover(); recovered != nil {
		*err = fmt.Errorf("document mutation panicked: %v\n%s", recovered, debug.Stack())
		slog.Error("dispatch: document mutation panicked", "room", room, "error", *err)
	}
}

// applyLive runs mutate against artifactID's live document inside the transaction the context's
// ledger joined, and credits actor with the content it changes. Every document write a caller
// makes runs here - an upload's, an edit's, an accepted or rejected suggestion's, an ask's text
// and its answer, a comment's anchor and its margin record - so two rules hold for each by
// structure rather than by its caller remembering them:
//
//   - It refuses a context Join did not return (errUnjoined), as SeedText and the version writes
//     do: an unjoined write would reach no transaction to be weighed and appended in.
//   - It weighs what the write leaves, and refuses one that would grow the document past what one
//     upload may hold (refuseGrowth), before anything is appended.
//
// It writes the transaction's fork of the room (see liveWrite), appends the update inside the
// transaction, and leaves the room and the credit to its ledger's Commit, so a transaction that
// does not commit never reaches the room, a browser or a version's authors. It returns
// websocket.ErrNoChanges when mutate wrote nothing, as the room's Apply does, and
// ErrServiceUnavailable when the room its slot is on failed before its append.
func (s *Service) applyLive(ctx context.Context, artifactID string, actor model.Actor, mutate func(*crdt.Doc, func(func(*crdt.Transaction))) error) error {
	if s.shuttingDown(artifactID) {
		return ErrServiceUnavailable
	}
	tx, joined := txFromContext(ctx)
	if !joined {
		return errUnjoined
	}
	ledger := ledgerFrom(ctx)
	write, err := s.joinLiveWrite(ctx, ledger, artifactID)
	if err != nil {
		return err
	}
	fork, err := s.forkLive(ctx, write)
	if err != nil {
		return err
	}
	// An operation that changed the fork but does not reach the end below leaves it holding
	// changes its transaction never recorded; the next operation rebuilds it. One that changed
	// nothing leaves the fork as it was, so the next operation keeps it.
	var updates [][]byte
	recorded := false
	defer func() {
		if !recorded && len(updates) > 0 {
			write.fork, write.forkedFrom = nil, weak.Pointer[crdt.Doc]{}
			// The rendering describes that fork, so it goes with it. forkLive's rebuild drops
			// it too; keeping the two lines that abandon a fork together is what covers an
			// operation that recorded the rendering and then failed to version.
			write.dropRendering()
			// A stamp this failed operation ran (stampAskBlockIDs, registerStampedAskBlocks)
			// staged its renamed ids and any carried-forward authors on these before refusal -
			// a later operation of the same transaction must not inherit them as its own.
			write.addedAskBlockIDs, write.renamedAskBlockIDs, write.carriedAskAuthors = nil, nil, nil
		}
	}()
	// The tree the operation starts from. One outside the Proof schema - the reader refuses it, or
	// only the renderer does - is the document's own refusal (OutsideSchemaError), which every
	// schema refusal this operation meets is then answered with, unless the operation repairs it.
	// Its rendering is taken only when something needs it: a failure to classify, or the content
	// comparison of a write that changed the fork.
	before, startErr := treeOf(fork)
	if startErr != nil && !errors.Is(startErr, ErrDocOutsideSchema) {
		return startErr
	}
	var beforeMarkdown string
	rendered := startErr != nil
	start := func() error {
		if !rendered {
			beforeMarkdown, startErr = documentMarkdown(before)
			rendered = true
		}
		return startErr
	}
	unsubscribe := fork.OnUpdate(func(update []byte, _ any) {
		updates = append(updates, append([]byte(nil), update...))
	})
	margin, stopMargin := watchMargin(fork)
	mutateErr := func() (err error) {
		defer recoverMutation(artifactID, &err)
		return mutate(fork, func(inner func(*crdt.Transaction)) { fork.Transact(inner) })
	}()
	stopMargin()
	unsubscribe()
	if mutateErr != nil {
		return joinedSchemaError(mutateErr, start)
	}
	if len(updates) == 0 {
		return websocket.ErrNoChanges
	}
	tree, err := treeOf(fork)
	var markdown string
	if err == nil {
		markdown, err = documentMarkdown(tree)
	}
	if err != nil {
		return joinedSchemaError(err, start)
	}
	// A write over a tree outside the Proof schema that leaves it readable is a repair, so it
	// necessarily changes the content a version stores, and what it leaves is weighed as a new
	// document's rendering is (SeedText): against nothing, with no anchor marks before it and no
	// typed-block state to be the only change. Other writes compare the markdown before and after,
	// the measure the room's update observer classifies a live update by (updateChangesMarkdown): a
	// write that leaves the rendered markdown alone - an anchor mark, a mark record's projection, an
	// attribute no rendering carries - changes nothing a version stores.
	contentChanged, weighedFrom := true, (*pmdoc.Node)(nil)
	err = start()
	if err != nil && !errors.Is(err, ErrDocOutsideSchema) {
		return err
	}
	unchanged := beforeMarkdown == markdown
	if err == nil {
		contentChanged, weighedFrom = !unchanged, before
	}
	if err := refuseGrowth(growth{fork: fork, before: beforeMarkdown, after: markdown, unchanged: unchanged, margin: margin, anchors: anchorWatch{before: weighedFrom, after: tree}, serverState: func() bool {
		return weighedFrom != nil && tree.EqualOutsideServerState(weighedFrom)
	}}); err != nil {
		return err
	}
	update, err := mergeUpdates(updates)
	if err != nil {
		return err
	}
	if _, err := s.persistence.AppendUpdateTx(ctx, tx, artifactID, update, contentChanged); err != nil {
		return fmt.Errorf("append transactional live document update: %w", err)
	}
	// The append holds the document's advisory lock until the transaction ends, so no eviction
	// of the room can finish before then, and a later reload holds the write. A room that failed
	// before it may already have reloaded without the write, beyond the writer slot, which is on
	// the failed room: the write cannot reach it coherently, so it fails - before it records the
	// rendering below, which a refused write must not leave behind.
	if err := write.state.failure(); err != nil {
		// The 503 this becomes is the only other trace of a room that stays failed, so the
		// refusal names the room and the cause here too (see awaitRoomRecovery).
		slog.Warn("dispatch: refuse a joined write on a failed document room",
			"room", artifactID, "error", err)
		return err
	}
	// The rendering this operation produced is the document as the transaction now sees it, so
	// a version it writes later snapshots this tree rather than walking and rendering the
	// document again (captureLiveTextAndAuthors). forkLive drops both the moment the fork moves.
	write.tree, write.markdown = tree, markdown
	if _, err := s.writeVersionTx(ctx, tx, artifactID, markdown, tree, actor, nil); err != nil {
		return fmt.Errorf("refresh transactional document anchors: %w", err)
	}
	write.anchorsTree = tree
	write.updates = append(write.updates, update)
	recorded = true
	if contentChanged {
		s.creditLiveWrite(write, actor)
		added := addedAskBlockIDs(before, tree)
		maps.DeleteFunc(added, func(id string, _ struct{}) bool {
			_, excluded := write.renamedAskBlockIDs[id]
			return excluded
		})
		mergeInto(&write.addedAskBlockIDs, added)
	}
	return nil
}

// joinedSchemaError is the error a joined operation answers for err, a failure it met after it
// read the tree it started from (start). A schema refusal is that tree's own when the tree is
// outside the schema - start's OutsideSchemaError, the same one every route answers - and
// otherwise the operation's fault, since the tree it started from was readable. A refusal of the
// caller's own ask block keeps its reason, which writeHandlerError answers before any schema
// refusal.
func joinedSchemaError(err error, start func() error) error {
	var invalidAsk *ErrInvalidAskBlock
	if !errors.Is(err, ErrDocSchema) || errors.As(err, &invalidAsk) {
		return err
	}
	if startErr := start(); errors.Is(startErr, ErrDocOutsideSchema) {
		return startErr
	}
	return producedSchemaError(err)
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
func (s *Service) SeedText(ctx context.Context, artifactID, markdown string, actor model.Actor) (string, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return "", errUnjoined
	}
	tree, err := parseInput(markdown)
	if err != nil {
		return "", err
	}
	if err := pmdoc.AskContentError(tree); err != nil {
		return "", &ErrInvalidAskBlock{Reason: err}
	}
	canonical, err := renderTree(tree)
	if err != nil {
		return "", err
	}
	if err := weighRendering("", canonical); err != nil {
		return "", err
	}
	update, err := encodeDocumentTree(tree)
	if err != nil {
		return "", fmt.Errorf("seed live document tree: %w", err)
	}
	if _, err := s.persistence.AppendUpdateTx(ctx, tx, artifactID, update, true); err != nil {
		return "", fmt.Errorf("seed live document: %w", err)
	}
	// The seeding actor is the caller's own first version author (written directly by the
	// caller, never through writeVersionTx), so it must not join `pending` - only the
	// settlement that indexes the seeded ask blocks needs to know who wrote them. The ledger
	// records it once the transaction commits (Ledger.creditLocked), so a seed that never commits
	// leaves the document no state.
	ledgerFrom(ctx).seeded(artifactID, tree, actor)
	return canonical, nil
}

// ReplaceText replaces the entire live document tree so connected clients
// receive document uploads as a regular server-side transaction.
func (s *Service) ReplaceText(ctx context.Context, artifactID, markdown string, actor model.Actor) (string, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return "", errUnjoined
	}
	anchors, err := s.openAnchoredMarks(ctx, s.queryFrom(ctx), artifactID)
	if err != nil {
		return "", err
	}
	// The live document's locks, in their order (see liveWrite), before the write reads the room:
	// its owner row and writer slot, then, with the room loaded, its advisory lock. The upload's
	// version clears every pending author and each in-flight credit its read of the room held
	// (Ledger.WroteVersion), so no edit may land between that read and its commit.
	if _, err := s.joinLiveWrite(ctx, ledgerFrom(ctx), artifactID); err != nil {
		return "", err
	}
	if err := s.warmLiveDocument(ctx, artifactID); err != nil {
		return "", err
	}
	if err := lockDocumentRoom(ctx, tx, artifactID); err != nil {
		return "", err
	}
	var canonical string
	var unchanged bool
	err = s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) error {
		fragment := doc.GetXmlFragment(fragmentName)
		current, err := treeOf(doc)
		var currentMarkdown string
		if err == nil {
			currentMarkdown, err = documentMarkdown(current)
		}
		// A tree outside the Proof schema - one treeOf refuses, or one it reads that does not
		// render - has no readable text to compare or reanchor against: this replacement repairs it.
		repairing := errors.Is(err, ErrDocOutsideSchema)
		if err != nil && !repairing {
			return err
		}
		if repairing {
			current = nil
		}
		target, err := parseReplacing(current, markdown)
		if err != nil {
			return err
		}
		// A repair is a whole new document, held to the ask content rule for every ask, as
		// RebuildDocument holds supplied markdown, and it keeps every open ask block.
		if repairing {
			if err := pmdoc.AskContentError(target); err != nil {
				return &ErrInvalidAskBlock{Reason: err}
			}
			if err := s.refuseDroppedAskBlocks(ctx, artifactID, target); err != nil {
				return &ErrInvalidAskBlock{Reason: err}
			}
		} else if err := refuseChangedAsks(current, target, pmdoc.AskContentError, newAskMarkdown()); err != nil {
			return &ErrInvalidAskBlock{Reason: err}
		}
		if canonical, err = renderTree(target); err != nil {
			return err
		}
		if !repairing && currentMarkdown == canonical {
			unchanged = true
			return nil
		}
		type reanchor struct {
			mark   anchoredMark
			range_ pmdoc.Range
			attrs  pmdoc.Attrs
		}
		reanchors := make([]reanchor, 0, len(anchors))
		if !repairing {
			for _, mark := range anchors {
				range_, _, found := pmdoc.FindMark(current, mark.markType, mark.anchor.MarkID)
				if !found {
					continue
				}
				attrs, found := pmdoc.MarkAttrs(current, mark.markType, mark.anchor.MarkID)
				if !found {
					return fmt.Errorf("%w: mark %q is missing attributes", ErrDocSchema, mark.anchor.MarkID)
				}
				reanchors = append(reanchors, reanchor{mark: mark, range_: range_, attrs: attrs})
			}
		}
		var updateErr error
		transact(func(transaction *crdt.Transaction) {
			// A repair keeps nothing of the unreadable tree, which pmdoc.Update would otherwise
			// diff against node by node, reading text content - an embed a crafted client wrote
			// into a paragraph, say - that it refuses.
			if length := fragment.Len(); repairing && length > 0 {
				fragment.Delete(transaction, 0, length)
			}
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
			return updateErr
		}
		return nil
	})
	if unchanged && errors.Is(err, websocket.ErrNoChanges) {
		return canonical, nil
	}
	if err != nil {
		return "", wrapUnlessTooLarge(err, "replace live document")
	}
	return canonical, nil
}

func (s *Service) refuseDroppedAskBlocks(ctx context.Context, artifactID string, target *pmdoc.Node) error {
	rows, err := s.queryFrom(ctx).Query(ctx, `
		select block_id from asks
		where block_artifact_id = $1 and state = 'open' and block_id is not null
	`, artifactID)
	if err != nil {
		return fmt.Errorf("read open ask blocks: %w", err)
	}
	defer rows.Close()
	open := map[string]struct{}{}
	for rows.Next() {
		var blockID string
		if err := rows.Scan(&blockID); err != nil {
			return fmt.Errorf("scan open ask block: %w", err)
		}
		open[blockID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read open ask blocks: %w", err)
	}
	pmdoc.Walk(target, func(node *pmdoc.Node) bool {
		if node.Type == "ask" {
			if blockID, ok := node.Attrs[pmdoc.BlockIDAttr].(string); ok {
				delete(open, blockID)
			}
		}
		return true
	})
	if len(open) == 0 {
		return nil
	}
	blockIDs := make([]string, 0, len(open))
	for blockID := range open {
		blockIDs = append(blockIDs, blockID)
	}
	sort.Strings(blockIDs)
	return fmt.Errorf("replacement removes open ask blocks %s", strings.Join(blockIDs, ", "))
}

// Text returns the rendered document the caller sees (readTree).
func (s *Service) Text(ctx context.Context, artifactID string) (string, error) {
	tree, err := s.readTree(ctx, artifactID)
	if err != nil || tree == nil {
		return "", err
	}
	return documentMarkdown(tree)
}

// TextWithToken returns canonical markdown and a token over its full Proof tree,
// including inline marks that canonical Markdown does not render.
func (s *Service) TextWithToken(ctx context.Context, artifactID string) (string, string, error) {
	tree, err := s.readTree(ctx, artifactID)
	if err != nil || tree == nil {
		return "", "", err
	}
	return renderTokenTree(tree)
}

// readTree returns the tree of the document the caller sees without loading or writing its room,
// so it also reads a closed issue's document: the calling transaction's fork's when it has one
// (joinRead), else the resident room's, as of one moment (liveTree), else the persisted
// document's. A document with no persisted state has no tree, and no error.
func (s *Service) readTree(ctx context.Context, artifactID string) (*pmdoc.Node, error) {
	tree, _, err := s.loadTree(ctx, artifactID)
	return tree, err
}

// loadTree is readTree with the durable state it decoded the document from, nil when it read a
// fork or the resident room. A durable history that does not decode is ErrDocumentUnloadable and
// fails the room, as ygo's own load of it would.
func (s *Service) loadTree(ctx context.Context, artifactID string) (*pmdoc.Node, *persistence.LoadResult, error) {
	if err := s.awaitRoomRecovery(ctx, artifactID); err != nil {
		return nil, nil, err
	}
	fork, err := s.joinRead(ctx, artifactID)
	if err != nil {
		return nil, nil, err
	}
	if fork != nil {
		tree, err := treeOf(fork)
		return tree, nil, err
	}
	if live := s.srv.GetDoc(artifactID); live != nil {
		tree, err := s.liveTree(artifactID, live)
		return tree, nil, err
	}
	loaded, err := s.persistence.Load(ctx, artifactID)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, err
		}
		s.failRoom(artifactID, err)
		return nil, nil, fmt.Errorf("%w: %w", ErrServiceUnavailable, err)
	}
	if len(loaded.Update) == 0 {
		return nil, &loaded, nil
	}
	doc := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		// The room carries the decode failure as its cause, which a reader that meets the
		// failed room answers as DOC_SERVICE_UNAVAILABLE; this read met the history itself.
		s.failRoom(artifactID, fmt.Errorf("decode live document: %w", err))
		return nil, nil, fmt.Errorf("%w: decode live document: %w", ErrDocumentUnloadable, err)
	}
	tree, err := treeOf(doc)
	return tree, &loaded, err
}

func renderTokenTree(tree *pmdoc.Node) (string, string, error) {
	markdown, err := documentMarkdown(tree)
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

// TextWithBlocks renders the document the caller sees (readTree) once and returns its
// canonical markdown beside the blocks whose byte ranges index into it.
func (s *Service) TextWithBlocks(ctx context.Context, artifactID string) (string, []model.ArtifactBlock, error) {
	tree, err := s.readTree(ctx, artifactID)
	if err != nil || tree == nil {
		return "", nil, err
	}
	tableDescendants, err := pmdoc.TableDescendantIDs(tree)
	if err != nil {
		return "", nil, documentSchemaError(err)
	}
	tokens, err := blockTokens(tree)
	if err != nil {
		return "", nil, documentSchemaError(err)
	}
	markdown, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil {
		return "", nil, documentSchemaError(err)
	}
	blocks := make([]model.ArtifactBlock, len(offsets))
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
	return markdown, blocks, nil
}

// BlockPath is where the block carrying blockID stands in the document the caller sees
// (readTree): pmdoc.ErrTargetNotFound when no block carries it, a document with no state
// included.
func (s *Service) BlockPath(ctx context.Context, artifactID, blockID string) (model.BlockPath, error) {
	tree, err := s.readTree(ctx, artifactID)
	if err != nil {
		return model.BlockPath{}, err
	}
	if tree == nil {
		return model.BlockPath{}, fmt.Errorf("%w: block %q", pmdoc.ErrTargetNotFound, blockID)
	}
	path, err := pmdoc.BlockPathOf(tree, blockID)
	if err != nil {
		return model.BlockPath{}, err
	}
	out := model.BlockPath{ID: path.ID, Type: path.Type, Path: make([]model.BlockPathEntry, len(path.Path))}
	for index, entry := range path.Path {
		out.Path[index] = model.BlockPathEntry(entry)
	}
	if path.Table != nil {
		table := model.TablePosition(*path.Table)
		out.Table = &table
	}
	return out, nil
}

// SnapshotVersion returns the current immutable version, adding an unnamed
// version only when the live text has diverged since the previous one.
func (s *Service) SnapshotVersion(ctx context.Context, artifactID string, actor model.Actor) (VersionResult, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return VersionResult{}, errUnjoined
	}
	tree, markdown, capture, err := s.captureLiveTextAndAuthors(ctx, artifactID, nil)
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
	result, err := s.writeVersionTx(ctx, tx, artifactID, markdown, tree, actor, &versionWrite{
		authors: actorSlice(capture.authors),
		capture: &capture,
	})
	if err != nil {
		return VersionResult{}, err
	}
	return VersionResult{Version: result.version, Wrote: true, Changes: result.changes}, nil
}

// prevalidateLiveOperations performs database-backed table-anchor checks
// before the Yjs transaction, retaining the table-mark snapshots the
// transaction re-derives before it writes.
func (s *Service) prevalidateLiveOperations(ctx context.Context, artifactID string, ops []model.EditOp) ([]tableAnchorSnapshot, error) {
	tree, err := s.docTree(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	return s.prevalidateOperations(ctx, artifactID, tree, ops)
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

// currentToken is the whole-document token of the document the caller sees, for an edit that
// applies no operation and so writes no tree of its own to take one from.
func (s *Service) currentToken(ctx context.Context, artifactID string) (string, error) {
	tree, err := s.docTree(ctx, artifactID)
	if err != nil {
		return "", err
	}
	return nodeToken(tree)
}

// ApplyOps resolves every requested operation against the document's one Yjs
// transaction. A conditional edit checks its precondition, resolves the batch,
// and writes the plan inside that transaction, so no live writer can enter the
// check-to-apply window.
func (s *Service) ApplyOps(ctx context.Context, artifactID string, ops []model.EditOp, actor model.Actor, precondition *model.EditPrecondition) (EditOutcome, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return EditOutcome{}, errUnjoined
	}
	if precondition == nil {
		return s.applyOpsUnconditional(ctx, artifactID, ops, actor)
	}
	// The live document's locks, in their order (see liveWrite): its owner row and, unless the
	// room has failed, its writer slot; then, with the room loaded, its advisory lock, held from
	// before the precondition is read.
	if _, err := s.joinLiveWrite(ctx, ledgerFrom(ctx), artifactID); err != nil {
		return EditOutcome{}, err
	}
	if err := s.warmLiveDocument(ctx, artifactID); err != nil {
		return EditOutcome{}, err
	}
	if err := lockDocumentRoom(ctx, tx, artifactID); err != nil {
		return EditOutcome{}, err
	}
	if len(ops) == 0 {
		// No operation to apply, so the document this check read is the document the caller's
		// next edit meets: its token is that edit's precondition.
		tree, err := s.docTree(ctx, artifactID)
		if err != nil {
			return EditOutcome{}, err
		}
		if err := checkEditPrecondition(tree, *precondition); err != nil {
			return EditOutcome{}, err
		}
		token, err := nodeToken(tree)
		if err != nil {
			return EditOutcome{}, err
		}
		return EditOutcome{Token: token}, nil
	}
	var (
		err       error
		outcome   EditOutcome
		snapshots []tableAnchorSnapshot
	)
	if hasTableAnchorMutation(ops) {
		snapshots, err = s.prevalidateLiveOperations(ctx, artifactID, ops)
		if err != nil {
			return EditOutcome{}, fmt.Errorf("prevalidate live document operations: %w", err)
		}
	}
	if err := lockTableAnchorRows(ctx, tx, artifactID, snapshots); err != nil {
		return EditOutcome{}, err
	}
	err = s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) error {
		fragment := doc.GetXmlFragment(fragmentName)
		// Read before the Yjs transaction opens: the state vector takes the document lock.
		since := authoredClock(ctx, artifactID, doc)
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
			stamped := stampAskBlockIDs(tree)
			batch, err := applyOperations(tree, ops)
			if err != nil {
				mutationErr = err
				return
			}
			if err := requirePreconditionCoverage(tree, ops, *precondition); err != nil {
				mutationErr = err
				return
			}
			next := batch.tree
			pmdoc.EnsureBlockIDs(next)
			if err := validateEditedAskBlocks(tree, next); err != nil {
				mutationErr = &ErrInvalidAskBlock{Reason: err}
				return
			}
			if outcome, mutationErr = batch.outcome(len(ops)); mutationErr != nil {
				return
			}
			mutationErr = recordInsertedText(ctx, artifactID, "", fragment, since, batch.writes, func() error {
				if err := pmdoc.Update(transaction, fragment, next); err != nil {
					return err
				}
				s.registerStampedAskBlocks(ctx, artifactID, stamped)
				return nil
			})
		})
		if mutationErr != nil {
			return mutationErr
		}
		return nil
	})
	if err != nil {
		// A batch that resolved and wrote nothing is not a failure: it is the verdict the route
		// mints no version for.
		if errors.Is(err, websocket.ErrNoChanges) {
			return outcome, nil
		}
		if isEditRefusal(err) {
			return EditOutcome{}, err
		}
		return EditOutcome{}, fmt.Errorf("apply live document operations: %w", err)
	}
	return outcome, nil
}

// applyOpsUnconditional preserves the precondition-free edit path's existing
// validation and live-mutation behavior. Its mutate callback below reads and writes applyLive's
// own fork, never the live room directly: applyLive refuses an unjoined context (errUnjoined) and
// always hands mutate a fork, a structural guarantee rather than a rule this function keeps.
func (s *Service) applyOpsUnconditional(ctx context.Context, artifactID string, ops []model.EditOp, actor model.Actor) (EditOutcome, error) {
	if len(ops) == 0 {
		// Nothing to apply: the caller still gets the token of the document as it stands, the
		// precondition its next edit passes.
		token, err := s.currentToken(ctx, artifactID)
		if err != nil {
			return EditOutcome{}, err
		}
		return EditOutcome{Token: token}, nil
	}
	var outcome EditOutcome
	err := s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) error {
		fragment := doc.GetXmlFragment(fragmentName)
		since := authoredClock(ctx, artifactID, doc)
		tree, err := treeOf(doc)
		if err != nil {
			return err
		}
		stamped := stampAskBlockIDs(tree)
		batch, err := s.applyOperations(ctx, artifactID, tree, ops)
		if err != nil {
			return err
		}
		next := batch.tree
		pmdoc.EnsureBlockIDs(next)
		if err := validateEditedAskBlocks(tree, next); err != nil {
			return &ErrInvalidAskBlock{Reason: err}
		}
		if outcome, err = batch.outcome(len(ops)); err != nil {
			return err
		}
		return recordInsertedText(ctx, artifactID, "", fragment, since, batch.writes, func() error {
			var updateErr error
			transact(func(transaction *crdt.Transaction) {
				if updateErr = pmdoc.Update(transaction, fragment, next); updateErr != nil {
					return
				}
				s.registerStampedAskBlocks(ctx, artifactID, stamped)
			})
			return updateErr
		})
	})
	if err != nil {
		if errors.Is(err, websocket.ErrNoChanges) {
			return outcome, nil
		}
		if isEditRefusal(err) {
			return EditOutcome{}, err
		}
		return EditOutcome{}, fmt.Errorf("apply live document operations: %w", err)
	}
	return outcome, nil
}

// SetBlockAttributes applies server-owned typed-block state through the
// transactional live-document mutation path. An ask's answer the document has no room for - its
// words or the options it selects (pmdoc.IsAnswerAttribute), which are caller text - is left out of
// the block: the block is written without either, saying who answered and when, and the caller's
// ask keeps the answer, as settlement leaves a returning block's answer out (withholdAnswers).
func (s *Service) SetBlockAttributes(
	ctx context.Context,
	artifactID, blockID string,
	attributes map[string]any,
	actor model.Actor,
) error {
	err := s.setBlockAttributes(ctx, artifactID, blockID, attributes, actor)
	if IsTooLarge(err) {
		if withheld, withholds := withoutAnswer(attributes); withholds {
			slog.Warn("dispatch: an answer is left out of its block", "room", artifactID, "block", blockID, "reason", err)
			// A refused write appends nothing, and the transaction's next write starts from its
			// fork as it was (applyLive).
			err = s.setBlockAttributes(ctx, artifactID, blockID, withheld, actor)
		}
	}
	// Setting attributes a block already carries writes nothing, which the live path reports as
	// ErrNoChanges; the block holds what the caller asked for, so that is success.
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return wrapUnlessTooLarge(err, "set live block attributes")
	}
	return nil
}

// withoutAnswer is attributes with each answer attribute they name (pmdoc.IsAnswerAttribute) taken
// off the block instead, and whether they name one with a value to leave out.
func withoutAnswer(attributes map[string]any) (withheld map[string]any, withholds bool) {
	withheld = make(map[string]any, len(attributes))
	for name, value := range attributes {
		if pmdoc.IsAnswerAttribute(name) {
			withholds = withholds || value != nil
			value = nil
		}
		withheld[name] = value
	}
	return withheld, withholds
}

func (s *Service) setBlockAttributes(ctx context.Context, artifactID, blockID string, attributes map[string]any, actor model.Actor) error {
	return s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) error {
		fragment := doc.GetXmlFragment(fragmentName)
		tree, err := treeOf(doc)
		if err != nil {
			return err
		}
		next, err := pmdoc.SetBlockAttributes(tree, blockID, pmdoc.Attrs(pmdoc.LineFeedAttrs(attributes)))
		if err != nil {
			return err
		}
		if next.EqualWithBlockIDs(tree) {
			return nil
		}
		var updateErr error
		transact(func(transaction *crdt.Transaction) {
			updateErr = pmdoc.Update(transaction, fragment, next)
		})
		if updateErr != nil {
			return updateErr
		}
		return nil
	})
}

// NamedVersion records the live document as a deliberately named immutable version. Like
// SeedText and SnapshotVersion it runs inside the caller's joined transaction, which credits
// its authors and publishes its events when it commits (Ledger.Commit).
func (s *Service) NamedVersion(ctx context.Context, artifactID, summary string, actor model.Actor) (VersionResult, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return VersionResult{}, errUnjoined
	}
	tree, markdown, capture, err := s.captureLiveTextAndAuthors(ctx, artifactID, &actor)
	if err != nil {
		return VersionResult{}, err
	}
	_, open, err := lockArtifactOwner(ctx, tx, artifactID)
	if err != nil {
		return VersionResult{}, err
	}
	if !open {
		return VersionResult{}, ErrIssueClosed
	}
	result, err := s.writeVersionTx(ctx, tx, artifactID, markdown, tree, actor, &versionWrite{
		named:   true,
		summary: new(summary),
		authors: actorSlice(capture.authors),
		capture: &capture,
	})
	if err != nil {
		return VersionResult{}, err
	}
	return VersionResult{Version: result.version, Wrote: true, Changes: result.changes}, nil
}

// serviceTransact wraps Server.Apply's transact so the room's update observer can tell the
// service's own repairs - the sweep of marks no row records - from browser peers' edits, and
// credits their changes to no one: the Apply call's origin is registered in serviceOrigins on the
// first transaction and forgotten by release. Observers fire before a transaction returns, so
// release is safe once the Apply callback is done with transact.
func (s *Service) serviceTransact(transact func(func(*crdt.Transaction))) (wrapped func(func(*crdt.Transaction)), release func()) {
	var origin any
	wrapped = func(inner func(*crdt.Transaction)) {
		transact(func(txn *crdt.Transaction) {
			if origin == nil {
				origin = txn.Origin
				s.serviceOrigins.Store(origin, struct{}{})
			}
			if s.inServiceTransaction != nil {
				s.inServiceTransaction(txn)
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

// stampAskBlockIDs stamps tree's block ids (EnsureBlockIDs) and returns each ask whose id it
// renamed, pairing the tree's ask-block order before and after. It runs eagerly, since the stamp
// must happen before the edit's own operations read tree's ids, but registering the rename into
// the room's bookkeeping waits until the write that ran it is certain to land
// (registerStampedAskBlocks): an earlier-looking success here does not mean the edit's later
// operations, validation or outcome will not still fail before the stamp's rename ever reaches
// the document.
func stampAskBlockIDs(tree *pmdoc.Node) []stampedAsk {
	before := askBlockOrder(tree)
	pmdoc.EnsureBlockIDs(tree)
	if len(before) == 0 {
		return nil
	}
	return stampedAskBlocks(before, askBlockOrder(tree))
}

// registerStampedAskBlocks stages, on the write's own liveWrite (joinedLiveWrite - every caller
// is joined, applyLive refuses otherwise before the mutate closure this always runs inside ever
// starts), every id the stamp (stampAskBlockIDs) renamed - for applyLive's own before/after diff
// to exclude, via write.renamedAskBlockIDs - and the author carried forward for each one that has
// a recorded author to carry (carriedAskAuthors, the same rule a settlement's or the backfill's
// own suppressed stamp applies), via write.carriedAskAuthors. Neither is written into the room's
// own bookkeeping here: Ledger.creditLocked registers both only once the write they belong to
// actually commits, the same point it already registers write.addedAskBlockIDs, since a write
// refused after this call returns (growth, a later validation failure) must not leave the room
// holding a trace of it (LEGION-503). Call once stampAskBlockIDs has run.
func (s *Service) registerStampedAskBlocks(ctx context.Context, room string, stamped []stampedAsk) {
	if len(stamped) == 0 {
		return
	}
	write := joinedLiveWrite(ctx, room)
	renamed := make(map[string]struct{}, len(stamped))
	for _, ask := range stamped {
		renamed[ask.minted] = struct{}{}
	}
	mergeInto(&write.renamedAskBlockIDs, renamed)

	state := s.lockState(room)
	carried := state.carriedAskAuthors(stamped)
	s.unlockState(room, state)
	mergeInto(&write.carriedAskAuthors, carried)
}

// captureAuthorsLocked is captureAuthors under room's state lock, held for just that call.
func (s *Service) captureAuthorsLocked(room string, owed map[string]model.Actor, write *liveWrite, actor *model.Actor) authorCapture {
	state := s.lockState(room)
	defer s.unlockState(room, state)
	return captureAuthors(state, owed, write, actor)
}

// captureLiveTextAndAuthors is the tree a version records and whom it lists, its authors taken
// under the document's advisory lock and no later than it reads that tree. It takes the
// document's locks in their order (see liveWrite) before anything else: the owner row and the
// writer slot, the room loaded, then the advisory lock, which every write to the pending authors
// and every in-flight credit's landing hold, so the authors it reads stay where it read them until
// the version's transaction ends. A transaction with a write of its own versions its fork, which
// joinRead brings up to date with the room: that is where a browser change made while the write
// was in flight merges with it - and where a write whose text that merge annihilated is refused
// rather than versioned as applied (refuseLostWrite, LEGION-269). Its authors are taken before
// that read, so each in-flight credit taken is for a change the room held and the fork takes in.
// Without a write of its own, a version reads the room as of one moment under its document lock
// (holdLive), after taking its authors under the room's state lock. Either way a credit observed
// after the take, for a change the version holds or not, stays pending for the next version
// (LEGION-503). The room is taken inside the Apply that loads and as docTree reads it: a room
// looked up again once that Apply returned can have been evicted in between.
func (s *Service) captureLiveTextAndAuthors(ctx context.Context, room string, actor *model.Actor) (*pmdoc.Node, string, authorCapture, error) {
	tx, _ := txFromContext(ctx)
	write := joinedLiveWrite(ctx, room)
	if write == nil {
		fork, err := s.joinRead(ctx, room)
		if err != nil {
			return nil, "", authorCapture{}, err
		}
		if err := s.refuseLostWrite(ctx, room, fork, actor); err != nil {
			return nil, "", authorCapture{}, err
		}
	}
	if err := s.warmLiveDocument(ctx, room); err != nil {
		return nil, "", authorCapture{}, err
	}
	// The warmed-read hook deliberately evicts a loaded room in tests. It runs before this
	// transaction takes the document lock: CloseRoom flushes a room's persistence, which may
	// need that lock, so invoking it from the tree read while we hold the lock would be a cycle.
	// The capture below takes the lock only after the hook returns, then reads R and F before
	// it holds the current room's tree.
	if write == nil && s.afterReadWarm != nil {
		s.afterReadWarm(room)
	}
	if err := lockDocumentRoom(ctx, tx, room); err != nil {
		return nil, "", authorCapture{}, err
	}
	owed, err := readPendingAuthors(ctx, tx, room)
	if err != nil {
		return nil, "", authorCapture{}, err
	}
	var tree *pmdoc.Node
	var capture authorCapture
	if write != nil {
		capture = s.captureAuthorsLocked(room, owed, write, actor)
		fork, err := s.joinRead(ctx, room)
		if err != nil {
			return nil, "", authorCapture{}, err
		}
		if err := s.refuseLostWrite(ctx, room, fork, actor); err != nil {
			return nil, "", authorCapture{}, err
		}
		if s.afterCaptureFork != nil {
			s.afterCaptureFork(room)
		}
		if write.tree != nil && write.fork == fork {
			return write.tree, write.markdown, capture, nil
		}
		if tree, err = treeOf(fork); err != nil {
			return nil, "", authorCapture{}, err
		}
	} else {
		capture = s.captureAuthorsLocked(room, owed, nil, actor)
		if s.afterCaptureAuthorsTake != nil {
			s.afterCaptureAuthorsTake(room)
		}
		var readErr error
		err = s.srv.Apply(ctx, room, func(live *crdt.Doc, _ func(func(*crdt.Transaction))) {
			var doc *crdt.Doc
			var release func()
			doc, release, readErr = s.holdLive(room, live)
			if readErr != nil {
				return
			}
			defer release()
			tree, readErr = treeOf(doc)
		})
		if readErr != nil {
			return nil, "", authorCapture{}, readErr
		}
		if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			return nil, "", authorCapture{}, fmt.Errorf("warm live document: %w", err)
		}
	}
	markdown, err := documentMarkdown(tree)
	if err != nil {
		return nil, "", authorCapture{}, err
	}
	return tree, markdown, capture, nil
}

// captureAuthors is whom a version lists: owed, the pending authors its transaction read under
// the document's advisory lock; the room's in-flight credits no committed version has listed; the
// authors the calling transaction's own write records once it commits; and actor. The version's
// commit deletes the pending authors it read and marks the in-flight credits it took consumed
// (Ledger.commit); a credit observed after this take is not among them and stays pending for the
// next version. The caller holds state.mu.
func captureAuthors(state *roomState, owed map[string]model.Actor, write *liveWrite, actor *model.Actor) authorCapture {
	capture := authorCapture{
		state:   state,
		authors: make(map[string]model.Actor, len(owed)+1),
		rKeys:   make([]model.Actor, 0, len(owed)),
	}
	for key, author := range owed {
		capture.authors[key] = author
		capture.rKeys = append(capture.rKeys, author)
	}
	capture.inflight = state.unconsumedInflightLocked(state.creditSeq.Load())
	for _, record := range capture.inflight {
		maps.Copy(capture.authors, record.authors)
	}
	if write != nil {
		maps.Copy(capture.authors, write.credits)
	}
	if actor != nil {
		capture.authors[actorKey(*actor)] = *actor
	}
	return capture
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
// version writes index references, move open approval asks to the new version, refresh anchors,
// and retain their author capture until the enclosing transaction commits. A nil write records no
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
	// The version's tree is the one the issue's task counts are read from, so they are written
	// with it (LEGION-542).
	if err := RecordTaskProgress(ctx, tx, artifactID, tree); err != nil {
		return versionWriteResult{}, err
	}
	if err := json.Unmarshal(authorsRaw, &version.Authors); err != nil {
		return versionWriteResult{}, fmt.Errorf("decode document version authors: %w", err)
	}
	changes, err := refs.ReplaceCounted(ctx, tx, "artifact", artifactID, markdown, s.serverURL)
	if err != nil {
		return versionWriteResult{}, err
	}
	// The open approval request follows the document version without leaving its thread or opening
	// another Inbox row. The request remains waiting on its agent until it is handed back.
	moved, err := MoveApprovalAsk(ctx, tx, s.events, artifactID, version, actor, s.serverURL)
	if err != nil {
		return versionWriteResult{}, err
	}
	for _, event := range moved {
		collectEvent(ctx, event)
	}
	// The transaction's own live operation already refreshed this tree's anchors (applyLive);
	// refreshing the same tree twice reads and re-derives every open anchor for no change.
	// Neither call goes: this is the only refresh a version written without a live write of its
	// own gets - settlement and a standalone named version - and the write == nil arm above is
	// the only one a mark-only write, which writes no version, gets at all.
	if live := joinedLiveWrite(ctx, artifactID); live == nil || live.anchorsTree != tree {
		if err := s.refreshAnchors(ctx, tx, artifactID, tree, actor); err != nil {
			return versionWriteResult{}, err
		}
	}
	if write.capture != nil {
		if ledger := ledgerFrom(ctx); ledger != nil && ledger.tx == tx {
			ledger.recordVersion(artifactID, version, *write.capture)
		}
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
