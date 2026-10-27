package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Snoozing an Inbox row is a human's own view state, like the Agents page's Clear: the ask
// stays open, stays listed, and reaches everyone else unchanged, while this viewer's Inbox
// folds it away until snoozed_until. The routes are human-only because the Inbox is; a
// session has no inbox to snooze a row out of.
//
// No job ever wakes a snoozed row. GET /api/v1/inbox carries snoozed_until on the row and
// the reader decides from it, so the moment a snooze passes the row is an ordinary row again
// on the next read.
//
// The row is keyed on the canonical login, not on the actor id: rows written before every
// identity lowercased the person it names can carry another casing of the same email, so keying
// on the actor id raw could give one person two snooze sets. model.CanonicalLogin is the form
// issues store assignees in and the form inboxAssigneeFilter compares against.
//
// A snooze or an un-snooze moves only its writer's Inbox, so it announces itself only to that
// person's other tabs and devices: it appends `user_state.updated` (askSnoozeEvent), the event a
// person's own pin, read mark and dismissal already append, with `changed: ["snoozed"]` and the
// ask's id. A write that moves nothing - a snooze to the moment the row already holds, an
// un-snooze of a row nobody snoozed - appends nothing, since each event refetches the Inbox in
// every tab the person has open.

// userAskSnooze is one row's snooze for the calling human: when the Inbox stops folding it
// away. The Inbox's "Until I clear it" is this timestamp set far enough out that only the
// reader's own un-snooze brings the row back.
type userAskSnooze struct {
	SnoozedUntil string `json:"snoozed_until"`
}

// PUT /api/v1/me/asks/{id}/snooze
func (s *server) putAskSnooze(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	askID, err := parseAskID(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var input struct {
		SnoozedUntil *string `json:"snoozed_until"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.SnoozedUntil == nil {
		writeError(w, "SNOOZE_INPUT", http.StatusBadRequest, "snoozed_until is required")
		return
	}
	snoozedUntil, err := time.Parse(time.RFC3339, *input.SnoozedUntil)
	if err != nil {
		writeError(w, "SNOOZE_INPUT", http.StatusBadRequest, "snoozed_until must be an RFC3339 timestamp")
		return
	}
	// A snooze that has already passed would fold nothing away. The bound allows no skew: the
	// Inbox's presets are its own to keep clear of it, and the one that can land near midnight
	// keeps a few seconds of today in hand rather than naming a moment this could refuse.
	if !snoozedUntil.After(time.Now()) {
		writeError(w, "SNOOZE_INPUT", http.StatusBadRequest, "snoozed_until must be in the future")
		return
	}
	// Selecting the ask rather than inserting its id makes an unknown ask pgx.ErrNoRows - a
	// 404 - instead of the foreign key's 500. previous is read from the statement's own snapshot,
	// before the write, so a snooze to the moment the row already holds is known to move nothing.
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var stored time.Time
	var previous *time.Time
	if err := tx.QueryRow(r.Context(), `
		with previous as (
			select snoozed_until from user_ask_snooze where login = $1 and ask_id = $2
		)
		insert into user_ask_snooze (login, ask_id, snoozed_until)
		select $1, id, $3 from asks where id = $2
		on conflict (login, ask_id) do update set snoozed_until = excluded.snoozed_until
		returning snoozed_until, (select snoozed_until from previous)
	`, model.CanonicalLogin(actor.ID), askID, snoozedUntil).Scan(&stored, &previous); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if previous == nil || !previous.Equal(stored) {
		if !s.announceAskSnooze(w, r, tx, actor, askID) {
			return
		}
	} else if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, userAskSnooze{SnoozedUntil: timestampValue(stored)})
}

// DELETE /api/v1/me/asks/{id}/snooze
func (s *server) deleteAskSnooze(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	askID, err := parseAskID(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// Un-snoozing a row that is not snoozed is the state the caller asked for, so it answers
	// the same 204 rather than a 404 the reader could do nothing with, and announces nothing.
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	removed, err := tx.Exec(r.Context(), `
		delete from user_ask_snooze where login = $1 and ask_id = $2
	`, model.CanonicalLogin(actor.ID), askID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if removed.RowsAffected() == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !s.announceAskSnooze(w, r, tx, actor, askID) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// announceAskSnooze appends the snooze's event in tx, commits, and publishes it. It answers false
// after writing the error response when any step fails.
func (s *server) announceAskSnooze(w http.ResponseWriter, r *http.Request, tx pgx.Tx, actor model.Actor, askID string) bool {
	event, err := s.askSnoozeEvent(r.Context(), tx, actor, askID)
	if err == nil {
		event, err = s.appendEvent(r.Context(), tx, event)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return false
	}
	s.publish(event)
	return true
}

// askSnoozeEvent is the `user_state.updated` a snooze or an un-snooze of askID appends, owned
// by the ask's project (its issue's, or its document's) as every `user_state.updated` is. It
// names the writer, `changed: ["snoozed"]` and the ask, so the writer's other tabs refetch the
// Inbox and nothing else. `state` is the field every `user_state.updated` carries: the writer's
// own row for the ask's issue, which a snooze does not move, read as GET /api/v1/me/state reads
// it (the default row when there is none, as for an ask on a project document). It names
// nobody but the writer.
func (s *server) askSnoozeEvent(ctx context.Context, tx pgx.Tx, actor model.Actor, askID string) (model.Event, error) {
	var project string
	state := userIssueState{}
	var dismissed []byte
	if err := tx.QueryRow(ctx, `
		select coalesce(i.project_key, ar.project_key),
		       coalesce(st.pinned, false), coalesce(st.last_read_seq, 0),
		       coalesce(st.dismissed, '[]'::jsonb), coalesce(st.seq, 0)
		from asks a
		left join issues i on i.key = a.issue_key
		left join artifacts ar on ar.id = a.artifact_id
		left join user_issue_state st on st.login = $2 and st.issue_key = a.issue_key
		where a.id = $1
	`, askID, actor.ID).Scan(&project, &state.Pinned, &state.LastReadSeq, &dismissed, &state.Seq); err != nil {
		return model.Event{}, err
	}
	if err := json.Unmarshal(dismissed, &state.Dismissed); err != nil {
		return model.Event{}, err
	}
	return projectOwner(project).event("user_state.updated", actor, map[string]any{
		"login":   model.CanonicalLogin(actor.ID),
		"state":   state,
		"changed": []string{"snoozed"},
		"ask_id":  askID,
	}), nil
}
