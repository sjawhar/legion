package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
)

// A session registered through one pod a moment ago is in the session bucket before the other pod's
// cache has it. A send or a role claim through that other pod reads the bucket on a cache miss, so it
// is answered rather than refused as `no live session` or `session is not registered`.
func TestSendAndRoleSetThroughAPodWhoseCacheHasNotSeenTheRegistrationAnswer200(t *testing.T) {
	t.Run("send", func(t *testing.T) {
		claimant, observer := trailingListeners(t)
		if err := claimant.sessions.Put("ses_unseen", session.SessionEntry{Port: 4242, MachineID: "test-machine"}); err != nil {
			t.Fatalf("register ses_unseen through the other pod: %v", err)
		}
		recorder := httptest.NewRecorder()
		sendHandler(observer, logging.New("test")).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages/send",
			strings.NewReader(`{"target_session":"ses_unseen","message":"hello"}`)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("send to a session this pod's cache has not seen: status %d %s, want 200", recorder.Code, recorder.Body.String())
		}
	})
	t.Run("role set", func(t *testing.T) {
		claimant, observer := trailingListeners(t)
		if err := claimant.sessions.Put("ses_unseen", session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
			t.Fatalf("register ses_unseen through the other pod: %v", err)
		}
		recorder := httptest.NewRecorder()
		roleSetHandler(observer, "test-machine", logging.New("test")).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set",
			strings.NewReader(`{"session_id":"ses_unseen","role":"unseen"}`)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("role set for a session this pod's cache has not seen: status %d %s, want 200", recorder.Code, recorder.Body.String())
		}
		if holder, err := claimant.registry.RoleHolder("unseen"); err != nil || holder != "ses_unseen" {
			t.Fatalf("unseen's holder after the role set = %q, %v; want ses_unseen", holder, err)
		}
	})
}

