package main

import (
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/session"
)

// The role lane forwards an envelope to its holder with StartRequestStoredTo, under
// contracts.RoleForwardDedupePrefix and the envelope's own dedupe key, to the holder's agent
// subject, which the stream captures, and chooses the forward's MsgId from the envelope before the
// prefix is added (roleForward.msgID). A forward of an envelope whose key names its event -
// Dispatch's, or a webhook's, whose key the prefix hides from
// contracts.DedupeKeyNamesTheUpstreamEvent - is named by that key and is stored once however often
// it is sent. Any other forward is named by its event id, so two distinct envelopes that share a key
// their sender chose are two messages, and the stream stores both. Each envelope is delivered by a
// handler with dedupe and attempt caches of its own, as another listener of the machine's queue
// group, or this one after a restart, would deliver it again.
func TestARoleForwardIsStoredOncePerMessageAndTwoUnderOneChosenKeyTwice(t *testing.T) {
	chosen := func(name string) contracts.Envelope {
		item := listenerTestEnvelope("", "shared-key")
		item.SourceEventID = "src-" + name
		return item
	}
	dispatch := listenerTestEnvelope("", "dispatch-7")
	dispatch.Source, dispatch.SourceEventID = "dispatch", "7"
	webhook := listenerTestEnvelope("", "github.guid-1")
	webhook.Source, webhook.SourceEventID = "github", "guid-1"
	for _, tc := range []struct {
		name, holder string
		sends        []contracts.Envelope
		want         uint64
	}{
		{"two envelopes under one key their caller chose", "ses_forward_chosen", []contracts.Envelope{chosen("first"), chosen("second")}, 2},
		{"a dispatch envelope sent twice", "ses_forward_dispatch", []contracts.Envelope{dispatch, dispatch}, 1},
		{"a webhook envelope sent twice", "ses_forward_webhook", []contracts.Envelope{webhook, webhook}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harness := newListenerDeliveryHarness(t)
			const role = "forward-dedupe"
			if err := harness.sessions.Put(tc.holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
				t.Fatalf("register the holder: %v", err)
			}
			if _, err := harness.registry.SetRole(tc.holder, "test-machine", role, false); err != nil {
				t.Fatalf("claim the role: %v", err)
			}
			subject := contracts.AgentSubject(tc.holder)
			forwardThrough := startRoleForward(harness.client)

			for index, item := range tc.sends {
				cfg := harness.config
				cfg.dedupeCache, cfg.attemptCache = dedupe.New(10*time.Minute), dedupe.New(5*time.Minute)
				t.Cleanup(cfg.dedupeCache.Stop)
				t.Cleanup(cfg.attemptCache.Stop)
				var forwardErr error
				forwards := 0
				cfg.forwardRole = func(subject string, item contracts.Envelope, msgID string, timeout time.Duration) (func() error, error) {
					forwards++
<<<<<<< conflict 1 of 1
+++++++ zwxwlryn 92c90c86 "merge: main into INFRA-771, past f5bdbada so worker-image/build has repo-paths.sh"
					wait, err := forwardThrough(subject, item, msgID, timeout)
					if err != nil {
						forwardErr = err
						return nil, err
					}
					return func() error {
						forwardErr = wait()
						return forwardErr
					}, nil
				}
				cfg = wireTestDeliveryWorkers(t, cfg)
%%%%%%% diff from: rmmmynwt 8bfdf258 "fix(dispatch): a version that restores the approved text keeps its approval (#2703)"
\\\\\\\        to: lmonpkut e2b93abb "ci: the envoy-go lane gets 60 minutes, and its test steps time out inside it (#2739)"
 					forwardErr = harness.client.RequestCoreTo(subject, item, msgID, timeout)
 					return forwardErr
 				})
-				cfg = wireTestDeliveryWorkers(t, cfg)
+				cfg = wireTestRoleForwarder(t, cfg)
>>>>>>> conflict 1 of 1 ends
				item.Topic = contracts.RoleTopicPrefix + role
				item.Payload = `{"type":"worker-queued"}`
				item.IssuedAt = contracts.NowMillis()
				coreNATSDeliveryHandler(cfg)(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
				cfg.roleForwards.settled(t)
				if forwards != 1 || forwardErr != nil {
					t.Fatalf("delivery %d forwarded %d times, the last returning %v; want one forward that returned nil", index+1, forwards, forwardErr)
				}
			}
			// The stream takes the messages one connection sends in order, so once it has
			// acknowledged a later one, every forward before it is stored or dropped.
			if _, err := harness.client.JS().Publish(subject+"_after", []byte("{}")); err != nil {
				t.Fatalf("publish the message after the forwards: %v", err)
			}
			info, err := harness.client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
			if err != nil {
				t.Fatalf("read the stream: %v", err)
			}
			if got := info.State.Subjects[subject]; got != tc.want {
				t.Fatalf("the stream holds %d copies on %s after %d forwards, want %d", got, subject, len(tc.sends), tc.want)
			}
		})
	}
}
