package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
)

// stop sends sessionID's clean stop as every normal end of a session does, DELETE
// /v1/sessions/<session_id> with query, which carries the stop's conditions or is empty for a stop
// that names none, and returns what the route answered.
func (rig *sessionConsumerReapRig) stop(t *testing.T, sessionID, query string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/v1/sessions/" + sessionID
	if query != "" {
		path += "?" + query
	}
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("stop %s: status %d: %s", sessionID, recorder.Code, recorder.Body.String())
	}
	return recorder
}

// requireNoConsumer fails the test unless the stream holds no consumer for sessionID, when saying
// when it was read.
func (rig *sessionConsumerReapRig) requireNoConsumer(t *testing.T, sessionID, when string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if info, err := rig.consumers.Info(ctx, sessionID); !errors.Is(err, bus.ErrSessionConsumerNotFound) {
		t.Fatalf("the consumer of %s %s: %+v, %v; want it deleted with the session's record",
			sessionID, when, info, err)
	}
}

// A clean stop takes the session's consumer with the record it deletes. Every normal end of a
// session comes through this route and never through the reaper - a plugin's deregistration, the
// Legion daemon's stop of a role, a /new, a fork, a branch delete - so a consumer this route left
// behind would stand for the whole of the stream's inactive threshold with no session to deliver
// to. Another session's consumer is untouched: the stop names one session.
func TestACleanStopDeletesTheStoppedSessionsConsumer(t *testing.T) {
	rig := newSessionConsumerReapRig(t)
	rig.register(t, "ses_clean_stop")
	rig.register(t, "ses_still_here")
	rig.requireConsumer(t, "ses_clean_stop", "after its registration")
	rig.requireConsumer(t, "ses_still_here", "after its registration")

	answer := rig.stop(t, "ses_clean_stop", "")

	if answer.Body.Len() != 0 {
		t.Fatalf("the stop answered with a body: %s", answer.Body.String())
	}
	if entry, err := rig.sessions.Get("ses_clean_stop"); err == nil {
		t.Fatalf("the stopped session's record stands: %+v", entry)
	}
	rig.requireNoConsumer(t, "ses_clean_stop", "after the stop that deleted its record")
	rig.requireConsumer(t, "ses_still_here", "after another session's clean stop")
}

// A conditional stop removes the session's consumer exactly when it removed the session's record.
// The daemon's stop of a role names the moment its probe judged the process gone: a registration
// written after that moment is the relaunch's, and it keeps the record and the consumer that
// session is receiving through, while a stop whose moment the stored registration is covered by
// takes both.
func TestAConditionalStopDeletesTheConsumerOnlyWhenItDeletesTheRecord(t *testing.T) {
	rig := newSessionConsumerReapRig(t)
	rig.register(t, "ses_relaunched")
	registered, err := rig.sessions.Get("ses_relaunched")
	if err != nil {
		t.Fatalf("read the relaunch's registration: %v", err)
	}
	rig.requireConsumer(t, "ses_relaunched", "after its registration")

	outlived := rig.stop(t, "ses_relaunched", fmt.Sprintf("if_not_registered_after=%d", registered.UpdatedAt-1))

	kept := stopAnswer(t, outlived)
	if kept.Deleted || kept.Reason != "the registration was written after the moment the delete belongs to" {
		t.Fatalf("answer = %+v, want the registration kept with its reason", kept)
	}
	if _, err := rig.sessions.Get("ses_relaunched"); err != nil {
		t.Fatalf("the kept registration is gone: %v", err)
	}
	rig.requireConsumer(t, "ses_relaunched", "after a stop its registration outlived")

	covered := rig.stop(t, "ses_relaunched", fmt.Sprintf("if_not_registered_after=%d", registered.UpdatedAt))

	deleted := stopAnswer(t, covered)
	if !deleted.Deleted || deleted.Reason != "" {
		t.Fatalf("answer = %+v, want the registration deleted", deleted)
	}
	if entry, err := rig.sessions.Get("ses_relaunched"); err == nil {
		t.Fatalf("the stopped session's record stands: %+v", entry)
	}
	rig.requireNoConsumer(t, "ses_relaunched", "after the stop that deleted its record")
}

// stopAnswer is the body a stop that named a condition answered.
func stopAnswer(t *testing.T, recorder *httptest.ResponseRecorder) deleteSessionResponse {
	t.Helper()
	var answer deleteSessionResponse
	if err := json.NewDecoder(recorder.Body).Decode(&answer); err != nil {
		t.Fatalf("decode the stop's answer: %v", err)
	}
	return answer
}
