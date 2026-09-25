package api

import (
	"net/http"
	"time"
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
// The row is keyed on the canonical login, not on the actor id: a human's actor id is the
// login as their identity source spells it (`identity.CookieIdentity` returns GitHub's
// display casing, `HeaderIdentity` the header verbatim; both lowercase only to check the
// allowlist), so keying on it raw would give one person two snooze sets across identity
// sources and orphan their rows when GitHub's casing changes. canonicalLogin is the form
// issues store assignees in and the form inboxAssigneeFilter compares against.

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
	// 404 - instead of the foreign key's 500.
	var stored time.Time
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `
		insert into user_ask_snooze (login, ask_id, snoozed_until)
		select $1, id, $3 from asks where id = $2
		on conflict (login, ask_id) do update set snoozed_until = excluded.snoozed_until
		returning snoozed_until
	`, canonicalLogin(actor.ID), askID, snoozedUntil).Scan(&stored); err != nil {
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
	// the same 204 rather than a 404 the reader could do nothing with.
	if _, err := s.deps.Store.Pool.Exec(r.Context(), `
		delete from user_ask_snooze where login = $1 and ask_id = $2
	`, canonicalLogin(actor.ID), askID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
