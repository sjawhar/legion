package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A run's record is rows and not process memory because the POST that asks a question and the
// GET that streams its answer are two HTTP requests, which a deployment of more than one pod
// spreads across pods (conn_age.go: kube-proxy and the front door spread load per connection,
// not per request). Every read and write of that record is here; the routes that call them are
// their own.
//
// The window a run has belongs to the record: going_requests.ends_at, written by the POST. A pod
// that dies mid-run stores no terminal frame, so a reader on another pod ends the stream itself
// once the run is past that moment with nothing terminal stored (readGoingStream), and
// sweepGoingRequests ends the runs nobody reads. At the deadline the running answer, a reader
// and the sweep may each try to end one run, so which of them ends it is settled by the table
// (the going_events_terminal index of migration 20261105152945) and never by whichever is first:
// every terminal write is an insert that does nothing on a conflict, and the writer that loses
// reads the stored terminal frame and reports that one (endGoingRun).

// goingRequestColumns is the going_requests select list scanGoingRequest reads, in scan order.
const goingRequestColumns = `id::text, author, project, words, created_at, ends_at`

// goingEventColumns is the going_events select list scanGoingEvent reads, in scan order.
const goingEventColumns = `sequence, name, data`

// goingTerminalNames are the two frame names that end a stream (going.Event.Terminal), the pair
// the going_events_terminal index holds as literals.
var goingTerminalNames = []string{contracts.GoingEventEnd, contracts.GoingEventError}

// goingTerminalWrites bounds how many sequences one terminal write tries. A write takes the next
// sequence and loses it only to a frame committed between its read of the last one and its own
// insert: to another terminal write, which ends the attempt with that stored frame, or to a
// frame of the run's own, after which the next sequence is free. The run writes its frames one
// after another, so two such losses in a row would need the run to be writing faster than this
// statement's round trip; the bound is there so a caller cannot spin, not because a third
// attempt is expected to be needed.
const goingTerminalWrites = 3

// goingRequest is one answer run as the record holds it: the question a reader asked, and the
// window the run has to answer it in.
type goingRequest struct {
	ID        string      `json:"id"`
	Author    model.Actor `json:"author"`
	Project   string      `json:"project"`
	Words     string      `json:"words"`
	CreatedAt time.Time   `json:"created_at"`
	EndsAt    time.Time   `json:"ends_at"`
}

// goingEvent is one stored frame of a run's stream: the frame, and the sequence it holds, which
// the stream writes as the frame's id so a reader that reconnects resumes after it.
type goingEvent struct {
	Sequence int
	Event    going.Event
}

// goingRunState is what a reader needs to decide whether a stream is over: the sequence of the
// run's one terminal frame, 0 while the run has stored none, and whether it is past its ends_at,
// after which a run that stored none is ended by whoever reads it. The terminal frame is a
// sequence and not a bool because a reader ends its stream on having been written that frame and
// not on the run having stored one: a terminal frame committed between a reader's read of the
// frames and its read of this state would otherwise close a stream that never carried it. The
// going_events_terminal index holds that there is at most one such frame, so one sequence says
// all of it.
type goingRunState struct {
	Terminal     int
	PastDeadline bool
}

