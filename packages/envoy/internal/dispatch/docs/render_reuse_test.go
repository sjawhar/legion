package docs

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// The tests here hold the reuse of renderings (renderedReplica.changedBy, warmFork's forkMarkdown,
// applyLive's before-rendering) to what rendering would have answered. Every expected value comes
// from a render of its own, never from the code under test: a shadow of each room (renderShadow)
// opens a second update log on the room's document, applies the logged base and every logged
// update in commit order to a copy of its own, and renders that copy after each one. Each case
// also asserts the exact number of times each rendering path ran (renderPathCounts), so none of
// them passes by rendering where it means to reuse, or by reusing where it means to render.

// renderReuseDocument is a generated document of about 400 KB, the size of the heaviest documents
// agents edit: headings, paragraphs, bullet and ordered lists, a table, callouts and ask blocks.
// Every section's sentences carry the section's number, so each one is a unique quote.
func renderReuseDocument() string {
	var b strings.Builder
	b.WriteString("# Equivalence specification\n\n")
	b.WriteString("| Area | Owner | State |\n| --- | --- | --- |\n")
	for row := range 12 {
		fmt.Fprintf(&b, "| Area %d | Team %d | open |\n", row, row%4)
	}
	b.WriteString("\n")
	for section := 0; b.Len() < 400<<10; section++ {
		fmt.Fprintf(&b, "## Section %d\n\n", section)
		fmt.Fprintf(&b, "Paragraph %d opens the section with enough words to read like specification prose about the subject at hand.\n\n", section)
		fmt.Fprintf(&b, "- Bullet %d alpha names one requirement of the section.\n- Bullet %d beta names another requirement of the section.\n\n", section, section)
		fmt.Fprintf(&b, "1. Step %d one is taken first.\n2. Step %d two is taken next.\n\n", section, section)
		if section%5 == 0 {
			fmt.Fprintf(&b, ":::callout{#callout-%d kind=\"note\" title=\"\"}\nCallout %d holds a note the readers of the section need.\n:::\n\n", section, section)
		}
		if section%40 == 0 {
			fmt.Fprintf(&b, ":::ask{#ask-%d urgency=\"high\" multiple=\"false\"}\nWhich option does section %d take?\n:::\n\n", section, section)
		}
	}
	return b.String()
}

// renderPaths is a reading of the service's renderPathCounts.
type renderPaths struct {
	observerReused, observerInRoom, observerRendered, observerMarginOnly uint64
	beforeFromReplica, beforeFromWrite, beforeRendered                   uint64
}

func readRenderPaths(service *Service) renderPaths {
	counts := &service.renderPaths
	return renderPaths{
		observerReused:     counts.observerReused.Load(),
		observerInRoom:     counts.observerInRoom.Load(),
		observerRendered:   counts.observerRendered.Load(),
		observerMarginOnly: counts.observerMarginOnly.Load(),
		beforeFromReplica:  counts.beforeFromReplica.Load(),
		beforeFromWrite:    counts.beforeFromWrite.Load(),
		beforeRendered:     counts.beforeRendered.Load(),
	}
}

func (p renderPaths) minus(base renderPaths) renderPaths {
	return renderPaths{
		observerReused:     p.observerReused - base.observerReused,
		observerInRoom:     p.observerInRoom - base.observerInRoom,
		observerRendered:   p.observerRendered - base.observerRendered,
		observerMarginOnly: p.observerMarginOnly - base.observerMarginOnly,
		beforeFromReplica:  p.beforeFromReplica - base.beforeFromReplica,
		beforeFromWrite:    p.beforeFromWrite - base.beforeFromWrite,
		beforeRendered:     p.beforeRendered - base.beforeRendered,
	}
}

// renderShadow follows every room instance the service loads for one document (watchRenders).
type renderShadow struct {
	service    *Service
	artifactID string

	mu sync.Mutex
	// rooms are the shadows of the room instances loaded since watchRenders, in load order, and
	// byDoc finds the one of a resident document; neither holds the document.
	rooms []*shadowRoom
	byDoc map[weak.Pointer[crdt.Doc]]*shadowRoom
	// failures are what the room observer found, reported by the next check.
	failures []string
	// aligned counts the observer runs after which the replica and the shadow stood at the same
	// commit and were compared.
	aligned int
	closed  bool

	// opCtx is the transaction of the operation under way (op), reads its fork reads.
	opCtx context.Context
	reads []shadowRead
	// forkMarkdownChecks counts the fork reads whose forkMarkdown was compared with a render of
	// the fork.
	forkMarkdownChecks int
	// onForkRead is the test's own action at a fork read, run after the shadow has copied the fork.
	onForkRead func()

	// ops are the operations of the transaction under way, and lastAfter the render of its fork as
	// its latest operation left it.
	ops       []shadowOp
	lastAfter *string
}

// shadowRoom is the shadow of one room instance: its own copy of the room's document, moved
// through the room's commits in order, and what each commit did to its rendering.
type shadowRoom struct {
	doc      *crdt.Doc
	markdown *string
	updates  []*shadowUpdate
}

type shadowUpdate struct {
	update   []byte
	origin   any
	changed  bool
	observed int
	// classes is how many classes the service recorded for the update, class the last one's
	// contentChanged.
	classes int
	class   bool
}

type shadowRead struct {
	markdown string
	err      error
}

type shadowOp struct {
	changed bool
}

