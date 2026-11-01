package docs

import (
	"log/slog"
	"sync"

	"github.com/reearth/ygo/crdt"
)

// A live write's own updates are stored directly after the room state its fork was read at (its
// read point), and the browser updates the room commits after that read and before it takes the
// write are stored after the write's. The room's update observer measured each of
// those against a room without the write, while its stored prefix holds the write, so each is
// measured again, as it is stored, against a state that holds the write: the room's shadow. Nothing
// waits for the write's publish to reach the room instead: the publish's apply can block on ygo's
// full persistence channel while the room's worker, which drains that channel, would wait on it.

// roomShadow is what a committed live write leaves its room (Ledger.commit, installShadowLocked):
// its frozen fork, the room as the write's read found it with the write's own updates applied,
// and the window of the room instance's commit positions whose classes are measured on it again.
// A durable class of that instance with from < seq < until is provisional; until is 0, open, until
// the drain that hands over the write's last published update sets it (reachShadow). Whoever
// stores a provisional class - the room's worker, a settlement, the next write, each in seq order
// inside flushThrough - applies it to doc and stores the class that render gives (measure).
//
// cursor, load, from, until and repairs are guarded by the mu of the state holding the shadow
// (roomState.shadow); doc and what follows it by the shadow's own mu, which is taken holding no
// other lock: never under a state's mu or a replica's. A state holds one shadow, freed once no
// provisional class is left (dropDrainedShadowLocked), when the write's publish ends without
// reaching the instance (closeShadow), or when the room fails (failRoomLocked).
type roomShadow struct {
	// cursor names the room instance whose classes the shadow measures. A cold write
	// (coldFork) whose room was still loading at its commit names the load instead, whose
	// instance the shadow measures once the load leaves one (cursorLocked).
	cursor *roomCursor
	load   *roomLoad
	from   uint64
	until  uint64
	// repairs are the repair updates the instance took inside the window, in commit order: the
	// classes after one are measured on a state that holds it (classifyDrained).
	repairs []shadowRepair

	mu  sync.Mutex
	doc *crdt.Doc
	// markdown is doc's rendering, nil while doc does not render; rendered says it was taken.
	// repaired is how many of repairs doc holds.
	markdown *string
	rendered bool
	repaired int
}

// shadowRepair is one repair update a room instance took inside a shadow's window, at the commit
// position seq.
type shadowRepair struct {
	seq    uint64
	update []byte
}

// newShadow is the shadow w leaves its room at its commit, or nil when it leaves none: it wrote
// nothing, its fork is gone (an operation abandoned it), or its warm read found no room instance.
// A cold write's shadow measures from the room instance's first commit, and names that instance
// at its commit (installShadowLocked).
func (w *liveWrite) newShadow() *roomShadow {
	if w.finished || len(w.updates) == 0 || w.fork == nil || (!w.cold && w.point.cursor == nil) {
		return nil
	}
	w.shadow = &roomShadow{doc: w.fork, cursor: w.point.cursor, from: w.point.seq}
	return w.shadow
}

// installShadowLocked makes shadow room's shadow, once the write that left it has committed and
// before its state, held across that commit (commitConsuming), lets a durable writer queued on the
// document's advisory lock read the room's queue. It replaces the state's earlier shadow, whose
// provisional classes the write stored ahead of its own (Ledger.flushThrough): each lies before the
// earlier write's last published update, which the write's read held. A cold write's shadow names
// the instance resident now, else the load under way; with neither, the room that loads next reads
// the write from the store and needs none. A shadow it does not install lets go of its document,
// so a cold write's fork is collected at releaseFork, before the publish loads the room. The
// caller holds state.mu.
func (s *Service) installShadowLocked(room string, state *roomState, shadow *roomShadow) {
	if state.released || state.failed != nil {
		// An uninstalled shadow is shared with no one, so its doc needs no sh.mu.
		shadow.doc = nil
		return
	}
	if shadow.cursor == nil {
		if live := s.srv.GetDoc(room); live != nil {
			shadow.cursor = s.cursorOf(live)
		} else if loading, ok := s.roomLoads.Load(room); ok {
			shadow.load = loading.(*roomLoad)
		}
		if shadow.cursor == nil && shadow.load == nil {
			shadow.doc = nil
			return
		}
	}
	state.shadow = shadow
}

// cursorLocked is the instance the shadow measures, nil while the load it names has left none.
// The caller holds the mu of the state holding the shadow.
func (sh *roomShadow) cursorLocked() *roomCursor {
	if sh.cursor == nil && sh.load != nil {
		sh.load.mu.Lock()
		if instance := sh.load.instance; instance != nil {
			sh.cursor = &instance.cursor
		}
		sh.load.mu.Unlock()
		if sh.cursor != nil {
			sh.load = nil
		}
	}
	return sh.cursor
}

// shadowWindow is a state's shadow and its window as of one hold of the state's mu: what a flush
// measures its classes against once it has released that lock.
type shadowWindow struct {
	shadow  *roomShadow
	cursor  *roomCursor
	from    uint64
	until   uint64
	repairs []shadowRepair
}

// shadowWindowLocked is state's shadow window, the zero window while it holds no shadow. The caller
// holds state.mu.
func (state *roomState) shadowWindowLocked() shadowWindow {
	shadow := state.shadow
	if shadow == nil {
		return shadowWindow{}
	}
	return shadowWindow{shadow: shadow, cursor: shadow.cursorLocked(), from: shadow.from, until: shadow.until, repairs: shadow.repairs}
}

