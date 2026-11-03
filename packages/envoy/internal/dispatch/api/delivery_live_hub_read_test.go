package api

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// notePause holds a hub goroutine at a note: the one that makes the next note an armed point
// matches waits there until the test resumes it, so what the test does meanwhile lands inside that
// moment of a recompute. The wait also ends with the hub's lifetime, so a test that fails while a
// recompute is held still stops its hub.
type notePause struct {
	mu    sync.Mutex
	armed *pausePoint
}

// pausePoint is one armed pause: reached closes once a note matched, note is that note, and
// resume lets the held goroutine go on.
type pausePoint struct {
	match   func(deliveryHubNote) bool
	reached chan struct{}
	note    deliveryHubNote
	resume  chan struct{}
	once    sync.Once
}

// pauseNotes wraps hub's observe, which hubOn has already set, so a test can hold the hub at a
// note. It is a configure function's work, before the hub starts.
func pauseNotes(hub *deliveryHub) *notePause {
	pause := &notePause{}
	observe, lifetime := hub.observe, hub.lifetime
	hub.observe = func(note deliveryHubNote) {
		observe(note)
		pause.mu.Lock()
		point := pause.armed
		if point == nil || !point.match(note) {
			pause.mu.Unlock()
			return
		}
		pause.armed = nil
		point.note = note
		pause.mu.Unlock()
		close(point.reached)
		select {
		case <-point.resume:
		case <-lifetime.Done():
		}
	}
	return pause
}

// at arms a pause at the next note match returns true for.
func (p *notePause) at(match func(deliveryHubNote) bool) *pausePoint {
	point := &pausePoint{match: match, reached: make(chan struct{}), resume: make(chan struct{})}
	p.mu.Lock()
	p.armed = point
	p.mu.Unlock()
	return point
}

// wait waits for the pause to hold a goroutine, and answers the note it holds it at.
func (p *pausePoint) wait(t *testing.T, what string) deliveryHubNote {
	t.Helper()
	select {
	case <-p.reached:
		return p.note
	case <-time.After(10 * time.Second):
		t.Fatalf("the hub made no note %s within 10s", what)
		return deliveryHubNote{}
	}
}

// proceed lets the held goroutine go on.
func (p *pausePoint) proceed() { p.once.Do(func() { close(p.resume) }) }

func noteIs(what string) func(deliveryHubNote) bool {
	return func(note deliveryHubNote) bool { return note.what == what }
}

// recomputesSince is how many recomputes a change caused since mark.
func (r *hubRig) recomputesSince(mark int) int {
	return r.count(mark, recomputeBegin(deliveryCauseChange))
}

