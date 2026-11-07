package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// maxEventReplay is one page of the event log readEventsAfterID reads: an events subscription's
// catch-up and the relay's read each page through the log at it.
const maxEventReplay = 1000

const eventSelect = `select e.id, e.issue_key, e.artifact_id::text, coalesce(e.project_key, i.project_key, ar.project_key, ''), e.seq, e.type, e.actor, e.notify, e.created_at, e.payload
	from events e
	left join issues i on i.key = e.issue_key
	left join artifacts ar on ar.id = e.artifact_id`

// listIssueEvents answers an issue's activity feed under an ETag of metadata alone: every append
// to this feed advances the issue's last_seq in the append's own transaction (events.Broker), so
// the issue row this is the feed of, that column, the paging query that chooses the page and the
// variant lines both lists' ETags carry are the whole of what decides the page. The feed's
// validator is therefore read before the page is: a reader holding the ETag is answered 304 for
// one short statement, and one that is not is answered from the kept bytes when the cache holds
// them, having built, encoded and compressed nothing (list_answers.go).
//
// What the validator does not cover is the handful of fields an ask event's payload is given at
// read time from rows elsewhere (attachAskEventFields): an inbound backlink counted on another
// issue's write moves no sequence of this one, so a reader is answered 304 while that count has
// moved. Those fields are an ask's own facts rather than the feed's, and the Inbox and the ask
// read answer them fresh.
func (s *server) listIssueEvents(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	options, err := parseEventListOptions(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	lastSeq, createdAt, err := s.issueEventSequence(r.Context(), r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	trees := wantsDisplayTrees(r)
	etag := eventFeedETag(lastSeq, createdAt, options, trees)
	if answerNotModified(w, r, etag) {
		return
	}
	key := listAnswerKeyFor(r, eventFeedRoute, r.PathValue("key"), etag)
	if s.listAnswers.answer(w, key) {
		return
	}
	events, err := s.readEvents(r.Context(), issueOwner(r.PathValue("key")), options)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var texts []string
	if trees {
		texts = eventDisplayTexts(nil, events...)
	}
	defer s.readDisplayTrees(r.Context(), trees, r.PathValue("key"), texts)()
	attachEventTrees(events, trees)
	encoded, err := encodeListAnswer(events)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.writeListAnswer(w, key, encoded)
}

// eventFeedETag hashes what one page of an issue's activity feed is made of: the issue whose feed
// it is, that issue's newest event sequence and the query that pages it, closed with the lines
// both lists' ETags carry (listETag). It is referencesETag's shape - one line per part, then the
// hash of them - and it names no issue key: a validator is compared against the one URL that gave
// it.
//
// The issue is named by its created_at rather than its key, because the key is the one part of it
// that comes back. last_seq is a counter of the row, so a key taken by a new issue stands at
// sequences the issue before it stood at, and nothing else in the hash would tell the two feeds
// apart: a reader of the new issue would be answered 304, or written the bytes this server kept
// for the old one. That is what the browser harness does between scenarios - it empties `issues`
// and restarts each project's numbering, so every scenario's issues take the keys of the last
// one's (packages/dispatch/e2e/seed.ts) - and it is the reading a restored database gives a
// process that outlived the restore. The comment list is held to the same rule by a counter that
// never comes back with the key (migration 20261106020619); the feed's column does come back, so
// the row's birth stamp is hashed in beside it. It is the same fact, read the same way, that
// tells the event relay an event log that was reset under its cursor from one that merely went
// quiet (eventLogWasReset).
func eventFeedETag(lastSeq int, createdAt time.Time, options eventListOptions, trees bool) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "created=%d\nlast_seq=%d\nafter=%d\nbefore=%d\nhas_before=%t\ndescending=%t\nlimit=%d\n",
		createdAt.UnixMicro(), lastSeq, options.after, options.before, options.hasBefore, options.descending, options.limit)
	for _, id := range options.ids {
		_, _ = fmt.Fprintf(hash, "id=%d\n", id)
	}
	return listETag(hash, trees)
}

// issueEventSequence is the newest event sequence of the issue key names and the created_at that
// tells that issue from the next one to take its key, both zero for a key no issue holds, whose
// feed is the empty page readEvents answers rather than a refusal. Every append to the issue
// advances the column in the append's own transaction (events/broker.go), and nothing moves the
// stamp, so this one statement is the whole of what the feed's validator reads.
func (s *server) issueEventSequence(ctx context.Context, key string) (int, time.Time, error) {
	var lastSeq int
	var createdAt time.Time
	err := s.deps.Store.Pool.QueryRow(ctx,
		`select last_seq, created_at from issues where key = $1`, key).Scan(&lastSeq, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, nil
	}
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("read the issue's event sequence: %w", err)
	}
	return lastSeq, createdAt, nil
}