// provisional is whether the update at seq of the instance cursor names lies inside the window.
func (w shadowWindow) provisional(cursor *roomCursor, seq uint64) bool {
	return w.cursor != nil && cursor == w.cursor && seq > w.from && (w.until == 0 || seq < w.until)
}

// recordRepairLocked keeps a repair's update the room took at seq inside state's shadow window, for
// the classes after it (measure). The caller holds state.mu.
func (state *roomState) recordRepairLocked(class documentUpdateClass) {
	if window := state.shadowWindowLocked(); window.provisional(class.cursor, class.seq) {
		window.shadow.repairs = append(window.shadow.repairs, shadowRepair{seq: class.seq, update: class.update})
	}
}

// dropDrainedShadowLocked frees state's shadow once its window is closed and no provisional class
// is left unstored. The caller holds state.mu.
func (state *roomState) dropDrainedShadowLocked() {
	window := state.shadowWindowLocked()
	if window.shadow == nil || window.until == 0 {
		return
	}
	for _, class := range state.updateClasses {
		if class.durable && !class.stored.Load() && window.provisional(class.cursor, class.seq) {
			return
		}
	}
	state.shadow = nil
}

// reachShadow closes shadow's window at seq, the commit position the drain of room's instance
// cursor names handed over the write's last published update at, and frees it when nothing in it
// is left unstored. A publish that reached another instance than the one the shadow measures - the
// room was replaced, or a cold write's room loaded holding the write - leaves no class to measure.
func (s *Service) reachShadow(room string, shadow *roomShadow, cursor *roomCursor, seq uint64) {
	state := s.lockExistingState(room)
	if state == nil {
		return
	}
	defer s.unlockState(room, state)
	if state.shadow != shadow {
		return
	}
	if shadow.cursorLocked() != cursor {
		state.shadow = nil
		return
	}
	shadow.until = seq
	state.dropDrainedShadowLocked()
}

// closeShadow ends write's shadow once its publish has returned: a window the publish never closed
// - it failed, or the room took nothing from it, holding the write already - has no class to
// measure again.
func (s *Service) closeShadow(write *liveWrite) {
	shadow := write.shadow
	if shadow == nil {
		return
	}
	write.shadow = nil
	state := write.state
	state.mu.Lock()
	if state.shadow == shadow && shadow.until == 0 {
		state.shadow = nil
	}
	s.unlockState(write.artifactID, state)
}

// storedClass is the class a flushed update is stored with, and the credit its append takes
// (UpdateCredit.take).
type storedClass struct {
	changed bool
	credit  *UpdateCredit
}

// storedClasses is the class each of classes, in seq order, is stored with: the one its update
// observer recorded, or for a provisional class the one window's shadow measures. A class measured
// again from a change to none writes no author, though its in-flight credit still lands with it
// (markStored); one measured from none to a change writes the authors the room recorded for it
// (documentUpdateClass.roomCredit). Every measure runs here, before the flush's first append.
func storedClasses(room string, window shadowWindow, classes []*queuedClass) []storedClass {
	stored := make([]storedClass, len(classes))
	for index, class := range classes {
		stored[index] = storedClass{changed: class.contentChanged, credit: class.credit}
		if window.shadow == nil || !window.provisional(class.cursor, class.seq) {
			continue
		}
		changed := window.shadow.measure(room, class, window.repairs)
		switch {
		case !changed:
			stored[index] = storedClass{}
		case !class.contentChanged:
			stored[index] = storedClass{changed: true, credit: class.roomCredit}
		}
	}
	return stored
}

// measure applies class's update to the shadow, after the repairs before it, and reports whether
// it changed the rendered markdown: true when either side does not render, the measure the room's
// replica uses (updateChangesMarkdown). A class is measured once: a flush that rolled back leaves
// its measure for the next, since the shadow holds the update already.
func (sh *roomShadow) measure(room string, class *queuedClass, repairs []shadowRepair) bool {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if class.shadowed {
		return class.shadowChanged
	}
	if !sh.rendered {
		sh.rendered = true
		sh.renderLocked()
	}
	if sh.repaired < len(repairs) && repairs[sh.repaired].seq < class.seq {
		for sh.repaired < len(repairs) && repairs[sh.repaired].seq < class.seq {
			sh.applyLocked(room, repairs[sh.repaired].update)
			sh.repaired++
		}
		sh.renderLocked()
	}
	before := sh.markdown
	sh.applyLocked(room, class.update)
	sh.renderLocked()
	changed := before == nil || sh.markdown == nil || *before != *sh.markdown
	class.shadowed, class.shadowChanged = true, changed
	return changed
}

// applyLocked applies update to the shadow's document. One that does not apply leaves the shadow
// no document, so every later class it measures counts as a change. The caller holds sh.mu.
func (sh *roomShadow) applyLocked(room string, update []byte) {
	if sh.doc == nil {
		return
	}
	if err := crdt.ApplyUpdateV1(sh.doc, update, nil); err != nil {
		slog.Error("dispatch: apply a room update to a live write's shadow; later updates it measures count as content changes", "room", room, "error", err)
		sh.doc = nil
	}
}

// renderLocked takes the shadow's rendering. A shadow holding no document renders none. The
// caller holds sh.mu.
func (sh *roomShadow) renderLocked() {
	sh.markdown = nil
	if sh.doc == nil {
		return
	}
	markdown, _, _, err := renderDocumentForUpdate(sh.doc.GetXmlFragment(fragmentName))
	if err != nil {
		return
	}
	sh.markdown = &markdown
}
