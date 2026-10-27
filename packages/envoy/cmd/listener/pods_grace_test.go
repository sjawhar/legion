package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// stateTestPod is one listener pod for a test of the state its pods share: its own NATS connection,
// its own handle on the listener state bucket and its delivery lease over it, and its interest and
// session registries wired to them as main wires them. startedAgo is how long before now the pod
// started, as its interest registry's clock reads while it opens.
type stateTestPod struct {
	deps  *listenerDeps
	lease *lease
}

func openStateTestPod(t *testing.T, instance string, startedAgo time.Duration, sessionOptions ...session.SessionRegistryOption) stateTestPod {
	t.Helper()
	client := setupPublishTestClient(t)
	stateKV := leaseBucket(t)
	podLease := testLease(stateKV, instance, productionLeaseConfig(), time.Now)
	var elapsed atomic.Int64
	elapsed.Store(int64(-startedAgo))
	registry, err := store.Open(client.Conn, store.WithReplicas(1),
		store.WithClock(func() time.Time { return time.Now().Add(time.Duration(elapsed.Load())) }),
		store.WithServiceSince(func() time.Time { return podLease.Status().ServiceSince }))
	if err != nil {
		t.Fatalf("open pod %s's interest registry: %v", instance, err)
	}
	t.Cleanup(registry.StopWatch)
	elapsed.Store(0)
	sessionOptions = append([]session.SessionRegistryOption{session.WithSessionReplicas(1), session.WithLastSeenBucket(stateKV)}, sessionOptions...)
	sessions, err := session.OpenSessionRegistry(client.Conn, sessionOptions...)
	if err != nil {
		t.Fatalf("open pod %s's session registry: %v", instance, err)
	}
	t.Cleanup(sessions.StopWatch)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sessions.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("warm pod %s's session cache: %v", instance, err)
	}
	return stateTestPod{deps: &listenerDeps{client: client, registry: registry, sessions: sessions}, lease: podLease}
}

// lapsedRoleAnswer is GET /v1/roles/<role>'s answer for a holder that is gone.
type lapsedRoleAnswer struct {
	Reason        roleHolderState `json:"reason"`
	Holder        string          `json:"holder"`
	LastSeen      *int64          `json:"last_seen"`
	ClaimReleased *bool           `json:"claim_released"`
}

// getLapsedRole asks pod for role, whose holder is gone, and returns the raw answer and its fields.
func (pod stateTestPod) getLapsedRole(t *testing.T, role string) (string, lapsedRoleAnswer) {
	t.Helper()
	recorder := httptest.NewRecorder()
	roleGetHandler(pod.deps).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/roles/"+role, nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/roles/%s: status %d %s, want 404 for a lapsed holder", role, recorder.Code, recorder.Body.String())
	}
	body := strings.TrimSpace(recorder.Body.String())
	var answer lapsedRoleAnswer
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("decode GET /v1/roles/%s: %v (%s)", role, err, body)
	}
	if answer.Reason != roleHolderLapsed || answer.ClaimReleased == nil {
		t.Fatalf("GET /v1/roles/%s = %s, want a lapsed holder whose claim this lookup released or kept", role, body)
	}
	return body, answer
}

func (pod stateTestPod) claim(t *testing.T, role, holder string) {
	t.Helper()
	if _, err := pod.deps.registry.SetRole(holder, "box-a", role, false); err != nil {
		t.Fatalf("claim %s for %s: %v", role, holder, err)
	}
}