func parseEventListOptions(r *http.Request) (eventListOptions, error) {
	query := r.URL.Query()
	hasBefore := query.Has("before")
	if hasBefore && strings.TrimSpace(query.Get("before")) == "" {
		return eventListOptions{}, errorf(http.StatusBadRequest, "INVALID_QUERY", "before must be a non-negative integer")
	}
	after, err := parseNonNegativeInt(query.Get("after"), "after")
	if err != nil {
		return eventListOptions{}, err
	}
	before, err := parseNonNegativeInt(query.Get("before"), "before")
	if err != nil {
		return eventListOptions{}, err
	}
	ids, err := parseEventIDs(query.Get("ids"))
	if err != nil {
		return eventListOptions{}, err
	}
	descending, err := parseEventOrder(query.Get("order"))
	if err != nil {
		return eventListOptions{}, err
	}
	limit, err := parseEventLimit(query.Get("limit"))
	if err != nil {
		return eventListOptions{}, err
	}
	if query.Has("after") && (hasBefore || descending) {
		return eventListOptions{}, errorf(http.StatusBadRequest, "INVALID_QUERY", "after cannot be combined with before or order=desc")
	}
	return eventListOptions{
		after:      after,
		before:     before,
		hasBefore:  hasBefore,
		descending: descending || hasBefore,
		ids:        ids,
		limit:      limit,
	}, nil
}

