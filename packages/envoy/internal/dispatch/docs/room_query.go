package docs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Room queries are how one envoy-dispatch process asks every other one about a document room and
// waits for the answers (plan D7): whether it holds the room; to make every update its room
// applied durable (flush); or to hand over the updates its room holds that the store does not
// hold yet, and who is connected to it there (share). They travel, as the rest of room sync does,
// through Postgres alone: a query on documentQueryChannel and each member's answer on
// documentAnswerChannel, both sent through the process's NOTIFY sender (roomSyncSender), so a
// request inside a transaction of the shared pool asks and hears back at once, not at its commit.
//
// Wire format, one JSON object per notification:
//
//	query:  {"id": query id, "s": asking process id, "room": artifact id, "kind": "holds"|"flush"|"share"}
//	answer: {"id": query id, "p": answering process id, "holds": bool, "actors": [actor, ...],
//	         "updates": base64 of a Yjs update, "too_large": bool, "part": n, "last": bool}
//
// Every member but the asker answers every query (roomSyncMembers): a member holding no room of
// the document answers {"holds": false} at once. A "flush" is answered only once every update the
// member's room applied before the query arrived has left its room's unstored updates - appended,
// or dropped with a room that failed, whose browsers send it again (roomInstance.unstored) - so
// the asker reads the store knowing it holds them. A "share" is answered at once, appending
// nothing, with "updates": the merge of the member's unstored updates, the one whose append is
// under way included, since an update stays listed until its append has returned
// (StoreUpdateContext). One over roomShareLimit answers "too_large" and no updates. A share's
// answer carries "actors" as well, the distinct actors connected to the member's room, which is
// what names a lost write's participants (lostedit.go); a holds or a flush answer carries none,
// since no asker of those reads them. An answer whose notification would pass
// roomSyncPayloadLimit is split into parts numbered from 0, actors and then the base64 of the
// updates spread across them in order, the last part flagged "last"; the asker puts them back
// together before it reads the answer.
//
// The asker waits for every member's whole answer for at most roomQueryWait, then goes on with
// what it has and logs a WARN naming the members that did not answer: a member on an older build,
// one that just died, or one whose flush could not finish in time. Nothing an asker decides needs
// every answer to be safe: a flush that was not answered leaves the asker reading the store as it
// stands, which every process's catch-up and the append gate keep correct (plan D3), and a share
// that was not answered judges a write's loss without that member's in-flight updates.

// The kinds of room query (see above).
const (
	roomQueryHolds = "holds"
	roomQueryFlush = "flush"
	roomQueryShare = "share"
)

const (
	// roomQueryWait is the longest an asker waits for the other members' answers.
	roomQueryWait = 500 * time.Millisecond
	// roomShareLimit bounds the updates one member shares (bytes of the merged Yjs update, before
	// base64): more is answered too_large, and the asker judges without them. It is about twelve
	// notifications, and a room holding that much its store does not is a room whose appends are
	// failing, not one a write is racing a person's keystrokes on.
	roomShareLimit = 64 << 10
	// roomFlushPoll is how often a member's flush looks again whether the updates it waits for
	// have left its room's unstored updates.
	roomFlushPoll = 2 * time.Millisecond
)

// roomQuery is one documentQueryChannel notification (see the wire format above).
type roomQuery struct {
	ID     string `json:"id"`
	Sender string `json:"s"`
	Room   string `json:"room"`
	Kind   string `json:"kind"`
}

// roomAnswerPart is one documentAnswerChannel notification: a member's answer, or one part of it
// (see the wire format above).
type roomAnswerPart struct {
	ID       string        `json:"id"`
	Process  string        `json:"p"`
	Holds    bool          `json:"holds"`
	Actors   []model.Actor `json:"actors,omitempty"`
	Updates  string        `json:"updates,omitempty"`
	TooLarge bool          `json:"too_large,omitempty"`
	Part     int           `json:"part"`
	Last     bool          `json:"last"`
}

