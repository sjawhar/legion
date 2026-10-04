package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type userIssueState struct {
	Pinned      bool     `json:"pinned"`
	LastReadSeq int      `json:"last_read_seq"`
	Dismissed   []string `json:"dismissed"`
	Seq         int64    `json:"seq"`
}

func (s *server) getUserState(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select issue_key, pinned, last_read_seq, dismissed, seq
		from user_issue_state where login = $1 order by issue_key
	`, actor.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	state := map[string]userIssueState{}
	for rows.Next() {
		var key string
		var value userIssueState
		var dismissed []byte
		if err := rows.Scan(&key, &value.Pinned, &value.LastReadSeq, &dismissed, &value.Seq); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := json.Unmarshal(dismissed, &value.Dismissed); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		state[key] = value
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, state)
}

func (s *server) putUserState(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Pinned      *bool        `json:"pinned"`
		LastReadSeq *int         `json:"last_read_seq"`
		Dismissed   *[]string    `json:"dismissed"`
		Seq         *int64       `json:"seq"`
		Actor       *model.Actor `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	if actor.Kind != "user" {
		writeError(w, "HUMAN_ONLY", http.StatusForbidden, "user state is only available to users")
		return
	}
	if input.LastReadSeq != nil && *input.LastReadSeq < 0 {
		writeError(w, "INVALID_STATE", http.StatusBadRequest, "last_read_seq must be non-negative")
		return
	}
	key := r.PathValue("key")
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `
		insert into user_issue_state (login, issue_key) values ($1, $2)
		on conflict (login, issue_key) do nothing
	`, actor.ID, key); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	value := userIssueState{}
	var encodedDismissed []byte
	if err := tx.QueryRow(r.Context(), `
		select pinned, last_read_seq, dismissed, seq
		from user_issue_state where login = $1 and issue_key = $2 for update
	`, actor.ID, key).Scan(&value.Pinned, &value.LastReadSeq, &encodedDismissed, &value.Seq); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := json.Unmarshal(encodedDismissed, &value.Dismissed); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.Seq != nil && *input.Seq <= value.Seq {
		WriteJSON(w, http.StatusConflict, map[string]any{
			"code":  "STATE_STALE",
			"error": "user state was updated by another write",
			"state": value,
		})
		return
	}
	if input.Pinned != nil {
		value.Pinned = *input.Pinned
	}
	if input.LastReadSeq != nil {
		value.LastReadSeq = *input.LastReadSeq
	}
	if input.Dismissed != nil {
		value.Dismissed = *input.Dismissed
	}
	if input.Seq != nil {
		value.Seq = *input.Seq
	}
	encodedDismissed, err = encodeJSON(value.Dismissed)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `
		update user_issue_state
		set pinned = $3, last_read_seq = $4, dismissed = $5, seq = $6
		where login = $1 and issue_key = $2
	`, actor.ID, key, value.Pinned, value.LastReadSeq, encodedDismissed, value.Seq); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var project string
	if err := tx.QueryRow(r.Context(), `select project_key from issues where key = $1`, key).Scan(&project); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, projectOwner(project).event(
		"user_state.updated", actor, map[string]any{"login": actor.ID, "state": value},
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	WriteJSON(w, http.StatusOK, value)
}

// userAgentState is one viewer's state for one agent's conversation. The Agents page hides every
// exchange whose newest message is at or before cleared_before (the viewer's Clear); read_through
// is how far the viewer has read; unread_replies counts the session's replies to messages this
// viewer sent that are newer than both and that the viewer has not read by id (read_replies),
// which is how the dashboard says an agent answered.
type userAgentState struct {
	ClearedBefore *string `json:"cleared_before,omitempty"`
	ReadThrough   *string `json:"read_through,omitempty"`
	UnreadReplies int     `json:"unread_replies"`
}

// agentStateCutoffSkew is how far ahead of the server clock a Clear or a read mark may land. The
// client stamps the cutoff with its own clock, and a browser a few seconds fast must not be
// refused.
const agentStateCutoffSkew = time.Minute

// userAgentStatesQuery reads a viewer's per-session state from the shared unreadDirectRepliesCTE
// alone: `direct_marks` is every session with a Clear (user_agent_state) or a read mark
// (user_agent_read), and `unread_direct_replies` counted per session is every session holding a
// reply the viewer has not read. Parameters are the fragment's own (unreadDirectRepliesArgs).
const userAgentStatesQuery = `
	with recursive` + unreadDirectRepliesCTE + `,
	unread as (
		select session_id, count(*)::int as unread from unread_direct_replies group by session_id
	)
	select coalesce(direct_marks.session_id, unread.session_id),
	       direct_marks.cleared_before, direct_marks.read_through,
	       coalesce(unread.unread, 0)
	from direct_marks full join unread on unread.session_id = direct_marks.session_id
	order by 1
`

