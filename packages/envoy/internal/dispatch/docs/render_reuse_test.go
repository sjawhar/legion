package docs

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
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
	observerReused, observerInRoom, observerRendered   uint64
	observerIncremental, observerUntouched             uint64
	observerMarginOnly                                 uint64
	beforeFromReplica, beforeFromWrite, beforeRendered uint64
	catchUpFromLoad, catchUpRendered                   uint64
}

func readRenderPaths(service *Service) renderPaths {
	counts := &service.renderPaths
	return renderPaths{
		observerReused:      counts.observerReused.Load(),
		observerInRoom:      counts.observerInRoom.Load(),
		observerRendered:    counts.observerRendered.Load(),
		observerIncremental: counts.observerIncremental.Load(),
		observerUntouched:   counts.observerUntouched.Load(),
		observerMarginOnly:  counts.observerMarginOnly.Load(),
		beforeFromReplica:   counts.beforeFromReplica.Load(),
		beforeFromWrite:     counts.beforeFromWrite.Load(),
		beforeRendered:      counts.beforeRendered.Load(),
		catchUpFromLoad:     counts.catchUpFromLoad.Load(),
		catchUpRendered:     counts.catchUpRendered.Load(),
	}
}

func (p renderPaths) minus(base renderPaths) renderPaths {
	return renderPaths{
		observerReused:      p.observerReused - base.observerReused,
		observerInRoom:      p.observerInRoom - base.observerInRoom,
		observerRendered:    p.observerRendered - base.observerRendered,
		observerIncremental: p.observerIncremental - base.observerIncremental,
		observerUntouched:   p.observerUntouched - base.observerUntouched,
		observerMarginOnly:  p.observerMarginOnly - base.observerMarginOnly,
		beforeFromReplica:   p.beforeFromReplica - base.beforeFromReplica,
		beforeFromWrite:     p.beforeFromWrite - base.beforeFromWrite,
		beforeRendered:      p.beforeRendered - base.beforeRendered,
		catchUpFromLoad:     p.catchUpFromLoad - base.catchUpFromLoad,
		catchUpRendered:     p.catchUpRendered - base.catchUpRendered,
	}
}

// observed is how many logged updates the update observer classified on any of its paths. A
// replica's start is no such path: it classifies no update.
func (p renderPaths) observed() int {
	return int(p.observerReused + p.observerInRoom + p.observerRendered + p.observerIncremental + p.observerUntouched + p.observerMarginOnly)
}

