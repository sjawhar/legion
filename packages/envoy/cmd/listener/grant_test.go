package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A listener whose NATS user holds exactly the grant envoy-infra renders for its machine
// (testnats.ListenerGrant) takes its delivery lease, renews it, delivers a notification through
// the delivery ledger, records a registered session's last-seen time, and releases the lease on
// SIGTERM, and the server refuses it nothing. Each write to the listener state bucket is read
// back by key, a direct get on the key's own subject, which the grant allows: a read by revision
// is a direct get on the stream's bare subject, which it refuses, so the lease was never held and
// no last-seen record was written.
func TestAListenerUnderItsMachinesGrantHoldsTheLeaseAndRecordsLastSeen(t *testing.T) {
	const (
		machineID = "box-granted"
		instance  = "pod-granted"
		sessionID = "ses_granted"
		topic     = "notifications.github.acme.granted.pr.1"
	)
	seed, public := testnats.User(t)
	granted := testnats.StartGranted(t, testnats.GrantedUser{Public: public, Grant: testnats.ListenerGrant(t, machineID)})
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), granted.URL(), machineID,
		"NATS_NKEY_SEED="+seed, "ENVOY_INSTANCE_ID="+instance, "ENVOY_HOST_BRIDGE=127.0.0.1")
	listener.waitHealthy(t)

	admin, err := bus.Connect([]string{granted.AdminURL}, bus.WithName("grant-test-admin"))
	if err != nil {
		t.Fatalf("connect as the admin: %v", err)
	}
	t.Cleanup(admin.Close)
	state, err := bus.OpenKeyValue(admin.JS(), bus.ListenerStateBucket)
	if err != nil {
		t.Fatalf("open the listener state bucket: %v", err)
	}
	leaseKey := "lease." + bus.ListenerDurable(machineID)
	readLeaseKey := func() (leaseRecord, natsgo.KeyValueEntry) {
		t.Helper()
		entry, err := state.Get(leaseKey)
		if err != nil {
			t.Fatalf("read %s: %v", leaseKey, err)
		}
		record, err := decodeLeaseRecord(entry.Value())
		if err != nil {
			t.Fatalf("decode %s %s: %v", leaseKey, entry.Value(), err)
		}
		return record, entry
	}
	held, taken := readLeaseKey()
	if held.Holder != instance || held.Released {
		t.Fatalf("%s = %+v, want held by %s", leaseKey, held, instance)
	}
	waitFor(t, 5*time.Second, "the holder to renew the lease", func() bool {
		record, entry := readLeaseKey()
		return record.Holder == instance && entry.Revision() > taken.Revision()
	})

	serving := startServingPort(t, sessionID, http.StatusNoContent)
	registerServingSession(t, listener, serving, "127.0.0.1", topic)
	publisher := connectPublisher(t, granted.AdminURL)
	item := newEnvelope("github", topic, "pull request 1 was opened")
	publish(t, publisher, item)
	requirePromptCarries(t, serving.awaitPrompt(t, 10*time.Second), item)
	keys, err := state.Keys()
	if err != nil {
		t.Fatalf("list the listener state bucket's keys: %v", err)
	}
	ledger := "delivery." + bus.ListenerDurable(machineID) + "."
	if !hasKeyWithPrefix(keys, ledger) {
		t.Fatalf("the listener state bucket holds no %s key after a delivery: %q", ledger, keys)
	}
	if _, err := state.Get("seen." + sessionID); err != nil {
		t.Fatalf("read %s's last-seen record: %v\n%s", sessionID, err, listener.Output.String())
	}

	listener.Terminate(t)
	listener.WaitExit(t, "SIGTERM")
	if released, _ := readLeaseKey(); !released.Released || released.Holder != instance {
		t.Fatalf("%s after SIGTERM = %+v, want released by %s", leaseKey, released, instance)
	}
	if log := granted.Log(); strings.Contains(log, "Violation") {
		t.Fatalf("the server refused the listener what its grant should allow:\n%s", log)
	}
}

func hasKeyWithPrefix(keys []string, prefix string) bool {
	for _, key := range keys {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// One machine's grant writes its own delivery lease as the lease does (Create, a read back by key,
// Update) and is refused a write to another machine's lease key or delivery ledger: no machine's
// user can take or clear another machine's lease.
func TestAMachinesGrantRefusesAnotherMachinesLeaseAndLedger(t *testing.T) {
	seed, public := testnats.User(t)
	granted := testnats.StartGranted(t, testnats.GrantedUser{Public: public, Grant: testnats.ListenerGrant(t, "box-a")})
	errs := make(chan error, 8)
	conn, err := testnats.ConnectSeed(t, granted.URL(), seed,
		natsgo.ErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) { errs <- err }))
	if err != nil {
		t.Fatalf("connect as box-a's user: %v", err)
	}
	t.Cleanup(conn.Close)
	kv, err := openListenerState(conn, 1, 2*time.Second)
	if err != nil {
		t.Fatalf("open the listener state bucket under box-a's grant: %v", err)
	}

	own := "lease." + bus.ListenerDurable("box-a")
	taken, err := kv.Create(own, []byte(`{"holder":"pod-a","boot":"0123456789abcdef"}`))
	if err != nil {
		t.Fatalf("take box-a's lease: %v", err)
	}
	if entry, err := kv.Get(own); err != nil || entry.Revision() != taken {
		t.Fatalf("read back box-a's lease = %v, %v; want revision %d", entry, err, taken)
	}
	if _, err := kv.Update(own, []byte(`{"holder":"pod-a","boot":"0123456789abcdef","released":true}`), taken); err != nil {
		t.Fatalf("release box-a's lease: %v", err)
	}
	select {
	case err := <-errs:
		t.Fatalf("the server refused box-a's own lease: %v", err)
	default:
	}

	for _, refused := range []struct {
		key   string
		write func(key string) error
	}{
		{"lease." + bus.ListenerDurable("box-b"), func(key string) error {
			_, err := kv.Create(key, []byte(`{"holder":"pod-a","boot":"0123456789abcdef"}`))
			return err
		}},
		{"delivery." + bus.ListenerDurable("box-b") + ".0123456789abcdef0123456789abcdef.ses_x", func(key string) error {
			_, err := kv.Put(key, []byte(`{"state":"delivered"}`))
			return err
		}},
	} {
		if err := refused.write(refused.key); err == nil {
			t.Fatalf("box-a's user wrote %s", refused.key)
		}
		subject := "$KV." + bus.ListenerStateBucket + "." + refused.key
		select {
		case err := <-errs:
			if !errors.Is(err, natsgo.ErrPermissionViolation) || !strings.Contains(err.Error(), `Publish to "`+subject+`"`) {
				t.Fatalf("the server's answer to the write of %s: %v, want its refusal", refused.key, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the server did not refuse box-a's write of %s", refused.key)
		}
	}
}
