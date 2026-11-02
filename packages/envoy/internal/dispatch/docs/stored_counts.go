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

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/persistence"
)

// This file holds what a socket's admission to a room that has not loaded is weighed at
// (reserveRoom): the count of the room's stored version (countStoredVersion), and the cache of
// those counts with the flights that share one count between the sockets waiting on it
// (storedCounts).

// reserveRoom admits a browser's socket to room's document against the room budget, before anything
// loads the document (ServeHTTP). A room already charged admits it with nothing reserved: the
// browser joins a room whose heap is counted. Otherwise it reserves what the stored document will
// cost at both copies (storedRoomReservation, roomCopies), the room's live document and the replica
// its load keeps, both made before anything can refuse them: the admission's own decode, which
// becomes the replica (loadTree, keepReplica), and ygo's decode into the room. The reservation is
// made before the load, not when the room has loaded: fifty browsers could otherwise pass an
// unreserved check and each build a whole cold copy before any of their rooms exists.
//
// A budget already past its limit, or one this document's reservation would pass, starts a sweep
// on a goroutine of its own (sweepRooms), never on this one, an HTTP handler whose load already has
// a documented wedge against a writer holding a pooled connection (onLoadDocument), and waits for
// what the sweeps can close - the rooms whose browsers have just gone, once their settlements have
// run and their closes flushed them - before it refuses (roomBudget.await), bounded by ctx and by
// admissionWait, which covers the settle delay and the close's flush, from the admission's start
// for both waits together. The refusal it then answers with is the transient close, which the
// editor's backoff retries. The caller releases the reservation on every way out of its handler.
func (s *Service) reserveRoom(ctx context.Context, room string) (*roomReservation, error) {
	if s.budget.join(room) {
		return nil, nil
	}
	deadline := time.Now().Add(s.admissionWait)
	if err := s.budget.refusal(ctx, deadline); err != nil {
		return nil, err
	}
	weight, err := s.storedRoomReservation(ctx, room)
	if err != nil {
		return nil, err
	}
	return s.budget.reserve(ctx, deadline, room, weight)
}