// watchRenders shadows every room service loads for artifactID from now on. Its observer is
// registered after the service's own (onLoadDocument) and before ygo's persistence observer, so it
// runs once the service's observer has classified the update, and while the class it recorded is
// still there to read (queueUpdateClass, consumeUpdateClass).
func watchRenders(t *testing.T, service *Service, artifactID string) *renderShadow {
	t.Helper()
	sh := &renderShadow{service: service, artifactID: artifactID, byDoc: make(map[weak.Pointer[crdt.Doc]]*shadowRoom)}
	load := service.srv.OnLoadDocument
	service.srv.OnLoadDocument = func(ctx context.Context, room string, doc *crdt.Doc) error {
		if err := load(ctx, room, doc); err != nil {
			return err
		}
		if room != artifactID {
			return nil
		}
		replica := service.replicaOf(doc)
		log := doc.OpenUpdateLog()
		shadow := &shadowRoom{}
		sh.mu.Lock()
		sh.rooms = append(sh.rooms, shadow)
		sh.byDoc[weak.Make(doc)] = shadow
		sh.mu.Unlock()
		doc.OnUpdate(func(update []byte, origin any) {
			sh.observed(shadow, log, replica, update, origin)
		})
		return nil
	}
	service.afterForkRead = func(room string) {
		if room != artifactID {
			return
		}
		sh.forkRead()
	}
	t.Cleanup(func() {
		sh.mu.Lock()
		sh.closed = true
		sh.mu.Unlock()
	})
	return sh
}

func (sh *renderShadow) failf(format string, args ...any) {
	sh.failures = append(sh.failures, fmt.Sprintf(format, args...))
}

// shadowRender renders doc as a read of the document would: the markdown, or nil outside the
// schema, and the ask blocks it holds.
func shadowRender(doc *crdt.Doc) (*string, map[string]struct{}, error) {
	tree, err := treeOf(doc)
	if err != nil {
		return nil, nil, err
	}
	markdown, err := documentMarkdown(tree)
	if err != nil {
		return nil, nil, err
	}
	return &markdown, askBlockIDs(tree), nil
}

// sameUpdate reports whether logged, an update the room's update log handed over, is update, the
// one an observer was called with: ygo hands both the very slice it encoded at the commit
// (crdt.LoggedUpdate), so the same backing array is the same commit, where equal bytes need not be
// - two commits that change nothing encode alike.
func sameUpdate(logged, update []byte) bool {
	return len(logged) > 0 && len(logged) == len(update) && &logged[0] == &update[0]
}

func sameRendering(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func describeRendering(markdown *string) string {
	if markdown == nil {
		return "outside the schema"
	}
	return fmt.Sprintf("%d bytes", len(*markdown))
}

// take moves the shadow through what log has logged since its last drain, rendering after each
// update. The caller holds sh.mu.
func (room *shadowRoom) take(sh *renderShadow, log *crdt.UpdateLog) {
	base, updates := log.Drain()
	if base != nil {
		room.doc = newDocumentCopy()
		if err := crdt.ApplyUpdateV1(room.doc, base, nil); err != nil {
			sh.failf("the shadow could not apply the room's base: %v", err)
		}
		room.markdown, _, _ = shadowRender(room.doc)
	}
	for _, logged := range updates {
		if room.doc == nil {
			sh.failf("the room logged an update before any base")
			return
		}
		if err := crdt.ApplyUpdateV1(room.doc, logged.Update, nil); err != nil {
			sh.failf("the shadow could not apply a logged update: %v", err)
		}
		markdown, _, _ := shadowRender(room.doc)
		changed := markdown == nil || room.markdown == nil || *markdown != *room.markdown
		room.markdown = markdown
		room.updates = append(room.updates, &shadowUpdate{update: logged.Update, origin: logged.Origin, changed: changed})
	}
}

// observed runs after the service's own update observer for each commit of room.
func (sh *renderShadow) observed(room *shadowRoom, log *crdt.UpdateLog, replica *renderedReplica, update []byte, origin any) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.closed {
		log.Drain()
		return
	}
	room.take(sh, log)
	var entry *shadowUpdate
	for _, candidate := range room.updates {
		if sameUpdate(candidate.update, update) {
			entry = candidate
		}
	}
	if entry == nil {
		sh.failf("an observer ran for an update the room never logged")
		return
	}
	entry.observed++
	if _, repair := origin.(*identityClosureOrigin); !repair {
		if state := sh.service.lockExistingState(sh.artifactID); state != nil {
			for _, class := range state.updateClasses {
				if bytes.Equal(class.update, update) {
					entry.classes++
					entry.class = class.contentChanged
				}
			}
			sh.service.unlockState(sh.artifactID, state)
		}
	}
	// The replica as this commit's observer left it, against the shadow at the same commit: under
	// the replica's lock, holding every commit its log handed over, and with no commit landing
	// while the shadow catches up (the replica's log is opened first, so a commit reaches it first).
	replica.mu.Lock()
	defer replica.mu.Unlock()
	if replica.doc == nil || replica.log.Pending() > 0 {
		return
	}
	room.take(sh, log)
	if replica.log.Pending() > 0 {
		return
	}
	sh.aligned++
	if !sameRendering(replica.markdown, room.markdown) {
		sh.failf("after commit %d the replica renders %s and the shadow %s", len(room.updates), describeRendering(replica.markdown), describeRendering(room.markdown))
	}
}

// forkRead is afterForkRead: it renders a copy of the fork the operation under way just read and
// holds what the write knows of that fork without rendering it to that render.
func (sh *renderShadow) forkRead() {
	sh.mu.Lock()
	if sh.closed || sh.opCtx == nil {
		sh.mu.Unlock()
		return
	}
	write := joinedLiveWrite(sh.opCtx, sh.artifactID)
	if write == nil || write.fork == nil {
		sh.failf("a fork read left the write no fork")
		sh.mu.Unlock()
		return
	}
	copied, err := snapshotDocument(write.fork)
	if err != nil {
		sh.failf("copy the fork: %v", err)
		sh.mu.Unlock()
		return
	}
	markdown, _, renderErr := shadowRender(copied)
	read := shadowRead{err: renderErr}
	if markdown != nil {
		read.markdown = *markdown
	}
	if write.forkMarkdown != nil {
		sh.forkMarkdownChecks++
		if markdown == nil || *write.forkMarkdown != *markdown {
			sh.failf("forkMarkdown (%d bytes) is not the fork's rendering (%s)", len(*write.forkMarkdown), describeRendering(markdown))
		}
	}
	if write.tree != nil && (markdown == nil || write.markdown != *markdown) {
		sh.failf("the write kept a rendering (%d bytes) that is not its fork's (%s)", len(write.markdown), describeRendering(markdown))
	}
	sh.reads = append(sh.reads, read)
	action := sh.onForkRead
	sh.mu.Unlock()
	if action != nil {
		action()
	}
}