// loadUserAgentStates runs userAgentStatesQuery for login, narrowed to sessionID when it is
// not nil. The read mark and the viewer's own direct messages are matched on the canonical
// login, so one person is one viewer however their identity source spells them; the Clear
// (user_agent_state, migration 0033) is keyed on the raw actor id.
func (s *server) loadUserAgentStates(ctx context.Context, login string, sessionID *string) (map[string]userAgentState, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, userAgentStatesQuery, unreadDirectRepliesArgs(login, sessionID)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := map[string]userAgentState{}
	for rows.Next() {
		var session string
		var clearedBefore, readThrough *time.Time
		var state userAgentState
		if err := rows.Scan(&session, &clearedBefore, &readThrough, &state.UnreadReplies); err != nil {
			return nil, err
		}
		state.ClearedBefore = timestampPtr(clearedBefore)
		state.ReadThrough = timestampPtr(readThrough)
		states[session] = state
	}
	return states, rows.Err()
}

func (s *server) getUserAgentState(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	states, err := s.loadUserAgentStates(r.Context(), actor.ID, nil)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, states)
}

// parseAgentStateCutoff reads one optional cutoff of a PUT: absent is nil, anything else must
// be an RFC3339 timestamp no further ahead of the server clock than agentStateCutoffSkew.
func parseAgentStateCutoff(name string, value *string) (*time.Time, error) {
	if value == nil {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, *value)
	if err != nil {
		return nil, errorf(http.StatusBadRequest, "INVALID_STATE", "%s must be an RFC3339 timestamp", name)
	}
	if parsed.After(time.Now().Add(agentStateCutoffSkew)) {
		return nil, errorf(http.StatusBadRequest, "INVALID_STATE", "%s must not be in the future", name)
	}
	return &parsed, nil
}