// storedRoomReservation is what an admission to room reserves: its latest version's stored
// markdown, counted (pmdoc.MeasureDocument) and measured in bytes, and the structs of its stored
// history (storedStructs), priced as a loaded room's rendering and struct total are (copyHeap), at
// both copies. A version is stored as its rendering (writeVersionTx), so the element count
// and the bytes are what the room's load counts of the rendering it loads with (keepReplica)
// wherever no update has been stored since that version, and that count replaces this reservation
// (roomBudget.load). So a mebibyte of prose reserves about the 54 MB it costs once counted
// (TestAnAdmissionReservesTheCountOfItsStoredVersion), where its bytes at the densest price would
// reserve 2.2 GB, past the whole budget, and refuse it on an empty pod; a document whose history
// outgrows its rendering reserves its structs, as its load is charged them
// (TestAStructDenseDocumentReservesAndIsChargedItsStructs); and a paragraph of a mebibyte reserves
// its rendered bytes, as its load is charged them
// (TestAParagraphOfAMebibyteReservesAndIsChargedItsRenderedBytes). Only markdown MeasureDocument
// cannot finish is priced from its bytes at the densest price (bytesOfHeapAByteAtTheDensest). A
// document with no version yet reserves nothing.
//
// The count is a whole goldmark parse: about 89 ms for a mebibyte, and up to 65,024,480 bytes of
// heap above its markdown for a mebibyte of the heaviest shape measured
// (storedCountHeapAtMost), and a walk of the stored history, which decodes nothing. It is paid on
// the path a person reaches when they open a document, once for each version in each process,
// since the count is kept (storedCounts) - never on a keystroke, and never inside a room's update
// observer. The admissions that miss one version at once share one count, the others waiting for
// the one that counts (storedCounts.await), and the counts of different documents run beside each
// other, each holding a grant of the copy-heap bound for what it reads and parses
// (storedCountWeight), so a burst of admissions holds no more of the pod's heap in its counts than
// that bound. Only the latest version's number is read on every admission; its markdown and the
// stored history are read only to count them.
func (s *Service) storedRoomReservation(ctx context.Context, room string) (int64, error) {
	var number int64
	err := s.queryFrom(ctx).QueryRow(ctx, `
		select number from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, room).Scan(&number)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read the number of the document's latest version: %w", err)
	}
	heap, kept := s.budget.stored.get(room, number)
	if !kept {
		heap, err = s.budget.stored.await(ctx, room, number, func(wanted context.Context) (int64, error) {
			return s.countStoredVersion(wanted, room, number)
		})
		if err != nil {
			return 0, err
		}
	}
	return roomWeight(heap, 0, 0), nil
}

// countStoredVersion is the price of one copy of room's version number (copyHeap), counted from its
// stored markdown and the structs of the history stored when it counts (storedStructs) and kept
// (storedCounts.put), unless a count that held its grant before this one has kept it already. It
// runs once for each version however many admissions miss it at once (storedCounts.await), so it
// belongs to the version rather than to the admission that started it.
//
// It opens one read-only snapshot of its own, through the pool rather than any caller's
// transaction, and weighs itself in it - the version's markdown and the largest row of the history
// it will walk (weighStoredCount) - before it covers a grant of the copy-heap bound for what those
// hold (storedCountWeight). So counts of different documents run beside each other, as many as the
// bound has room for, in the order they asked for their weight (podCopyHeaps), rather than one at a
// time: fifty browsers opening fifty documents of a mebibyte at once, their rooms loading beside
// the counts, were refused in two minutes while the counts took one turn at a time, and in 16.1
// to 20.5 s once each count held a grant instead, the last of the thirty-nine the element price of
// 470 admitted answered at 1m12s to 1m23s against 1m49s before; measured again on 2026-10-29
// beside the other work that sandbox was carrying, the eleven were refused in 23.2 s and the last
// admitted at 1m27.7s, the decodes of those rooms' loads most of it, which ask for six times the
// bound's time the counts do. At the price of 500 the budget holds thirty-seven such rooms, and
// the run of 2026-11-01 under sjawhar/ygo v1.54.1-sami.3 refused the thirteen in 10.5 s and
// admitted the last at 1m15.2s
// (TestTheRoomBudgetRefusesWhereItsOwnArithmeticSays). The grant is covered inside the
// transaction the count already holds, so no holder of the bound waits for a pooled connection
// (podCopyHeaps).
//
// The wait for that grant lasts as long as wanted holds, which is for as long as any admission
// still waits for this count. Only once the grant is held does the count's own read bound start,
// on a context no admission's end cancels, bounded as a cold read's fold is (coldReadTimeout): a
// count that had to wait within that bound saw it pass while it waited, which answered 22 of those
// fifty browsers 503 at 60.3 s. What it counted is kept for the next admission even if every
// admission waiting for it has gone. Its error is the end of wanted, a read's, or a panic
// recovered from the parse or the walk: the count runs on a goroutine of its own
// (storedCounts.await), where nothing else could recover it.
func (s *Service) countStoredVersion(wanted context.Context, room string, number int64) (heap int64, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("counting the stored document panicked: %v\n%s", recovered, debug.Stack())
			slog.Error("dispatch: counting a stored document for its admission panicked", "room", room, "error", err)
		}
	}()
	counts := &s.budget.stored
	// The weighing read is bound as the count's own read is, and is the admissions' to cancel,
	// since nothing is counted yet. The rollback that returns the connection on every way out is
	// given a bound of its own, no admission's end cancels and no earlier wait has spent: a
	// rollback on a context already done closes the pooled connection rather than returning it,
	// and a count that waited long for its grant is exactly when the pool can least afford that.
	weighing, stopWeighing := context.WithTimeout(wanted, coldReadTimeout)
	defer stopWeighing()
	tx, err := s.store.Pool.BeginTx(weighing, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, fmt.Errorf("begin the read of the document's stored version: %w", err)
	}
	defer func() {
		stopping, stop := context.WithTimeout(context.WithoutCancel(wanted), coldReadTimeout)
		defer stop()
		_ = tx.Rollback(stopping)
	}()
	head, markdownBytes, rowBytes, err := weighStoredCount(weighing, tx, room, number)
	if err != nil {
		return 0, err
	}
	grant := newCopyGrant()
	if err := grant.coverWeight(wanted, storedCountWeight(markdownBytes, rowBytes)); err != nil {
		return 0, err
	}
	defer grant.release()
	if heap, ok := counts.get(room, number); ok {
		return heap, nil
	}
	reading, stopReading := context.WithTimeout(context.WithoutCancel(wanted), coldReadTimeout)
	defer stopReading()
	var markdown string
	if err := tx.QueryRow(reading, `
		select coalesce(markdown, '') from artifact_versions where artifact_id = $1 and number = $2
	`, room, number).Scan(&markdown); err != nil {
		return 0, fmt.Errorf("read the document's version %d: %w", number, err)
	}
	if counts.beforeCount != nil {
		counts.beforeCount(room, number)
	}
	structs, err := storedStructs(reading, tx, room, head)
	if err != nil {
		return 0, err
	}
	heap = copyHeap(int64(len(markdown)), countElements(markdown), structs)
	counts.put(&storedCount{room: room, number: number, heap: heap})
	return heap, nil
}

// weighStoredCount is what a count of room's version number holds at its peak, read in tx's one
// snapshot before the count covers its grant (storedCountWeight): the head its walk reads through
// (headVersion), the bytes of the version's markdown, which the count reads whole and parses, and
// the largest update row of that history, which is what the walk holds at once (storedStructs). A
// document with no stored update is weighed at a head of 0 and no row.
func weighStoredCount(ctx context.Context, tx pgx.Tx, room string, number int64) (head persistence.Version, markdown, row int64, err error) {
	var version int64
	if err := tx.QueryRow(ctx, `
		select h.version,
			coalesce((select octet_length(markdown) from artifact_versions where artifact_id = $1 and number = $2), 0),
			coalesce((select max(octet_length(update)) from doc_updates where artifact_id = $1 and version <= h.version), 0)
		from (select `+headVersion+` as version) h
	`, room, number).Scan(&version, &markdown, &row); err != nil {
		return 0, 0, 0, fmt.Errorf("weigh the document's stored version %d: %w", number, err)
	}
	return persistence.Version(version), markdown, row, nil
}

// storedStructs is the structs room's stored history holds, as a room's load would read it: every
// stored update through head, the rows a load's fold folds (foldRead), read one row at a time in
// the count's snapshot here however that fold reads them (eachUpdateThrough), each walked as a
// room's own state is (walkUpdate, fromRoom), which reads the
// update's headers and the lengths of its contents, decodes nothing and allocates nothing per
// struct, its structs summed. A compacted history is one update, the state a compaction encoded
// from its fold (compactRoom, tailRule), whose count is the state's own as its room's load counts
// it (countRoomStructs), but for the splits that parked structs can still cause. A history with
// updates appended since its last compaction counts each update's structs as stored: a run typed
// one transaction at a time is a struct for each keystroke here, where a decode merges what it
// can (squashRuns), and the splits a delete range makes are not here. Either way the room's load
// replaces the reservation with the count of the room it decoded (chargeLoadedRoom). An update
// the walk refuses would fail the room's load; it counts what the walk read before refusing.
func storedStructs(ctx context.Context, tx pgx.Tx, room string, head persistence.Version) (int64, error) {
	var structs uint64
	if err := eachUpdateThrough(ctx, tx, room, head, func(update []byte) error {
		tally, _ := walkUpdate(update, fromRoom, nil)
		structs += tally.structs
		return nil
	}); err != nil {
		return 0, err
	}
	return int64(structs), nil
}

// storedCountsHeld is the most documents whose stored version's count is kept (storedCounts). An
// entry is an id and two integers, a few hundred bytes with its list element and its map slot, so
// 4,096 of them hold about a mebibyte, and counting entries bounds their weight: the cold reads'
// cache weighs each of its entries (documentRead.weight) because they run from bytes to
// megabytes, and these do not vary. A document the bound has dropped costs its next admission one
// parse and one walk of its stored history again, nothing more.
const storedCountsHeld = 4_096

// storedCounts keeps, for each document an admission counted, the price of one copy of its latest
// stored version (countStoredVersion), the most recently admitted first, for at most
// storedCountsHeld documents. It is shaped as the cold reads' cache is
// (documentReads): one mutex, a map into a list kept in the order of use, one entry for a
// document, a newer version's count replacing an older one's, and the counts in flight, so that
// the admissions missing one version at once share one count rather than each parsing it (await).
// Unlike that cache's singleflight group, the counts in flight know how many admissions wait for
// each, so a count every admission has left ends its wait for its grant rather than parse for
// nobody.
//
// Unlike that cache's renderings, an element count never goes stale, and needs no stamp: a
// version's markdown is written once, by the insert that makes the version (api's
// writeDocumentVersion and createIssue), and no running server rewrites it - the other
// writes to artifact_versions move a file's content (files' moveRow and RestoreRows) or are
// migrations, which run before it serves - so the count of a document's version number is that
// version's for as long as it stands. The struct count is kept beside it, for the version: it is
// of the history as stored when the version was first counted, and updates stored after that move
// it only once a later version is counted, which costs nothing more than the reservation's
// staleness, since an admission's reservation stands only until the room loads and its own count
// replaces it (roomBudget.load). An entry is used only for the version the store names latest at
// the admission.
type storedCounts struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List
	// flights is the counts running or waiting for their grant of the copy-heap bound, one for
	// each (document, version) however many admissions miss it at once (await).
	flights map[string]*countFlight
	// beforeCount runs in a count once it has read the version's markdown and before it parses it.
	// Nil outside tests; tests use it to count the parses and to hold one while admissions arrive.
	beforeCount func(room string, number int64)
}

// storedCount is the price of one copy of room's version number (copyHeap): its elements, its
// bytes or the structs of its stored history, whichever is the most.
type storedCount struct {
	room         string
	number, heap int64
}

// get is the price of one copy of room's version number when it is kept, which it marks the most
// recently used.
func (c *storedCounts) get(room string, number int64) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[room]
	if !ok {
		return 0, false
	}
	count := element.Value.(*storedCount)
	if count.number != number {
		return 0, false
	}
	c.order.MoveToFront(element)
	return count.heap, true
}

// put keeps count as its document's, in place of a count of an older version, then drops the least
// recently used past storedCountsHeld. A count of an older version than the one kept is dropped
// instead: a slow count must not put back a version the store has moved past.
func (c *storedCounts) put(count *storedCount) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries, c.order = make(map[string]*list.Element), list.New()
	}
	if element, ok := c.entries[count.room]; ok {
		if element.Value.(*storedCount).number > count.number {
			return
		}
		c.order.Remove(element)
	}
	c.entries[count.room] = c.order.PushFront(count)
	if c.order.Len() > storedCountsHeld {
		delete(c.entries, c.order.Remove(c.order.Back()).(*storedCount).room)
	}
}

// countFlight is one count of a stored version (countStoredVersion) and the admissions waiting for
// it.
type countFlight struct {
	// done is closed once the count has returned heap and err.
	done chan struct{}
	heap int64
	err  error
	// waiting is how many admissions wait for the count. storedCounts.mu guards it.
	waiting int
	// abandon ends the count's wait for its grant of the copy-heap bound once no admission waits
	// for it.
	abandon context.CancelFunc
}

// await is the price count gives of one copy of room's version number, counted once however many
// admissions miss it at once: the first starts count on a goroutine of its own, and the rest wait
// for that one. The count is the version's, not an admission's: ctx bounds only this admission's
// wait, so an admission whose request ends returns at once with ctx's error and leaves the count to
// the others, and count's context, wanted, ends only once every admission waiting for it has gone,
// which ends a count still waiting for its grant of the copy-heap bound (countStoredVersion). An
// admission that misses the version after that starts a count of its own. An admission waiting
// here holds nothing of that bound, so the counts ahead of it are never waiting for what it holds.
func (c *storedCounts) await(ctx context.Context, room string, number int64, count func(wanted context.Context) (int64, error)) (int64, error) {
	key := room + "@" + strconv.FormatInt(number, 10)
	c.mu.Lock()
	flight := c.flights[key]
	if flight == nil {
		wanted, abandon := context.WithCancel(context.WithoutCancel(ctx))
		flight = &countFlight{done: make(chan struct{}), abandon: abandon}
		if c.flights == nil {
			c.flights = make(map[string]*countFlight)
		}
		c.flights[key] = flight
		go func() {
			defer abandon()
			heap, err := count(wanted)
			c.mu.Lock()
			c.forgetLocked(key, flight)
			c.mu.Unlock()
			flight.heap, flight.err = heap, err
			close(flight.done)
		}()
	}
	flight.waiting++
	c.mu.Unlock()
	select {
	case <-flight.done:
		return flight.heap, flight.err
	case <-ctx.Done():
		c.mu.Lock()
		defer c.mu.Unlock()
		if flight.waiting--; flight.waiting == 0 {
			flight.abandon()
			c.forgetLocked(key, flight)
		}
		return 0, ctx.Err()
	}
}

// forgetLocked drops flight as key's count in flight, unless a later count has replaced it. Its
// caller holds mu.
func (c *storedCounts) forgetLocked(key string, flight *countFlight) {
	if c.flights[key] == flight {
		delete(c.flights, key)
	}
}