// A role set rewrites the claimant's registration as self-subscribed through this listener. On a pod
// whose cache holds an older copy, that rewrite lands on what the bucket holds, so the newer host
// another pod recorded is kept rather than replaced by the stale copy's.
func TestARoleSetOnAPodWhoseCacheTrailsKeepsTheNewerHost(t *testing.T) {
	claimant, observer := trailingListeners(t)
	const sessionID = "ses_moved"
	register := func(host string) {
		t.Helper()
		if err := claimant.sessions.Put(sessionID, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true, Host: host}); err != nil {
			t.Fatalf("register %s on %s through the other pod: %v", sessionID, host, err)
		}
	}
	register("example-host-a")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := observer.sessions.Refresh(ctx, sessionID); err != nil {
		t.Fatalf("load %s into the trailing pod's cache: %v", sessionID, err)
	}
	register("example-host-b")
	recorder := httptest.NewRecorder()
	roleSetHandler(observer, "test-machine", logging.New("test")).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set",
		strings.NewReader(`{"session_id":"ses_moved","role":"mover"}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("role set on the trailing pod: status %d %s, want 200", recorder.Code, recorder.Body.String())
	}
	got, err := claimant.sessions.Refresh(ctx, sessionID)
	if err != nil {
		t.Fatalf("read %s back: %v", sessionID, err)
	}
	if got.Host != "example-host-b" || !got.SelfSubscribed {
		t.Fatalf("stored registration after the role set = %+v, want host example-host-b, self-subscribed", got)
	}
}

// A soft claim is weighed on the client instance its registration carries - one Oh My Pi Envoy
// extension instance, one Claude Code channel-server process - and that id names the client, never
// the listener pod that took the registration. Every pod of the machine serves `/v1`, so a session
// that registers and claims through the other pod after a take-over must be answered exactly as the
// pod that registered its predecessor would have answered: the same conversation's fork takes the
// role, another client instance is refused. The claiming pod's cache has seen neither registration,
// so both are read from the bucket the pods share.
func TestASoftClaimThroughAnotherPodWeighsTheClientInstanceNotThePod(t *testing.T) {
	const (
		forkingInstance = "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"
		otherInstance   = "9b8a7c6d5e4f3a2b9b8a7c6d5e4f3a2b"
	)
	// ses_pred registers and claims role through one pod; ses_child, belonging to
	// claimantInstance, then registers and soft-claims role through the other.
	claimThroughTheOtherPod := func(t *testing.T, role, claimantInstance string) (predecessorPod, claimPod *listenerDeps, recorder *httptest.ResponseRecorder) {
		t.Helper()
		predecessorPod, claimPod = trailingListeners(t)
		register := func(d *listenerDeps, sessionID, instanceID string) {
			t.Helper()
			entry := session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true, ClientInstanceID: instanceID}
			if err := d.sessions.Put(sessionID, entry); err != nil {
				t.Fatalf("register %s through its own pod: %v", sessionID, err)
			}
		}
		claim := func(d *listenerDeps, body string) *httptest.ResponseRecorder {
			t.Helper()
			recorder := httptest.NewRecorder()
			roleSetHandler(d, "test-machine", logging.New("test")).ServeHTTP(recorder,
				httptest.NewRequest(http.MethodPost, "/v1/roles/set", strings.NewReader(body)))
			return recorder
		}
		register(predecessorPod, "ses_pred", forkingInstance)
		if seed := claim(predecessorPod, `{"session_id":"ses_pred","role":"`+role+`"}`); seed.Code != http.StatusOK {
			t.Fatalf("ses_pred claims %s through its own pod: status %d %s", role, seed.Code, seed.Body.String())
		}
		register(claimPod, "ses_child", claimantInstance)
		if _, err := claimPod.sessions.Get("ses_pred"); err == nil {
			t.Fatal("the claiming pod's cache already holds the predecessor, so the claim never has to read the bucket")
		}
		return predecessorPod, claimPod, claim(claimPod,
			`{"session_id":"ses_child","role":"`+role+`","soft":true,"previous_session_id":"ses_pred"}`)
	}

	t.Run("the predecessor's own client instance takes the role through the other pod", func(t *testing.T) {
		predecessorPod, _, recorder := claimThroughTheOtherPod(t, "forked", forkingInstance)
		if recorder.Code != http.StatusOK {
			t.Fatalf("soft claim of the same client instance's role through the other pod: status %d %s, want 200", recorder.Code, recorder.Body.String())
		}
		if holder, err := predecessorPod.registry.RoleHolder("forked"); err != nil || holder != "ses_child" {
			t.Fatalf("forked's holder after the claim = %q, %v; want ses_child", holder, err)
		}
	})

	t.Run("another client instance is refused through the other pod", func(t *testing.T) {
		predecessorPod, _, recorder := claimThroughTheOtherPod(t, "contested", otherInstance)
		if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), `"holder":"ses_pred"`) {
			t.Fatalf("soft claim naming a live predecessor of another client instance through the other pod: status %d %s, want 409 naming ses_pred", recorder.Code, recorder.Body.String())
		}
		if holder, err := predecessorPod.registry.RoleHolder("contested"); err != nil || holder != "ses_pred" {
			t.Fatalf("contested's holder after the refused claim = %q, %v; want ses_pred", holder, err)
		}
	})
}

// racedSessionKV is a session bucket another writer races on every Create and Update, so each
// write's expected revision is stale.
type racedSessionKV struct {
	nats.KeyValue
}

func (kv racedSessionKV) race(key string) {
	_, _ = kv.KeyValue.Put(key, []byte(`{"port":0,"machine_id":"box-b","self_subscribed":true}`))
}

func (kv racedSessionKV) Create(key string, value []byte) (uint64, error) {
	kv.race(key)
	return kv.KeyValue.Create(key, value)
}

func (kv racedSessionKV) Update(key string, value []byte, last uint64) (uint64, error) {
	kv.race(key)
	return kv.KeyValue.Update(key, value, last)
}

// raceSessionWrites makes every registration write of sessions lose its compare-and-set, by swapping
// the handle its watcher hands out, as stallSessionReads does.
func raceSessionWrites(t *testing.T, sessions *session.SessionRegistry) {
	t.Helper()
	field := reflect.ValueOf(sessions).Elem().FieldByName("watcher")
	watcher := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	raced := bus.KeyValue{KeyValue: racedSessionKV{KeyValue: watcher.Interface().(*kvwatch.Watcher).KV().KeyValue}}
	watcher.Set(reflect.ValueOf(kvwatch.New("session registry", raced, func(nats.KeyValueEntry) {}, func() {})))
}

// A registration write that loses its compare-and-set on every attempt is a transient failure: the
// role set answers 503 with Retry-After, never a 500 or an overwrite of the other writer's value.
func TestARoleSetWhoseRegistrationWriteLosesEveryRaceAnswers503(t *testing.T) {
	_, observer := trailingListeners(t)
	if err := observer.sessions.Put("ses_raced", session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register ses_raced: %v", err)
	}
	raceSessionWrites(t, observer.sessions)
	recorder := httptest.NewRecorder()
	roleSetHandler(observer, "test-machine", logging.New("test")).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set",
		strings.NewReader(`{"session_id":"ses_raced","role":"raced"}`)))
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") == "" {
		t.Fatalf("role set while every registration write races: status %d, Retry-After %q, %s; want 503 with Retry-After",
			recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
	}
}

// A registration, an interest and a role claim stored by a newer build carry a field this build does
// not know. A heartbeat (subscribe) and a role set through this build keep it in all three.
func TestAHeartbeatAndARoleSetKeepFieldsThisBuildDoesNotKnow(t *testing.T) {
	registry, sessions := setupSessionsTest(t, nil, nil)
	client := setupPublishTestClient(t)
	d := &listenerDeps{client: client, registry: registry, sessions: sessions}
	const sessionID = "ses_newer_build"
	bucket := func(name string) nats.KeyValue {
		t.Helper()
		kv, err := client.JS().KeyValue(name)
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		return kv
	}
	sessionKV, interestKV, roleKV := bucket(bus.SessionBucket), bucket(bus.InterestBucket), bucket(bus.RoleClaimBucket)
	for _, seed := range []struct {
		kv         nats.KeyValue
		key, value string
	}{
		{sessionKV, sessionID, `{"port":4242,"machine_id":"test-machine","dir":"/work","future_field":"session"}`},
		{interestKV, sessionID, `{"session_id":"ses_newer_build","machine_id":"test-machine","dir":"/work","topics":["notifications.agent.ses_newer_build"],"future_field":"interest"}`},
		{roleKV, "keeper", `{"holder_session_id":"ses_before","claimed_at":1,"future_field":"claim"}`},
	} {
		if _, err := seed.kv.Put(seed.key, []byte(seed.value)); err != nil {
			t.Fatalf("store a newer build's %s value: %v", seed.kv.Bucket(), err)
		}
	}
	waitFor(t, 5*time.Second, "the session cache to hold the newer build's registration", func() bool {
		_, err := sessions.Get(sessionID)
		return err == nil
	})
	kept := func(when string) {
		t.Helper()
		for _, want := range []struct {
			kv        nats.KeyValue
			key, want string
		}{
			{sessionKV, sessionID, `"session"`},
			{interestKV, sessionID, `"interest"`},
			{roleKV, "keeper", `"claim"`},
		} {
			entry, err := want.kv.Get(want.key)
			if err != nil {
				t.Fatalf("%s: read %s from %s: %v", when, want.key, want.kv.Bucket(), err)
			}
			var stored map[string]json.RawMessage
			if err := json.Unmarshal(entry.Value(), &stored); err != nil {
				t.Fatalf("%s: decode %s: %v", when, entry.Value(), err)
			}
			if got := string(stored["future_field"]); got != want.want {
				t.Fatalf("%s: %s's future_field in %s = %q, want %s; stored %s", when, want.key, want.kv.Bucket(), got, want.want, entry.Value())
			}
		}
	}

	recorder := httptest.NewRecorder()
	subscribeHandler(d, "test-machine", logging.New("test")).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe",
		strings.NewReader(`{"session_id":"ses_newer_build","topics":["notifications.team.example"],"port":4242,"dir":"/work"}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("heartbeat: status %d %s, want 200", recorder.Code, recorder.Body.String())
	}
	kept("after a heartbeat")

	recorder = httptest.NewRecorder()
	roleSetHandler(d, "test-machine", logging.New("test")).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set",
		strings.NewReader(`{"session_id":"ses_newer_build","role":"keeper"}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("role set: status %d %s, want 200", recorder.Code, recorder.Body.String())
	}
	kept("after a role set")
	if holder, err := registry.RoleHolder("keeper"); err != nil || holder != sessionID {
		t.Fatalf("keeper's holder after the role set = %q, %v; want %s", holder, err, sessionID)
	}
}