// roomAnswer is one member's whole answer, its parts put back together.
type roomAnswer struct {
	process  string
	holds    bool
	actors   []model.Actor
	updates  []byte
	tooLarge bool
}

// roomQueries is a process's side of the room queries: the queries it is waiting on answers to,
// and what answers the other processes' queries (query, answer: room sync's query and answer
// lanes call them, roomSync.handlers).
type roomQueries struct {
	service *Service
	mu      sync.Mutex
	pending map[string]*pendingRoomQuery
}

// pendingRoomQuery is one query of this process's waiting on answers: the parts of each member's
// answer received so far, keyed by member, until that answer is whole and moves to answers. done
// is closed once no member is waited on.
type pendingRoomQuery struct {
	waiting map[string]*roomAnswerParts
	answers []roomAnswer
	done    chan struct{}
}

// roomAnswerParts is one member's answer as its parts arrive; last is the number of its last
// part, -1 until that part has arrived.
type roomAnswerParts struct {
	parts map[int]roomAnswerPart
	last  int
}

func newRoomQueries(service *Service) *roomQueries {
	return &roomQueries{service: service, pending: make(map[string]*pendingRoomQuery)}
}

// roomQueryMembersStatement reads the members (roomSyncMembersStatement) and, when $2 is true,
// whether the connection reading them holds room $1's advisory lock (lockDocumentRoom, which takes
// pg_advisory_xact_lock on hashtext of the id: Postgres shows that bigint key's high half as the
// lock's classid and its low half as its objid, objsubid 1). One statement, one round trip: a flush
// reads both from inside the asker's transaction.
const roomQueryMembersStatement = `
	select coalesce(array_agg(application_name order by application_name), '{}'),
	       $2::boolean and exists (
	           select 1 from pg_locks
	           where locktype = 'advisory' and pid = pg_backend_pid() and granted and objsubid = 1
	             and classid = ((hashtext($1)::bigint >> 32) & 4294967295)::oid
	             and objid = (hashtext($1)::bigint & 4294967295)::oid)
	` + roomSyncMembership

// readRoomQueryMembers is the members, by process id, through q, and whether q's connection holds
// room's advisory lock, which it reads only when checkLock is set.
func readRoomQueryMembers(ctx context.Context, q Queryer, room string, checkLock bool) (members []string, locked bool, err error) {
	var names []string
	if err := q.QueryRow(ctx, roomQueryMembersStatement, room, checkLock).Scan(&names, &locked); err != nil {
		return nil, false, fmt.Errorf("read the room-sync members: %w", err)
	}
	return roomSyncMemberIDs(names), locked, nil
}