// op runs one operation of the transaction joined in ctx and records whether a version written
// from it must be a content change: what a render of the fork before it (its first fork read, or
// the fork as the previous operation left it when the operation read none) and of the fork after
// it say.
func (sh *renderShadow) op(t *testing.T, ctx context.Context, run func(ctx context.Context) error) *liveWrite {
	t.Helper()
	sh.mu.Lock()
	sh.opCtx, sh.reads = ctx, nil
	sh.mu.Unlock()
	err := run(ctx)
	sh.mu.Lock()
	reads := sh.reads
	sh.opCtx, sh.reads = nil, nil
	sh.mu.Unlock()
	if err != nil {
		t.Fatalf("operation: %v", err)
	}
	write := joinedLiveWrite(ctx, sh.artifactID)
	if write == nil || write.fork == nil {
		t.Fatal("the operation left no write with a fork")
	}
	var before shadowRead
	switch {
	case len(reads) > 0:
		before = reads[0]
	case sh.lastAfter != nil:
		before = shadowRead{markdown: *sh.lastAfter}
	default:
		t.Fatal("the operation read no fork")
	}
	copied, err := snapshotDocument(write.fork)
	if err != nil {
		t.Fatalf("copy the fork: %v", err)
	}
	after, _, err := shadowRender(copied)
	if err != nil {
		t.Fatalf("render the fork the operation left: %v", err)
	}
	if write.tree == nil || write.markdown != *after {
		t.Errorf("the write's rendering (%d bytes) is not the fork's (%d bytes)", len(write.markdown), len(*after))
	}
	sh.ops = append(sh.ops, shadowOp{changed: before.err != nil || before.markdown != *after})
	sh.lastAfter = after
	return write
}

// write runs ops as one committed transaction and returns its write.
func (sh *renderShadow) write(t *testing.T, ops ...func(ctx context.Context) error) *liveWrite {
	t.Helper()
	var write *liveWrite
	if err := joinedWrite(sh.service, func(ctx context.Context) error {
		for _, run := range ops {
			write = sh.op(t, ctx, run)
		}
		return nil
	}); err != nil {
		t.Fatalf("commit the write: %v", err)
	}
	return write
}

// versions checks the content_changed of every version write appended against the renders of its
// operations' forks, and returns them.
func (sh *renderShadow) versions(t *testing.T, write *liveWrite) []bool {
	t.Helper()
	if len(write.versions) != len(sh.ops) {
		t.Fatalf("the write appended %d versions for %d operations", len(write.versions), len(sh.ops))
	}
	stored := make([]bool, len(write.versions))
	for index, version := range write.versions {
		if err := sh.service.store.Pool.QueryRow(context.Background(), `
			select content_changed from doc_updates where artifact_id = $1 and version = $2
		`, sh.artifactID, int64(version)).Scan(&stored[index]); err != nil {
			t.Fatalf("read version %d: %v", version, err)
		}
		if want := sh.ops[index].changed; stored[index] != want {
			t.Errorf("version %d content_changed = %t, the renders of its fork say %t", version, stored[index], want)
		}
	}
	sh.ops, sh.lastAfter = nil, nil
	return stored
}

// renderMeasure is the service's path counts, the commits shadowed and the replica comparisons
// made as of one moment (measure).
type renderMeasure struct {
	sh      *renderShadow
	paths   renderPaths
	updates int
	aligned int
}

func (sh *renderShadow) measure() renderMeasure {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return renderMeasure{sh: sh, paths: readRenderPaths(sh.service), updates: sh.loggedLocked(), aligned: sh.aligned}
}

func (sh *renderShadow) loggedLocked() int {
	logged := 0
	for _, room := range sh.rooms {
		logged += len(room.updates)
	}
	return logged
}

// expect asserts that each path ran exactly as often as want says since m, that the room
// observers classified every update logged since m once, recovered of them by recover, which
// takes no path, and that the replica was compared with the shadow after every other one.
func (m renderMeasure) expect(t *testing.T, want renderPaths, recovered int) {
	t.Helper()
	got := readRenderPaths(m.sh.service).minus(m.paths)
	if got != want {
		t.Errorf("render paths = %+v, want %+v", got, want)
	}
	m.sh.mu.Lock()
	logged, aligned := m.sh.loggedLocked()-m.updates, m.sh.aligned-m.aligned
	m.sh.mu.Unlock()
	if observed := int(got.observerReused + got.observerInRoom + got.observerRendered + got.observerMarginOnly); observed+recovered != logged {
		t.Errorf("the observers classified %d updates and recovered %d, of %d logged", observed, recovered, logged)
	}
	if aligned != logged-recovered {
		t.Errorf("the replica was compared with the shadow after %d of %d commits", aligned, logged-recovered)
	}
}