// eventLogUnbrokenThrough is the id before the first one missing from the event log counted up
// from 1, or its newest id when none is missing: every id at or below it is a committed event.
// It reads the whole log, so feedEvents asks it only of a log that was reset beneath the relay,
// whose ids restart at 1 (`truncate events restart identity`) and which is as short as the time
// since that reset.
func (s *server) eventLogUnbrokenThrough(ctx context.Context) (int64, error) {
	var through int64
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select coalesce(
			(select min(n) - 1 from (select id, row_number() over (order by id) as n from events) numbered where id <> n),
			(select max(id) from events),
			0)
	`).Scan(&through)
	return through, err
}

// eventLogHead is the event log's newest committed id, 0 for an empty log. It is no cursor: ids
// are not commit order, so a lower id can still be open below it. feedEvents only compares a
// page's since and the relay's cursor against it, to tell an id of this log from one of a log it
// replaced.
func (s *server) eventLogHead(ctx context.Context) (int64, error) {
	var head int64
	err := s.deps.Store.Pool.QueryRow(ctx, `select coalesce(max(id), 0) from events`).Scan(&head)
	return head, err
}

type eventListOptions struct {
	after      int64
	before     int64
	hasBefore  bool
	descending bool
	ids        []int64
	limit      int
}

func (s *server) readEvents(ctx context.Context, owner owner, options eventListOptions) ([]model.Event, error) {
	var predicate, ownerValue string
	switch {
	case owner.IssueKey != nil:
		predicate, ownerValue = "e.issue_key = $1", *owner.IssueKey
	case owner.ArtifactID != nil:
		predicate, ownerValue = "e.artifact_id = $1", *owner.ArtifactID
	default:
		return nil, errorf(http.StatusBadRequest, "OWNER_INVALID", "owner requires exactly one issue or artifact")
	}
	if len(options.ids) > 0 {
		return s.readEventRows(ctx, eventSelect+`
			where `+predicate+` and e.id = any($2)
			order by e.seq asc
		`, ownerValue, options.ids)
	}
	if options.descending {
		if options.hasBefore {
			return s.readEventRows(ctx, eventSelect+`
				where `+predicate+` and e.seq < $2
				order by e.seq desc limit $3
			`, ownerValue, options.before, options.limit)
		}
		return s.readEventRows(ctx, eventSelect+`
			where `+predicate+`
			order by e.seq desc limit $2
		`, ownerValue, options.limit)
	}
	return s.readEventRows(ctx, eventSelect+`
		where `+predicate+` and e.seq > $2
		order by e.seq asc limit $3
	`, ownerValue, options.after, options.limit)
}

// readEventsAfterID reads one page of the global event log in id order: the events whose ids are
// in (after, through]. Ids are not commit order, so a reader that moves its cursor to a page's
// last row passes a through at or below the safe horizon (events.Horizon), where every event is
// final: no lower id the page skipped can commit later.
func (s *server) readEventsAfterID(ctx context.Context, after, through int64) ([]model.Event, error) {
	return s.readEventRows(ctx, eventSelect+`
		where e.id > $1 and e.id <= $2
		order by e.id asc limit $3
	`, after, through, maxEventReplay)
}

// eventFeedHeadWait bounds how long eventFeedHead waits on a process that has proven no horizon
// yet, while no mark it keeps counts (an event insert of this database open since before every
// mark). It waits nowhere else.
const eventFeedHeadWait = 2 * time.Second

// ErrEventHeadUnproven is eventFeedHead's answer when it has no id it can vouch for: GET
// /auth/whoami answers it, and only it, 503 EVENT_HEAD_UNAVAILABLE, which the page asks again for
// as long as it lasts. A failed read of the database or the horizon's pool is another error, which
// whoami answers 503 EVENT_HEAD_READ_FAILED and the page retries on its bounded rule.
var ErrEventHeadUnproven = errors.New("no event id is proven final yet: an event insert has been open since before every mark")

// eventFeedHead is the event_head GET /auth/whoami answers, which a page resumes its first events
// subscription from: an id at or below which every event is final, so a page that starts its reads
// after it and is replayed every event above it misses nothing that commits later.
//
// It answers at once (server.writerFloor): the newest of the server's marks that counts, a mark
// older than every transaction that holds a write lock on events, or the horizon already proven
// here or persisted by any process when none counts. A mark taken a moment ago counts as soon as the event
// writes open then have ended, which for this service's own writes is milliseconds, so the head
// trails the log's newest id by about that. A transaction that holds a transaction id and has
// inserted no event, here or in another database, keeps nothing waiting.
//
// It never answers 0 for a log it cannot vouch for: a process with no mark counting and no horizon
// proven or persisted (a first boot, a reset log) would send its page to replay the whole log. That
// one waits, eventFeedHeadWait at most, marking again, and past it answers ErrEventHeadUnproven,
// which is logged and which the page asks again for.
func (s *server) eventFeedHead(ctx context.Context) (int64, error) {
	floor, ok, _, err := s.writerFloor(ctx)
	if err != nil || ok || floor > 0 {
		return floor, err
	}
	deadline := time.Now().Add(eventFeedHeadWait)
	for wait := 10 * time.Millisecond; time.Now().Before(deadline); wait = min(2*wait, 250*time.Millisecond) {
		if !waitFor(ctx, wait) {
			return 0, ctx.Err()
		}
		if floor, ok, _, err = s.writerFloor(ctx); err != nil || ok || floor > 0 {
			return floor, err
		}
	}
	slog.Warn("dispatch: whoami has no event_head to answer: no horizon is proven yet and an event insert of this database has been open since before every mark", "wait", eventFeedHeadWait)
	return 0, ErrEventHeadUnproven
}

func (s *server) readEventRows(ctx context.Context, query string, arguments ...any) ([]model.Event, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []model.Event{}
	for rows.Next() {
		var event model.Event
		var actor, payload []byte
		if err := rows.Scan(
			&event.ID,
			&event.IssueKey,
			&event.ArtifactID,
			&event.Project,
			&event.Seq,
			&event.Type,
			&actor,
			&event.Notify,
			&event.CreatedAt,
			&payload,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(actor, &event.Actor); err != nil {
			return nil, fmt.Errorf("decode event actor: %w", err)
		}
		event.Payload = json.RawMessage(payload)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.attachAskEventFields(ctx, events); err != nil {
		return nil, err
	}
	return events, nil
}

// decodedAskEventPayload is the ask payload that needs read-time fields added before it is written
// to an event response. Other event payloads remain their database JSON.
type decodedAskEventPayload struct {
	event   *model.Event
	payload map[string]any
}

// attachAskEventFields fills the ask fields an event payload cannot carry from the write that
// appended it: the id of the event that opened the ask, its inbound backlink count, the part ids
// of the options and answer it shows with the ask's part revision, and its anchor document. Each
// is the ask's fact now, not the write's. Ask payloads are decoded because those fields are
// added, and an ask.answered payload's selected lists are read as model.AskAnswer reads them
// (normalizeAnsweredSelected); every other payload passes through as the database returned it.
func (s *server) attachAskEventFields(ctx context.Context, events []model.Event) error {
	asks := []model.Ask{}
	payloads := []decodedAskEventPayload{}
	for index := range events {
		switch events[index].Type {
		case "ask.opened", "ask.anchor_refreshed", "ask.answered", "ask.resolved", "ask.edited", "ask.handed_back":
		default:
			continue
		}
		raw, ok := events[index].Payload.(json.RawMessage)
		if !ok {
			return fmt.Errorf("decode %s payload: expected raw JSON", events[index].Type)
		}
		payload := map[string]any{}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode %s payload: %w", events[index].Type, err)
		}
		if events[index].Type == "ask.answered" {
			if err := normalizeAnsweredSelected(raw, payload); err != nil {
				return err
			}
		}
		askID, ok := payload["id"].(string)
		if !ok || askID == "" {
			return fmt.Errorf("decode %s payload: ask id missing", events[index].Type)
		}
		shell, err := askEventShell(askID, raw)
		if err != nil {
			return err
		}
		asks = append(asks, shell)
		payloads = append(payloads, decodedAskEventPayload{event: &events[index], payload: payload})
	}
	askPointers := make([]*model.Ask, len(asks))
	for index := range asks {
		askPointers[index] = &asks[index]
	}
	if err := loadAskDetails(ctx, s.deps.Store.Pool, askPointers); err != nil {
		return err
	}
	for index := range payloads {
		payloads[index].payload["opened_event_id"] = *asks[index].OpenedEventID
		payloads[index].payload[model.ReferencedByCountKey] = *asks[index].ReferencedByCount
		attachAskEventPartIDs(payloads[index].payload, asks[index])
	}
	if err := s.attachAskDocumentArtifacts(ctx, payloads); err != nil {
		return err
	}
	for _, payload := range payloads {
		encoded, err := json.Marshal(payload.payload)
		if err != nil {
			return fmt.Errorf("encode %s payload: %w", payload.event.Type, err)
		}
		payload.event.Payload = json.RawMessage(encoded)
	}
	return nil
}

// askEventShell is the ask attachAskPartIDs matches the stored parts against: its id and the
// options and the answer this payload itself carries, as the write recorded them. Their text is
// what identifies a part, so a payload the ask has moved past - an ask.opened whose options an
// edit has since replaced, an ask.answered superseded by another answer - matches no part of the
// ask now and carries no id. The payload's own fields are never rewritten from the ask's row;
// only the ids attachAskEventPartIDs writes are added.
func askEventShell(askID string, raw json.RawMessage) (model.Ask, error) {
	var shell struct {
		Options []model.AskOption `json:"options"`
		Answer  *model.AskAnswer  `json:"answer"`
	}
	if err := json.Unmarshal(raw, &shell); err != nil {
		return model.Ask{}, fmt.Errorf("decode the options and answer of ask %s: %w", askID, err)
	}
	return model.Ask{ID: askID, Options: shell.Options, Answer: shell.Answer}, nil
}

// attachAskEventPartIDs writes the part ids attachAskPartIDs read onto the payload's own options
// and answer, with the ask's part revision beside them, so a payload names each option it shows
// as every other read of the ask names it. An option or an answer no part of the ask stands for
// now - an ask.opened of an ask whose options an edit has since replaced, a superseded answer -
// carries no id, and an ask whose parts are not indexed yet no revision either: attachAskPartIDs
// filled neither.
func attachAskEventPartIDs(payload map[string]any, ask model.Ask) {
	if ask.Revision > 0 {
		payload["revision"] = ask.Revision
	}
	options, _ := payload["options"].([]any)
	for index, option := range options {
		entry, decoded := option.(map[string]any)
		if !decoded || ask.Options[index].ID == "" {
			continue
		}
		entry["id"] = ask.Options[index].ID
	}
	if ask.Answer == nil || ask.Answer.ID == "" {
		return
	}
	if answer, carried := payload["answer"].(map[string]any); carried {
		answer["id"] = ask.Answer.ID
	}
}

// normalizeAnsweredSelected writes model.AskAnswer's reading of selected into an ask.answered
// payload's answer and previous_answer: an ask.answered event written before answers stored an
// empty list holds null there, and a client reads the list's length. Every other field of either
// answer stays as the payload holds it, and an answer the payload does not carry stays absent.
func normalizeAnsweredSelected(raw json.RawMessage, payload map[string]any) error {
	var answers struct {
		Answer         *model.AskAnswer `json:"answer"`
		PreviousAnswer *model.AskAnswer `json:"previous_answer"`
	}
	if err := json.Unmarshal(raw, &answers); err != nil {
		return fmt.Errorf("decode ask.answered answers: %w", err)
	}
	setSelected := func(key string, answer *model.AskAnswer) {
		if fields, ok := payload[key].(map[string]any); ok && answer != nil {
			fields["selected"] = answer.Selected
		}
	}
	setSelected("answer", answers.Answer)
	setSelected("previous_answer", answers.PreviousAnswer)
	return nil
}

// attachAskDocumentArtifacts hydrates anchor_artifact and approval_artifact on every ask event
// payload that names one, the same live lookup ask_rows.go's scanAskRow does for a read: an
// anchored ask's quoted document, or an approval request's document, both resolved by their
// current slug and primary flag rather than a value frozen at write time.
func (s *server) attachAskDocumentArtifacts(ctx context.Context, eventPayloads []decodedAskEventPayload) error {
	payloads := map[string][]decodedAskEventPayload{}
	for _, eventPayload := range eventPayloads {
		payload := eventPayload.payload
		_, hasAnchor := payload["anchor"].(map[string]any)
		_, hasApproval := payload["approval"].(map[string]any)
		if !hasAnchor && !hasApproval {
			if payload["anchor"] != nil {
				return fmt.Errorf("decode %s payload: anchor must be an object", eventPayload.event.Type)
			}
			continue
		}
		askID, ok := payload["id"].(string)
		if !ok || askID == "" {
			return fmt.Errorf("decode %s payload: ask id missing", eventPayload.event.Type)
		}
		payloads[askID] = append(payloads[askID], eventPayload)
	}
	if len(payloads) == 0 {
		return nil
	}
	askIDs := make([]string, 0, len(payloads))
	for askID := range payloads {
		askIDs = append(askIDs, askID)
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select a.id::text, aa.project_key, aa.slug, aa.name, aa.is_primary,
			pa.project_key, pa.slug, pa.name, pa.is_primary
		from asks a
		left join artifacts aa on aa.id = `+store.CastableUUID(`a.anchor->>'artifact_id'`)+`
		left join artifacts pa on pa.id = (a.approval->>'artifact_id')::uuid
		where a.id::text = any($1)
	`, askIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	anchorArtifacts := make(map[string]model.AskAnchorArtifact, len(payloads))
	approvalArtifacts := make(map[string]model.AskAnchorArtifact, len(payloads))
	for rows.Next() {
		var askID string
		var anchor, approval nullableAskArtifactRef
		if err := rows.Scan(
			&askID,
			&anchor.project, &anchor.slug, &anchor.name, &anchor.primary,
			&approval.project, &approval.slug, &approval.name, &approval.primary,
		); err != nil {
			return err
		}
		if artifact := anchor.artifact(); artifact != nil {
			anchorArtifacts[askID] = *artifact
		}
		if artifact := approval.artifact(); artifact != nil {
			approvalArtifacts[askID] = *artifact
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for askID, asks := range payloads {
		anchorArtifact, anchorFound := anchorArtifacts[askID]
		approvalArtifact, approvalFound := approvalArtifacts[askID]
		for _, ask := range asks {
			delete(ask.payload, "anchor_artifact")
			delete(ask.payload, "approval_artifact")
			if anchorFound {
				ask.payload["anchor_artifact"] = anchorArtifact
			}
			if approvalFound {
				ask.payload["approval_artifact"] = approvalArtifact
			}
		}
	}
	return nil
}

func parseNonNegativeInt(raw, field string) (int64, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, errorf(http.StatusBadRequest, "INVALID_QUERY", "%s must be a non-negative integer", field)
	}
	return value, nil
}

func parseEventLimit(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 200, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 200 {
		return 0, errorf(http.StatusBadRequest, "INVALID_QUERY", "limit must be between 1 and 200")
	}
	return limit, nil
}

func parseEventOrder(raw string) (bool, error) {
	switch strings.TrimSpace(raw) {
	case "", "asc":
		return false, nil
	case "desc":
		return true, nil
	default:
		return false, errorf(http.StatusBadRequest, "INVALID_QUERY", "order must be asc or desc")
	}
}

func parseEventIDs(raw string) ([]int64, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 50 {
		return nil, errorf(http.StatusBadRequest, "INVALID_QUERY", "ids must include at most 50 values")
	}
	ids := make([]int64, len(parts))
	for index, part := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id < 1 {
			return nil, errorf(http.StatusBadRequest, "INVALID_QUERY", "ids must be positive integers")
		}
		ids[index] = id
	}
	return ids, nil
}