// TestAMarkMadeBeforeAReadTakesItsPositionIsThatReadsToAnswer holds a recompute while its read
// has not yet taken the view's marks over, and merges a second pull request there that the intake
// reads at once: first before the read reads the newest change position (the window
// sjawhar/legion#2686 forced with delays), then after it, with the merge past that position. Each
// mark is for something committed before the reads that follow, so the one recompute's delta holds
// both merges and no second recompute follows.
func TestAMarkMadeBeforeAReadTakesItsPositionIsThatReadsToAnswer(t *testing.T) {
	// No t.Parallel: this test reads the event log by id cursor, and the safe horizon it waits
	// on is proven from pg_snapshot_xmin, the oldest transaction id running anywhere on the
	// instance (internal/dispatch/events, horizon.go). Keeping only the tests that hold a
	// lock on purpose out of the parallel set left five of these timing out on events_start;
	// the whole family is out of it (dispatch://INFRA-1075).
	holds := []struct {
		name  string
		match func(deliveryHubNote) bool
	}{
		{"before its position", recomputeBegin(deliveryCauseChange)},
		{"past its position", noteIs("recompute position")},
	}
	for _, hold := range holds {
		t.Run(hold.name, func(t *testing.T) {
			var pause *notePause
			rig := newHubRig(t, func(hub *deliveryHub) { pause = pauseNotes(hub) })
			seedHubSettings(t, rig.database)
			conn := newFakeLiveConn()
			rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
			held := conn.snapshot(t, 5*time.Second, "t").Body
			rig.settle(t)

			start := rig.mark()
			point := pause.at(hold.match)
			upsertPR(t, rig.database, hubPR(1, rig.clock.Now().Add(-2*time.Hour)))
			note := point.wait(t, hold.name)
			upsertPR(t, rig.database, hubPR(2, rig.clock.Now().Add(-time.Hour)))
			second := latestChange(t, rig.database)
			rig.hub.catchUpNow()
			if changeSeq, _ := rig.cursors(); changeSeq != second {
				t.Fatalf("the hub's change cursor while the read is held = %d, want the second merge's %d", changeSeq, second)
			}
			if note.what == "recompute position" && note.through >= second {
				t.Fatalf("the read's position %d already holds the second merge (%d): the window was not forced", note.through, second)
			}
			point.proceed()

			delta := conn.delta(t, 5*time.Second, "t")
			if got := slices.Sorted(slices.Values(liveUpsertIDs(t, delta, "prs"))); !slices.Equal(got, []string{"acme/widgets#1", "acme/widgets#2"}) {
				t.Fatalf("the recompute's delta upserts %v, want both merges", got)
			}
			requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, held, delta, "")
			rig.waitNoRecompute(t, 2*rig.hub.quiet)
			holdsSecond := func(note deliveryHubNote) bool {
				return note.what == "recompute position" && note.through >= second
			}
			if n := rig.count(start, holdsSecond); note.what == "recompute begin" && n != 1 {
				t.Fatalf("reads whose position holds the second merge = %d, want the held read's 1", n)
			}
			if n := rig.recomputesSince(start); n != 1 {
				t.Fatalf("recomputes for two merges the one read holds = %d, want 1", n)
			}
		})
	}
}

// TestAChangeAfterAReadsPositionRecomputesTheViewAgain holds a recompute once its read is done and
// merges a pull request there, past the read's position: the read never saw it, so the view is
// marked again and a second recompute sends it.
func TestAChangeAfterAReadsPositionRecomputesTheViewAgain(t *testing.T) {
	// No t.Parallel: this test reads the event log by id cursor, and the safe horizon it waits
	// on is proven from pg_snapshot_xmin, the oldest transaction id running anywhere on the
	// instance (internal/dispatch/events, horizon.go). Keeping only the tests that hold a
	// lock on purpose out of the parallel set left five of these timing out on events_start;
	// the whole family is out of it (dispatch://INFRA-1075).
	var pause *notePause
	rig := newHubRig(t, func(hub *deliveryHub) { pause = pauseNotes(hub) })
	seedHubSettings(t, rig.database)
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	held := conn.snapshot(t, 5*time.Second, "t").Body
	rig.settle(t)

	start := rig.mark()
	point := pause.at(noteIs("recompute read"))
	upsertPR(t, rig.database, hubPR(1, rig.clock.Now().Add(-2*time.Hour)))
	note := point.wait(t, "recompute read")
	upsertPR(t, rig.database, hubPR(2, rig.clock.Now().Add(-time.Hour)))
	second := latestChange(t, rig.database)
	if note.through >= second {
		t.Fatalf("the read's position %d holds the second merge (%d)", note.through, second)
	}
	rig.hub.catchUpNow()
	point.proceed()

	first := conn.delta(t, 5*time.Second, "t")
	if got := liveUpsertIDs(t, first, "prs"); !slices.Equal(got, []string{"acme/widgets#1"}) {
		t.Fatalf("the first recompute's delta upserts %v, want only the merge its read held", got)
	}
	// The first answer is its read's, from before the write made while the read was held, so it
	// lands on no GET made now; the second answer is checked against one.
	held = applyLiveDelta(t, deliveryLiveTimeline, held, first)
	again := conn.delta(t, 5*time.Second, "t")
	if got := liveUpsertIDs(t, again, "prs"); !slices.Equal(got, []string{"acme/widgets#2"}) {
		t.Fatalf("the second recompute's delta upserts %v, want the merge after the read", got)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, held, again, "")
	if n := rig.recomputesSince(start); n != 2 {
		t.Fatalf("recomputes for a merge and one after its read = %d, want 2", n)
	}
}