// withoutCatchUps is p without the counts of the replica's starts (catchUpFromLoad,
// catchUpRendered): which of the two a first start counts turns on whether a collection ran
// between the room's load and its first commit, which a test that does not control the collector
// cannot say.
func (p renderPaths) withoutCatchUps() renderPaths {
	p.catchUpFromLoad, p.catchUpRendered = 0, 0
	return p
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
// still there to read (recordUpdateClass, consumeUpdateClass).
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

func sameRendering(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// replicaMarkdown is the markdown of the rendering the replica keeps, or nil for a replica that
// keeps none. The caller holds the replica's lock.
func replicaMarkdown(replica *renderedReplica) *string {
	if replica.rendering == nil {
		return nil
	}
	return &replica.rendering.markdown
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
	if !sameRendering(replicaMarkdown(replica), room.markdown) {
		sh.failf("after commit %d the replica renders %s and the shadow %s", len(room.updates), describeRendering(replicaMarkdown(replica)), describeRendering(room.markdown))
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
// takes no path, and that the replica was compared with the shadow after every other one. It
// leaves out the replica's starts (withoutCatchUps), so want names none; a test that controls the
// collector asserts them itself (since).
func (m renderMeasure) expect(t *testing.T, want renderPaths, recovered int) {
	t.Helper()
	got := m.since().withoutCatchUps()
	if got != want {
		t.Errorf("render paths = %+v, want %+v", got, want)
	}
	m.sh.mu.Lock()
	logged, aligned := m.sh.loggedLocked()-m.updates, m.sh.aligned-m.aligned
	m.sh.mu.Unlock()
	if observed := got.observed(); observed+recovered != logged {
		t.Errorf("the observers classified %d updates and recovered %d, of %d logged", observed, recovered, logged)
	}
	if aligned != logged-recovered {
		t.Errorf("the replica was compared with the shadow after %d of %d commits", aligned, logged-recovered)
	}
}

// since is the service's path counts since m.
func (m renderMeasure) since() renderPaths {
	return readRenderPaths(m.sh.service).minus(m.paths)
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
				if rendering := published.rendering; rendering.rendered != nil || rendering.askBlocks != nil || rendering.forkReadMark != (forkReadMark{}) {
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
		if !sameRendering(replicaMarkdown(replica), room.markdown) {
			t.Errorf("the replica renders %s and the shadow %s", describeRendering(replicaMarkdown(replica)), describeRendering(room.markdown))
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
// replica past that read: the write's update is measured by the observer, and so is the browser's.
// The browser's adds or drops a top-level block, which changes the document's fragment, so it is
// rendered whole; the write's changes one paragraph, which is rendered alone and spliced in. A
// delete-only edit, which advances no clock of the room's state vector, is told by the foreign
// count all the same.
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
			m.expect(t, renderPaths{beforeFromReplica: 1, observerRendered: 1, observerIncremental: 1}, 0)
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
// backfill re-minting the id a browser's block repeats. Each update that changes inside one block
// - the repair's paragraph, the backfill's re-minted id, the write's paragraph - is spliced in; the
// browser's appended block is rendered whole.
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
		m.expect(t, renderPaths{beforeFromReplica: 1, observerIncremental: 2}, 0)
		sh.versions(t, write)
	})
	t.Run("identity repair", func(t *testing.T) {
		service, artifactID, sh := newWarmRenderReuseRoom(t)
		first, _ := liveTree(t, service, artifactID).Children[0].Attrs[pmdoc.BlockIDAttr].(string)
		if first == "" {
			t.Fatal("the document's first block has no id to repeat")
		}
		repaired := false
		sh.onForkRead = func() {
			if repaired {
				return
			}
			repaired = true
			editAsConnectedPeer(t, service, artifactID, 2, reuseBob, appendBlockWithCraftedID(t, "A paragraph repeating the first block's id.\n", first))
			awaitAppendLandedOrQueued(t, service, artifactID)
			report := service.backfillBlockIDs(context.Background(), artifactID)
			if report.Err != nil || report.Stamped == 0 {
				t.Fatalf("the backfill repaired nothing: %+v", report)
			}
		}
		m := sh.measure()
		write := sh.write(t, applyOps(service, artifactID, reuseAgent, model.EditOp{Op: "replace", Find: "Paragraph 13 opens", With: "Paragraph 13 now opens"}))
		sh.onForkRead = nil
		if !repaired {
			t.Fatal("the write never read its fork")
		}
		waitForLandedAppends(t, service, artifactID)
		sh.check(t)
		m.expect(t, renderPaths{beforeFromReplica: 1, observerRendered: 1, observerIncremental: 2}, 0)
		sh.versions(t, write)
	})
}

// E7. A room evicted and reloaded between the write's fork read and its publish has a replica of
// its own, at another epoch, so the write's rendering is never taken there, though the write's
// update, which deletes text, carries the old instance's whole delete set. Reloaded before the
// write's append, the room measures the update, which changes one paragraph, so it is spliced in;
// reloaded after its commit, the room's load already holds it - and with a browser's edit, which
// appends a block and so is rendered whole, the new replica's foreign count is the fork read's, so
// only the epoch tells the two apart.
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
				m.expect(t, renderPaths{beforeFromReplica: 1, observerIncremental: 1}, 0)
			}
			sh.versions(t, write)
		})
	}
}

// E8. A replica that recovers between the write's fork read and its publish starts again at a
// fresh epoch, so the write's rendering is not taken: the publish is measured, and spliced in.
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
	m.expect(t, renderPaths{beforeFromReplica: 1, observerIncremental: 1}, 1)
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

// E9. A browser commit F between a transaction's two operations: the first operation's counters
// predate F, so its update is measured; the second's fork moved, so its before-rendering renders;
// and its update takes the write's rendering exactly when F was consumed before its fork read.
// Every update measured changes one paragraph, so each is spliced in.
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
			if consumedFirst {
				m.expect(t, renderPaths{beforeFromReplica: 1, beforeRendered: 1, observerIncremental: 2, observerReused: 1}, 0)
			} else {
				m.expect(t, renderPaths{beforeFromReplica: 1, beforeRendered: 1, observerIncremental: 3}, 0)
			}
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
// load read the head before it (roomLoad.took): its update is not in that room, so it is measured,
// and spliced in, since it changes one paragraph.
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
	m.expect(t, renderPaths{beforeRendered: 1, observerIncremental: 1}, 0)
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
// rendering, and the version is above the room's loaded head, so the update, which deletes text
// in one paragraph, is measured, and spliced in.
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
	m.expect(t, renderPaths{beforeRendered: 1, observerIncremental: 1}, 0)
	sh.versions(t, write)
}

