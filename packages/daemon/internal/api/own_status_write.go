package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
)

// setDispatchStatus writes an issue's status in Dispatch as the daemon, answering a refused or
// failed write 502, and returns the issue as Dispatch answered the write
// (dispatch.Client.SetStatus), which applyOwnStatusWrite applies to the record.
func (s *server) setDispatchStatus(w http.ResponseWriter, r *http.Request, issue, status string) (dispatch.Issue, bool) {
	if s.dispatch == nil {
		writeFailure(w, http.StatusInternalServerError, "DISPATCH_UNAVAILABLE", "Dispatch is unavailable")
		return dispatch.Issue{}, false
	}
	written, err := s.dispatch.SetStatus(r.Context(), issue, status)
	var dispatchError *dispatch.Error
	switch {
	case errors.As(err, &dispatchError):
		writeFailure(w, http.StatusBadGateway, dispatchError.Code, dispatchError.Message)
		return dispatch.Issue{}, false
	case err != nil:
		writeFailure(w, http.StatusBadGateway, "DISPATCH_FAILED", "Dispatch status update failed")
		return dispatch.Issue{}, false
	}
	return written, true
}

// applyOwnStatusWrite applies the daemon's own status write to the record in the request that made
// it, as the write's event applies it when the Dispatch consumer delivers it. Without it the record
// moved only on that event, so every route that writes a status waited for the consumer: a consumer
// running half an hour behind left a released child unrecorded, and its tree's architect waiting on
// it, for that half hour.
//
// written is the issue as Dispatch answered the write, the state the write's event carries, at that
// event's sequence (intake.OwnStatusWrite). It applies through the fact path every event takes, under
// the fact lock and with the same handlers, as Dispatch's view of the issue that settles under the
// lock (intake.DispatchIssue.Observed): an issue the workflow still does not record takes the view,
// as the consumer prepares an unrecorded issue's event; one it records takes the event itself, with
// the daemon's own actor. Either way the record stands at the write's sequence, so the event, when
// the consumer delivers it, is at or below the record's sequence and changes nothing, in the
// workflow, in admission and in the Dispatch mirror alike. The fact is a status move
// (intake.DispatchIssue.StatusMove): it lands ahead of events the consumer has not delivered, so
// the workflow takes its status even where the record last saw Dispatch at it. A write of a status
// Dispatch already showed makes no event and is answered at the sequence Dispatch had run to, so it
// applies over the events up to there, as the daemon's move: what the caller asked for. A Dispatch
// that answered no sequence gives the event no fence to find, so nothing applies here and the event
// applies the write, as before. Nor does an issue of another Dispatch project, which the controller's
// set_status can name: the consumer drops every event of another project (intake's
// decodeDispatchFact), so the record never takes one, and neither does this.
//
// Once the write has landed, a failure here leaves the record to the event and answers the caller
// so: its retry writes the same status again, which Dispatch answers with the issue at the same
// sequence and no new event, and applies it then. If the event arrived first, the record already
// took the move, and the retry is answered as for an issue already in that state (park_child's
// CHILD_NOT_RUNNING, rerun_child's CHILD_RUNNING).
func (s *server) applyOwnStatusWrite(w http.ResponseWriter, r *http.Request, written dispatch.Issue) bool {
	if written.LastSeq <= 0 || !s.ownDispatchIssue(written.Key) {
		return true
	}
	event := intake.OwnStatusWrite(written)
	fact := intake.ObservedIssue(written)
	fact.Delivered = &event
	if s.prepare != nil {
		prepared, err := s.prepare(r.Context(), fact)
		var later *intake.RetryLater
		switch {
		case errors.As(err, &later):
			writeFailure(w, http.StatusConflict, "STATUS_APPLY_BACKOFF", fmt.Sprintf("%s: Dispatch shows %s; %v; retry after %s", written.Key, written.Status, err, later.After))
			return false
		case err != nil:
			writeFailure(w, http.StatusBadGateway, "REPOSITORY_READ_FAILED", fmt.Sprintf("Dispatch shows %s %s, but its repository could not be read: %v; retry", written.Key, written.Status, err))
			return false
		}
		fact = prepared
	}
	result, err := intake.ApplyFact(r.Context(), s.pool, "api", requestFactID("status/own-write"), fact, s.handlers...)
	if err != nil {
		writeFailure(w, http.StatusInternalServerError, "FACT_APPLY_FAILED", fmt.Sprintf("Dispatch shows %s %s, but the record did not take it; retry", written.Key, written.Status))
		return false
	}
	if result.Refusal != nil {
		writeFailure(w, result.Refusal.Status, result.Refusal.Code, result.Refusal.Message)
		return false
	}
	return true
}

// ownDispatchIssue says whether key is an issue of the daemon's own Dispatch project, by its key's
// project as intake's decodeDispatchFact reads it. A server given no Dispatch project (a test's)
// takes every key as its own.
func (s *server) ownDispatchIssue(key string) bool {
	project, _, _ := strings.Cut(key, "-")
	return s.dispatchProject == "" || project == s.dispatchProject
}
