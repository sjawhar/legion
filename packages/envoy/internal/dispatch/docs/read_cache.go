package docs

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sync/singleflight"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// DocumentStamp names one stored state of a document: Version is the head Load folds the stored
// updates through (PgVersioned.head: a pending prune's ceiling, else the newest update's version,
// else 0, a document with no stored update), and Row is the id of the transaction that wrote the
// update row at that version, Postgres's xmin ("" where no row stands there).
//
// The version alone does not name a state: a prune deletes the updates past its target, and the
// next append takes the number the first of them had (appendUpdateTxClass), with other content.
// The row's transaction does: every writer of doc_updates either adds a row above the head (an
// append, a rebuild's seed), deletes every row (a rebuild, Delete), deletes rows above a target and
// leaves the target's row as it was (a prune), or rewrites the row at the head with the state the
// rows it deletes made (a compaction keeping one row), which gives it a new transaction id over the
// same state. So two reads of one stamp read one state, on any task sharing the database. xmin is
// 32 bits and wraps: a false match needs a row re-appended at the same version exactly 2^32
// transactions after the one it replaced, while a reading of the old row is still held.
type DocumentStamp struct {
	Version int64
	Row     string
}

// readDocumentStamp is the room's DocumentStamp, read through q in one statement, so the version
// and the xmin of the row standing there are of one snapshot: the head as head reads it
// (headVersion), then the row at it by its primary key.
func readDocumentStamp(ctx context.Context, q Queryer, room string) (DocumentStamp, error) {
	var stamp DocumentStamp
	if err := q.QueryRow(ctx, `
		select h.version, coalesce(u.xmin::text, '')
		from (select `+headVersion+` as version) h
		left join doc_updates u on u.artifact_id = $1 and u.version = h.version
	`, room).Scan(&stamp.Version, &stamp.Row); err != nil {
		return DocumentStamp{}, fmt.Errorf("read document stamp: %w", err)
	}
	return stamp, nil
}

// documentReadBudget is the most weight (documentRead.weight) the cold reads' cache holds:
// production's task has 4,096 MiB, and one request may raise it by at most 256 MiB
// (cmd/dispatch/memory_test.go). An entry heavier than an eighth of it is never stored.
const documentReadBudget = 256 << 20

// coldReadTimeout bounds a cold read's fold of the stored history (LoadDocument), which belongs to
// the document rather than to the request that started it, so no request's end cancels it.
var coldReadTimeout = 60 * time.Second

// documentRead is everything the four reads of a document no room holds answer (Text,
// TextWithToken, TextWithBlocks and BlockPath), rendered from one tree of one stored state, the
// state stamp names. Nothing writes it once it is built: a read hands out its strings, a copy of
// its blocks (a caller writes References into its slice) and a copy of a path's entries.
type documentRead struct {
	artifactID string
	stamp      DocumentStamp
	markdown   string
	token      string
	blocks     []model.ArtifactBlock
	paths      map[string]pmdoc.BlockPath
	weight     int64
}

// documentReads holds a documentRead for each document a cold read rendered, the most recently
// read first, while their weights together stay within budget: one entry for a document, a
// rendering of a newer stamp replacing the older. A read uses an entry only while the store's stamp
// is still its stamp, so an entry another task's write has moved past is never served.
type documentReads struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List
	bytes   int64
	budget  int64
	// flights folds each (document, stamp) once however many reads miss it at once.
	flights singleflight.Group
}

func newDocumentReads(budget int64) *documentReads {
	return &documentReads{entries: make(map[string]*list.Element), order: list.New(), budget: budget}
}

// get is artifactID's rendering when it was rendered from the state stamp names, which it marks the
// most recently read; nil otherwise.
func (r *documentReads) get(artifactID string, stamp DocumentStamp) *documentRead {
	r.mu.Lock()
	defer r.mu.Unlock()
	element, ok := r.entries[artifactID]
	if !ok {
		return nil
	}
	read := element.Value.(*documentRead)
	if read.stamp != stamp {
		return nil
	}
	r.order.MoveToFront(element)
	return read
}

// put stores read as its document's rendering in place of any it held, then drops the least
// recently read renderings until the cache's weight is within its budget. A rendering heavier than
// an eighth of the budget is not stored, and the one it replaces goes. A rendering of an older
// stamp than the one it replaces, from a slow fold, costs the next read a miss, never a stale read:
// get compares stamps.
func (r *documentReads) put(read *documentRead) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if element, ok := r.entries[read.artifactID]; ok {
		r.removeLocked(element)
	}
	if read.weight > r.budget/8 {
		return
	}
	r.entries[read.artifactID] = r.order.PushFront(read)
	r.bytes += read.weight
	for r.bytes > r.budget {
		r.removeLocked(r.order.Back())
	}
}

func (r *documentReads) removeLocked(element *list.Element) {
	read := r.order.Remove(element).(*documentRead)
	delete(r.entries, read.artifactID)
	r.bytes -= read.weight
}

// Overheads documentRead.weight counts beside the bytes its strings and slices hold: the entry's
// own struct with its list element and map entry, and each path's map entry.
const (
	readEntryOverhead = int64(unsafe.Sizeof(documentRead{})) + 128
	readPathOverhead  = 64
)