// putUserAgentState records a Clear (cleared_before, which replaces the previous one), a read
// mark (read_through, which only ever moves forward, so a tab that read less a moment ago cannot
// make a reply unread again), and/or replies read one by one (read_replies, the session's own
// messages by id, which a view that shows only some of a session's replies writes so it marks
// nothing it did not show), announces the write to the viewer's other tabs unless it was only
// replies already read by id or passed by the read mark (one the Clear hides is neither), and
// answers with the session's whole state.
func (s *server) putUserAgentState(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	var input struct {
		ClearedBefore *string  `json:"cleared_before"`
		ReadThrough   *string  `json:"read_through"`
		ReadReplies   []string `json:"read_replies"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.ClearedBefore == nil && input.ReadThrough == nil && len(input.ReadReplies) == 0 {
		writeError(w, "INVALID_STATE", http.StatusBadRequest, "cleared_before, read_through or read_replies is required")
		return
	}
	clearedBefore, err := parseAgentStateCutoff("cleared_before", input.ClearedBefore)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	readThrough, err := parseAgentStateCutoff("read_through", input.ReadThrough)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// An id is taken in any form uuid.Parse reads and sent to Postgres in the canonical one, which
	// Postgres takes; a urn:uuid: id, which Postgres refuses, would otherwise fail the write.
	readReplies := make([]string, 0, len(input.ReadReplies))
	for _, id := range input.ReadReplies {
		parsed, err := uuid.Parse(id)
		if err != nil {
			s.writeHandlerError(w, errorf(http.StatusBadRequest, "INVALID_STATE",
				"read_replies: %q is not a message id", id))
			return
		}
		readReplies = append(readReplies, parsed.String())
	}
	// The insert below locks each new row in the order it is given, so two writes naming the same
	// new replies in opposite orders would wait on each other until Postgres killed one (40P01).
	// One order for every write, the ids' own, leaves no cycle to wait in.
	slices.Sort(readReplies)
	sessionID := r.PathValue("session_id")
	// The state is announced on an event the session owns, and only a session id a route can
	// name can own one, so any other is refused here rather than failing the write.
	if _, err := model.ParseRoute("session:" + sessionID); err != nil {
		s.writeHandlerError(w, errorf(http.StatusBadRequest, "INVALID_STATE",
			"session id %q is not one Dispatch can route", sessionID))
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	// A Clear or a read mark is announced whenever it is given; replies read by id only when one
	// of them gets a row (below).
	announce := clearedBefore != nil || readThrough != nil
	// A cutoff up to agentStateCutoffSkew ahead of the server clock is accepted, so a browser a few
	// seconds fast is not refused, but it is stored as no later than now: a reply that lands in
	// the gap is still after the viewer's Clear and read mark, so it shows and it counts.
	if clearedBefore != nil {
		// The Clear is keyed on the raw actor id on purpose: user_agent_state is migration 0033's
		// table and its rows predate the canonical convention 0048 and 0051 follow. This is not
		// the missing canonicalLogin a grep for one would take it for.
		if _, err := tx.Exec(r.Context(), `
			insert into user_agent_state (login, session_id, cleared_before)
			values ($1, $2, least($3::timestamptz, now()))
			on conflict (login, session_id) do update set cleared_before = excluded.cleared_before
		`, actor.ID, sessionID, *clearedBefore); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if readThrough != nil {
		if _, err := tx.Exec(r.Context(), `
			insert into user_agent_read (login, session_id, read_through)
			values ($1, $2, least($3::timestamptz, now()))
			on conflict (login, session_id) do update set
				read_through = greatest(user_agent_read.read_through, excluded.read_through)
		`, canonicalLogin(actor.ID), sessionID, *readThrough); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if len(readReplies) > 0 {
		// The check proves each id is a message the path's session wrote, and no more: the unread
		// count reads a row only for that session's reply under a direct message this viewer sent
		// (unreadDirectRepliesCTE), so a row naming any other message the session wrote is inert.
		var foreign string
		err := tx.QueryRow(r.Context(), `
			select wanted.id::text from unnest($1::uuid[]) as wanted(id)
			where not exists (
				select 1 from messages
				where messages.id = wanted.id
				  and messages.author->>'kind' = 'session' and messages.author->>'id' = $2
			)
			limit 1
		`, readReplies, sessionID).Scan(&foreign)
		if err == nil {
			s.writeHandlerError(w, errorf(http.StatusBadRequest, "INVALID_STATE",
				"read_replies: %s is not a message session %s wrote", foreign, sessionID))
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			s.writeHandlerError(w, err)
			return
		}
		// A reply the session's read mark has already passed is read without a row, and the
		// prune below would delete one again, so only the others are written; a revisit of
		// replies the mark covers writes nothing and announces nothing. The rows go in the ids'
		// sorted order (ordinality), so two writes lock new rows in one order.
		inserted, err := tx.Exec(r.Context(), `
			insert into user_agent_reply_read (login, session_id, reply_id)
			select $1, $2, wanted.id
			from unnest($3::uuid[]) with ordinality as wanted(id, position)
			where not exists (
				select 1 from messages
				join user_agent_read on user_agent_read.login = $1 and user_agent_read.session_id = $2
				where messages.id = wanted.id and messages.created_at <= user_agent_read.read_through
			)
			order by wanted.position
			on conflict do nothing
		`, canonicalLogin(actor.ID), sessionID, readReplies)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		announce = announce || inserted.RowsAffected() > 0
	}
	if readThrough != nil {
		// A reply at or before the session's read mark is read whatever user_agent_reply_read
		// holds, and the mark only moves forward, so the rows of the replies it now passes are dead
		// and are deleted here; without this a row would outlive every reason for it. The rows are
		// the ones read under this session (user_agent_reply_read_session serves the lookup), all
		// of them its own replies, and a session answers only what was delivered to it, so its
		// replies sit under roots targeted at it, where this mark is the one the unread count
		// reads. A Clear plays no part: it can move back.
		if _, err := tx.Exec(r.Context(), `
			delete from user_agent_reply_read
			using messages, user_agent_read
			where user_agent_reply_read.login = $1 and user_agent_reply_read.session_id = $2
			  and user_agent_read.login = $1 and user_agent_read.session_id = $2
			  and messages.id = user_agent_reply_read.reply_id
			  and messages.created_at <= user_agent_read.read_through
		`, canonicalLogin(actor.ID), sessionID); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	// Replies read by id that all had a row or sat at or before the read mark change nothing, so
	// nothing is announced: a broadcast page sends its replies again on every visit while the
	// session has an unread reply elsewhere, and each event would refetch the badge in every tab
	// the viewer has open.
	if !announce {
		// Nothing was written; end the transaction before the state is read through the pool.
		if err := tx.Rollback(r.Context()); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	} else {
		// The viewer's other open tabs and devices refresh their badge from this event rather
		// than waiting for a focus or the next message. It is owned by the session, like its
		// messages, so the outbox publishes it nowhere and it reaches only the dashboard's event
		// stream.
		event, err := s.appendEvent(r.Context(), tx, model.Event{
			Type:    "user_agent_state.updated",
			Actor:   actor,
			Payload: model.UserAgentStateEventPayload{Login: actor.ID, SessionID: sessionID},
		})
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		s.publish(event)
	}
	states, err := s.loadUserAgentStates(r.Context(), actor.ID, &sessionID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, states[sessionID])
}