// E15. A peer sends the room the second of its two transactions first: its structs wait for the
// first transaction's, which the room lacks, so the room parks them and the commit integrates
// nothing. The replica takes what each commit parked beside what it integrated
// (crdt.LoggedUpdate.Parked, renderedReplica), so it holds what the room holds parked while it
// waits, and when the first transaction arrives both integrate what waited behind it: the room,
// the replica and a render of the room all hold the text of both transactions, and the commit
// that brought them is a content change.
func TestRenderReuseAReplicaRebuiltFromTheLogHoldsWhatTheRoomParked(t *testing.T) {
	service, artifactID, sh := newWarmRenderReuseRoom(t)
	live := service.srv.GetDoc(artifactID)
	replica := service.replicaOf(live)
	if replica == nil {
		t.Fatal("the warm room has no replica")
	}
	// A log of the test's own, opened before the peer's commits, to read what each one parked:
	// the replica's log hands its updates to the observer, which drains it.
	parked := live.OpenUpdateLog()
	t.Cleanup(parked.Close)

	const firstText = "A paragraph the peer's first transaction wrote."
	const secondText = "A paragraph the peer's second transaction wrote."
	peer := newRoomPeer(t, service, artifactID, 2, reuseBob, nil)
	before := peer.doc.StateVector()
	peerAppendsBlock(t, peer, firstText)
	between := peer.doc.StateVector()
	peerAppendsBlock(t, peer, secondText)
	first := crdt.EncodeStateAsUpdateV1(peer.doc, before)
	second := crdt.EncodeStateAsUpdateV1(peer.doc, between)

	m := sh.measure()
	sendPeerUpdate(t, live, second)
	logged := drainOneLoggedUpdate(t, parked, "the peer's second transaction")
	if logged.Parked == nil {
		t.Fatal("the room logged nothing parked for an update whose structs wait for an earlier one of the same peer")
	}
	if waiting := live.PendingStats(); waiting.Items == 0 {
		t.Fatal("the room parked nothing, so nothing here is tested")
	}
	replicaHoldsTheRoom(t, live, replica, "the peer's second transaction")
	if rendered := renderOfRoom(t, live); strings.Contains(rendered, firstText) || strings.Contains(rendered, secondText) {
		t.Error("the room renders a paragraph of the peer's while the transaction that holds its place is missing")
	}

	sendPeerUpdate(t, live, first)
	integrated := drainOneLoggedUpdate(t, parked, "the peer's first transaction")
	if integrated.Parked != nil {
		t.Error("the commit that fills the gap parked something of its own, so nothing here is tested")
	}
	if waiting := live.PendingStats(); waiting.Items != 0 {
		t.Fatalf("the room still holds %d structs parked once the first transaction arrived", waiting.Items)
	}
	replicaHoldsTheRoom(t, live, replica, "the peer's first transaction")
	waitForLandedAppends(t, service, artifactID)
	sh.check(t)
	rendered := renderOfRoom(t, live)
	if !strings.Contains(rendered, firstText) || !strings.Contains(rendered, secondText) {
		t.Error("the room's rendering is missing the text one of the peer's transactions wrote")
	}
	if markdown := heldReplicaMarkdown(t, replica); markdown == nil || *markdown != rendered {
		t.Errorf("the replica renders %s and the room %d bytes", describeRendering(markdown), len(rendered))
	}
	// The commit that integrated what the room parked rendered two blocks more, so it changed the
	// room's markdown; the commit that only parked changed nothing of it.
	if changed := storedClassOf(t, service, artifactID, integrated.Update); !changed {
		t.Error("the commit that integrated what the room parked was not recorded as a content change")
	}
	if changed := storedClassOf(t, service, artifactID, logged.Update); changed {
		t.Error("the commit that only parked was recorded as a content change")
	}
	m.expect(t, renderPaths{observerUntouched: 1, observerRendered: 1}, 0)
}

// peerAppendsBlock writes markdown as a paragraph at the end of the peer's document, in one
// transaction of its own, and sends the room nothing: the test sends the transactions itself, out
// of the order they were made (sendPeerUpdate).
func peerAppendsBlock(t *testing.T, peer *roomPeer, markdown string) {
	t.Helper()
	tree, err := treeOf(peer.doc)
	if err != nil {
		t.Fatalf("read the peer's document: %v", err)
	}
	want := appendBlocks(t, markdown+"\n")(tree)
	var updateErr error
	peer.doc.Transact(func(txn *crdt.Transaction) { updateErr = pmdoc.Update(txn, peer.fragment, want) })
	if updateErr != nil {
		t.Fatalf("write the peer's paragraph: %v", updateErr)
	}
}

