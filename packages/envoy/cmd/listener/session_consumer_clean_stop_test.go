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
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
)

// stop sends sessionID's clean stop as every normal end of a session does, DELETE
// /v1/sessions/<session_id> with query, which carries the stop's conditions or is empty for a stop
// that names none, and returns what the route answered.
func (rig *sessionConsumerReapRig) stop(t *testing.T, sessionID, query string) *httptest.ResponseRecorder {
	t.Helper()
	return stopSession(t, rig.mux, sessionID, query)
}

// stopSession sends sessionID's clean stop to the routes mux holds, with query carrying the stop's
// conditions or empty for a stop that names none, and returns what the route answered. It is a
// func of its own because the rows below drive the same route over two rigs: the one that reads
// the stream's consumers, and the one that relays them to a session.
func stopSession(t *testing.T, mux *http.ServeMux, sessionID, query string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/v1/sessions/" + sessionID
	if query != "" {
		path += "?" + query
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, path, nil))
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

// keptStopSilence is how long the session of a kept stop is watched for a frame it must not
// receive: one it already holds, sent to it again. A stop that deleted the consumer leaves the
// relay pulling one the stream no longer holds, and the relay restarts at the ack floor
// relayRetryFirst after that pull failed, so this window is several times that wait. Nothing
// sends the session a frame inside it otherwise: the frame it holds is answered, so the relay
// does not take it for unreachable (relayReceiptWait), and the consumer's ack wait is a minute
// (bus.SessionConsumerAckWait).
const keptStopSilence = 4 * relayRetryFirst

// A conditional stop that keeps the session's record leaves that session's consumer exactly as it
// found it, whichever of the stop's conditions kept the record. The record stands, so the session
// is still there, and the consumer is what it receives through: the stream must hold the very
// consumer it held before the stop, the frames it has sent and has no ack for still in flight,
// and the next message published for the session must reach it through that consumer.
//
// Deleting a kept consumer and giving it back is not the same thing, which is why this row reads
// the consumer's own creation and what the session receives rather than only whether a consumer
// exists: a delete restores the consumer of every session whose record is in the bucket
// (sessionDelivery.restoreRegistered), and a kept record is one, so a stop that deleted it leaves
// a consumer behind it. The replacement is a new consumer created at the sequence after the ack
// floor, so every frame the session holds unacked is sent to it again, and the relay pulling the
// consumer that went relays nothing until its retry.
func TestAConditionalStopThatKeepsTheRecordLeavesTheConsumerAlone(t *testing.T) {
	const storedInstance = "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"
	const stoppingInstance = "0b7a4c1e9d3f5a2b0b7a4c1e9d3f5a2b"
	for _, kept := range []struct {
		name         string
		registration func(t *testing.T, sessionID string) string
		query        func(stored session.SessionEntry) string
		reason       string
	}{
		{
			name: "the record is another client instance's",
			registration: func(t *testing.T, sessionID string) string {
				return instanceRegistration(t, sessionID, "epoch-1", storedInstance)
			},
			query:  func(session.SessionEntry) string { return "client_instance_id=" + stoppingInstance },
			reason: "the registration belongs to another client instance",
		},
		{
			name: "the record was written after the moment the stop belongs to",
			registration: func(t *testing.T, sessionID string) string {
				return consumerRegistration(sessionID, "epoch-1")
			},
			query: func(stored session.SessionEntry) string {
				return fmt.Sprintf("if_not_registered_after=%d", stored.UpdatedAt-1)
			},
			reason: "the registration was written after the moment the delete belongs to",
		},
	} {
		t.Run(kept.name, func(t *testing.T) {
			rig := newRelayRig(t)
			sessionID := "ses_kept_stop_" + id.New()
			agent := contracts.AgentSubject(sessionID)
			standIn := startSessionStandIn(t, rig.url, sessionID, answerReceived)
			rig.subscribe(t, kept.registration(t, sessionID))
			held, _ := rig.storeEvent(t, agent)
			requireFrames(t, standIn.collect(t, 1, 10*time.Second), held)
			created := consumerCreatedAt(t, rig, sessionID, "before the stop")

			answer := stopAnswer(t, stopSession(t, rig.mux, sessionID, kept.query(rig.entry(t, sessionID))))

			if answer.Deleted || answer.Reason != kept.reason {
				t.Fatalf("answer = %+v, want the registration kept for the reason %q", answer, kept.reason)
			}
			if _, err := rig.harness.sessions.Get(sessionID); err != nil {
				t.Fatalf("the kept registration of %s is gone: %v", sessionID, err)
			}
			standIn.none(t, keptStopSilence)

			next, _ := rig.storeEvent(t, agent)

			requireFrames(t, standIn.collect(t, 1, 10*time.Second), next)
			if again := consumerCreatedAt(t, rig, sessionID, "after the stop that kept its record"); !again.Equal(created) {
				t.Fatalf("the consumer of %s was created at %s and the stop that kept its record left one created at %s; want the session's own consumer untouched",
					sessionID, created, again)
			}
		})
	}
}