// check compares, for the resident room, the replica's rendering, the shadow's and a render of the
// room as of now, the ask blocks the room records with that render's, every class the observers
// recorded with the shadow's, and asserts every published update's rendering left its origin.
func (sh *renderShadow) check(t *testing.T) {
	t.Helper()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	for _, failure := range sh.failures {
		t.Error(failure)
	}
	sh.failures = nil
	for roomIndex, room := range sh.rooms {
		for index, update := range room.updates {
			if update.observed != 1 {
				t.Errorf("room %d commit %d: observed %d times", roomIndex, index, update.observed)
			}
			if published, ok := update.origin.(*liveWriteOrigin); ok {
				if rendering := published.rendering; rendering.markdown != "" || rendering.askBlocks != nil || rendering.forkReadMark != (forkReadMark{}) {
					t.Errorf("room %d commit %d: the published update kept its rendering", roomIndex, index)
				}
			}
			if _, repair := update.origin.(*identityClosureOrigin); repair {
				continue
			}
			if update.classes != 1 {
				t.Errorf("room %d commit %d: %d classes recorded", roomIndex, index, update.classes)
			} else if update.class != update.changed {
				t.Errorf("room %d commit %d: classified content_changed=%t, the shadow says %t", roomIndex, index, update.class, update.changed)
			}
		}
	}
	live := sh.service.srv.GetDoc(sh.artifactID)
	if live == nil {
		t.Fatal("no room is resident to check")
	}
	room := sh.byDoc[weak.Make(live)]
	if room == nil {
		t.Fatal("the resident room was loaded before the shadow watched")
	}
	snapshot, err := snapshotDocument(live)
	if err != nil {
		t.Fatalf("copy the room: %v", err)
	}
	rendered, asks, _ := shadowRender(snapshot)
	if !sameRendering(rendered, room.markdown) {
		t.Errorf("the room renders %s and its shadow %s", describeRendering(rendered), describeRendering(room.markdown))
	}
	if room.doc != nil {
		replica := sh.service.replicaOf(live)
		if replica == nil {
			t.Fatal("the room has no replica")
		}
		deadline := time.Now().Add(5 * time.Second)
		for replica.hold() == nil {
			if time.Now().After(deadline) {
				t.Fatal("the replica never stood at the room's latest commit")
			}
			time.Sleep(time.Millisecond)
		}
		if !sameRendering(replica.markdown, room.markdown) {
			t.Errorf("the replica renders %s and the shadow %s", describeRendering(replica.markdown), describeRendering(room.markdown))
		}
		replica.mu.Unlock()
	}
	if rendered == nil {
		return
	}
	state := sh.service.room(sh.artifactID)
	state.mu.Lock()
	recorded, authors := maps.Clone(state.askBlocks), maps.Clone(state.askAuthors)
	state.mu.Unlock()
	if !sameIDs(recorded, asks) {
		t.Errorf("the room records ask blocks %v, its rendering holds %v", sortedIDs(recorded), sortedIDs(asks))
	}
	for id := range authors {
		if _, held := asks[id]; !held {
			t.Errorf("the room records an author for ask block %q, which its rendering does not hold", id)
		}
	}
}

func sameIDs(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if _, ok := b[id]; !ok {
			return false
		}
	}
	return true
}

func sortedIDs(ids map[string]struct{}) []string {
	return slices.Sorted(maps.Keys(ids))
}

// newRenderReuseService is a service whose settlement waits an hour, with renders watched from
// before the document is seeded with renderReuseDocument. No room holds the document yet.
func newRenderReuseService(t *testing.T) (*Service, string, *renderShadow) {
	t.Helper()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	sh := watchRenders(t, service, artifactID)
	seedServiceText(t, service, artifactID, renderReuseDocument())
	return service, artifactID, sh
}

// newWarmRenderReuseRoom is newRenderReuseService with the document's room loaded and its replica
// started by one browser edit, durable, so the replica stands at the room's latest commit with one
// foreign update consumed.
func newWarmRenderReuseRoom(t *testing.T) (*Service, string, *renderShadow) {
	t.Helper()
	service, artifactID, sh := newRenderReuseService(t)
	editAsConnectedPeer(t, service, artifactID, 1, model.Actor{Kind: "user", ID: "carol"}, replaceRun("Paragraph 0 opens", "Paragraph 0 first opens"))
	waitForLandedAppends(t, service, artifactID)
	replica := service.replicaOf(service.srv.GetDoc(artifactID))
	if replica == nil || replica.doc == nil || replica.foreign.Load() != 1 {
		t.Fatal("the warm-up edit left no replica standing at one foreign update")
	}
	sh.check(t)
	return service, artifactID, sh
}

func applyOps(service *Service, artifactID string, actor model.Actor, ops ...model.EditOp) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		_, err := service.ApplyOps(ctx, artifactID, ops, actor, nil)
		return err
	}
}

var (
	reuseAgent = model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	reuseAlice = model.Actor{Kind: "user", ID: "alice"}
	reuseBob   = model.Actor{Kind: "user", ID: "bob"}
)

// E1. A warm edit of a caught-up room takes the replica's rendering as its fork's, and the room's
// observer takes the edit's rendering as the replica's: nothing renders but the edit's own
// after-render.
func TestRenderReuseAWarmReplaceRendersOnlyItsResult(t *testing.T) {
	service, artifactID, sh := newWarmRenderReuseRoom(t)
	m := sh.measure()
	write := sh.write(t, applyOps(service, artifactID, reuseAgent, model.EditOp{Op: "replace", Find: "Paragraph 7 opens", With: "Paragraph 7 now opens"}))
	sh.check(t)
	m.expect(t, renderPaths{beforeFromReplica: 1, observerReused: 1}, 0)
	if stored := sh.versions(t, write); len(stored) != 1 || !stored[0] {
		t.Errorf("content_changed = %v, want [true]", stored)
	}
	if sh.forkMarkdownChecks != 1 {
		t.Errorf("%d fork reads took the replica's rendering, want 1", sh.forkMarkdownChecks)
	}
}

// E2. An anchored root comment is MarkQuote then ProjectMark in one transaction. MarkQuote takes
// the replica's rendering as its before-rendering, and the observer reuses the rendering it
// published. ProjectMark is a margin write (applyMargin), which renders nothing before it writes,
// so it takes no before-rendering at all, not even its predecessor's (no beforeFromWrite), and its
// publish into the room its fork was warm-built from is classified by the margin-only path. Neither
// changes content.
func TestRenderReuseAnAnchoredCommentRendersNothingTwice(t *testing.T) {
	service, artifactID, sh := newWarmRenderReuseRoom(t)
	m := sh.measure()
	write := sh.write(t,
		func(ctx context.Context) error {
			_, err := service.MarkQuote(ctx, artifactID, MarkSpec{Kind: MarkComment, ID: "c-reuse", By: reuseAlice}, "Step 3 two is taken next", nil)
			return err
		},
		func(ctx context.Context) error {
			return service.ProjectMark(ctx, artifactID, "c-reuse", MarkRecord{
				Kind: "comment", By: "user:alice", CreatedAt: "2026-09-23T00:00:00Z", Text: "note",
			}, reuseAlice)
		},
	)
	sh.check(t)
	m.expect(t, renderPaths{beforeFromReplica: 1, observerReused: 1, observerMarginOnly: 1}, 0)
	if stored := sh.versions(t, write); len(stored) != 2 || stored[0] || stored[1] {
		t.Errorf("content_changed = %v, want [false false]", stored)
	}
}