// TestAChangeLogResetDuringAReadKeepsTheViewDirty resets the change log while a read holds its
// position and before it takes the marks over, then merges a pull request once the read is done.
// The merge is change 1 of the new log, at or below the read's position in the old one, so it marks
// nothing; the reset's own mark, which the read cannot take over since its position may name the
// old log, recomputes the view again and sends the merge.
func TestAChangeLogResetDuringAReadKeepsTheViewDirty(t *testing.T) {
	// No t.Parallel: this test reads the event log by id cursor, and the safe horizon it waits
	// on is proven from pg_snapshot_xmin, the oldest transaction id running anywhere on the
	// instance (internal/dispatch/events, horizon.go). Keeping only the tests that hold a
	// lock on purpose out of the parallel set left five of these timing out on events_start;
	// the whole family is out of it (dispatch://INFRA-1075).
	var pause *notePause
	rig := newHubRig(t, func(hub *deliveryHub) { pause = pauseNotes(hub) })
	seedHubSettings(t, rig.database)
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	held := conn.snapshot(t, 5*time.Second, "t").Body
	rig.settle(t)

	start := rig.mark()
	position := pause.at(noteIs("recompute position"))
	upsertPR(t, rig.database, hubPR(1, rig.clock.Now().Add(-2*time.Hour)))
	note := position.wait(t, "recompute position")
	truncateChanges(t, rig.database)
	rig.hub.catchUpNow()
	if changeSeq, _ := rig.cursors(); changeSeq != 0 {
		t.Fatalf("the hub's change cursor after the reset = %d, want 0", changeSeq)
	}
	read := pause.at(noteIs("recompute read"))
	position.proceed()
	read.wait(t, "recompute read")
	upsertPR(t, rig.database, hubPR(2, rig.clock.Now().Add(-time.Hour)))
	rig.hub.catchUpNow()
	if changeSeq, _ := rig.cursors(); changeSeq != 1 || changeSeq > note.through {
		t.Fatalf("the hub's change cursor after the merge = %d, want 1, at or below the read's position %d", changeSeq, note.through)
	}
	read.proceed()

	first := conn.delta(t, 5*time.Second, "t")
	if got := liveUpsertIDs(t, first, "prs"); !slices.Equal(got, []string{"acme/widgets#1"}) {
		t.Fatalf("the first recompute's delta upserts %v, want only the merge its read held", got)
	}
	held = applyLiveDelta(t, deliveryLiveTimeline, held, first)
	again := conn.delta(t, 5*time.Second, "t")
	if got := liveUpsertIDs(t, again, "prs"); !slices.Equal(got, []string{"acme/widgets#2"}) {
		t.Fatalf("the recompute after the reset's delta upserts %v, want the merge after the read", got)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, held, again, "")
	if n := rig.recomputesSince(start); n != 2 {
		t.Fatalf("recomputes across a reset during a read = %d, want 2", n)
	}
}