// A clean stop deletes the consumer of a session whose caller hung up the moment its record was
// gone. That caller is the ordinary one: a plugin sends its deregistration and exits, and the
// connection it drops a moment later cancels the request's context. The consumer delete runs on a
// context detached from the request for that reason (context.WithoutCancel in
// deleteSessionConsumer), and the cancellation protects nothing by then: the record is already
// deleted, so the session is gone whatever happens next, while a consumer left behind stands for
// the whole of the stream's inactive threshold with no session to deliver to.
//
// The hang-up is placed rather than raced. The test holds the session's lock, which the consumer
// delete takes before its first call (sessionDelivery.deleteConsumer), so the handler reaches that
// lock only once it has deleted the record, and the request's context is cancelled there, between
// the record delete and the consumer delete the route exists to make.
func TestACleanStopDeletesTheConsumerOfACallerThatHungUpOnceItsRecordWasGone(t *testing.T) {
	rig := newSessionConsumerReapRig(t)
	const sessionID = "ses_hung_up"
	rig.register(t, sessionID)
	rig.requireConsumer(t, sessionID, "after its registration")
	hangUp, hungUp := context.WithCancel(context.Background())
	defer hungUp()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete, "/v1/sessions/"+sessionID, nil).WithContext(hangUp)
	unlock := rig.delivery.locks.lock(sessionID)
	served := make(chan struct{})
	go func() {
		defer close(served)
		rig.mux.ServeHTTP(recorder, request)
	}()
	awaitDeleteWaitingForTheLock(t, &rig.delivery.locks, sessionID)
	rig.waitForRecordDeleted(t, sessionID)

	hungUp()
	unlock()
	<-served

	if recorder.Code != http.StatusOK {
		t.Fatalf("the stop of %s answered %d: %s", sessionID, recorder.Code, recorder.Body.String())
	}
	rig.requireNoConsumer(t, sessionID, "after the stop whose caller hung up on it")
}

// answerReceived answers a frame received and nothing more: the session holds it and has not
// written it, so the consumer still holds that message for an ack. A frame answered at all is one
// the relay does not take for unreachable, so the session goes on being relayed.
func answerReceived(relayedFrame) []string { return []string{relayReplyReceived} }

// instanceRegistration is the registration of a session in consumer mode at epoch whose client
// minted the instance id instanceID, as every client that mints one sends on every registration:
// what a conditional stop naming another instance compares against (session.InstanceMatches).
func instanceRegistration(t *testing.T, sessionID, epoch, instanceID string) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(consumerRegistration(sessionID, epoch)), &body); err != nil {
		t.Fatalf("decode the registration of %s: %v", sessionID, err)
	}
	body["client_instance_id"] = instanceID
	registration, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the registration of %s: %v", sessionID, err)
	}
	return string(registration)
}

// consumerCreatedAt is when the stream created sessionID's consumer, read through the test's own
// view of the session consumers, when saying when it was read. A delete and the restore that
// follows it give the session a consumer created later; nothing else the listener does to a
// consumer changes it.
func consumerCreatedAt(t *testing.T, rig *relayRig, sessionID, when string) time.Time {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := rig.consumers.Info(ctx, sessionID)
	if err != nil {
		t.Fatalf("the consumer of %s %s: %v; want the stream holding it", sessionID, when, err)
	}
	return info.Created
}