// sendPeerUpdate applies one of the peer's updates to the room as its connection would, and
// returns once the room's update observer has run for it.
func sendPeerUpdate(t *testing.T, live *crdt.Doc, update []byte) {
	t.Helper()
	if err := crdt.ApplyUpdateV1(live, update, "peer"); err != nil {
		t.Fatalf("apply the peer's update to the room: %v", err)
	}
}

// drainOneLoggedUpdate is the one commit log has taken since the last drain, named by what made
// it.
func drainOneLoggedUpdate(t *testing.T, log *crdt.UpdateLog, made string) crdt.LoggedUpdate {
	t.Helper()
	_, updates := log.Drain()
	if len(updates) != 1 {
		t.Fatalf("the room logged %d commits for %s, want 1", len(updates), made)
	}
	return updates[0]
}

// replicaHoldsTheRoom asserts that the replica, once it holds every logged commit, is the room:
// the same state vector, the same structs and deletions parked (crdt.Doc.PendingStats), the same
// tree, and the same whole state, which carries what is parked (crdt.EncodeStateAsUpdateV1). The
// room is read first, with nothing else writing it, so neither read walks a document another
// holds.
func replicaHoldsTheRoom(t *testing.T, live *crdt.Doc, replica *renderedReplica, after string) {
	t.Helper()
	clocks, waiting, tree := live.StateVector(), live.PendingStats(), live.GetXmlFragment(fragmentName).ToXML()
	state := crdt.EncodeStateAsUpdateV1(live, nil)
	doc := heldReplica(t, replica)
	defer replica.mu.Unlock()
	if got := doc.StateVector(); !maps.Equal(got, clocks) {
		t.Errorf("after %s the replica stands at state vector %v and the room at %v", after, got, clocks)
	}
	if got := doc.PendingStats(); got.Items != waiting.Items || got.DeleteRanges != waiting.DeleteRanges || !slices.Equal(got.MissingFor, waiting.MissingFor) {
		t.Errorf("after %s the replica holds %+v parked and the room %+v", after, got, waiting)
	}
	if got := doc.GetXmlFragment(fragmentName).ToXML(); got != tree {
		t.Errorf("after %s the replica holds a tree of %d bytes and the room one of %d", after, len(got), len(tree))
	}
	if got := crdt.EncodeStateAsUpdateV1(doc, nil); !bytes.Equal(got, state) {
		t.Errorf("after %s the replica's whole state is %d bytes and the room's %d", after, len(got), len(state))
	}
}

// heldReplica is the replica's document once it holds every commit the room has logged, with the
// replica's lock held for the caller to release (renderedReplica.hold).
func heldReplica(t *testing.T, replica *renderedReplica) *crdt.Doc {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if doc := replica.hold(); doc != nil {
			return doc
		}
		if time.Now().After(deadline) {
			t.Fatal("the replica never stood at the room's latest commit")
		}
		time.Sleep(time.Millisecond)
	}
}

// heldReplicaMarkdown is the markdown of the rendering the replica keeps once it holds every
// logged commit, or nil where it keeps none.
func heldReplicaMarkdown(t *testing.T, replica *renderedReplica) *string {
	t.Helper()
	heldReplica(t, replica)
	defer replica.mu.Unlock()
	return replicaMarkdown(replica)
}

// renderOfRoom is a render of the room as of now, taken from a copy of it, as a read of the
// document renders it.
func renderOfRoom(t *testing.T, live *crdt.Doc) string {
	t.Helper()
	copied, err := snapshotDocument(live)
	if err != nil {
		t.Fatalf("copy the room: %v", err)
	}
	markdown, _, err := shadowRender(copied)
	if err != nil || markdown == nil {
		t.Fatalf("render the room: %v", err)
	}
	return *markdown
}

// storedClassOf is the content_changed the room's persistence stored for update, the very bytes
// its commit was logged and observed with (sameUpdate).
func storedClassOf(t *testing.T, service *Service, artifactID string, update []byte) bool {
	t.Helper()
	var changed bool
	if err := service.store.Pool.QueryRow(context.Background(), `
		select content_changed from doc_updates where artifact_id = $1 and update = $2
	`, artifactID, update).Scan(&changed); err != nil {
		t.Fatalf("read the stored class of a %d-byte update: %v", len(update), err)
	}
	return changed
}