// E3. A warm write that adds an ask block hands the observer the ask blocks it walked, and the
// room records the block and its author as rendering would: the write's actor.
func TestRenderReuseAWarmAskKeepsItsAuthor(t *testing.T) {
	service, artifactID, sh := newWarmRenderReuseRoom(t)
	m := sh.measure()
	write := sh.write(t, applyOps(service, artifactID, reuseAgent, model.EditOp{
		Op: "insert", After: "end", Markdown: ":::ask{#reuse-ask urgency=\"high\" multiple=\"false\"}\nWhich store backs the cache?\n:::",
	}))
	sh.check(t)
	m.expect(t, renderPaths{beforeFromReplica: 1, observerReused: 1}, 0)
	if stored := sh.versions(t, write); len(stored) != 1 || !stored[0] {
		t.Errorf("content_changed = %v, want [true]", stored)
	}
	state := service.room(artifactID)
	state.mu.Lock()
	_, recorded := state.askBlocks["reuse-ask"]
	author, authored := state.askAuthors["reuse-ask"]
	state.mu.Unlock()
	if !recorded {
		t.Error("the room does not record the write's ask block")
	}
	if !authored || author != reuseAgent {
		t.Errorf("the room owes the ask block to %#v (recorded %t), want %#v", author, authored, reuseAgent)
	}
	settleCurrentGeneration(t, service, artifactID)
	if author := blockAskAuthor(t, service, artifactID, "reuse-ask"); author != reuseAgent {
		t.Errorf("the settled ask's author = %#v, want %#v", author, reuseAgent)
	}
	sh.check(t)
}

// E4, E5. A browser edit committed into the room once the write's fork has read it moves the
// replica past that read: the write's update is rendered, and so is the browser's. A delete-only
// edit, which advances no clock of the room's state vector, is told by the foreign count all the
// same.
func TestRenderReuseABrowserEditAfterTheForkReadIsRendered(t *testing.T) {
	for _, test := range []struct {
		name       string
		edit       func(t *testing.T) func(*pmdoc.Node) *pmdoc.Node
		deleteOnly bool
	}{
		{name: "insert", edit: func(t *testing.T) func(*pmdoc.Node) *pmdoc.Node {
			return appendBlocks(t, "A paragraph the browser typed.\n")
		}},
		{name: "delete only", deleteOnly: true, edit: func(t *testing.T) func(*pmdoc.Node) *pmdoc.Node {
			return dropBlock(t, "- Bullet 9 alpha")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID, sh := newWarmRenderReuseRoom(t)
			edited := false
			sh.onForkRead = func() {
				if edited {
					return
				}
				edited = true
				room := service.srv.GetDoc(artifactID)
				clocks := room.StateVector()
				editAsConnectedPeer(t, service, artifactID, 2, reuseBob, test.edit(t))
				awaitAppendLandedOrQueued(t, service, artifactID)
				if test.deleteOnly && !maps.Equal(clocks, room.StateVector()) {
					t.Error("the browser's delete moved the room's state vector, so it is not delete-only")
				}
			}
			m := sh.measure()
			write := sh.write(t, applyOps(service, artifactID, reuseAgent, model.EditOp{Op: "replace", Find: "Paragraph 11 opens", With: "Paragraph 11 now opens"}))
			sh.onForkRead = nil
			if !edited {
				t.Fatal("the write never read its fork")
			}
			waitForLandedAppends(t, service, artifactID)
			sh.check(t)
			m.expect(t, renderPaths{beforeFromReplica: 1, observerRendered: 2}, 0)
			sh.versions(t, write)
		})
	}
}

// serviceEdit writes edit into the room as the service's own repair (serviceTransact).
func serviceEdit(t *testing.T, service *Service, artifactID string, edit func(*pmdoc.Node) *pmdoc.Node) {
	t.Helper()
	var updateErr error
	err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		fragment := doc.GetXmlFragment(fragmentName)
		tree, err := pmdoc.Read(fragment)
		if err != nil {
			updateErr = err
			return
		}
		want := edit(tree)
		wrapped, release := service.serviceTransact(transact)
		defer release()
		wrapped(func(txn *crdt.Transaction) { updateErr = pmdoc.Update(txn, fragment, want) })
	})
	if err != nil || updateErr != nil {
		t.Fatalf("service edit: %v %v", err, updateErr)
	}
}

