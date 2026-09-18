package main

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
	"unsafe"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// racingRoleDeleteKV races a fresh SetRole against a role KV's conditional
// Delete: it runs beforeFirstDelete (which installs a replacement claim) and
// returns err on the first call, then delegates normally.
type racingRoleDeleteKV struct {
	natsgo.KeyValue

	deletes           int
	beforeFirstDelete func()
	err               error
}

func (kv *racingRoleDeleteKV) Delete(key string, opts ...natsgo.DeleteOpt) error {
	kv.deletes++
	if kv.deletes == 1 {
		kv.beforeFirstDelete()
		return kv.err
	}
	return kv.KeyValue.Delete(key, opts...)
}

// registryRoleKV and setRegistryRoleKV reach into store.Registry's
// unexported roleKV field so a test can race its conditional Delete the same
// way internal/store/kv_test.go does from inside package store.
func registryRoleKV(t *testing.T, registry *store.Registry) natsgo.KeyValue {
	t.Helper()
	field := reflect.ValueOf(registry).Elem().FieldByName("roleKV")
	return reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface().(natsgo.KeyValue)
}

func setRegistryRoleKV(t *testing.T, registry *store.Registry, kv natsgo.KeyValue) {
	t.Helper()
	field := reflect.ValueOf(registry).Elem().FieldByName("roleKV")
	reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Set(reflect.ValueOf(kv))
}

// TestCoreRoleDeliveryReresolvesSupersededLapsedHolder is the regression test
// for the core (synchronous) role-lane delivery path's counterpart to the
// HTTP handlers' CAS-truthfulness fix: ReleaseExpiredRoleClaim's conditional
// Delete can lose a race to a fresh SetRole and come back ErrKeyExists
// (store.ExpiredRoleClaimSuperseded) after another writer has already
// installed a live replacement claim. roleTopicDelivery must re-resolve to
// that replacement and forward to it, not report delivery_failed about the
// session the fresh claim already superseded -- exactly the "told something
// false about role state" failure this PR exists to eliminate.
func TestCoreRoleDeliveryReresolvesSupersededLapsedHolder(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const (
		role        = "reviewer"
		oldHolder   = "ses_lapsed"
		newHolder   = "ses_replacement"
		roleSubject = "notifications.role.reviewer"
	)

	if _, err := harness.registry.SetRole(oldHolder, "test-machine", role, false); err != nil {
		t.Fatalf("seed lapsed role claim: %v", err)
	}
	if err := harness.sessions.Put(newHolder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register replacement holder: %v", err)
	}

	baseRoleKV := registryRoleKV(t, harness.registry)
	setRegistryRoleKV(t, harness.registry, &racingRoleDeleteKV{
		KeyValue: baseRoleKV,
		beforeFirstDelete: func() {
			if _, err := harness.registry.SetRole(newHolder, "test-machine", role, false); err != nil {
				t.Fatalf("replace role claim during expired-claim release: %v", err)
			}
		},
		err: natsgo.ErrKeyExists,
	})

	exceptions, err := harness.client.Conn.SubscribeSync("notifications.envoy.exceptions." + roleSubject)
	if err != nil {
		t.Fatalf("subscribe to exception lane: %v", err)
	}
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush exception subscription: %v", err)
	}

	forwardedSubjects := []string{}
	config := harness.config
	config.forwardRole = func(subject string, item contracts.Envelope, timeout time.Duration) error {
		forwardedSubjects = append(forwardedSubjects, subject)
		return nil
	}

	item := listenerTestEnvelope(roleSubject, "core-role-supersession-race")
	roleTopicDelivery(config, deliveryMessage{mode: coreDelivery}, item)
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush post-delivery publications: %v", err)
	}

	finalHolder, err := harness.registry.RoleHolder(role)
	if err != nil {
		t.Fatalf("read final role holder: %v", err)
	}
	exceptionReason := ""
	if message, err := exceptions.NextMsg(time.Second); err == nil {
		var envelope contracts.Envelope
		if err := json.Unmarshal(message.Data, &envelope); err != nil {
			t.Fatalf("decode delivery exception envelope: %v", err)
		}
		var payload struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(envelope.Payload), &payload); err != nil {
			t.Fatalf("decode delivery exception payload: %v", err)
		}
		exceptionReason = payload.Reason
	} else if !errors.Is(err, natsgo.ErrTimeout) {
		t.Fatalf("read exception lane: %v", err)
	}

	wantSubject := contracts.AgentSubject(newHolder)
	if len(forwardedSubjects) != 1 || forwardedSubjects[0] != wantSubject || exceptionReason != "" {
		t.Fatalf("superseded lapsed holder race: final holder=%q forwarded=%v exception_reason=%q; want one forward to %q and no delivery exception", finalHolder, forwardedSubjects, exceptionReason, wantSubject)
	}
}