// typingDocument is renderReuseDocument behind a section a browser types into - a paragraph, a
// nested list, a code block, and paragraphs it splits, deletes and leaves to an agent - and ahead
// of appendices holding deeper nested lists and more code blocks.
func typingDocument() string {
	var b strings.Builder
	b.WriteString("# Typing specification\n\n")
	b.WriteString("The typed paragraph opens the document with words a browser types into.\n\n")
	b.WriteString("- Outer item one\n  - Nested item one holds the typing.\n  - Nested item two waits.\n- Outer item two\n\n")
	b.WriteString("```go\nfunc typed() {}\n```\n\n")
	b.WriteString("The split paragraph keeps its words together until the browser presses Enter in it twice.\n\n")
	b.WriteString("The doomed paragraph is deleted by the browser.\n\n")
	b.WriteString("The agent paragraph is rewritten by an agent.\n\n")
	b.WriteString(renderReuseDocument())
	for section := range 20 {
		fmt.Fprintf(&b, "## Appendix %d\n\n- Appendix %d outer\n  - Appendix %d nested\n    - Appendix %d deeper\n\n```text\nappendix %d code\n```\n\n", section, section, section, section, section)
	}
	return b.String()
}

// splitParagraph is the edit Enter makes in the top-level paragraph whose text starts with
// prefix, typed just before the word at: the words from at on move into a new paragraph after it,
// which the browser editor stamps with a block id of its own.
func splitParagraph(t *testing.T, prefix, at string) func(*pmdoc.Node) *pmdoc.Node {
	t.Helper()
	return func(tree *pmdoc.Node) *pmdoc.Node {
		for index, block := range tree.Children {
			if block.Type != "paragraph" || len(block.Children) != 1 || !strings.HasPrefix(block.Children[0].Text, prefix) {
				continue
			}
			text := block.Children[0].Text
			cut := strings.Index(text, " "+at)
			if cut < 0 {
				t.Fatalf("the paragraph %q has no word %q to split before", text, at)
			}
			parsed, err := pmdoc.Parse(text[cut+1:] + "\n")
			if err != nil {
				t.Fatalf("parse the split paragraph's tail: %v", err)
			}
			pmdoc.EnsureBlockIDs(parsed)
			block.Children[0].Text = text[:cut]
			tree.Children = slices.Insert(tree.Children, index+1, parsed.Children...)
			return tree
		}
		t.Fatalf("no top-level paragraph starts %q", prefix)
		return tree
	}
}

// insertBlocksAfter is the edit that inserts the blocks parsed from markdown, stamped with block
// ids as the browser editor would, after the top-level paragraph whose text starts with prefix.
func insertBlocksAfter(t *testing.T, prefix, markdown string) func(*pmdoc.Node) *pmdoc.Node {
	t.Helper()
	parsed, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse inserted blocks: %v", err)
	}
	pmdoc.EnsureBlockIDs(parsed)
	return func(tree *pmdoc.Node) *pmdoc.Node {
		for index, block := range tree.Children {
			var text strings.Builder
			for _, run := range block.Children {
				text.WriteString(run.Text)
			}
			if block.Type == "paragraph" && strings.HasPrefix(text.String(), prefix) {
				tree.Children = slices.Insert(tree.Children, index+1, parsed.Children...)
				return tree
			}
		}
		t.Fatalf("no top-level paragraph starts %q", prefix)
		return tree
	}
}

