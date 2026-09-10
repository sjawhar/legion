package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

type versionPending struct {
	generation uint64
	authors    map[string]model.Actor
}

type versionWrite struct {
	named   bool
	summary *string
	authors []model.Actor
	capture *versionPending
}

func (s *Service) applyLive(ctx context.Context, artifactID string, mutate func(*crdt.Doc, func(func(*crdt.Transaction))) (bool, error)) error {
	tx, joinedTransaction := txFromContext(ctx)
	var slot *suppressSlot
	var state *roomState
	if joinedTransaction {
		slot = s.prepareSuppressedPersistence(artifactID)
		state = s.room(artifactID)
		state.mu.Lock()
		state.gen++
		if state.settle != nil && state.settle.Stop() {
			s.settleWG.Done()
		}
		state.suppressSettle++
		state.mu.Unlock()
		defer func() {
			state.mu.Lock()
			state.suppressSettle--
			state.mu.Unlock()
		}()
	}
	changed := false
	var markdown string
	var tree *pmdoc.Node
	var updates [][]byte
	var mutateErr error
	err := s.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		var unsubscribe func()
		if joinedTransaction {
			unsubscribe = doc.OnUpdate(func(update []byte, _ any) {
				updates = append(updates, append([]byte(nil), update...))
			})
			defer unsubscribe()
		}
		changed, mutateErr = mutate(doc, transact)
		if mutateErr != nil || !changed {
			return
		}
		tree, mutateErr = treeOf(doc)
		if mutateErr != nil {
			return
		}
		markdown, mutateErr = renderTree(tree)
	})
	if mutateErr != nil {
		if joinedTransaction {
			s.cancelSuppressedPersistence(artifactID, slot)
		}
		return mutateErr
	}
	if !joinedTransaction {
		return err
	}
	if err != nil || !changed {
		s.cancelSuppressedPersistence(artifactID, slot)
		return err
	}
	update, err := mergeUpdates(updates)
	if err != nil {
		s.cancelSuppressedPersistence(artifactID, slot)
		return err
	}
	s.finishSuppressedPersistence(slot, update)
	if _, err := s.persistence.AppendUpdateTx(ctx, tx, artifactID, update); err != nil {
		s.failRoom(artifactID, err)
		return fmt.Errorf("append transactional live document update: %w", err)
	}
	if _, err := s.writeVersionTx(ctx, tx, artifactID, markdown, tree, nil); err != nil {
		return fmt.Errorf("reresolve transactional document anchors: %w", err)
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
func (s *Service) SeedText(ctx context.Context, tx pgx.Tx, artifactID, markdown string) (string, error) {
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
	if _, err := s.persistence.AppendUpdateTx(ctx, tx, artifactID, crdt.EncodeStateAsUpdateV1(doc, nil)); err != nil {
		return "", fmt.Errorf("seed live document: %w", err)
	}
	return canonical, nil
}

// ReplaceText replaces the entire live document tree so connected clients
// receive document uploads as a regular server-side transaction.
func (s *Service) ReplaceText(ctx context.Context, artifactID, markdown string, actor model.Actor) (string, error) {
	var canonical string
	var unchanged bool
	err := s.applyLive(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) (bool, error) {
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
		s.recordActor(artifactID, actor)
		var updateErr error
		transact(func(transaction *crdt.Transaction) {
			updateErr = pmdoc.Update(transaction, fragment, target)
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

// SnapshotVersion returns the current immutable version, adding an unnamed
// version only when the live text has diverged since the previous one.
func (s *Service) SnapshotVersion(ctx context.Context, tx pgx.Tx, artifactID string, actor model.Actor) (model.Version, bool, error) {
	tree, markdown, capture, authors, err := s.captureLiveTextAndAuthors(ctx, artifactID, nil)
	if err != nil {
		return model.Version{}, false, err
	}
	latest, err := latestVersion(ctx, tx, artifactID)
	if err != nil {
		return model.Version{}, false, err
	}
	if latest.markdown == markdown {
		return latest.Version, false, nil
	}
	capture.authors[actorKey(actor)] = actor
	authors = actorSlice(capture.authors)
	version, err := s.writeVersionTx(ctx, tx, artifactID, markdown, tree, &versionWrite{
		authors: authors,
		capture: &capture,
	})
	if err != nil {
		return model.Version{}, false, err
	}
	return version, true, nil
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

// ApplyOps resolves every requested operation against the document locked by
// Server.Apply, then applies the complete plan in one transaction.
func (s *Service) ApplyOps(ctx context.Context, artifactID string, ops []model.EditOp, actor model.Actor) (int, error) {
	if len(ops) == 0 {
		return 0, nil
	}
	err := s.applyLive(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) (bool, error) {
		fragment := doc.GetXmlFragment(fragmentName)
		tree, err := treeOf(doc)
		if err != nil {
			return false, err
		}
		next, err := applyOperations(tree, ops)
		if err != nil {
			return false, err
		}
		s.recordActor(artifactID, actor)
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
		return 0, fmt.Errorf("apply live document operations: %w", err)
	}
	return len(ops), nil
}

// ApplyReplace resolves a stored markdown anchor against the document locked
// by Server.Apply and replaces the corresponding ProseMirror range.
func (s *Service) ApplyReplace(ctx context.Context, artifactID string, anchor model.Anchor, with string, actor model.Actor) error {
	err := s.applyLive(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) (bool, error) {
		fragment := doc.GetXmlFragment(fragmentName)
		tree, err := treeOf(doc)
		if err != nil {
			return false, err
		}
		markdown, positions, err := pmdoc.Render(tree)
		if err != nil {
			if errors.Is(err, pmdoc.ErrSchema) {
				return false, fmt.Errorf("%w: %v", ErrDocSchema, err)
			}
			return false, err
		}
		resolved := text.Reresolve(markdown, anchor)
		if resolved.Orphaned {
			return false, text.ErrTargetNotFound
		}
		target, err := inlineAware(with)
		if err != nil {
			return false, err
		}
		next, err := pmdoc.Splice(tree, pmdoc.Range{
			From: positions.ToPM(resolved.From),
			To:   positions.ToPM(resolved.To),
		}, target)
		if err != nil {
			return false, err
		}
		s.recordActor(artifactID, actor)
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
		return fmt.Errorf("apply live document replacement: %w", err)
	}
	return nil
}

// NamedVersion records the live text as a deliberately named immutable version.
// NamedVersion records the live document as a deliberately named immutable version.
func (s *Service) NamedVersion(ctx context.Context, artifactID, summary string, actor model.Actor) (model.Version, error) {
	tree, markdown, capture, authors, err := s.captureLiveTextAndAuthors(ctx, artifactID, &actor)
	if err != nil {
		return model.Version{}, err
	}
	_, joinedTransaction := txFromContext(ctx)
	var version model.Version
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		var open bool
		if err := tx.QueryRow(ctx, `
			select i.closed_at is null
			from artifacts a join issues i on i.key = a.issue_key
			where a.id = $1 for update
		`, artifactID).Scan(&open); err != nil {
			return fmt.Errorf("lock document artifact: %w", err)
		}
		if !open {
			return ErrIssueClosed
		}

		version, err = s.writeVersionTx(ctx, tx, artifactID, markdown, tree, &versionWrite{
			named:   true,
			summary: new(summary),
			authors: authors,
			capture: &capture,
		})
		return err
	})
	if err != nil {
		s.discardPendingVersion(artifactID, version)
		return model.Version{}, err
	}
	if !joinedTransaction {
		s.CommitVersion(artifactID, version)
	}
	return version, nil
}

func (s *Service) reresolveAnchors(ctx context.Context, tx pgx.Tx, artifactID, markdown string) error {
	for _, target := range []struct {
		table string
		where string
	}{
		{table: "asks", where: "state = 'open'"},
		{table: "comments", where: "not resolved"},
	} {
		rows, err := tx.Query(ctx, fmt.Sprintf(`
			select id::text, anchor from %s
			where anchor is not null and anchor->>'artifact_id' = $1 and %s
		`, target.table, target.where), artifactID)
		if err != nil {
			return fmt.Errorf("list %s anchors: %w", target.table, err)
		}
		type row struct {
			id      string
			encoded []byte
		}
		var anchored []row
		for rows.Next() {
			var anchor row
			if err := rows.Scan(&anchor.id, &anchor.encoded); err != nil {
				rows.Close()
				return fmt.Errorf("scan %s anchor: %w", target.table, err)
			}
			anchored = append(anchored, anchor)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("list %s anchors: %w", target.table, err)
		}
		rows.Close()
		for _, row := range anchored {
			var anchor model.Anchor
			if err := json.Unmarshal(row.encoded, &anchor); err != nil {
				return fmt.Errorf("decode %s anchor: %w", target.table, err)
			}
			encoded, err := json.Marshal(text.Reresolve(markdown, anchor))
			if err != nil {
				return fmt.Errorf("encode %s anchor: %w", target.table, err)
			}
			if _, err := tx.Exec(ctx, fmt.Sprintf(`update %s set anchor = $2 where id = $1`, target.table), row.id, encoded); err != nil {
				return fmt.Errorf("update %s anchor: %w", target.table, err)
			}
		}
	}
	return nil
}

func (s *Service) recordActor(room string, actor model.Actor) {
	state := s.room(room)
	state.mu.Lock()
	state.pending[actorKey(actor)] = actor
	state.mu.Unlock()
}

func (s *Service) captureLiveTextAndAuthors(ctx context.Context, room string, actor *model.Actor) (*pmdoc.Node, string, versionPending, []model.Actor, error) {
	if s.srv.GetDoc(room) == nil {
		err := s.srv.Apply(ctx, room, func(_ *crdt.Doc, _ func(func(*crdt.Transaction))) {})
		if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			return nil, "", versionPending{}, nil, fmt.Errorf("warm live document: %w", err)
		}
	}

	state := s.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
	doc := s.srv.GetDoc(room)
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
	capture, authors := captureAuthors(state, actor)
	return tree, markdown, capture, authors, nil
}

func captureAuthors(state *roomState, actor *model.Actor) (versionPending, []model.Actor) {
	authors := make(map[string]model.Actor, len(state.pending)+1)
	for key, pendingActor := range state.pending {
		authors[key] = pendingActor
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
	markdown string
}, error) {
	var version struct {
		model.Version
		markdown string
	}
	var authors []byte
	if err := tx.QueryRow(ctx, `
		select number, named, summary, authors, created_at, markdown
		from artifact_versions where artifact_id = $1
		order by number desc limit 1
	`, artifactID).Scan(&version.Number, &version.Named, &version.Summary, &authors, &version.CreatedAt, &version.markdown); err != nil {
		return version, fmt.Errorf("read latest document version: %w", err)
	}
	if err := json.Unmarshal(authors, &version.Authors); err != nil {
		return version, fmt.Errorf("decode latest document version authors: %w", err)
	}
	return version, nil
}

// writeVersionTx is the only path that changes the durable version protocol:
// version writes index references, re-resolve anchors, and retain its author
// capture until the enclosing transaction commits. A nil write records no
// version but keeps a transactional tree mutation's anchors in the same path.
func (s *Service) writeVersionTx(ctx context.Context, tx pgx.Tx, artifactID, markdown string, tree *pmdoc.Node, write *versionWrite) (model.Version, error) {
	if write == nil {
		return model.Version{}, s.reresolveAnchors(ctx, tx, artifactID, markdown)
	}

	encodedAuthors, err := json.Marshal(write.authors)
	if err != nil {
		return model.Version{}, fmt.Errorf("encode document version authors: %w", err)
	}
	var version model.Version
	var authorsRaw []byte
	if err := tx.QueryRow(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors, named, summary)
		select $1, coalesce(max(number), 0) + 1, $2, $3, $4, $5
		from artifact_versions where artifact_id = $1
		returning number, named, summary, authors, created_at
	`, artifactID, markdown, encodedAuthors, write.named, write.summary).Scan(
		&version.Number, &version.Named, &version.Summary, &authorsRaw, &version.CreatedAt,
	); err != nil {
		return model.Version{}, fmt.Errorf("write document version: %w", err)
	}
	if err := json.Unmarshal(authorsRaw, &version.Authors); err != nil {
		return model.Version{}, fmt.Errorf("decode document version authors: %w", err)
	}
	if err := s.indexDocumentReferences(ctx, tx, artifactID, markdown); err != nil {
		return model.Version{}, err
	}
	if err := s.reresolveAnchors(ctx, tx, artifactID, markdown); err != nil {
		return model.Version{}, err
	}
	if write.capture != nil {
		s.rememberPendingVersion(artifactID, version, *write.capture)
	}
	return version, nil
}

func (s *Service) indexDocumentReferences(ctx context.Context, tx pgx.Tx, artifactID, markdown string) error {
	if _, err := tx.Exec(ctx, `delete from refs where from_kind = 'artifact' and from_id = $1`, artifactID); err != nil {
		return fmt.Errorf("clear document references: %w", err)
	}
	for _, ref := range text.Extract(markdown, s.serverURL) {
		if ref.Kind == "url" {
			continue
		}
		toID := ref.ID
		if ref.Kind == "artifact" {
			toID = ref.IssueKey + "/" + ref.ID
		}
		if _, err := tx.Exec(ctx, `
			insert into refs (from_kind, from_id, to_kind, to_id)
			values ('artifact', $1, $2, $3)
			on conflict do nothing
		`, artifactID, ref.Kind, toID); err != nil {
			return fmt.Errorf("write document reference: %w", err)
		}
	}
	return nil
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