// recordGoingRequest records one question: the asking actor, the project it was asked under (the
// empty string for every project, as search reads its own filter) and the words of the question,
// and answers the stored row. ends_at is created_at plus contracts.GoingRunWindow: both come
// from one now(), the transaction's own, so the window is exactly that long.
//
// The insert is made in a transaction of the shared pool rather than as a bare statement, because
// the POST that calls it is a keyed write: the commit step marks the request's Idempotency-Key
// committed with the write itself, which is what answers a repeat after a crash KEY_COMMITTED
// rather than recording the question a second time (keyed_write_guard_test.go).
func (s *server) recordGoingRequest(ctx context.Context, author model.Actor, project, words string) (goingRequest, error) {
	stored, err := json.Marshal(author)
	if err != nil {
		return goingRequest{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return goingRequest{}, err
	}
	defer tx.Rollback(ctx)
	request, err := scanGoingRequest(tx.QueryRow(ctx, `
		insert into going_requests (author, project, words, ends_at)
		values ($1::jsonb, $2::text, $3::text, now() + make_interval(secs => $4))
		returning `+goingRequestColumns, stored, project, words, contracts.GoingRunWindow.Seconds()))
	if err != nil {
		return goingRequest{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return goingRequest{}, err
	}
	return request, nil
}

// readGoingRequest is one run by id, pgx.ErrNoRows when the record holds none: a run that was
// never asked, or one the sweep has deleted past contracts.GoingRequestTTL. What a run the
// caller does not own answers is the caller's to decide, which is why the row carries its
// Author: model.Actor.SameCredential compares it with the reader's.
func (s *server) readGoingRequest(ctx context.Context, id string) (goingRequest, error) {
	return scanGoingRequest(s.deps.Store.Pool.QueryRow(ctx, `
		select `+goingRequestColumns+` from going_requests where id = $1::uuid
	`, id))
}

// appendGoingEvent stores event as the run's next frame and answers the sequence it took. The
// sequence is read and written in the one statement, as a handoff's version is (handoffs.go), so
// a frame never takes a sequence the record already holds. Only the process running the answer
// appends a frame that does not end the stream, and it appends them one after another; a
// terminal frame goes through endGoingRun, which the table settles. A frame the record stores
// none of - the run is already ended, or the sequence went to another writer - is pgx.ErrNoRows,
// which tells an answer still writing that its run is over.
func (s *server) appendGoingEvent(ctx context.Context, requestID string, event going.Event) (int, error) {
	data, err := json.Marshal(event.Data)
	if err != nil {
		return 0, err
	}
	var sequence int
	err = s.deps.Store.Pool.QueryRow(ctx, goingAppendEvent, requestID, event.Name, data, goingTerminalNames).Scan(&sequence)
	return sequence, err
}

// goingNextEvent inserts one frame at the run's next sequence, read and written in the one
// statement, doing nothing where another writer took that sequence in between and returning no
// row where it stored none.
//
// The having is what keeps a frame of a run the record no longer holds from being an error:
// the TTL delete of another pod's sweep may remove a run between any reader of it and this
// write, and the foreign key would then fail the statement - and, in the sweep's own batch,
// every write queued after it, since that batch is one implicit transaction. A run that is
// gone stores nothing instead, which is pgx.ErrNoRows to the caller: what an ended run already
// answers, and what a writer with nothing left to write is owed.
const goingNextEvent = `
	insert into going_events (request_id, sequence, name, data)
	select $1::uuid, coalesce(max(sequence), 0) + 1, $2::text, $3::jsonb
	from going_events where request_id = $1::uuid
	having exists (select 1 from going_requests where id = $1::uuid)`

// goingAppendEvent is that insert for a frame that does not end the stream: the condition added
// here stores nothing once the run has a terminal frame, so an answer still writing when a
// reader at the deadline or the sweep ended the run under it adds nothing after the frame a
// reader stopped on. bool_or answers null over a run with no frames yet, which coalesce reads
// as the run not having ended.
const goingAppendEvent = goingNextEvent + `
	and not coalesce(bool_or(name = any($4::text[])), false)
	on conflict do nothing
	returning sequence`

// goingEndEvent is that insert for a terminal frame, which asks no more of the run than that it
// still exists: what keeps a run to one ending is the going_events_terminal index, which this
// conflicts with where the run is already ended, and not a read taken before the insert.
const goingEndEvent = goingNextEvent + `
	on conflict do nothing
	returning sequence`

// endGoingRun ends the run with event, a terminal frame, and answers the terminal frame the run
// actually has: the one this call stored, or the one already stored when another writer ended
// the run first. A run is ended once, by the table and not by whoever got there first, so a
// writer that loses reports the stored frame rather than storing its own at the next sequence:
// the answer, the reader at the deadline and the sweep then all say the same thing about one
// run. event must be terminal (going.Event.Terminal); anything else would end no run and is
// refused here rather than stored as a frame no reader stops on.
func (s *server) endGoingRun(ctx context.Context, requestID string, event going.Event) (goingEvent, error) {
	if !event.Terminal() {
		return goingEvent{}, fmt.Errorf("end going run %s: %s is not a terminal event", requestID, event.Name)
	}
	data, err := json.Marshal(event.Data)
	if err != nil {
		return goingEvent{}, err
	}
	for range goingTerminalWrites {
		var sequence int
		err := s.deps.Store.Pool.QueryRow(ctx, goingEndEvent, requestID, event.Name, data).Scan(&sequence)
		if err == nil {
			return goingEvent{Sequence: sequence, Event: event}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return goingEvent{}, err
		}
		stored, err := s.readGoingTerminalEvent(ctx, requestID)
		if err == nil {
			return stored, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return goingEvent{}, err
		}
		// The sequence went to a frame of the run's own, not to another terminal write: the next
		// one is free.
	}
	return goingEvent{}, fmt.Errorf("end going run %s: %d writes each lost their sequence", requestID, goingTerminalWrites)
}

// readGoingTerminalEvent is the one terminal frame a run stored, pgx.ErrNoRows while it has
// stored none. The going_events_terminal index holds that there is at most one.
func (s *server) readGoingTerminalEvent(ctx context.Context, requestID string) (goingEvent, error) {
	return scanGoingEvent(s.deps.Store.Pool.QueryRow(ctx, `
		select `+goingEventColumns+`
		from going_events
		where request_id = $1::uuid and name = any($2::text[])
	`, requestID, goingTerminalNames))
}

// goingEventsAfter is every frame of one run past a sequence, in stream order: $1 the run, $2
// that sequence; scanGoingEvent decodes each row. It is named rather than written inline
// because readGoingStream queues it in a batch beside the state read.
const goingEventsAfter = `
	select ` + goingEventColumns + `
	from going_events
	where request_id = $1::uuid and sequence > $2
	order by sequence`

// goingRunStateOf reads one run's state, $1 the run and $2 the terminal frame names: the
// sequence of its terminal frame, 0 while it has stored none, and whether now is past its
// ends_at. Queued beside goingEventsAfter, for the reason it is named.
const goingRunStateOf = `
	select coalesce(
	         (select e.sequence from going_events e where e.request_id = r.id and e.name = any($2::text[])),
	         0
	       ),
	       now() >= r.ends_at
	from going_requests r
	where r.id = $1::uuid`

// readGoingStream is what one poll of an open stream reads: the frames past after, and the run's
// state, in one round trip (store.Pool.SendBatch), where a poll of a stream costs two. Either
// statement failing is the whole read's failure, which ends the stream, and pgx.ErrNoRows is the
// sweep having deleted the run under its reader.
//
// The two statements take their own snapshots, each at its own moment, so a frame committed
// between them is in one answer and not the other. Which order they run in therefore settles
// nothing, and it is immaterial: a terminal frame in the frames ends the stream on being
// written, and a terminal sequence in the state ends it only once the reader has reached that
// sequence (streamGoingAnswer), so neither order can close a stream that never carried its
// ending.
func (s *server) readGoingStream(ctx context.Context, requestID string, after int) ([]goingEvent, goingRunState, error) {
	var frames []goingEvent
	var state goingRunState
	batch := &pgx.Batch{}
	batch.Queue(goingEventsAfter, requestID, after).Query(func(rows pgx.Rows) error {
		collected, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (goingEvent, error) {
			return scanGoingEvent(row)
		})
		frames = collected
		return err
	})
	batch.Queue(goingRunStateOf, requestID, goingTerminalNames).QueryRow(func(row pgx.Row) error {
		return row.Scan(&state.Terminal, &state.PastDeadline)
	})
	if err := s.deps.Store.Pool.SendBatch(ctx, batch).Close(); err != nil {
		return nil, goingRunState{}, err
	}
	return frames, state, nil
}

// scanGoingRequest decodes one goingRequestColumns row.
func scanGoingRequest(row pgx.Row) (goingRequest, error) {
	var request goingRequest
	var author []byte
	if err := row.Scan(&request.ID, &author, &request.Project, &request.Words, &request.CreatedAt, &request.EndsAt); err != nil {
		return goingRequest{}, err
	}
	if err := json.Unmarshal(author, &request.Author); err != nil {
		return goingRequest{}, fmt.Errorf("decode going request author: %w", err)
	}
	return request, nil
}

// scanGoingEvent decodes one goingEventColumns row. The payload stays the JSON the row holds
// (json.RawMessage, which marshals to exactly those bytes) rather than one of going's payload
// types: the frame a reader is written is then the frame that was stored, whatever wrote it, and
// nothing here needs a second switch over the event names to pick a type to decode into.
func scanGoingEvent(row pgx.Row) (goingEvent, error) {
	var event goingEvent
	var data []byte
	if err := row.Scan(&event.Sequence, &event.Event.Name, &data); err != nil {
		return goingEvent{}, err
	}
	event.Event.Data = json.RawMessage(data)
	return event, nil
}

// sweepGoingRequests ends every run that is past its ends_at with no terminal frame stored, with
// the deadline's error (contracts.GoingErrorAnswerDidNotArrive), and then deletes every run
// older than contracts.GoingRequestTTL, its frames going with it (on delete cascade). Ending
// those runs is what nothing else does: the pod that was answering one may have died, and
// nobody may be reading it.
//
// Each overdue run is ended by goingEndEvent, the statement every other terminal write uses, so
// the sweep holds no second copy of how a frame takes its next sequence: the ids are read
// first, then one queued write each, which is one round trip for the pass. A run a reader or
// the answer itself ended between the read and the write keeps that frame, since the write does
// nothing on a conflict; a run whose next sequence went to a frame of its own meanwhile stores
// nothing here and is ended by the next pass, where endGoingRun, which has a reader waiting on
// it, tries the sequence after it at once. A run deleted between the read and the write - this
// pass's own delete below runs in another pod too - stores nothing either (goingNextEvent), so
// one missing run neither fails the pass nor costs the runs queued behind it their ending. The
// delete is why a reader that reconnects within the hour still finds its answer, and nothing
// older is kept.
func sweepGoingRequests(ctx context.Context, pool *store.Pool) error {
	deadline := going.Fault(contracts.GoingErrorAnswerDidNotArrive)
	data, err := json.Marshal(deadline.Data)
	if err != nil {
		return err
	}
	rows, err := pool.Query(ctx, `
		select r.id::text
		from going_requests r
		where r.ends_at <= now()
		  and not exists (select 1 from going_events e where e.request_id = r.id and e.name = any($1::text[]))
	`, goingTerminalNames)
	if err != nil {
		return err
	}
	overdue, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if len(overdue) > 0 {
		batch := &pgx.Batch{}
		for _, id := range overdue {
			batch.Queue(goingEndEvent, id, deadline.Name, data)
		}
		if err := pool.SendBatch(ctx, batch).Close(); err != nil {
			return err
		}
	}
	_, err = pool.Exec(ctx, `
		delete from going_requests where created_at < now() - make_interval(secs => $1)
	`, contracts.GoingRequestTTL.Seconds())
	return err
}