// A2. A browser typing into a live room on a real-size document: words, spaces and backspaces in
// a top-level paragraph and in a nested list item, a bold mark, two Enters, an ask block the
// browser inserts and then types into, a block delete and a margin record, interleaved with an
// agent's write and a forced recover. Every update's class, the room's ask blocks and their
// authors, and the replica's markdown after every commit equal the shadow's independent whole
// render; and the observer renders the document whole only for the edits that insert, delete or
// split a top-level block, splicing every keystroke's block in and reading nothing for the
// margin record.
func TestRenderReuseABrowserTypingScript(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	sh := watchRenders(t, service, artifactID)
	document := typingDocument()
	if len(document) < 390<<10 {
		t.Fatalf("the typing document is %d bytes, short of a real-size document", len(document))
	}
	seedServiceText(t, service, artifactID, document)
	browser := newRoomPeer(t, service, artifactID, 1, reuseBob, nil)
	m := sh.measure()

	var spliced, untouched, enters, blockEdits, agentWrites, recovered int
	typeAt := func(prefix string, at int, typed string) {
		t.Helper()
		for offset, char := range typed {
			text := browser.text(prefix)
			browser.send(func(txn *crdt.Transaction) { text.Insert(txn, at+offset, string(char), nil) })
			spliced++
		}
	}
	backspace := func(prefix string, at, count int) {
		t.Helper()
		for typed := range count {
			text := browser.text(prefix)
			browser.send(func(txn *crdt.Transaction) { text.Delete(txn, at-1-typed, 1) })
			spliced++
		}
	}
	const typedParagraph, nestedItem, question = "The typed paragraph", "Nested item one", "Which store backs the typing"

	// Words, a space and backspaces in a top-level paragraph, then in a nested list item.
	typeAt(typedParagraph, len(typedParagraph), " quickly")
	backspace(typedParagraph, len(typedParagraph+" quickly"), 2)
	typeAt(typedParagraph, len(typedParagraph+" quick"), "ly")
	typeAt(nestedItem, len(nestedItem), " now")
	backspace(nestedItem, len(nestedItem+" now"), 1)
	typeAt(nestedItem, len(nestedItem+" no"), "w")
	// A bold mark over one word.
	text := browser.text(typedParagraph)
	browser.send(func(txn *crdt.Transaction) {
		text.Format(txn, len("The "), len("typed"), crdt.Attributes{"strong": map[string]any{}})
	})
	spliced++
	// Enter, twice, splitting a paragraph.
	browser.editTree(splitParagraph(t, "The split paragraph", "until"))
	browser.editTree(splitParagraph(t, "until the browser", "presses"))
	enters += 2
	// An ask block the browser inserts, then types into.
	browser.editTree(insertBlocksAfter(t, typedParagraph, ":::ask{#typed-ask urgency=\"high\" multiple=\"false\"}\n"+question+"?\n:::\n"))
	blockEdits++
	typeAt(question, len(question), " now")
	// A block delete.
	browser.editTree(dropBlock(t, "The doomed paragraph"))
	blockEdits++
	// An agent's write, which the browser takes in, then types after.
	waitForLandedAppends(t, service, artifactID)
	write := sh.write(t, applyOps(service, artifactID, reuseAgent, model.EditOp{Op: "replace", Find: "The agent paragraph is rewritten", With: "The agent paragraph was rewritten"}))
	agentWrites++
	if stored := sh.versions(t, write); len(stored) != 1 || !stored[0] {
		t.Errorf("the agent's content_changed = %v, want [true]", stored)
	}
	browser.sync()
	typeAt(typedParagraph, len(typedParagraph+" quickly"), ",")
	// A margin record, which changes nothing the rendering reads.
	marks := browser.doc.GetMap(marksMapName)
	browser.send(func(txn *crdt.Transaction) {
		marks.Set(txn, "c-typing", map[string]any{"kind": "comment", "by": "user:bob", "text": "a margin note"})
	})
	untouched++
	// A forced recover: the replica cannot place the next keystroke, and starts again at the one
	// after it.
	replica := service.replicaOf(service.srv.GetDoc(artifactID))
	replica.mu.Lock()
	replica.doc = nil
	replica.mu.Unlock()
	text = browser.text(typedParagraph)
	browser.send(func(txn *crdt.Transaction) { text.Insert(txn, len(typedParagraph+" quickly,"), "!", nil) })
	recovered++
	typeAt(nestedItem, len(nestedItem+" now"), " yes")

	waitForLandedAppends(t, service, artifactID)
	sh.check(t)
	m.expect(t, renderPaths{
		observerRendered:    uint64(enters + blockEdits),
		observerIncremental: uint64(spliced),
		observerUntouched:   uint64(untouched),
		observerReused:      uint64(agentWrites),
		beforeFromReplica:   uint64(agentWrites),
	}, recovered)
	state := service.room(artifactID)
	state.mu.Lock()
	author, authored := state.askAuthors["typed-ask"]
	state.mu.Unlock()
	if !authored || author != reuseBob {
		t.Errorf("the room owes the browser's ask block to %#v (recorded %t), want %#v", author, authored, reuseBob)
	}
}