// E6. A commit of the service's own into the room while the write holds its slot moves the replica
// past the write's fork read just as a browser's does. So does an identity repair: the block-id
// backfill re-minting the id a browser's block repeats.
func TestRenderReuseAServiceCommitAfterTheForkReadIsRendered(t *testing.T) {
	t.Run("service repair", func(t *testing.T) {
		service, artifactID, sh := newWarmRenderReuseRoom(t)
		edited := false
		sh.onForkRead = func() {
			if !edited {
				edited = true
				serviceEdit(t, service, artifactID, replaceRun("Paragraph 12 opens", "Paragraph 12 now opens"))
			}
		}
		m := sh.measure()
		write := sh.write(t, applyOps(service, artifactID, reuseAgent, model.EditOp{Op: "replace", Find: "Paragraph 13 opens", With: "Paragraph 13 now opens"}))
		sh.onForkRead = nil
		if !edited {
			t.Fatal("the write never read its fork")
		}
		sh.check(t)
		m.expect(t, renderPaths{beforeFromReplica: 1, observerRendered: 2}, 0)
		sh.versions(t, write)
	})
	t.Run("identity repair", func(t *testing.T) {
		service, artifactID, sh := newWarmRenderReuseRoom(t)
		first, _ := liveTree(t, service, artifactID).Children[0].Attrs[pmdoc.BlockIDAttr].(string)
		if first == "" {
			t.Fatal("the document's first block has no id to repeat")
		}
		read := false
		sh.onForkRead = func() {
			if read {
				return
			}
			read = true
			editAsConnectedPeer(t, service, artifactID, 2, reuseBob, appendBlockWithCraftedID(t, "A paragraph repeating the first block's id.\n", first))
			awaitAppendLandedOrQueued(t, service, artifactID)
		}
		// The repair appends in its own transaction, under the document's advisory lock the write
		// holds from its fork read until it commits, so it runs in the window after that commit and
		// before the publish: its commit still reaches the room after the write's fork read, which
		// is what moves the replica past that read.
		repaired := false
		service.afterLiveCommit = onFirst(artifactID, func() {
			report := service.backfillBlockIDs(context.Background(), artifactID)
			if report.Err != nil || report.Stamped == 0 {
				t.Errorf("the backfill repaired nothing: %+v", report)
				return
			}
			repaired = true
		})
		defer func() { service.afterLiveCommit = nil }()
		m := sh.measure()
		write := sh.write(t, applyOps(service, artifactID, reuseAgent, model.EditOp{Op: "replace", Find: "Paragraph 13 opens", With: "Paragraph 13 now opens"}))
		sh.onForkRead = nil
		if !read {
			t.Fatal("the write never read its fork")
		}
		if !repaired {
			t.Fatal("the backfill never repaired the repeated block id")
		}
		waitForLandedAppends(t, service, artifactID)
		sh.check(t)
		m.expect(t, renderPaths{beforeFromReplica: 1, observerRendered: 3}, 0)
		sh.versions(t, write)
	})
}

// E7. A room evicted and reloaded between the write's fork read and its publish has a replica of
// its own, at another epoch, so the write's rendering is never taken there, though the write's
// update, which deletes text, carries the old instance's whole delete set. Reloaded before the
// write's append, the room renders the update; reloaded after its commit, the room's load already
// holds it - and with a browser's edit the new replica's foreign count is the fork read's, so only
// the epoch tells the two apart.
func TestRenderReuseAReloadedRoomNeverTakesTheWritesRendering(t *testing.T) {
	for _, afterCommit := range []bool{false, true} {
		name := "before the append"
		if afterCommit {
			name = "after the commit"
		}
		t.Run(name, func(t *testing.T) {
			service, artifactID, sh := newWarmRenderReuseRoom(t)
			reload := func() {
				closeDocumentRoom(t, service, artifactID)
				if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
					t.Fatalf("reload the room: %v", err)
				}
			}
			reloaded := false
			if !afterCommit {
				sh.onForkRead = func() {
					if !reloaded {
						reloaded = true
						reload()
					}
				}
			}
			m := sh.measure()
			ctx := context.Background()
			tx, err := service.store.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback(ctx)
			joined, ledger := service.Join(ctx, tx)
			defer ledger.Discard()
			write := sh.op(t, joined, applyOps(service, artifactID, reuseAgent, model.EditOp{Op: "delete", Find: "Paragraph 21 opens the section with enough words"}))
			sh.onForkRead = nil
			if err := ledger.commit(ctx); err != nil {
				t.Fatalf("commit the write: %v", err)
			}
			rendering := write.renderings[0]
			if !rendering.warm {
				t.Fatal("the write's fork was not read warm, so nothing here is tested")
			}
			if afterCommit {
				reload()
				reloaded = true
				editAsConnectedPeer(t, service, artifactID, 2, reuseBob, appendBlocks(t, "A paragraph typed into the reloaded room.\n"))
				waitForLandedAppends(t, service, artifactID)
			}
			if !reloaded {
				t.Fatal("the room was never reloaded")
			}
			replica := service.replicaOf(service.srv.GetDoc(artifactID))
			if replica.epoch.Load() == rendering.epoch {
				t.Errorf("the reloaded room's replica stands at the write's epoch %d", rendering.epoch)
			}
			if afterCommit && replica.foreign.Load() != rendering.foreign {
				t.Fatalf("the reloaded replica's foreign count is %d, the fork read's %d", replica.foreign.Load(), rendering.foreign)
			}
			ledger.publish()
			sh.check(t)
			if afterCommit {
				m.expect(t, renderPaths{beforeFromReplica: 1, observerRendered: 1, observerInRoom: 1}, 0)
			} else {
				m.expect(t, renderPaths{beforeFromReplica: 1, observerRendered: 1}, 0)
			}
			sh.versions(t, write)
		})
	}
}

// E8. A replica that recovers between the write's fork read and its publish starts again at a
// fresh epoch, so the write's rendering is not taken: the publish is rendered.
func TestRenderReuseARecoveredReplicaRendersThePublish(t *testing.T) {
	service, artifactID, sh := newWarmRenderReuseRoom(t)
	recovered := false
	var readEpoch uint64
	sh.onForkRead = func() {
		if recovered {
			return
		}
		recovered = true
		write := joinedLiveWriteOf(t, service, artifactID)
		readEpoch = write.forkRead.epoch
		replica := service.replicaOf(service.srv.GetDoc(artifactID))
		replica.mu.Lock()
		replica.doc = nil
		replica.mu.Unlock()
		editAsConnectedPeer(t, service, artifactID, 2, reuseBob, appendBlocks(t, "A paragraph the recovering replica could not place.\n"))
		awaitAppendLandedOrQueued(t, service, artifactID)
		if replica.epoch.Load() == readEpoch {
			t.Error("the recovered replica kept the epoch the fork read found")
		}
	}
	m := sh.measure()
	write := sh.write(t, applyOps(service, artifactID, reuseAgent, model.EditOp{Op: "replace", Find: "Paragraph 14 opens", With: "Paragraph 14 now opens"}))
	sh.onForkRead = nil
	if !recovered {
		t.Fatal("the write never read its fork")
	}
	waitForLandedAppends(t, service, artifactID)
	sh.check(t)
	m.expect(t, renderPaths{beforeFromReplica: 1, observerRendered: 1}, 1)
	sh.versions(t, write)
}

