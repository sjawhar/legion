package docs

import (
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"weak"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// renderedReplica is the copy of a room's document its update observer renders and the room's
// reads walk (readLive). ygo fires the observer after the transaction has released the document's
// lock (sjawhar/ygo v1.51.3-sami.4, crdt/doc.go, transactInternal), and a walk of the live tree
// takes no lock (crdt/yxml.go), so a render or read of the live tree can walk it while another
// transaction writes it: a torn walk reads a healthy document as one outside the schema, logs a
// false WARN and counts the update as a content change. Only a holder of mu writes or walks the
// replica.
//
// The replica moves only through the room's commits, one at a time and in the order they committed
// (crdt.UpdateLog). ygo appends each committed transaction's update to the room's log under the
// document's lock, before any observer of that transaction runs, and the update observer that holds
// mu drains the log and classifies each update it applies against the replica as that update left
// it. Observers of two transactions run concurrently and in either order, so whichever runs first
// classifies both, each against the room as the commit before it left it. Catching the replica up
// to the live document instead took in every commit since the last one rendered: a published
// write's observer that ran first counted a browser's edit in its own change, which credits no
// one, and the browser's own observer then found nothing changed and credited no one either. The
// update an observer is handed cannot stand in for the log: each update carries the room's whole
// delete set, so the later update applied first deletes what the earlier one replaced while its
// own insertions wait for the earlier one's - a tree no transaction left.
//
// A logged commit is two parts, and the replica takes both: the update ygo handed the room's
// observers (crdt.LoggedUpdate.Update), what the transaction integrated, and what it parked
// (Parked) - the structs and deletions waiting for something the document lacks, which the room
// holds and the update leaves out, as Yjs's update event does. So the replica is the room's copy,
// what the room holds parked included, and when the commit that fills a gap arrives the replica
// integrates what waited behind it just as the room does. The copy would converge on the room's
// tree without the parked part - a logged update is the room's whole store past the commit's
// beginning with the room's whole delete set, so the commit that fills the gap carries what it
// integrates from the queue too - but until then the replica would not be the room: what a reader
// takes from it (readLive) and its encoded state, which carries parked structs
// (crdt.EncodeStateAsUpdateV1), would both leave out what the room is holding.
//
// Applying the parked part integrates nothing, so it renders nothing new: its transaction names
// no changed type (crdt.Transaction.ChangedTypes), so it touches no block of the record the
// update is classified against (noteChangedTypes, touchedBlocks), which observe resets before the
// update's own apply and again once the update is classified. Both transactions therefore record
// into the one update's record, which is what the classification is of: the room's state as the
// commit left it, what it parked included. One commit is consumed, classified and recorded once,
// whether it parked anything or not (observe).
//
// The observer renders the replica after an update only when it can neither know the update is a
// margin write's publish into the room its fork was warm-built from (liveWriteOrigin.marginOnly
// says why that changes no text), nor reuse the rendering the published live write that made the
// update made of it, nor know the update was already in the room (changedBy). All three are exact.
// The write's rendering (publishedRendering) is taken only on the replica, epoch and foreign
// count the operation's own fork read found, the replica's before that read's encode: a replica
// still at both has consumed, through that read, every commit the fork was read from, and since
// then only published live writes' commits, which under the writer slot are this write's earlier
// updates, which the fork holds too - so the replica after the update is the fork after the
// operation, and rendering is deterministic over one state. ygo puts the
// room's whole delete set in every update, so an update means that only on the room instance the
// fork was read from: a cold fork, read from the store, records no rendering, and a room reloaded
// since the read has a replica of its own at another epoch. An update already in the room
// (liveWriteOrigin.inRoom) has a version at or below the head its room instance's load folded
// (roomInstance.loadedHead), so the loaded document held it, and the replica, which starts from the
// room as of its first logged commit, holds the load: applying it changed nothing. Nothing makes
// that history other than what the load folded: RebuildDocument, the one writer that replaces a
// document's history, runs only on a history that cannot load (rebuild.go), and the write's own
// refuseUndecodable proved the history through its version decodes; PruneAfter has no production
// caller. A replica outside the schema (a nil markdown) renders the update, so such an update
// still logs and counts as changed.
//
// Where the observer does measure an update itself (updateChangesMarkdown), it renders no more of
// the replica than the update can have changed, and each of its three paths is exact too. The
// replica records, for the update it applies, the top-level blocks of its document fragment the
// update changed anything in (noteChangedTypes, touchedBlocks): a change anywhere in a block
// integrates or deletes an item in a type that block contains, and a block inserted, deleted or
// moved changes the fragment itself, which is recorded as an update to render whole. The
// rendering reads nothing but the fragment. So an update that touched
// no block and not the fragment left the rendering as it was (untouched); one that touched some
// blocks, and not the fragment, left every other block's tree as it was, and pmdoc.Layout.Splice
// renders just those blocks into the markdown, answering exactly what a whole render answers or
// declining where a guard says the rest of the document could read them differently
// (incremental); anything else is rendered whole, as it always was (whole).
//
// The replica starts from the state the log records as of just before the room's first transaction
// since its load, so a room that is only read holds no replica. The load rendered that very state
// already (onLoadDocument), and hands that rendering, with its layout, to the replica's first run
// (replicaRendering), which takes it in place of rendering the base again (start says why it is
// exact). The replica holds it only through a weak pointer, so it adds no resident memory: the
// next collection takes it, and a first run that starts after that renders the base as every run
// after a recover does.
type renderedReplica struct {
	mu sync.Mutex
	// waiting counts the update observers waiting for mu (lockForUpdate).
	waiting atomic.Int32
	log     *crdt.UpdateLog
	doc     *crdt.Doc
	// fragment is doc's document fragment, the one shared type the rendering reads; nil with doc.
	fragment *crdt.YXmlFragment
	// rendering is the room's rendering as the replica's latest update left it, or as the log's
	// base rendered before any (replicaRendering): the markdown and the pmdoc.Layout recorded
	// with it. It is nil while the document is outside the schema, and replaced whole, never
	// written in place, so a holder of one the replica has let go of keeps a markdown and a
	// layout that go together (renderingAt).
	rendering *replicaRendering
	// touched records the top-level blocks of fragment the update being applied changed, and
	// whether it is to be rendered whole (touchedBlocks). noteChangedTypes records into it as
	// doc's transactions commit (start), and observe resets it before each update it applies and
	// again once that update is classified, so no block reference outlives the update's
	// classification - on the recover path too, which resets it as it drops the document.
	touched touchedBlocks
	// epoch names the replica's run from one state it started from: keepReplica draws it from
	// replicaEpochs when it makes the replica, and recover draws a fresh one whenever the replica
	// starts again, so it is unique in the process. No replica of another room instance - the one
	// a reload leaves behind has a replica of its own - and no run of this replica before a
	// recover ever stands at the epoch this one does. It is written under mu and read without it.
	epoch atomic.Uint64
	// foreign counts the logged updates the replica has consumed - applied, or handed to recover -
	// whose origin is not a published live write's (*liveWriteOrigin). observe adds to it while it
	// holds mu, update by update in commit order (consumed), and anyone may read it without mu. A
	// reader that reads epoch and foreign before it reads the room's state at some moment, and
	// later finds both unchanged on a replica that holds every logged commit (hold), knows that
	// every commit the replica consumed since that moment was a published live write's: under the
	// writer slot, only the reader's own write's earlier updates. A race goes the safe way: a
	// foreign commit the replica consumes after the read changes the count.
	foreign atomic.Uint64
	// paths is the service's count of the way each rendering was had (renderPathCounts), which
	// keepReplica hands over: the replica counts the paths its update observer takes.
	paths *renderPathCounts
	// loaded is the load's rendering of the room (replicaRendering), held weakly, which
	// keepReplica hands over and the replica's first run takes (start). start and recover clear
	// it, under mu, so no run but the first ever takes it.
	loaded weak.Pointer[replicaRendering]
}

// replicaRendering is one rendering of a room's document: the markdown, and the pmdoc.Layout the
// same render recorded of it, valid only with that markdown. The two are one value, so a
// rendering never stands without the layout it was recorded with: the replica keeps one
// (renderedReplica.rendering), a room's load hands the replica one of the state it starts from
// (onLoadDocument, renderedReplica.start), and a published live write's operation offers the
// room's update observer one of the state its update leaves (publishedRendering). A rendering is
// never written in place once made, so every holder of one reads the markdown and the layout of
// one render. Only a rendering no observer ever takes carries no layout: a margin write's
// publish, whose markdown the observer compares and nothing more (renderedReplica.changedBy).
type replicaRendering struct {
	markdown string
	layout   *pmdoc.Layout
}

// replicaEpochs is where every renderedReplica.epoch is drawn from, so no two replicas in the
// process, and no two runs of one replica either side of a recover, ever stand at one epoch.
var replicaEpochs atomic.Uint64

// renderPathCounts counts, one counter per path, how the renderings a room's update observer, its
// replica's start and a document write need were had, so a test can assert that the path it means
// to exercise ran rather than pass by rendering the replica, and a measurement build can report how
// often each one fires.
// The service keeps one (Service.renderPaths) and hands each replica a pointer to it (keepReplica).
// Nothing in the service reads them.
type renderPathCounts struct {
	// observerReused counts the logged updates the update observer classified with the rendering
	// a published live write made of the very state the update left, in place of one of its own.
	observerReused atomic.Uint64
	// observerInRoom counts the logged updates the update observer classified as unchanged without
	// rendering, since the room's load already held them (roomInstance.loadedHead).
	observerInRoom atomic.Uint64
	// observerRendered counts the logged updates the update observer applied and then rendered the
	// replica whole for (updateChangesMarkdown's whole path).
	observerRendered atomic.Uint64
	// observerMarginOnly counts the logged updates the update observer classified as unchanged
	// without rendering, since they were margin writes' publishes into the room instance their
	// fork was warm-built from (liveWriteOrigin.marginOnly).
	observerMarginOnly atomic.Uint64
	// observerIncremental counts the logged updates the update observer applied and then rendered
	// only the top-level blocks they changed for, splicing them into the replica's markdown
	// (updateChangesMarkdown's incremental path).
	observerIncremental atomic.Uint64
	// observerUntouched counts the logged updates the update observer classified as unchanged
	// without rendering, since they changed nothing under the document's fragment
	// (updateChangesMarkdown's untouched path).
	observerUntouched atomic.Uint64
	// beforeFromReplica counts the operations whose rendering of their fork before they applied was
	// the room replica's, which held the very state the fork was read from.
	beforeFromReplica atomic.Uint64
	// beforeFromWrite counts the operations whose rendering of the fork before them was the one the
	// write's previous operation made of the fork it left.
	beforeFromWrite atomic.Uint64
	// beforeRendered counts the operations that rendered their fork before applying.
	beforeRendered atomic.Uint64
	// catchUpFromLoad counts the replica runs that took the load's rendering of their base
	// (replicaRendering) in place of rendering it (renderedReplica.start).
	catchUpFromLoad atomic.Uint64
	// catchUpRendered counts the replica runs that rendered the base they started from: a first
	// run whose load's rendering was collected or never made, and every run after a recover.
	catchUpRendered atomic.Uint64
}

// maxTouchedBlocks is the most top-level blocks one update may change a type in and still be
// rendered block by block: touchedBlocks holds no more, so an update's record stays a few words
// whatever it changed, and an update past it is rendered whole.
const maxTouchedBlocks = 64

// touchedBlocks is what the update a replica applies touched under its document fragment: blocks
// lists the top-level blocks it changed something in, each once, and whole says that it changed
// the fragment itself, touched more blocks than blocks holds (maxTouchedBlocks), or changed
// something no walk could place in a block, so the update is rendered whole and blocks no
// longer matters. noteChangedTypes records into it; updateChangesMarkdown and
// spliceTouched read it, under the replica's mu.
type touchedBlocks struct {
	blocks []crdt.SharedType
	whole  bool
}

// touch records block, a top-level block of the fragment, as changed by the update, unless the
// update is already rendered whole; a block past maxTouchedBlocks renders it whole.
func (t *touchedBlocks) touch(block crdt.SharedType) {
	if t.whole || slices.Contains(t.blocks, block) {
		return
	}
	if len(t.blocks) == maxTouchedBlocks {
		t.whole = true
		return
	}
	t.blocks = append(t.blocks, block)
}

// reset forgets what the last update touched, dropping its references to the blocks and keeping
// the list's array for the next update.
func (t *touchedBlocks) reset() {
	clear(t.blocks)
	t.blocks, t.whole = t.blocks[:0], false
}

// lockForUpdate takes mu for the update observer. A read only tries mu, and takes it only while
// no observer waits for it, so the observer waits at most for the one read already walking the
// replica: a read that took mu from under a waiting observer would make it wait for a second walk.
func (r *renderedReplica) lockForUpdate() {
	r.waiting.Add(1)
	r.mu.Lock()
	r.waiting.Add(-1)
}

// observe is the update observer's turn with the replica: it waits for mu (lockForUpdate), drains
// the room's update log, and moves the replica through each update the log hands over, in commit
// order, classifying each as it takes it (changedBy). It calls classify, when given, with each
// update, whether that update changed the room's rendered markdown, and asks, the ask blocks it
// left, when it did, still holding mu, so the room's own record of what each update introduced
// moves forward in commit order. The observer is the one holder of mu that waits for it, and the
// one that drains the log: every committed update is classified exactly once, by whichever
// observer reaches the log first.
//
// Each update is applied in the two parts the log hands over: Update, the structs and deletions
// the transaction integrated, and then what it parked (crdt.LoggedUpdate.Parked), nil when it
// parked nothing. Either failing leaves the replica holding a state the room never had, so it
// goes to recover with the rest of the drain. The two are one commit: it is consumed, classified
// and recorded once, and renderedReplica says why the second apply changes no rendering.
//
// asks answers the id of every ask block the update left. It finds them only when called, so a
// classify that never reads them - an identity repair's, which returns before it does - walks
// nothing, and it is valid only during classify's call, since it reads the replica, which only a
// holder of mu may. classify is handed nil asks when there are no ask blocks to record: the update
// changed no rendered markdown, was handed to recover, or left a document that does not render.
// An asks that answers nil says the same of an update it was called for: it could not read the
// document it had to walk, and logged that, so its caller records nothing of the update's ask
// blocks rather than take an empty set for the room's own (observeAskBlocksForUpdate).
func (r *renderedReplica) observe(room string, classify func(update crdt.LoggedUpdate, changed bool, asks func() map[string]struct{})) {
	r.lockForUpdate()
	defer r.mu.Unlock()
	base, updates := r.log.Drain()
	if base != nil {
		r.start(room, base)
	}
	for i, update := range updates {
		if r.doc == nil {
			r.recover(room, updates[i:], classify)
			return
		}
		r.touched.reset()
		if err := crdt.ApplyUpdateV1(r.doc, update.Update, nil); err != nil {
			slog.Error("dispatch: apply a committed update to the document's rendered copy", "room", room, "error", err)
			r.recover(room, updates[i:], classify)
			return
		}
		if update.Parked != nil {
			if err := crdt.ApplyUpdateV1(r.doc, update.Parked, nil); err != nil {
				slog.Error("dispatch: apply what a committed update parked to the document's rendered copy", "room", room, "error", err)
				r.recover(room, updates[i:], classify)
				return
			}
		}
		published, rendering := r.consumed(update)
		changed, asks := r.changedBy(room, published, rendering)
		if classify != nil {
			classify(update, changed, asks)
		}
		r.touched.reset()
	}
}

// noteChangedTypes records into what txn, a transaction of fragment's document, changed under
// fragment: each type ygo says txn changed (crdt.Transaction.ChangedTypes) is fragment itself, so
// the update is rendered whole, or sits in a top-level block, the type just below fragment on its
// Parent chain, which it records as touched; a chain that ends at another root type is outside
// what the rendering reads, and touches nothing. A type more than pmdoc.MaxTreeDepth levels down
// stands deeper than any node a document in the schema holds, so the walk up from it stops there
// and the update is rendered whole, which is exact for any update: each walk costs at most the
// schema's depth, however deep a peer nested the types it changed. start registers
// it on each document the replica makes, after applying the base, so it runs for the transactions
// of the updates observe applies, under mu.
func noteChangedTypes(txn *crdt.Transaction, fragment *crdt.YXmlFragment, into *touchedBlocks) {
	if into.whole {
		return
	}
	root := crdt.SharedType(fragment)
	for _, changed := range txn.ChangedTypes() {
		if changed == root {
			into.whole = true
			return
		}
		block, parent := changed, changed.Parent()
		for level := 1; parent != nil && parent != root; level++ {
			if level == pmdoc.MaxTreeDepth {
				into.whole = true
				return
			}
			block, parent = parent, parent.Parent()
		}
		if parent == nil {
			continue
		}
		into.touch(block)
		if into.whole {
			return
		}
	}
}

// changedBy classifies the update the replica has just applied and counted (consumed), published
// when a published live write made it, nil otherwise, and rendering that write's operation's
// rendering of it: whether it changed the room's rendered markdown, and the ask blocks it left
// when it did (observe). It measures the update against the replica (updateChangesMarkdown) only
// when nothing else tells. A published live write's update can tell in three ways, tried in this
// order:
//
//   - it is a margin write's publish into the room instance its fork was warm-built from
//     (liveWriteOrigin.marginOnly, which says why that changes no text), so it changed nothing,
//     while the replica renders at all: one outside the schema renders the update, since the
//     render is what says what the document is now;
//   - its operation's rendering (publishedRendering), when that rendering was made on this
//     replica's epoch and foreign count and the replica still stands at both (standsAt): the
//     replica is then the fork the operation rendered, so the rendering is the replica's, and the
//     update changed the markdown exactly when it differs from the replica's before it. The
//     replica takes that rendering, markdown and layout together (replicaRendering). A change the
//     operation did not see as one carries no ask blocks, so it is measured after all;
//   - its room instance's load already held it (liveWriteOrigin.inRoom), so applying it changed
//     nothing, while the replica renders at all: one outside the schema renders the update, so it
//     still logs and counts as changed.
//
// renderedReplica says why the last two are exact. The caller holds mu.
func (r *renderedReplica) changedBy(room string, published *liveWriteOrigin, rendering publishedRendering) (bool, func() map[string]struct{}) {
	if published != nil && published.marginOnly && r.rendering != nil {
		r.paths.observerMarginOnly.Add(1)
		return false, nil
	}
	if published != nil {
		if r.standsAt(rendering.forkReadMark) {
			if rendered := rendering.rendered; rendered != nil {
				if r.rendering != nil && r.rendering.markdown == rendered.markdown {
					r.paths.observerReused.Add(1)
					return false, nil
				}
				if askBlocks := rendering.askBlocks; askBlocks != nil {
					// The operation's rendering is its own value, which the replica takes
					// whole: it holds nothing of the ask block ids beside it.
					r.rendering = rendered
					r.paths.observerReused.Add(1)
					return true, func() map[string]struct{} { return askBlocks }
				}
			}
		} else if published.inRoom && r.rendering != nil {
			r.paths.observerInRoom.Add(1)
			return false, nil
		}
	}
	return r.updateChangesMarkdown(room)
}

// consumed counts update, which the replica has just applied or handed to recover, in foreign
// unless a published live write committed it. For such a write's update it returns the origin and
// takes the operation's rendering out of it (liveWriteOrigin.rendering), so nothing of it - its
// markdown, its layout or its ask blocks - outlives the update's classification; any other update
// gets nil and no rendering. The caller holds mu.
func (r *renderedReplica) consumed(update crdt.LoggedUpdate) (*liveWriteOrigin, publishedRendering) {
	published, _ := update.Origin.(*liveWriteOrigin)
	if published == nil {
		r.foreign.Add(1)
		return nil, publishedRendering{}
	}
	rendering := published.rendering
	published.rendering = publishedRendering{}
	return published, rendering
}

// mark is the replica's epoch and foreign count as of now, read without mu, epoch first: what a
// fork read records just before it encodes the room (warmFork).
func (r *renderedReplica) mark() forkReadMark {
	return forkReadMark{epoch: r.epoch.Load(), foreign: r.foreign.Load(), warm: true}
}

// standsAt reports whether mark is a warm fork read's and the replica still stands at its epoch
// and foreign count: since that read it has consumed only published live writes' commits.
func (r *renderedReplica) standsAt(mark forkReadMark) bool {
	return mark.warm && r.epoch.Load() == mark.epoch && r.foreign.Load() == mark.foreign
}

// renderingAt is the rendered markdown of the state a warm fork read found the room in, which the
// read recorded as mark (warmFork), or nil. It takes the replica as a read does (hold) and answers
// only when the replica holds every logged commit and still stands at mark (standsAt): it then
// holds exactly the state the fork read, so its markdown is the fork's. It answers nil, holding
// nothing, for a replica it cannot take, one that no longer stands at mark, and one outside the
// schema. The pointer is into the replica's own rendering, which no one writes in place: the
// replica replaces the rendering with a new one, never mutates the one it held (replicaRendering),
// so the caller may keep the markdown once mu is released.
func (r *renderedReplica) renderingAt(mark forkReadMark) *string {
	if r.hold() == nil {
		return nil
	}
	defer r.mu.Unlock()
	if !r.standsAt(mark) || r.rendering == nil {
		return nil
	}
	return &r.rendering.markdown
}

// start makes the replica from base, the room's state as of just before the first update its log
// hands over, and takes base's rendering (replicaRendering) as the one that update is
// measured against. It subscribes noteChangedTypes to the new document's transactions once base
// is applied, with that document's own fragment, so what it records is each later update's. A
// base that does not apply leaves no replica (recover).
//
// The replica's first run takes the load's rendering (loaded) as base's rather than render base,
// when the rendering is still there: a weak pointer that no longer answers it, or a load that made
// none (a document outside the schema), leaves the run to render base, as every run after a
// recover does. start clears loaded either way, base applied or not, so no later run takes it.
// The load's rendering is base's exactly:
//
//   - base is the room's whole state as of just before the first transaction to run on it after
//     its log opened: ygo encodes it under the document's lock before that transaction's fn runs,
//     for each open log still waiting for one (sjawhar/ygo v1.51.3-sami.4, crdt/update_log.go,
//     OpenUpdateLog and recordUpdateLogBases; crdt/doc.go, transactInternal), and only a
//     transaction changes the state, so base is the state the log opened on;
//   - the load renders the room's tree before it opens the log (onLoadDocument), and nothing
//     commits on the room in between: ygo hands the room's document to no peer or caller until
//     the load hook returns (provider/websocket/server.go, loadRoom closes the room's ready barrier
//     after it), and the load hands the document to nothing between the render and OpenUpdateLog.
//     So the state the load rendered is the state the log opened on, which is base;
//   - rendering is deterministic over one state, and the load's read of the tree (pmdoc.Read)
//     differs from the one here (pmdoc.ReadForRendering) only in copying the attribute values it
//     reads, which the render does not write.
//
// A run after a recover starts from the base the log records at some later commit
// (crdt.UpdateLog.Reset), which the load never rendered; recover clears loaded too, so even a
// recover before the first run leaves nothing to take. The caller holds mu.
func (r *renderedReplica) start(room string, base []byte) {
	loaded := r.loaded.Value()
	r.loaded = weak.Pointer[replicaRendering]{}
	doc := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(doc, base, nil); err != nil {
		slog.Error("dispatch: make the document's rendered copy", "room", room, "error", err)
		r.doc, r.fragment, r.rendering = nil, nil, nil
		return
	}
	fragment := doc.GetXmlFragment(fragmentName)
	r.doc, r.fragment = doc, fragment
	doc.OnAfterTransaction(func(txn *crdt.Transaction) { noteChangedTypes(txn, fragment, &r.touched) })
	if loaded != nil {
		r.rendering = loaded
		r.paths.catchUpFromLoad.Add(1)
		return
	}
	r.paths.catchUpRendered.Add(1)
	r.rendering = nil
	if markdown, _, layout, err := renderDocumentForUpdate(r.fragment); err == nil {
		r.rendering = &replicaRendering{markdown: markdown, layout: layout}
	}
}

// recover answers a replica that could not take an update, which leaves it holding no state the
// room had: the log hands over every update it holds and records a fresh base at the room's next
// commit (crdt.UpdateLog.Reset), so the replica starts again exactly there, at a fresh epoch. Each
// update it could not place - the rest of this drain and what the reset handed over - is consumed
// all the same (consumed), and classified as a content change with no ask blocks, since nothing
// tells what it changed: crediting a connected browser with an edit that changed nothing loses
// less than an edit credited to nobody. The replica drops the load's rendering (loaded), if its
// first run has not taken it: nothing ties the base the reset records to the state the load
// rendered (start).
func (r *renderedReplica) recover(room string, unplaced []crdt.LoggedUpdate, classify func(update crdt.LoggedUpdate, changed bool, asks func() map[string]struct{})) {
	unplaced = append(unplaced, r.log.Reset()...)
	r.doc, r.fragment, r.rendering = nil, nil, nil
	r.loaded = weak.Pointer[replicaRendering]{}
	r.touched.reset()
	r.epoch.Store(replicaEpochs.Add(1))
	for _, update := range unplaced {
		r.consumed(update)
	}
	slog.Error("dispatch: the document's rendered copy starts again at the room's next commit; updates it could not place count as content changes",
		"room", room, "updates", len(unplaced))
	if classify == nil {
		return
	}
	for _, update := range unplaced {
		classify(update, true, nil)
	}
}

// keepReplica makes the replica live's update observer keeps, moving through log, live's update
// log, and lists it for live's reads (readLive) while live is resident (listResident). The listing
// holds the replica weakly too, and the observer, which live holds, holds the replica, so the
// replica, a whole copy of live, is collected with live and in the same cycle. A reader holding an
// evicted instance of the room reaches that instance's replica or none, never its successor's. The
// replica takes a fresh epoch (replicaEpochs), counts its paths on the service's renderPaths, and
// learns what each update touched through noteChangedTypes (start).
//
// loaded is the load's rendering of live as log opened on it, or nil when the load made none. The
// replica holds it only weakly, for its first run to take in place of rendering its base (start):
// once the caller drops it, nothing holds it, so it adds no resident memory and the next
// collection takes it.
func (s *Service) keepReplica(live *crdt.Doc, log *crdt.UpdateLog, loaded *replicaRendering) *renderedReplica {
	replica := &renderedReplica{log: log, paths: &s.renderPaths, loaded: weak.Make(loaded)}
	replica.epoch.Store(replicaEpochs.Add(1))
	listResident(&s.replicas, live, weak.Make(replica))
	return replica
}

// readLive runs read against live, a room's resident document, as of one moment: live's replica
// when it holds every update the room has committed (renderedReplica.hold), else a copy taken under
// live's document lock (snapshotDocument) - while live has no replica, since the room has had no
// transaction since it loaded, whenever another holds the replica's lock or the update observer
// waits for it, and in the moment between a commit and the observer that classifies it. A read
// never waits for that lock: the update observer takes it for each peer update before ygo
// broadcasts the update to the room's other browsers, so a read queued behind it would stall their
// keystrokes for its walk, and every read queued behind that one too. It opens no transaction on
// live, since ygo hands the room's persistence an update for every transaction it commits, even one
// that only reads. The caller must not hold live's document lock - be inside a Yjs transaction on
// it - since the copy encodes under that lock, and read must not keep doc past its return.
func (s *Service) readLive(room string, live *crdt.Doc, read func(doc *crdt.Doc)) error {
	doc, release, err := s.holdLive(room, live)
	if err != nil {
		return err
	}
	defer release()
	read(doc)
	return nil
}

// holdLive is live as of one moment, as readLive reads it, for the caller to walk until it calls
// release: the replica, holding its lock, or a copy. A caller that must take the room as of one
// moment under a lock of its own takes it here under that lock and walks it once it has released
// that lock (captureLiveTextAndAuthors).
func (s *Service) holdLive(room string, live *crdt.Doc) (doc *crdt.Doc, release func(), err error) {
	if live == nil {
		return nil, nil, errDocUnloaded
	}
	if replica := s.replicaOf(live); replica != nil {
		if doc := replica.hold(); doc != nil {
			return doc, replica.mu.Unlock, nil
		}
	}
	copied, err := snapshotDocument(live)
	if err != nil {
		return nil, nil, err
	}
	return copied, func() {}, nil
}

// replicaOf is the replica live's update observer keeps (keepReplica, from the room's load), or nil
// for a document no room loaded or whose replica has been collected. live is a room's resident
// document, the key the replica is listed under.
func (s *Service) replicaOf(live *crdt.Doc) *renderedReplica {
	if listed, ok := s.replicas.Load(weak.Make(live)); ok {
		return listed.(weak.Pointer[renderedReplica]).Value()
	}
	return nil
}

// liveTree is the tree of live, the document resident under room, as of one moment (readLive).
func (s *Service) liveTree(room string, live *crdt.Doc) (*pmdoc.Node, error) {
	var tree *pmdoc.Node
	var treeErr error
	if err := s.readLive(room, live, func(doc *crdt.Doc) { tree, treeErr = treeOf(doc) }); err != nil {
		return nil, err
	}
	return tree, treeErr
}

// hold returns the replica holding mu, which the caller releases, when it holds every update the
// room has committed - its log holds none the observer has yet to classify - so it is the room as
// of the last commit. It returns nil, holding nothing, for a replica another holds mu on (the
// update observer, or another read) or the observer waits for (lockForUpdate), one the room's
// first transaction has not made yet, and one behind the room's latest commit: a read never moves
// the replica, since an update applied by a read would be rendered by no observer and credited to
// nobody, which is the loss the log exists to prevent (renderedReplica).
func (r *renderedReplica) hold() *crdt.Doc {
	if r.waiting.Load() > 0 || !r.mu.TryLock() {
		return nil
	}
	if r.doc == nil || r.log.Pending() > 0 {
		r.mu.Unlock()
		return nil
	}
	return r.doc
}

// updateChangesMarkdown reports whether the update the replica took last changed the room's
// rendered markdown, the only document content a version stores, keeps the new rendering
// (replicaRendering), and answers, when it changed, the ask blocks the update left (observe). It
// reads the replica, the room's document as of that update, so its caller holds mu, and it reads
// no more of it than the update can have changed (touchedBlocks), on one of three paths:
//
//   - untouched: an update that touched no block and not the fragment, on a replica that holds a
//     rendering, changed nothing: nothing is read or rendered;
//   - incremental: an update that touched some top-level blocks and not the fragment itself, on
//     such a replica, left the fragment's blocks as many and in the order the layout
//     recorded, so pmdoc.Layout.Splice renders just the touched blocks into the markdown, read
//     by pmdoc.ReadBlockForRendering, unless one of its guards declines;
//   - whole: anything else - the fragment itself changed, more blocks were touched than the
//     replica records, the replica holds no rendering, a touched block is no longer
//     among the fragment's children, or Splice declined - renders the replica whole through
//     renderDocumentForUpdate, recording a fresh layout from the same render.
//
// renderedReplica says why the first two answer exactly what the whole render would. The trees
// read never outlive the caller's turn with the replica, so they carry none of pmdoc.Read's copy.
// An update that changes only what no rendering carries - an anchor mark, or a heading id or list
// item label the browser editor derives - is no content change. A document that does not render
// counts as changed, with no ask blocks, and leaves the replica no rendering. It is the
// observer's measure of every update, which changedBy reaches without it only where it already
// knows the answer this would give: a published write's own rendering of the same state, or an
// update the room's load already held.
func (r *renderedReplica) updateChangesMarkdown(room string) (bool, func() map[string]struct{}) {
	if !r.touched.whole && r.rendering != nil {
		if len(r.touched.blocks) == 0 {
			r.paths.observerUntouched.Add(1)
			return false, nil
		}
		if changed, asks, ok := r.spliceTouched(room); ok {
			r.paths.observerIncremental.Add(1)
			return changed, asks
		}
	}
	r.paths.observerRendered.Add(1)
	markdown, tree, layout, err := renderDocumentForUpdate(r.fragment)
	if err != nil {
		r.rendering = nil
		if errors.Is(err, ErrDocOutsideSchema) {
			slog.Warn("dispatch: updated document outside Proof schema", "room", room, "error", err)
		} else {
			slog.Error("dispatch: read updated document", "room", room, "error", err)
		}
		return true, nil
	}
	if !r.keep(markdown, layout) {
		return false, nil
	}
	return true, func() map[string]struct{} { return askBlockIDs(tree) }
}

// spliceTouched is updateChangesMarkdown's incremental path: it splices the touched blocks'
// rendering into the replica's markdown (pmdoc.Layout.Splice) and reports whether that changed
// it, with the ask blocks the update left when it did, or ok false where the update must be
// rendered whole. Splice wants the document's top-level blocks as many and in the order the layout
// recorded, which an update that did not change the fragment leaves them, and every touched block
// among them, which a touched block deleted with its parent's own deletion is not; it checks the
// rest itself. The ask blocks are those of the blocks the new layout says hold one, read only when
// asked for: the document node is never an ask, so they are the whole tree's. A block the walk
// cannot read leaves it with no answer to give, so it logs the failure and answers nil, which
// records nothing of the update's ask blocks (observeAskBlocksForUpdate), as a whole render that
// cannot read the document does. The caller holds mu.
func (r *renderedReplica) spliceTouched(room string) (changed bool, asks func() map[string]struct{}, ok bool) {
	children := r.fragment.Children()
	if len(children) != r.rendering.layout.Blocks() {
		return false, nil, false
	}
	touched := make([]int, 0, len(r.touched.blocks))
	for index, child := range children {
		element, isElement := child.(*crdt.YXmlElement)
		if !isElement {
			return false, nil, false
		}
		if slices.Contains(r.touched.blocks, crdt.SharedType(element)) {
			touched = append(touched, index)
		}
	}
	if len(touched) != len(r.touched.blocks) {
		return false, nil, false
	}
	read := func(index int) (*pmdoc.Node, error) {
		return pmdoc.ReadBlockForRendering(children[index].(*crdt.YXmlElement))
	}
	markdown, layout, ok := r.rendering.layout.Splice(r.rendering.markdown, touched, read)
	if !ok {
		return false, nil, false
	}
	if !r.keep(markdown, layout) {
		return false, nil, true
	}
	return true, func() map[string]struct{} {
		ids := map[string]struct{}{}
		for _, index := range layout.AskBlocks() {
			block, err := read(index)
			if err != nil {
				slog.Error("dispatch: read a block of the updated document to list its ask blocks", "room", room, "error", err)
				return nil
			}
			addAskBlockIDs(ids, block)
		}
		return ids
	}, true
}

// keep makes markdown, rendered with layout, the replica's rendering (replicaRendering) and
// reports whether it changed the replica's markdown: the replica takes layout either way, and
// keeps the markdown string it holds where markdown equals it. The caller holds mu.
func (r *renderedReplica) keep(markdown string, layout *pmdoc.Layout) bool {
	if held := r.rendering; held != nil && held.markdown == markdown {
		// The replica's own string, not the equal one just rendered, with the layout the new
		// render recorded, which is the one later updates splice into.
		r.rendering = &replicaRendering{markdown: held.markdown, layout: layout}
		return false
	}
	r.rendering = &replicaRendering{markdown: markdown, layout: layout}
	return true
}