// measure is the bytes the rendering's strings and slices hold, and its fixed overheads. A string
// several of them share, such as a block id that is a path's key and its block's id, is counted
// where each holds it. A table's position, which every block in one cell shares, is counted for
// each, and a row's cells' opening words once, on the row's own path.
func (read *documentRead) measure() int64 {
	weight := readEntryOverhead + int64(len(read.artifactID)+len(read.stamp.Row)+len(read.markdown)+len(read.token))
	for _, block := range read.blocks {
		weight += int64(unsafe.Sizeof(block)) + int64(len(block.ID)+len(block.Type)+len(block.Token)) +
			int64(len(block.DescendantIDs))*int64(unsafe.Sizeof(""))
	}
	for id, path := range read.paths {
		weight += readPathOverhead + int64(len(id)+len(path.Type)) + int64(len(path.Path))*int64(unsafe.Sizeof(pmdoc.BlockPathEntry{}))
		if path.Table == nil {
			continue
		}
		weight += int64(unsafe.Sizeof(pmdoc.TablePosition{}))
		if path.Table.Row != nil && path.Table.Column == nil {
			for _, cell := range path.Table.Cells {
				weight += int64(unsafe.Sizeof(cell)) + int64(len(cell))
			}
		}
	}
	return weight
}

// buildDocumentRead renders tree, the document as of stamp, into everything the four reads answer,
// with one render: an error when any of them refuses the tree, a document outside the schema, as
// the read computing it from the tree would.
func buildDocumentRead(artifactID string, stamp DocumentStamp, tree *pmdoc.Node) (*documentRead, error) {
	markdown, blocks, err := documentBlocks(tree)
	if err != nil {
		return nil, err
	}
	token, err := nodeToken(tree)
	if err != nil {
		return nil, err
	}
	paths, err := pmdoc.BlockPaths(tree)
	if err != nil {
		return nil, err
	}
	read := &documentRead{artifactID: artifactID, stamp: stamp, markdown: markdown, token: token, blocks: blocks, paths: paths}
	read.weight = read.measure()
	return read, nil
}

// coldFold is a fold of a document's stored history: read, its rendering, which it stored; tree,
// the tree it was rendered from, which a read computes its answer from when read is nil because
// the tree is outside what the rendering takes; treeErr, why the stored document has no tree. All
// three are zero for a document with no stored state.
type coldFold struct {
	read    *documentRead
	tree    *pmdoc.Node
	treeErr error
}

// coldRead is the read of artifactID when neither a transaction's fork nor a resident room holds
// it: the rendering of the stored state the store's stamp names now, from the cache, or else from
// one fold of the history (foldDocument) that every read missing that state at once shares. It
// returns the rendering, or a nil rendering and the tree when the tree is outside what the
// rendering takes, or neither for a document with no stored state.
//
// The fold is the document's: it runs on a context no request's end cancels, bounded by the
// cache's timeout, so a read whose request ends gets its own context's error at once and leaves
// the fold to the others. Only the read's own context decides that it ended. A fold that fails
// fails the room, as a failed room load does, and reaches each read as ErrServiceUnavailable,
// unless the history does not decode, which is ErrDocumentUnloadable, the state a rebuild repairs.
func (s *Service) coldRead(ctx context.Context, artifactID string) (*documentRead, *pmdoc.Node, error) {
	stamp, err := s.persistence.DocumentStamp(ctx, artifactID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, ctxErr
		}
		s.failRoom(artifactID, err)
		return nil, nil, fmt.Errorf("%w: %w", ErrServiceUnavailable, err)
	}
	if stamp.Version == 0 {
		return nil, nil, nil
	}
	if read := s.reads.get(artifactID, stamp); read != nil {
		return read, nil, nil
	}
	key := artifactID + "@" + strconv.FormatInt(stamp.Version, 10) + "/" + stamp.Row
	flight := s.reads.flights.DoChan(key, func() (any, error) {
		fold, err := s.foldDocument(ctx, artifactID)
		if err != nil {
			s.failRoom(artifactID, err)
		}
		return fold, err
	})
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case result := <-flight:
		if result.Err != nil {
			if errors.Is(result.Err, ErrDocumentUnloadable) {
				return nil, nil, result.Err
			}
			return nil, nil, fmt.Errorf("%w: %w", ErrServiceUnavailable, result.Err)
		}
		fold := result.Val.(coldFold)
		return fold.read, fold.tree, fold.treeErr
	}
}

// foldDocument loads artifactID's stored history decoded once (LoadDocument), reads its tree and
// renders it, storing the rendering under the stamp the load read in its own transaction, never
// the one read before the miss, so a write between the two cannot store old content under a new
// stamp. Its error is the load's, or a panic recovered from any of it: singleflight re-panics an
// unrecovered panic of a call it has waiters for in a goroutine nothing can recover, where a read
// panicking on the request's own goroutine would fail that one request.
func (s *Service) foldDocument(ctx context.Context, artifactID string) (fold coldFold, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("cold document read panicked: %v\n%s", recovered, debug.Stack())
			slog.Error("dispatch: cold document read panicked", "room", artifactID, "error", err)
		}
	}()
	foldCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), coldReadTimeout)
	defer cancel()
	loaded, err := s.persistence.LoadDocument(foldCtx, artifactID)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && errors.Is(foldCtx.Err(), context.DeadlineExceeded) {
			return coldFold{}, fmt.Errorf("cold document read timed out after %s", coldReadTimeout)
		}
		return coldFold{}, err
	}
	if loaded.Doc == nil {
		return coldFold{}, nil
	}
	tree, err := treeOf(loaded.Doc)
	if err != nil {
		return coldFold{treeErr: err}, nil
	}
	read, err := buildDocumentRead(artifactID, loaded.Stamp, tree)
	if err != nil {
		return coldFold{tree: tree}, nil
	}
	s.reads.put(read)
	return coldFold{read: read, tree: tree}, nil
}