// joinedLiveWriteOf is the write open on artifactID's room (its writer slot).
func joinedLiveWriteOf(t *testing.T, service *Service, artifactID string) *liveWrite {
	t.Helper()
	state := service.room(artifactID)
	state.mu.Lock()
	write := state.liveWriter
	state.mu.Unlock()
	if write == nil {
		t.Fatal("no write is open on the room")
	}
	return write
}

// E9. A browser commit F between a transaction's two operations: the transaction reads the
// document once, so F never moves its fork - the second operation takes the rendering the first
// left - and F moves the replica past that read, so the observer renders both of the write's
// updates, whether the room consumed F before the second operation or after it.
func TestRenderReuseAForeignCommitBetweenOperations(t *testing.T) {
	for _, consumedFirst := range []bool{true, false} {
		name := "F consumed later"
		if consumedFirst {
			name = "F consumed first"
		}
		t.Run(name, func(t *testing.T) {
			service, artifactID, sh := newWarmRenderReuseRoom(t)
			m := sh.measure()
			var write *liveWrite
			if err := joinedWrite(service, func(ctx context.Context) error {
				sh.op(t, ctx, applyOps(service, artifactID, reuseAgent, model.EditOp{Op: "replace", Find: "Paragraph 15 opens", With: "Paragraph 15 now opens"}))
				release := holdPeerEdit(t, service, artifactID, &service.beforeObserveUpdate, replaceRun("Paragraph 16 opens", "Paragraph 16 then opens"))
				if consumedFirst {
					release()
				}
				write = sh.op(t, ctx, applyOps(service, artifactID, reuseAgent, model.EditOp{Op: "replace", Find: "Paragraph 17 opens", With: "Paragraph 17 now opens"}))
				if !consumedFirst {
					release()
				}
				return nil
			}); err != nil {
				t.Fatalf("commit the write: %v", err)
			}
			service.beforeObserveUpdate = nil
			waitForLandedAppends(t, service, artifactID)
			sh.check(t)
			// The transaction reads the document once, so F never reaches its fork: the second
			// operation takes the rendering the first left, and both published updates are
			// rendered by the observer, since F moved the replica's foreign count past the mark
			// that one fork read took. When the room consumes F makes no difference to either.
			m.expect(t, renderPaths{beforeFromReplica: 1, beforeFromWrite: 1, observerRendered: 3}, 0)
			sh.versions(t, write)
		})
	}
}

// E10. A cold write's publish loads the room from the store, which holds the write, so every one
// of its updates is already in the room and nothing renders it; each is no content change.
func TestRenderReuseAColdWriteIsAlreadyInTheRoom(t *testing.T) {
	for _, test := range []struct {
		name string
		ops  []model.EditOp
		want renderPaths
	}{
		{
			name: "one operation",
			ops:  []model.EditOp{{Op: "replace", Find: "Paragraph 18 opens", With: "Paragraph 18 now opens"}},
			want: renderPaths{beforeRendered: 1, observerInRoom: 1},
		},
		{
			name: "two operations",
			ops: []model.EditOp{
				{Op: "replace", Find: "Paragraph 18 opens", With: "Paragraph 18 now opens"},
				{Op: "replace", Find: "Paragraph 19 opens", With: "Paragraph 19 now opens"},
			},
			want: renderPaths{beforeRendered: 1, beforeFromWrite: 1, observerInRoom: 2},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID, sh := newRenderReuseService(t)
			m := sh.measure()
			var ops []func(ctx context.Context) error
			for _, op := range test.ops {
				ops = append(ops, applyOps(service, artifactID, reuseAgent, op))
			}
			write := sh.write(t, ops...)
			sh.check(t)
			m.expect(t, test.want, 0)
			sh.versions(t, write)
			sh.mu.Lock()
			for _, room := range sh.rooms {
				for index, update := range room.updates {
					if update.class {
						t.Errorf("commit %d of the cold publish was classified a content change", index)
					}
				}
			}
			sh.mu.Unlock()
		})
	}
}