// A3. A splice's walk of the ask blocks the layout flags reads each of those blocks; one it
// cannot read leaves the room's ask blocks and their authors as they are, as an update whose own
// render failed does, rather than taking an empty set for the room's record and forgetting every
// author. No update can leave the room an unreadable flagged block - a change that broke one
// would touch it, so the update carrying it is rendered whole and the layout recorded afresh -
// so the test breaks the block in the replica's own copy, the one the walk reads, after the
// layout that flags it was recorded.
func TestRenderReuseAnUnreadableAskBlockKeepsTheRoomsAskAuthors(t *testing.T) {
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "The typed paragraph opens the document.\n\nThe last paragraph closes it.\n")
	browser := newRoomPeer(t, service, artifactID, 1, reuseBob, nil)
	browser.editTree(insertBlocksAfter(t, "The typed paragraph",
		":::ask{#unreadable-ask urgency=\"high\" multiple=\"false\"}\nWhich option does the room take?\n:::\n"))
	waitForLandedAppends(t, service, artifactID)
	state := service.room(artifactID)
	state.mu.Lock()
	blocks, authors := maps.Clone(state.askBlocks), maps.Clone(state.askAuthors)
	state.mu.Unlock()
	if _, recorded := blocks["unreadable-ask"]; !recorded || authors["unreadable-ask"] != reuseBob {
		t.Fatalf("the room records ask blocks %v with authors %v, want the browser's ask block credited to it", sortedIDs(blocks), authors)
	}

	// The replica's own ask block is given a chain deeper than a document in the schema nests, so
	// pmdoc.ReadBlockForRendering refuses it. The replica's next update resets what this
	// transaction's finder recorded (renderedReplica.observe), so the keystroke below is still
	// measured as touching its own block alone.
	replica := service.replicaOf(service.srv.GetDoc(artifactID))
	replica.mu.Lock()
	ask := topLevelAskElement(t, replica.fragment)
	replica.doc.Transact(func(txn *crdt.Transaction) {
		docstest.WriteDeepChain(txn, &ask.YXmlFragment, pmdoc.MaxTreeDepth+1, "deeper than the schema nests")
	})
	replica.mu.Unlock()

	before := readRenderPaths(service)
	text := browser.text("The last paragraph")
	browser.send(func(txn *crdt.Transaction) { text.Insert(txn, len("The last paragraph"), " still", nil) })
	waitForLandedAppends(t, service, artifactID)
	if paths := readRenderPaths(service).minus(before).withoutCatchUps(); paths != (renderPaths{observerIncremental: 1}) {
		t.Fatalf("the keystroke took the paths %+v, want one incremental render", paths)
	}
	state.mu.Lock()
	keptBlocks, keptAuthors := maps.Clone(state.askBlocks), maps.Clone(state.askAuthors)
	state.mu.Unlock()
	if !maps.Equal(keptBlocks, blocks) || !maps.Equal(keptAuthors, authors) {
		t.Errorf("after the keystroke the room records ask blocks %v with authors %v, want %v with %v",
			sortedIDs(keptBlocks), keptAuthors, sortedIDs(blocks), authors)
	}
	if logged := logs.String(); !strings.Contains(logged, "read a block of the updated document to list its ask blocks") {
		t.Errorf("the failed read logged %q, want the refusal of the block it could not read", logged)
	}
}

// topLevelAskElement is the one ask block among fragment's top-level blocks.
func topLevelAskElement(t *testing.T, fragment *crdt.YXmlFragment) *crdt.YXmlElement {
	t.Helper()
	for _, child := range fragment.Children() {
		if element, isElement := child.(*crdt.YXmlElement); isElement && element.NodeName == "ask" {
			return element
		}
	}
	t.Fatal("the document holds no top-level ask block")
	return nil
}