// ask sends a kind query about room to every other member, read through q, and returns the whole
// answers that arrive within roomQueryWait. asked says whether any other member was there to ask.
// A query this process could not send, or whose answers it could not hear - its own listener is
// not connected, so it is no member - is logged and answered by nobody; only a failed read of the
// members, which leaves a transaction q belongs to aborted, is an error.
//
// A flush is never sent while q's connection holds room's advisory lock: every member's append
// waits for that lock, so no member could answer before the wait ran out. A caller that asks for a
// flush holding it is a mistake in this package's lock order (see joinLiveWrite), logged at ERROR,
// and the flush is skipped.
func (r *roomQueries) ask(ctx context.Context, q Queryer, room, kind string) (answers []roomAnswer, asked bool, err error) {
	self := r.service.processID
	members, locked, err := readRoomQueryMembers(ctx, q, room, kind == roomQueryFlush)
	if err != nil {
		return nil, false, err
	}
	others := slices.DeleteFunc(slices.Clone(members), func(member string) bool { return member == self })
	if len(others) == 0 {
		return nil, false, nil
	}
	if locked {
		slog.Error("dispatch: a document room flush was asked for while holding the document's advisory lock, which every other process's append waits on; the flush is skipped",
			"room", room)
		return nil, true, nil
	}
	if !slices.Contains(members, self) {
		slog.Warn("dispatch: this process's room-sync listener is not connected, so it cannot hear the other processes answer a document room query; it goes on without them",
			"room", room, "kind", kind, "members", others)
		return nil, true, nil
	}
	query := roomQuery{ID: uuid.NewString(), Sender: self, Room: room, Kind: kind}
	payload, err := json.Marshal(query)
	if err != nil {
		return nil, true, fmt.Errorf("encode a document room query: %w", err)
	}
	pending := &pendingRoomQuery{waiting: make(map[string]*roomAnswerParts, len(others)), done: make(chan struct{})}
	for _, member := range others {
		pending.waiting[member] = &roomAnswerParts{parts: make(map[int]roomAnswerPart), last: -1}
	}
	r.mu.Lock()
	r.pending[query.ID] = pending
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.pending, query.ID)
		r.mu.Unlock()
	}()
	started := time.Now()
	if err := r.service.roomSyncSender.Notify(ctx, documentQueryChannel, string(payload)); err != nil {
		if ctx.Err() != nil {
			return nil, true, ctx.Err()
		}
		slog.Warn("dispatch: send a document room query to the other processes; going on without their answers",
			"room", room, "kind", kind, "error", err)
		return nil, true, nil
	}
	timer := time.NewTimer(roomQueryWait)
	defer timer.Stop()
	select {
	case <-pending.done:
	case <-timer.C:
	case <-ctx.Done():
		return nil, true, ctx.Err()
	}
	r.mu.Lock()
	answers = pending.answers
	missing := slices.Collect(maps.Keys(pending.waiting))
	r.mu.Unlock()
	if len(missing) > 0 {
		slices.Sort(missing)
		slog.Warn("dispatch: other processes did not answer a document room query in time; going on without them",
			"room", room, "kind", kind, "members", missing, "waited", time.Since(started))
	}
	for _, answer := range answers {
		if answer.tooLarge {
			slog.Warn("dispatch: another process's room holds more not-yet-stored updates than one query shares; judging without them",
				"room", room, "process", answer.process, "limit_bytes", roomShareLimit)
		}
	}
	return answers, true, nil
}