// TestAnIssueTitleChangeDuringTheReadThatFirstNamesItRecomputesAgain merges a pull request naming
// an issue no page names yet, holds the recompute once its read is done, and retitles the issue
// there. The view's last answer did not name the issue, so the event marked nothing; the read
// names it under its old title, so the recompute marks the view again and the next one sends the
// new title.
func TestAnIssueTitleChangeDuringTheReadThatFirstNamesItRecomputesAgain(t *testing.T) {
	// No t.Parallel: this test reads the event log by id cursor, and the safe horizon it waits
	// on is proven from pg_snapshot_xmin, the oldest transaction id running anywhere on the
	// instance (internal/dispatch/events, horizon.go). Keeping only the tests that hold a
	// lock on purpose out of the parallel set left five of these timing out on events_start;
	// the whole family is out of it (dispatch://INFRA-1075).
	var pause *notePause
	rig := newHubRig(t, func(hub *deliveryHub) { pause = pauseNotes(hub) })
	seedHubSettings(t, rig.database)
	issue := createHubIssue(t, rig.handler, map[string]any{"title": "Ship widgets"})
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	held := conn.snapshot(t, 5*time.Second, "t").Body
	rig.settle(t)

	start := rig.mark()
	point := pause.at(noteIs("recompute read"))
	upsertPR(t, rig.database, prNaming(1, rig.clock.Now().Add(-2*time.Hour), issue))
	point.wait(t, "recompute read")
	patchHubIssue(t, rig.handler, issue, map[string]any{"title": "Ship the widgets"})
	rig.waitForEventCursor(t, eventHead(t, rig.database), "the retitle while the read is held")
	point.proceed()

	first := conn.delta(t, 5*time.Second, "t")
	if got := liveMapValues(t, first, "issue_titles"); got[issue] != "Ship widgets" {
		t.Fatalf("the first recompute's issue_titles = %v, want %s under the title its read saw", got, issue)
	}
	held = applyLiveDelta(t, deliveryLiveTimeline, held, first)
	again := conn.delta(t, 5*time.Second, "t")
	if got := liveMapValues(t, again, "issue_titles"); got[issue] != "Ship the widgets" {
		t.Fatalf("the second recompute's issue_titles = %v, want %s retitled", got, issue)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, held, again, "")
	if n := rig.recomputesSince(start); n != 2 {
		t.Fatalf("recomputes for a merge and a retitle during its read = %d, want 2", n)
	}
}

// TestASessionTitleChangeDuringTheReadThatFirstNamesItRecomputesAgain is the same for a session:
// a pull request naming a session no page shows yet, and the session's first titled event while
// the read is held. The read shows the session by its id, so the recompute marks the view again
// and the next one sends the title.
func TestASessionTitleChangeDuringTheReadThatFirstNamesItRecomputesAgain(t *testing.T) {
	// No t.Parallel: this test reads the event log by id cursor, and the safe horizon it waits
	// on is proven from pg_snapshot_xmin, the oldest transaction id running anywhere on the
	// instance (internal/dispatch/events, horizon.go). Keeping only the tests that hold a
	// lock on purpose out of the parallel set left five of these timing out on events_start;
	// the whole family is out of it (dispatch://INFRA-1075).
	var pause *notePause
	rig := newHubRig(t, func(hub *deliveryHub) { pause = pauseNotes(hub) })
	seedHubSettings(t, rig.database)
	createHubIssue(t, rig.handler, map[string]any{"title": "Hold the project"})
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	held := conn.snapshot(t, 5*time.Second, "t").Body
	rig.settle(t)

	const session = "ses_hub_reader"
	start := rig.mark()
	point := pause.at(noteIs("recompute read"))
	pr := hubPR(1, rig.clock.Now().Add(-2*time.Hour))
	pr.Sessions = []string{session}
	upsertPR(t, rig.database, pr)
	point.wait(t, "recompute read")
	appendHubEvent(t, rig, projectOwner("HUB").event("user_state.updated",
		model.Actor{Kind: "session", ID: session, Origin: &model.ActorOrigin{SessionTitle: "Hub reader"}}, map[string]any{}))
	rig.waitForEventCursor(t, eventHead(t, rig.database), "the session's titled event while the read is held")
	point.proceed()

	first := conn.delta(t, 5*time.Second, "t")
	if got := liveMapValues(t, first, "session_titles"); got[session] != session {
		t.Fatalf("the first recompute's session_titles = %v, want %s shown by its id", got, session)
	}
	held = applyLiveDelta(t, deliveryLiveTimeline, held, first)
	again := conn.delta(t, 5*time.Second, "t")
	if got := liveMapValues(t, again, "session_titles"); got[session] != "Hub reader" {
		t.Fatalf("the second recompute's session_titles = %v, want %s titled", got, session)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, held, again, "")
	if n := rig.recomputesSince(start); n != 2 {
		t.Fatalf("recomputes for a merge and a session title during its read = %d, want 2", n)
	}
}