// A4. A room's replica takes, as its base's, the rendering the room's load made of the state the
// replica starts from, in place of rendering that base again, when its first run starts while the
// rendering is still there; a collection since the load, or a recover, leaves the run to render its
// base. Either way the replica equals the shadow's independent render after every commit. Each case
// that needs the load's rendering alive turns the collector off from just before the load until the
// replica has started (pauseCollections), since the replica holds the rendering only weakly and a
// collection in between would otherwise decide the counts; the collector runs again before the
// shadow renders, so a race build's memory does not grow through the whole test.
func TestRenderReuseACatchUpTakesTheLoadsRendering(t *testing.T) {
	t.Run("the first commit takes the load's rendering", func(t *testing.T) {
		service, artifactID, sh := newRenderReuseService(t)
		m := sh.measure()
		resume := pauseCollections(t)
		replica := loadRoomForCatchUp(t, service, artifactID)
		if heldLoadRendering(replica) == nil {
			t.Fatal("the replica holds no rendering of the load before its first commit")
		}
		firstCatchUpCommit(t, service, artifactID, sh, m, resume)
		expectCatchUps(t, m, 1, 0)
		if heldLoadRendering(replica) != nil {
			t.Error("the replica still holds the load's rendering after its first run")
		}
	})
	t.Run("a collection before the first commit leaves it to render", func(t *testing.T) {
		service, artifactID, sh := newRenderReuseService(t)
		m := sh.measure()
		replica := loadRoomForCatchUp(t, service, artifactID)
		runtime.GC()
		if heldLoadRendering(replica) != nil {
			t.Fatal("the load's rendering outlived a collection, so something holds it strongly")
		}
		firstCatchUpCommit(t, service, artifactID, sh, m, func() {})
		expectCatchUps(t, m, 0, 1)
	})
	t.Run("a run after a recover renders", func(t *testing.T) {
		service, artifactID, sh := newRenderReuseService(t)
		m := sh.measure()
		resume := pauseCollections(t)
		replica := loadRoomForCatchUp(t, service, artifactID)
		firstCatchUpCommit(t, service, artifactID, sh, m, resume)
		expectCatchUps(t, m, 1, 0)
		m = sh.measure()
		// As E8 does: a replica that holds no document hands the next update to recover.
		replica.mu.Lock()
		replica.doc = nil
		replica.mu.Unlock()
		editAsConnectedPeer(t, service, artifactID, 2, reuseBob, appendBlocks(t, "A paragraph the recovering replica could not place.\n"))
		waitForLandedAppends(t, service, artifactID)
		editAsConnectedPeer(t, service, artifactID, 3, reuseBob, replaceRun("Paragraph 1 opens", "Paragraph 1 first opens"))
		waitForLandedAppends(t, service, artifactID)
		sh.check(t)
		m.expect(t, renderPaths{observerIncremental: 1}, 1)
		expectCatchUps(t, m, 0, 1)
	})
	t.Run("a recover before the first run drops the load's rendering", func(t *testing.T) {
		service, artifactID, sh := newRenderReuseService(t)
		m := sh.measure()
		resume := pauseCollections(t)
		replica := loadRoomForCatchUp(t, service, artifactID)
		if heldLoadRendering(replica) == nil {
			t.Fatal("the replica holds no rendering of the load before its first commit")
		}
		replica.mu.Lock()
		replica.recover(artifactID, nil, nil)
		replica.mu.Unlock()
		resume()
		if heldLoadRendering(replica) != nil {
			t.Fatal("the recovered replica still holds the load's rendering")
		}
		firstCatchUpCommit(t, service, artifactID, sh, m, func() {})
		expectCatchUps(t, m, 0, 1)
	})
}

// pauseCollections turns the garbage collector off until the answered resume is called, or t
// ends, so nothing collects the load's rendering, which the replica holds only weakly, before the
// test's first commit has started the replica.
func pauseCollections(t *testing.T) (resume func()) {
	t.Helper()
	previous := debug.SetGCPercent(-1)
	var once sync.Once
	resume = func() { once.Do(func() { debug.SetGCPercent(previous) }) }
	t.Cleanup(resume)
	return resume
}

// loadRoomForCatchUp loads artifactID's room, committing nothing, and answers its replica, which
// has not started.
func loadRoomForCatchUp(t *testing.T, service *Service, artifactID string) *renderedReplica {
	t.Helper()
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	replica := service.replicaOf(service.srv.GetDoc(artifactID))
	if replica == nil {
		t.Fatal("the loaded room has no replica")
	}
	replica.mu.Lock()
	defer replica.mu.Unlock()
	if replica.doc != nil {
		t.Fatal("the room committed during its load, so its replica has started")
	}
	return replica
}

// heldLoadRendering is the load's rendering replica still holds, or nil.
func heldLoadRendering(replica *renderedReplica) *replicaRendering {
	replica.mu.Lock()
	defer replica.mu.Unlock()
	return replica.loaded.Value()
}

// firstCatchUpCommit makes the loaded room's first commit, a browser's edit of one paragraph,
// which starts the replica and is spliced in, calls resume once that commit has landed, and checks
// the replica against the shadow.
func firstCatchUpCommit(t *testing.T, service *Service, artifactID string, sh *renderShadow, m renderMeasure, resume func()) {
	t.Helper()
	editAsConnectedPeer(t, service, artifactID, 1, reuseBob, replaceRun("Paragraph 0 opens", "Paragraph 0 first opens"))
	waitForLandedAppends(t, service, artifactID)
	resume()
	sh.check(t)
	m.expect(t, renderPaths{observerIncremental: 1}, 0)
}

// expectCatchUps asserts how many replica runs took the load's rendering and how many rendered
// their base since m.
func expectCatchUps(t *testing.T, m renderMeasure, fromLoad, rendered uint64) {
	t.Helper()
	if got := m.since(); got.catchUpFromLoad != fromLoad || got.catchUpRendered != rendered {
		t.Errorf("catch-ups: %d from the load, %d rendered; want %d and %d", got.catchUpFromLoad, got.catchUpRendered, fromLoad, rendered)
	}
}