// E11. A cold write whose version is appended while a room load is under way meets a room whose
// load read the head before it (roomLoad.took): its update is not in that room, so it renders.
func TestRenderReuseAColdWriteAppendedDuringALoadIsRendered(t *testing.T) {
	service, artifactID, sh := newRenderReuseService(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	load := service.srv.OnLoadDocument
	service.srv.OnLoadDocument = func(ctx context.Context, room string, doc *crdt.Doc) error {
		if room == artifactID {
			once.Do(func() {
				close(entered)
				<-release
			})
		}
		return load(ctx, room, doc)
	}
	loaded := make(chan error, 1)
	go func() { loaded <- service.warmLiveDocument(context.Background(), artifactID) }()
	select {
	case <-entered:
	case <-time.After(roomWait(t)):
		t.Fatal("the room's load never reached the window it is held in")
	}
	m := sh.measure()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, _, err := lockArtifactOwner(ctx, tx, artifactID); err != nil {
		t.Fatalf("lock document owner: %v", err)
	}
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	write := sh.op(t, joined, applyOps(service, artifactID, reuseAgent, model.EditOp{Op: "replace", Find: "Paragraph 22 opens", With: "Paragraph 22 now opens"}))
	if !write.cold {
		t.Fatal("the write forked the loading room, so nothing here is tested")
	}
	if err := ledger.commit(ctx); err != nil {
		t.Fatalf("commit the write: %v", err)
	}
	close(release)
	if err := <-loaded; err != nil {
		t.Fatalf("load the room: %v", err)
	}
	instance := service.instanceOf(service.srv.GetDoc(artifactID))
	if instance == nil || instance.loadedHead >= int64(write.versions[0]) {
		t.Fatalf("the room's load holds the write's version %d (loaded head %+v)", write.versions[0], instance)
	}
	ledger.publish()
	sh.check(t)
	m.expect(t, renderPaths{beforeRendered: 1, observerRendered: 1}, 0)
	sh.versions(t, write)
}

// E12. A document outside the schema: a warm repair's rendering is taken and counted a change; a
// document pushed outside the schema after the fork read renders, and WARNs, as before; a cold
// repair is already in the room it loads.
func TestRenderReuseADocumentOutsideTheSchema(t *testing.T) {
	const updatedOutside = "updated document outside Proof schema"
	captureWarnings := func(t *testing.T) *storetest.LockedLog {
		logs := &storetest.LockedLog{}
		logtest.SetDefault(t, slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
		return logs
	}
	replaceWhole := func(service *Service, artifactID string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := service.ReplaceText(ctx, artifactID, renderReuseDocument(), reuseAgent)
			return err
		}
	}
	t.Run("warm repair", func(t *testing.T) {
		service, artifactID, sh := newWarmRenderReuseRoom(t)
		logs := captureWarnings(t)
		writeSchemaInvalidElement(t, service, artifactID)
		m := sh.measure()
		write := sh.write(t, replaceWhole(service, artifactID))
		sh.check(t)
		m.expect(t, renderPaths{observerReused: 1}, 0)
		if stored := sh.versions(t, write); len(stored) != 1 || !stored[0] {
			t.Errorf("content_changed = %v, want [true]", stored)
		}
		if warned := strings.Count(logs.String(), updatedOutside); warned != 1 {
			t.Errorf("%d %q WARNs, want 1, the crafted element's:\n%s", warned, updatedOutside, logs.String())
		}
	})
	t.Run("pushed outside after the fork read", func(t *testing.T) {
		service, artifactID, sh := newWarmRenderReuseRoom(t)
		logs := captureWarnings(t)
		pushed := false
		sh.onForkRead = func() {
			if !pushed {
				pushed = true
				writeSchemaInvalidElement(t, service, artifactID)
			}
		}
		m := sh.measure()
		write := sh.write(t, applyOps(service, artifactID, reuseAgent, model.EditOp{Op: "replace", Find: "Paragraph 23 opens", With: "Paragraph 23 now opens"}))
		sh.onForkRead = nil
		if !pushed {
			t.Fatal("the write never read its fork")
		}
		sh.check(t)
		m.expect(t, renderPaths{beforeFromReplica: 1, observerRendered: 2}, 0)
		sh.versions(t, write)
		if warned := strings.Count(logs.String(), updatedOutside); warned != 2 {
			t.Errorf("%d %q WARNs, want 2:\n%s", warned, updatedOutside, logs.String())
		}
	})
	t.Run("cold repair", func(t *testing.T) {
		service, artifactID, sh := newWarmRenderReuseRoom(t)
		writeSchemaInvalidElement(t, service, artifactID)
		closeDocumentRoom(t, service, artifactID)
		logs := captureWarnings(t)
		m := sh.measure()
		write := sh.write(t, replaceWhole(service, artifactID))
		sh.check(t)
		m.expect(t, renderPaths{observerInRoom: 1}, 0)
		if stored := sh.versions(t, write); len(stored) != 1 || !stored[0] {
			t.Errorf("content_changed = %v, want [true]", stored)
		}
		if warned := strings.Count(logs.String(), "outside Proof schema"); warned != 0 {
			t.Errorf("%d schema WARNs during the cold repair, want none:\n%s", warned, logs.String())
		}
	})
}

// E13. A committed warm write keeps no fork, rendering, update or published rendering: each goes
// with the publish (finishLiveWrite). TestAColdWriteLoadsNoRoomUntilItCommits holds a cold write to
// the same.
func TestRenderReuseACommittedWriteKeepsNoRendering(t *testing.T) {
	service, artifactID, sh := newWarmRenderReuseRoom(t)
	m := sh.measure()
	write := sh.write(t,
		applyOps(service, artifactID, reuseAgent, model.EditOp{Op: "replace", Find: "Paragraph 24 opens", With: "Paragraph 24 now opens"}),
		applyOps(service, artifactID, reuseAgent, model.EditOp{Op: "replace", Find: "Paragraph 25 opens", With: "Paragraph 25 now opens"}),
	)
	if write.fork != nil || write.tree != nil || write.updates != nil || write.renderings != nil || write.marginRooms != nil || write.forkMarkdown != nil {
		t.Errorf("the committed write kept fork=%t tree=%t updates=%d renderings=%d marginRooms=%d forkMarkdown=%t",
			write.fork != nil, write.tree != nil, len(write.updates), len(write.renderings), len(write.marginRooms), write.forkMarkdown != nil)
	}
	sh.check(t)
	m.expect(t, renderPaths{beforeFromReplica: 1, beforeFromWrite: 1, observerReused: 2}, 0)
	sh.versions(t, write)
}

// E14. A room resident but behind the durable head (another process appended) makes the write
// fork cold, from the store, and the publish goes into that room: a cold fork carries no
// rendering, and the version is above the room's loaded head, so the update, which deletes text,
// renders.
func TestRenderReuseARoomBehindTheHeadRendersThePublish(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	taskA, _ := newTestServiceInstance(t, database)
	taskB, _ := newTestServiceInstance(t, storetest.SecondProcess(t, database))
	sh := watchRenders(t, taskA, artifactID)
	seedServiceText(t, taskA, artifactID, renderReuseDocument())
	if err := taskA.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load task A's room: %v", err)
	}
	editAsBrowserPeer(t, taskB, artifactID, 1, reuseBob, appendBlocks(t, "A paragraph typed on the other process.\n"))
	waitForLandedAppends(t, taskB, artifactID)
	if !roomBehind(t, taskA, artifactID) {
		t.Fatal("task A's room stands at the head after task B's edit, so nothing here is tested")
	}
	m := sh.measure()
	cold := false
	write := sh.write(t, func(ctx context.Context) error {
		if err := applyOps(taskA, artifactID, reuseAgent, model.EditOp{Op: "delete", Find: "Paragraph 26 opens the section with enough words"})(ctx); err != nil {
			return err
		}
		cold = joinedLiveWrite(ctx, artifactID).cold
		return nil
	})
	if !cold {
		t.Fatal("the write forked the room behind the head, so nothing here is tested")
	}
	sh.check(t)
	m.expect(t, renderPaths{beforeRendered: 1, observerRendered: 1}, 0)
	sh.versions(t, write)
}