// answer is documentAnswerChannel's handler, on room sync's answer lane: it files one part of a
// member's answer under the query this process is waiting on, and closes that query's wait once
// every member's answer is whole. A part for a query nobody waits on any more, or from a process
// not asked, is dropped.
func (r *roomQueries) answer(_ context.Context, payload string) {
	var part roomAnswerPart
	if err := json.Unmarshal([]byte(payload), &part); err != nil || part.ID == "" || part.Part < 0 {
		slog.Debug("dispatch: drop a document room answer that does not parse", "error", err)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pending := r.pending[part.ID]
	if pending == nil {
		return
	}
	parts := pending.waiting[part.Process]
	if parts == nil {
		return
	}
	parts.parts[part.Part] = part
	if part.Last {
		parts.last = part.Part
	}
	if parts.last < 0 || len(parts.parts) < parts.last+1 {
		return
	}
	answer, err := parts.assemble()
	if err != nil {
		slog.Debug("dispatch: drop a document room answer that does not decode", "process", part.Process, "error", err)
		return
	}
	delete(pending.waiting, part.Process)
	pending.answers = append(pending.answers, answer)
	if len(pending.waiting) == 0 {
		close(pending.done)
	}
}

// assemble puts a member's answer back together from parts 0 through last, every one of which has
// arrived.
func (p *roomAnswerParts) assemble() (roomAnswer, error) {
	answer := roomAnswer{}
	var updates strings.Builder
	for number := 0; number <= p.last; number++ {
		part, ok := p.parts[number]
		if !ok {
			return roomAnswer{}, fmt.Errorf("part %d of %d is missing", number, p.last+1)
		}
		answer.process = part.Process
		answer.holds = answer.holds || part.Holds
		answer.tooLarge = answer.tooLarge || part.TooLarge
		answer.actors = append(answer.actors, part.Actors...)
		updates.WriteString(part.Updates)
	}
	if updates.Len() > 0 {
		decoded, err := base64.StdEncoding.DecodeString(updates.String())
		if err != nil {
			return roomAnswer{}, fmt.Errorf("decode the shared updates: %w", err)
		}
		answer.updates = decoded
	}
	return answer, nil
}

// query is documentQueryChannel's handler, on room sync's query lane. This process's own queries
// are dropped. A holds or share query is answered here; a flush waits for the room's appends on a
// goroutine of its own, bounded by roomQueryWait, so a slow flush never holds up the next query.
func (r *roomQueries) query(ctx context.Context, payload string) {
	var query roomQuery
	if err := json.Unmarshal([]byte(payload), &query); err != nil || query.ID == "" || query.Room == "" {
		slog.Debug("dispatch: drop a document room query that does not parse", "error", err)
		return
	}
	if query.Sender == r.service.processID {
		return
	}
	s := r.service
	live := s.srv.GetDoc(query.Room)
	if live == nil {
		r.send(ctx, query, roomAnswerPart{}, nil, nil)
		return
	}
	answer := roomAnswerPart{Holds: true}
	switch query.Kind {
	case roomQueryShare:
		updates, err := s.unstoredShare(query.Room, live)
		if err != nil {
			slog.Warn("dispatch: merge a document room's not-yet-stored updates to share with another process; sharing none",
				"room", query.Room, "error", err)
			updates = nil
		}
		if len(updates) > roomShareLimit {
			answer.TooLarge, updates = true, nil
		}
		r.send(ctx, query, answer, s.connectedActors(query.Room), updates)
	case roomQueryFlush:
		classes := s.unstoredSnapshot(query.Room, live)
		go func() {
			flushed, cancel := context.WithTimeout(ctx, roomQueryWait)
			defer cancel()
			if err := awaitStored(flushed, s.instanceOf(live), classes); err != nil {
				slog.Warn("dispatch: a document room's updates were not stored in time to answer another process's flush; it is left unanswered",
					"room", query.Room, "asker", query.Sender, "waiting", len(classes), "error", err)
				return
			}
			r.send(ctx, query, answer, nil, nil)
		}()
	default:
		r.send(ctx, query, answer, nil, nil)
	}
}

// send sends answer, to query, with actors and updates, in as many parts as their size takes
// (roomAnswerPayloads).
func (r *roomQueries) send(ctx context.Context, query roomQuery, answer roomAnswerPart, actors []model.Actor, updates []byte) {
	answer.ID, answer.Process = query.ID, r.service.processID
	payloads, err := roomAnswerPayloads(answer, actors, base64.StdEncoding.EncodeToString(updates))
	if err != nil {
		slog.Error("dispatch: encode a document room answer", "room", query.Room, "error", err)
		return
	}
	sending, cancel := context.WithTimeout(ctx, roomSyncSendTimeout)
	defer cancel()
	for _, payload := range payloads {
		if err := r.service.roomSyncSender.Notify(sending, documentAnswerChannel, payload); err != nil {
			if !errors.Is(err, context.Canceled) {
				slog.Warn("dispatch: answer another process's document room query", "room", query.Room, "asker", query.Sender, "error", err)
			}
			return
		}
	}
}

// roomAnswerPayloads packs answer with actors and updates (base64) into the fewest notification
// payloads each within roomSyncPayloadLimit: the actors first, whole, then the updates cut across
// as many parts as they fill, each part numbered and the last flagged. An actor whose entry alone
// does not fit is dropped with a WARN.
func roomAnswerPayloads(answer roomAnswerPart, actors []model.Actor, updates string) ([]string, error) {
	var payloads []string
	part := answer
	part.Part, part.Actors, part.Updates, part.Last = 0, nil, "", false
	size := func() (int, error) {
		encoded, err := json.Marshal(part)
		return len(encoded), err
	}
	next := func() error {
		encoded, err := json.Marshal(part)
		if err != nil {
			return err
		}
		payloads = append(payloads, string(encoded))
		part.Part, part.Actors, part.Updates = part.Part+1, nil, ""
		return nil
	}
	for _, actor := range actors {
		part.Actors = append(part.Actors, actor)
		fits, err := size()
		if err != nil {
			return nil, err
		}
		if fits <= roomSyncPayloadLimit {
			continue
		}
		part.Actors = part.Actors[:len(part.Actors)-1]
		if len(part.Actors) > 0 {
			if err := next(); err != nil {
				return nil, err
			}
			part.Actors = []model.Actor{actor}
			if fits, err = size(); err != nil {
				return nil, err
			} else if fits <= roomSyncPayloadLimit {
				continue
			}
			part.Actors = nil
		}
		slog.Warn("dispatch: drop an actor too large for a document room answer", "kind", actor.Kind, "id", actor.ID)
	}
	// The updates field's own framing, `,"updates":""`; base64 needs no JSON escaping.
	const updatesFraming = len(`,"updates":""`)
	for updates != "" {
		used, err := size()
		if err != nil {
			return nil, err
		}
		room := roomSyncPayloadLimit - used - updatesFraming
		if room <= 0 {
			if err := next(); err != nil {
				return nil, err
			}
			continue
		}
		cut := min(room, len(updates))
		part.Updates, updates = updates[:cut], updates[cut:]
		if updates != "" {
			if err := next(); err != nil {
				return nil, err
			}
		}
	}
	part.Last = true
	if err := next(); err != nil {
		return nil, err
	}
	return payloads, nil
}

// connectedActors is the distinct actors connected to room on this process.
func (s *Service) connectedActors(room string) []model.Actor {
	state := s.lockExistingState(room)
	if state == nil {
		return nil
	}
	distinct := make(map[string]model.Actor, len(state.connected))
	for _, actor := range state.connected {
		distinct[actorKey(actor)] = actor
	}
	s.unlockState(room, state)
	return actorSlice(distinct)
}

// unstoredSnapshot is every update live, room's resident instance, applied that has not left its
// unstored updates yet (roomInstance.unstored), after draining the room's update log
// (observeRoomUpdates) so every update committed to the room by now is classified, and listed.
func (s *Service) unstoredSnapshot(room string, live *crdt.Doc) []*queuedClass {
	s.observeRoomUpdates(room, live)
	instance := s.instanceOf(live)
	if instance == nil {
		return nil
	}
	return instance.unstoredUpdates()
}

// unstoredShare is what a share answers: the merge of every update live, room's resident instance,
// holds that the store may not (unstoredSnapshot), nil for none.
func (s *Service) unstoredShare(room string, live *crdt.Doc) ([]byte, error) {
	classes := s.unstoredSnapshot(room, live)
	if len(classes) == 0 {
		return nil, nil
	}
	updates := make([][]byte, 0, len(classes))
	for _, class := range classes {
		updates = append(updates, class.update)
	}
	return mergeUpdates(updates)
}

// awaitStored returns once none of classes is among instance's unstored updates any more: each
// was appended, or dropped with a room that failed (StoreUpdateContext). It looks every
// roomFlushPoll until ctx ends.
func awaitStored(ctx context.Context, instance *roomInstance, classes []*queuedClass) error {
	if instance == nil || len(classes) == 0 {
		return nil
	}
	ticker := time.NewTicker(roomFlushPoll)
	defer ticker.Stop()
	for {
		listed := instance.unstoredUpdates()
		if !slices.ContainsFunc(classes, func(class *queuedClass) bool { return slices.Contains(listed, class) }) {
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