// A role whose holder is gone is answered alike by every pod of the machine, whenever each pod
// started: the claim's grace counts from the service start in the delivery lease record, which each
// pod's lease reads. Before, each pod counted it from its own start, so a pod that had run for long
// released a claim from before the machine's pods began serving again, and one that started a
// moment ago kept a claim made long after the machine's service began.
func TestEveryPodAnswersARoleWhoseHolderLapsedAlike(t *testing.T) {
	t.Run("the machine has served since long before the claim", func(t *testing.T) {
		kv := leaseBucket(t)
		serviceSince := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
		putLease(t, kv, `{"holder":"pod-z","boot":"0123456789abcdef","service_since":"`+serviceSince+`"}`)
		a := openStateTestPod(t, "pod-a", 10*time.Minute)
		a.claim(t, "role-a", "ses_gone_a")
		a.claim(t, "role-b", "ses_gone_b")
		b := openStateTestPod(t, "pod-b", 0)
		a.lease.step()
		b.lease.step()

		_, onA := a.getLapsedRole(t, "role-a")
		_, onB := b.getLapsedRole(t, "role-b")
		if !*onA.ClaimReleased || !*onB.ClaimReleased {
			t.Fatalf("claim released on pod a = %v, on pod b = %v; want both released: each claim was made after the machine's service start",
				*onA.ClaimReleased, *onB.ClaimReleased)
		}
	})

	t.Run("the machine began serving again after the claim", func(t *testing.T) {
		a := openStateTestPod(t, "pod-a", 10*time.Minute)
		a.claim(t, "role", "ses_gone")
		// Pod a takes the absent lease record, which starts the machine's service at the server's
		// time of that write, after the claim; pod b, starting now, reads that record.
		a.lease.step()
		b := openStateTestPod(t, "pod-b", 0)
		b.lease.step()
		if since := b.lease.Status().ServiceSince; since.IsZero() || !since.Equal(a.lease.Status().ServiceSince) {
			t.Fatalf("pod b read service_since %v, pod a wrote %v", since, a.lease.Status().ServiceSince)
		}

		bodyA, onA := a.getLapsedRole(t, "role")
		bodyB, _ := b.getLapsedRole(t, "role")
		if *onA.ClaimReleased {
			t.Fatalf("pod a, running since long before the claim, released it: %s; want it kept for one session TTL after the machine's service start", bodyA)
		}
		if bodyA != bodyB {
			t.Fatalf("pod a answers %s, pod b answers %s", bodyA, bodyB)
		}
	})
}

// A role holder whose session heartbeated only through pod A and then expired is reported with one
// last_seen by every pod, pod B started after it lapsed included. Before, the last-seen time lived
// in the memory of a pod whose cache had held the session, so pod B reported none.
func TestARoleHoldersLastSeenIsTheSameOnAPodStartedAfterItLapsed(t *testing.T) {
	const (
		holder = "ses_lapsing_holder"
		ttl    = 3 * time.Second
	)
	a := openStateTestPod(t, "pod-a", 0, session.WithSessionTTL(ttl))
	if err := a.deps.sessions.Put(holder, session.SessionEntry{MachineID: "box-a", SelfSubscribed: true}); err != nil {
		t.Fatalf("register %s through pod a: %v", holder, err)
	}
	a.claim(t, "role-a", holder)
	a.claim(t, "role-b", holder)
	sessionKV, err := a.deps.client.JS().KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open the session bucket: %v", err)
	}
	deadline := time.Now().Add(ttl + 10*time.Second)
	for {
		if _, err := sessionKV.Get(holder); errors.Is(err, natsgo.ErrKeyNotFound) {
			break
		} else if err != nil {
			t.Fatalf("read %s while it expires: %v", holder, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's registration was not removed within %s of its write", holder, ttl+10*time.Second)
		}
		time.Sleep(50 * time.Millisecond)
	}
	b := openStateTestPod(t, "pod-b", 0)

	bodyA, onA := a.getLapsedRole(t, "role-a")
	bodyB, onB := b.getLapsedRole(t, "role-b")
	if onA.LastSeen == nil || onB.LastSeen == nil || *onA.LastSeen != *onB.LastSeen {
		t.Fatalf("pod a answers %s, pod b answers %s; want one last_seen on both", bodyA, bodyB)
	}
}
