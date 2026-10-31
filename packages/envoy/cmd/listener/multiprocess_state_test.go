//go:build multiprocess

// The rows of the two-pod suite (multiprocess_harness_test.go) about the state the machine's pods
// share: the delivery ledger, the delivery exceptions one pod's work spares the other, the writes
// a pod whose cache trails must not lose, a pod-local address another pod accepted, and the role
// and last-seen answers every pod must give alike. Its first helpers read a pod's log, its cache
// and the buckets, which these rows do more than the take-over ones.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
)

// mpLineWith is the first line of output containing every one of parts, or "".
func mpLineWith(output string, parts ...string) string {
	for line := range strings.SplitSeq(output, "\n") {
		if !slices.ContainsFunc(parts, func(part string) bool { return !strings.Contains(line, part) }) {
			return line
		}
	}
	return ""
}

// awaitEventLine waits up to within for the pod to log the record msg about event, and returns it.
func (p *mpPod) awaitEventLine(t *testing.T, within time.Duration, msg, event string) string {
	t.Helper()
	var line string
	mpEventually(t, within, fmt.Sprintf("%s to log %q for %s", p.instance, msg, event), func() bool {
		line = mpLineWith(p.Output.String(), `"msg":"`+msg+`"`, `"event_id":"`+event+`"`)
		return line != ""
	})
	return line
}

// swallowNextAck has the pod's relay swallow the next acknowledgement the pod sends the durable
// (nats.go's "+ACK"), and everything the pod sends after it on that link, so the pod handles a
// message to its end and the durable never hears it. The returned func waits for that ack.
func (p *mpPod) swallowNextAck(t *testing.T) (swallowed func()) {
	t.Helper()
	p.relay.arm("+ACK", 1)
	return func() {
		t.Helper()
		select {
		case <-p.relay.swallowed:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s acknowledged nothing to the durable within 10 s", p.instance)
		}
	}
}

// setRole claims role for sessionID through the pod and requires a 200.
func (p *mpPod) setRole(t *testing.T, sessionID, role string) {
	t.Helper()
	answer := p.call(http.MethodPost, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q}`, sessionID, role))
	if answer.status != http.StatusOK {
		t.Fatalf("claim %s for %s through %s: status %d %s %v", role, sessionID, p.instance, answer.status, answer.body, answer.err)
	}
}

// lapsedRole reads GET /v1/roles/<role> on the pod, which must answer 404 for a holder that is
// gone, and returns the answer's fields.
func (p *mpPod) lapsedRole(t *testing.T, role string) map[string]any {
	t.Helper()
	answer := p.call(http.MethodGet, "/v1/roles/"+role, "")
	var body map[string]any
	if answer.err != nil || answer.status != http.StatusNotFound || json.Unmarshal([]byte(answer.body), &body) != nil || body["reason"] != "holder_lapsed" {
		t.Fatalf("GET /v1/roles/%s on %s: %d %s %v, want 404 for a lapsed holder", role, p.instance, answer.status, strings.TrimSpace(answer.body), answer.err)
	}
	return body
}

// requireSameLapsedAnswer fails the test unless two answers for lapsed holders (lapsedRole), about
// one holder, say the same of it in each of fields, compared in the order given.
func requireSameLapsedAnswer(t *testing.T, a, b *mpPod, onA, onB map[string]any, fields ...string) {
	t.Helper()
	for _, field := range fields {
		if fmt.Sprint(onA[field]) != fmt.Sprint(onB[field]) {
			t.Fatalf("%s answers %s %v, %s answers %v; want one answer from every pod: %v and %v", a.instance, field, onA[field], b.instance, onB[field], onA, onB)
		}
	}
}

// cachedSession is the pod's GET /v1/sessions row for sessionID, which the pod answers from its
// cache, or nil while it lists none.
func (p *mpPod) cachedSession(sessionID string) map[string]any {
	answer := p.call(http.MethodGet, "/v1/sessions", "")
	var rows []map[string]any
	if answer.err != nil || answer.status != http.StatusOK || json.Unmarshal([]byte(answer.body), &rows) != nil {
		return nil
	}
	for _, row := range rows {
		if row["session_id"] == sessionID {
			return row
		}
	}
	return nil
}

// cachedTopics is the topics of sessionID's interest as the pod's GET /v1/interests/<session_id>,
// which it answers from its cache, lists them.
func (p *mpPod) cachedTopics(sessionID string) []string {
	answer := p.call(http.MethodGet, "/v1/interests/"+sessionID, "")
	var interest struct {
		Topics []string `json:"topics"`
	}
	if answer.err != nil || answer.status != http.StatusOK || json.Unmarshal([]byte(answer.body), &interest) != nil {
		return nil
	}
	return interest.Topics
}

// stored is key's value in bucket, read from the bucket itself.
func (p *mpPublisher) stored(t *testing.T, bucket, key string) map[string]any {
	t.Helper()
	kv, err := p.client.JS().KeyValue(bucket)
	if err != nil {
		t.Fatalf("open %s: %v", bucket, err)
	}
	entry, err := kv.Get(key)
	if err != nil {
		t.Fatalf("read %s from %s: %v", key, bucket, err)
	}
	var value map[string]any
	if err := json.Unmarshal(entry.Value(), &value); err != nil {
		t.Fatalf("decode %s's %s: %v (%s)", bucket, key, err, entry.Value())
	}
	return value
}

// mpException is a delivery exception a listener published: its own id, and the event, recipient
// and reason its payload reports.
type mpException struct {
	id, event, recipient, reason string
}

// watchExceptions subscribes, over core NATS, to the exception lane of sessionID's inbox, where a
// listener reports a message to it it could not deliver. Every publish there reaches it, a repeat the
// stream does not store again included.
func (p *mpPublisher) watchExceptions(t *testing.T, sessionID string) *natsgo.Subscription {
	t.Helper()
	subject := bus.ExceptionTopicPrefix + contracts.AgentSubject(sessionID)
	subscription, err := p.client.Conn.SubscribeSync(subject)
	if err != nil {
		t.Fatalf("subscribe to %s: %v", subject, err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	if err := p.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the subscription to %s: %v", subject, err)
	}
	return subscription
}

// mpDecodeException reads a delivery exception off the bus.
func mpDecodeException(t *testing.T, data []byte) mpException {
	t.Helper()
	var item contracts.Envelope
	if err := json.Unmarshal(data, &item); err != nil {
		t.Fatalf("decode a delivery exception: %v (%s)", err, data)
	}
	var payload struct {
		EventID          string `json:"event_id"`
		Reason           string `json:"reason"`
		RecipientSession string `json:"recipient_session"`
	}
	if err := json.Unmarshal([]byte(item.Payload), &payload); err != nil {
		t.Fatalf("decode a delivery exception's payload: %v (%s)", err, item.Payload)
	}
	return mpException{id: item.EventID, event: payload.EventID, recipient: payload.RecipientSession, reason: payload.Reason}
}

func mpNextException(t *testing.T, subscription *natsgo.Subscription, within time.Duration) mpException {
	t.Helper()
	msg, err := subscription.NextMsg(within)
	if err != nil {
		t.Fatalf("no delivery exception on %s within %s: %v", subscription.Subject, within, err)
	}
	return mpDecodeException(t, msg.Data)
}

// The machine's pods share one delivery ledger. The active pod prompts session S, the prompt
// lands, and the pod is SIGKILLed before the durable has its acknowledgement (the relay swallows
// it). The standby takes over, the durable gives it the message again, and it finds the delivery
// in the ledger: S is not prompted a second time. Before INFRA-557 each pod remembers only its own
// deliveries, so the standby prompts S again once the message comes back after the 60 s ack wait.
func TestTwoPodsDoNotPromptAgainWhatAKilledPodDelivered(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartServingPort(t)
	const sessionID = "ses_mp_ledger"
	a.registerPort(t, serving, sessionID)
	publisher := mpConnectPublisher(t, uri)

	swallowed := a.swallowNextAck(t)
	m := publisher.mustPublish(t, sessionID)
	swallowed()
	mpEventually(t, time.Second, a.instance+"'s prompt to "+sessionID+" to have landed", func() bool { return serving.promptsTo(sessionID) == 1 })
	killed := a.kill(t)

	// Long enough for a build before INFRA-557 to take the message back, after its 60 s ack wait.
	b.awaitEventLine(t, 90*time.Second, "listener received", m.event)
	tookBack := time.Since(killed)
	time.Sleep(2 * time.Second) // a prompt goes out the moment the message is received
	serving.requireOnce(t, []mpMessage{m})
	if line := mpLineWith(b.Output.String(), `"msg":"listener dedupe skip"`, `"event_id":"`+m.event+`"`); line == "" {
		t.Fatalf("%s took %s back and did not skip it as delivered", b.instance, m.event)
	}
	t.Logf("%s took %s back %s after the kill and skipped it as delivered", b.instance, m.event, tookBack.Round(time.Millisecond))
}

// An attempt, then a crash: the active pod records in the shared ledger that it is prompting
// session S and is SIGKILLed while that prompt is unanswered (the serving port holds it). The
// standby takes the killed pod's expired lease, so the attempt is a former holder's: it takes the
// attempt over at once rather than deferring it as a live pod's, and S is prompted, once, within
// the hard take-over bound of the kill (a repeat with the same event id is allowed; the held
// prompt was never answered). Before INFRA-557 the durable's 60 s ack wait keeps the message from
// the standby past that bound.
func TestTwoPodsPromptWhatAKilledPodOnlyAttempted(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartServingPort(t)
	const sessionID = "ses_mp_attempt"
	a.registerPort(t, serving, sessionID)
	publisher := mpConnectPublisher(t, uri)

	inHand := serving.hold(sessionID)
	m := publisher.mustPublish(t, sessionID)
	select {
	case <-inHand:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never prompted %s", a.instance, sessionID)
	}
	killed := a.kill(t)

	const bound = mpHardBound
	messages := []mpMessage{m}
	serving.awaitAll(t, messages, bound+5*time.Second)
	serving.requireWithin(t, messages, bound, killed)
	time.Sleep(2 * time.Second)
	serving.requireOnce(t, messages)
	if line := mpLineWith(b.Output.String(), `"msg":"listener delivery deferred to another pod's attempt"`, `"event_id":"`+m.event+`"`); line != "" {
		t.Fatalf("%s deferred %s as a live pod's attempt, though %s held the lease it took over: %s", b.instance, m.event, a.instance, line)
	}
}

// A message to a session that is not registered is reported no_holder. When the pod that reported
// it is SIGKILLed before the durable has its acknowledgement, the standby takes the message over
// and reports it again: both exceptions carry one id, and the stream stores one. Before INFRA-557
// each report minted an id of its own, and the stream kept both. The pods run with no unplaceable
// retry window (ENVOY_UNPLACEABLE_RETRY_WINDOW=0), so each gives the message up, with its
// no_holder, on the delivery it handles rather than deferring it for a minute first.
func TestTwoPodsReportARedeliveredNoHolderOnceUnderOneID(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri, "ENVOY_UNPLACEABLE_RETRY_WINDOW=0")
	publisher := mpConnectPublisher(t, uri)
	const nobody = "ses_mp_nobody"
	exceptions := publisher.watchExceptions(t, nobody)

	swallowed := a.swallowNextAck(t)
	m := publisher.mustPublish(t, nobody)
	swallowed()
	first := mpNextException(t, exceptions, 10*time.Second)
	killed := a.kill(t)
	// Long enough for a build before INFRA-557 to take the message back, after its 60 s ack wait.
	second := mpNextException(t, exceptions, 90*time.Second)
	t.Logf("%s reported %s again %s after %s was killed", b.instance, m.event, time.Since(killed).Round(time.Millisecond), a.instance)

	for _, exception := range []mpException{first, second} {
		if exception.event != m.event || exception.recipient != nobody || exception.reason != "no_holder" {
			t.Fatalf("exception %+v, want no_holder for %s to %s", exception, m.event, nobody)
		}
	}
	if first.id != second.id {
		t.Fatalf("the two reports of %s's no_holder carry the ids %s and %s, want one", m.event, first.id, second.id)
	}
	subject := bus.ExceptionTopicPrefix + contracts.AgentSubject(nobody)
	info, err := publisher.client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
	if err != nil {
		t.Fatalf("read the stream's messages on %s: %v", subject, err)
	}
	if stored := info.State.Subjects[subject]; stored != 1 {
		t.Fatalf("the stream holds %d exceptions on %s, want 1", stored, subject)
	}
}

// A send and a role claim through the pod whose cache trails: the standby sits behind a 300 ms slow
// link, a session registers through the active pod, and at once a POST /v1/messages/send to it, and
// for another such session a role set, go to the standby. Both answer 200, read from the bucket.
// Before INFRA-557 the standby answers from its cache alone, which has not seen the registration:
// 404 "no live session" and 404 "session is not registered". The link is slow enough that the
// standby's cache trails each write by far more than the test takes to make its next request, and
// fast enough that a role set's eight or so bucket round trips fit the route's 3.5 s budget
// (sendRequestBudget).
func TestTwoPodsSendAndRoleSetThroughThePodWhoseCacheTrails(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	b.relay.delay(300 * time.Millisecond)
	const target, claimant, role = "ses_mp_unseen_target", "ses_mp_unseen_claimant", "mp-unseen"

	a.subscribe(t, fmt.Sprintf(`{"session_id":%q,"topics":[],"self_subscribed":true}`, target))
	send := b.call(http.MethodPost, "/v1/messages/send", fmt.Sprintf(`{"target_session":%q,"message":"sent through the other pod","source":"agent","source_session":"ses_mp_sender"}`, target))
	a.subscribe(t, fmt.Sprintf(`{"session_id":%q,"topics":[],"self_subscribed":true}`, claimant))
	claim := b.call(http.MethodPost, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q}`, claimant, role))
	if send.status != http.StatusOK {
		t.Fatalf("send to %s through %s at once after it registered through %s: %d %s %v, want 200", target, b.instance, a.instance, send.status, strings.TrimSpace(send.body), send.err)
	}
	if claim.status != http.StatusOK {
		t.Fatalf("role set for %s through %s at once after it registered through %s: %d %s %v, want 200", claimant, b.instance, a.instance, claim.status, strings.TrimSpace(claim.body), claim.err)
	}
	mpEventually(t, 5*time.Second, a.instance+" to answer "+claimant+" as "+role+"'s holder", func() bool {
		answer := a.call(http.MethodGet, "/v1/roles/"+role, "")
		return answer.status == http.StatusOK && strings.Contains(answer.body, `"holder":"`+claimant+`"`)
	})
	t.Logf("the send answered in %s and the role set in %s", send.took.Round(time.Millisecond), claim.took.Round(time.Millisecond))
}

// Writes through the pod whose cache trails keep the other pod's newer write: the standby sits
// behind a 300 ms slow link, so a write through the active pod reaches its cache 300 ms late (slow
// enough to trail, fast enough that a role set's bucket round trips fit sendRequestBudget). A
// heartbeat on the standby within that window, of a process that does not drive the session, keeps
// the driving claim written through the active pod; a heartbeat on the standby keeps a topic added
// through the active pod; and a role set on the standby keeps the newer host recorded through the
// active pod. Before INFRA-557 the standby wrote its stale copy over each.
func TestTwoPodsWritesOnThePodWhoseCacheTrailsKeepTheNewerWrite(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	b.relay.delay(300 * time.Millisecond)
	publisher := mpConnectPublisher(t, uri)

	t.Run("a driving claim", func(t *testing.T) {
		const sessionID = "ses_mp_cas_driving"
		bystander := fmt.Sprintf(`{"session_id":%q,"topics":[],"port":34751,"host":"127.0.0.1","dir":"/work/bystander"}`, sessionID)
		a.subscribe(t, bystander)
		mpEventually(t, 10*time.Second, b.instance+" to hold the bystander's registration", func() bool {
			row := b.cachedSession(sessionID)
			return row != nil && row["port"] == float64(34751)
		})
		// The heartbeat on the standby writes the session's interest first, and its reply comes over
		// the slow link 300 ms later; only then does it write the registration. The driving
		// claim lands on the active pod inside those 300 ms, so the standby's cache has not seen
		// it when the registration is written. (A claim written before the heartbeat began would have
		// reached the standby's cache by then, ahead of that reply.)
		heartbeat := make(chan mpAnswer, 1)
		go func() { heartbeat <- b.call(http.MethodPost, "/v1/interests/subscribe", bystander) }()
		time.Sleep(150 * time.Millisecond)
		a.subscribe(t, fmt.Sprintf(`{"session_id":%q,"topics":[],"port":42145,"host":"127.0.0.1","dir":"/work/driver","driving":true}`, sessionID))
		if answer := <-heartbeat; answer.status != http.StatusOK {
			t.Fatalf("the bystander's heartbeat on %s: status %d %s %v", b.instance, answer.status, answer.body, answer.err)
		}
		if stored := publisher.stored(t, bus.SessionBucket, sessionID); stored["port"] != float64(42145) || stored["driving"] != true {
			t.Fatalf("%s's registration after the bystander's heartbeat on %s: %v, want the driving claim on port 42145", sessionID, b.instance, stored)
		}
	})

	t.Run("a topic", func(t *testing.T) {
		const sessionID, first, added = "ses_mp_cas_topic", "notifications.team.mp-first", "notifications.team.mp-added"
		heartbeat := fmt.Sprintf(`{"session_id":%q,"topics":[%q],"self_subscribed":true}`, sessionID, first)
		a.subscribe(t, heartbeat)
		mpEventually(t, 10*time.Second, b.instance+" to hold "+sessionID+"'s interest", func() bool {
			return slices.Contains(b.cachedTopics(sessionID), first)
		})
		a.subscribe(t, fmt.Sprintf(`{"session_id":%q,"topics":[%q],"self_subscribed":true}`, sessionID, added))
		b.subscribe(t, heartbeat)
		stored := publisher.stored(t, bus.InterestBucket, sessionID)
		topics, _ := stored["topics"].([]any)
		if !slices.Contains(topics, any(first)) || !slices.Contains(topics, any(added)) {
			t.Fatalf("%s's interest after a heartbeat on %s: %v, want %s and %s", sessionID, b.instance, stored, first, added)
		}
	})

	t.Run("a host", func(t *testing.T) {
		const sessionID, role = "ses_mp_cas_host", "mp-cas-host"
		register := func(host string) {
			t.Helper()
			a.subscribe(t, fmt.Sprintf(`{"session_id":%q,"topics":[],"self_subscribed":true,"host":%q}`, sessionID, host))
		}
		register("example-host-a")
		mpEventually(t, 10*time.Second, b.instance+" to hold "+sessionID+" on example-host-a", func() bool {
			row := b.cachedSession(sessionID)
			return row != nil && row["host"] == "example-host-a"
		})
		register("example-host-b")
		b.setRole(t, sessionID, role)
		if stored := publisher.stored(t, bus.SessionBucket, sessionID); stored["host"] != "example-host-b" || stored["self_subscribed"] != true {
			t.Fatalf("%s's registration after a role set on %s: %v, want host example-host-b, self-subscribed", sessionID, b.instance, stored)
		}
	})
}

// A delivery address local to a pod: both pods reach their machine's sessions through a loopback
// host bridge, and each names its own host. A session registers on its port through the active
// pod; once the standby has taken over, it does not prompt that session on its own loopback, which
// is not the accepting pod's: it reports delivery_failed, address not reachable from this listener
// pod, and the session is sent nothing. Two pods that share a host share its loopback, and the
// standby of such a pair delivers. Before INFRA-557 the standby prompts its own loopback.
func TestTwoPodsRefuseAPodLocalAddressAnotherPodAccepted(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	for _, pair := range []struct {
		name         string
		hostA, hostB string
	}{
		{"pods on two hosts", "node-a", "node-b"},
		{"pods on one host", "node-a", "node-a"},
	} {
		t.Run(pair.name, func(t *testing.T) {
			uri := mpNATS(t)
			a := mpStartPod(t, binary, uri, "pod-a", "ENVOY_HOST_BRIDGE=127.0.0.1", "ENVOY_POD_HOSTNAME="+pair.hostA)
			mpWaitActive(t, a, 30*time.Second)
			b := mpStartPod(t, binary, uri, "pod-b", "ENVOY_HOST_BRIDGE=127.0.0.1", "ENVOY_POD_HOSTNAME="+pair.hostB)
			mpWaitStandby(t, b, 30*time.Second)
			serving := mpStartServingPort(t)
			const sessionID = "ses_mp_pod_local"
			a.registerPort(t, serving, sessionID)
			publisher := mpConnectPublisher(t, uri)
			exceptions := publisher.watchExceptions(t, sessionID)

			a.Terminate(t)
			a.WaitExit(t, "SIGTERM")
			// A build before INFRA-557 keeps the durable through its shutdown, and the standby binds
			// only after it.
			mpWaitActive(t, b, 30*time.Second)
			m := publisher.mustPublish(t, sessionID)

			if pair.hostA == pair.hostB {
				serving.awaitAll(t, []mpMessage{m}, mpGracefulBound)
				time.Sleep(time.Second)
				serving.requireOnce(t, []mpMessage{m})
				if msg, err := exceptions.NextMsg(time.Second); err == nil {
					t.Fatalf("%s delivered %s and reported it too: %+v", b.instance, m.event, mpDecodeException(t, msg.Data))
				}
				return
			}
			var exception mpException
			for deadline := time.Now().Add(mpGracefulBound); ; {
				if prompts := serving.promptsTo(sessionID); prompts > 0 {
					t.Fatalf("%s prompted %s %d times on its own loopback, which %s accepted the registration for", b.instance, sessionID, prompts, a.instance)
				}
				if msg, err := exceptions.NextMsg(100 * time.Millisecond); err == nil {
					exception = mpDecodeException(t, msg.Data)
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s neither delivered %s nor reported it within %s", b.instance, m.event, mpGracefulBound)
				}
			}
			if exception.event != m.event || exception.recipient != sessionID || exception.reason != "delivery_failed" {
				t.Fatalf("exception %+v, want delivery_failed for %s to %s", exception, m.event, sessionID)
			}
			if line := mpLineWith(b.Output.String(), `"event_id":"`+m.event+`"`, "address not reachable from this listener pod"); line == "" {
				t.Fatalf("%s logged no line about %s naming the address not reachable from this listener pod", b.instance, m.event)
			}
			time.Sleep(time.Second)
			if prompts := serving.promptsTo(sessionID); prompts > 0 {
				t.Fatalf("%s prompted %s %d times after it reported the address unreachable", b.instance, sessionID, prompts)
			}
		})
	}
}

// A role claim whose holder is gone is answered alike by every pod, whenever each started: the
// active pod has served for a while when the claim is made and its holder's session then ends,
// and a standby starts after. Each releases the claim it is asked about. Before INFRA-557 each pod
// counted the claim's grace from its own start, so the pod that started after the claim retained
// it while the other released it.
func TestTwoPodsAnswerARoleWhoseHolderLapsedAlike(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a := mpStartPod(t, binary, uri, "pod-a")
	mpWaitActive(t, a, 30*time.Second)
	time.Sleep(2 * time.Second) // the machine serves for a while before the claim
	const holder = "ses_mp_grace_holder"
	a.subscribe(t, fmt.Sprintf(`{"session_id":%q,"topics":[],"self_subscribed":true}`, holder))
	// One role for each pod to look up: a lookup that releases a claim leaves the other pod none to
	// judge.
	a.setRole(t, holder, "mp-grace-a")
	a.setRole(t, holder, "mp-grace-b")
	if answer := a.call(http.MethodDelete, "/v1/sessions/"+holder, ""); answer.status != http.StatusOK {
		t.Fatalf("end %s's session through %s: %d %s %v", holder, a.instance, answer.status, answer.body, answer.err)
	}
	b := mpStartPod(t, binary, uri, "pod-b")
	mpWaitStandby(t, b, 30*time.Second)

	onA := a.lapsedRole(t, "mp-grace-a")
	onB := b.lapsedRole(t, "mp-grace-b")
	requireSameLapsedAnswer(t, a, b, onA, onB, "holder", "claim_released", "last_seen")
	if onA["claim_released"] != true {
		t.Fatalf("%s kept a claim made after the machine began serving: %v", a.instance, onA)
	}
}

// A role holder whose session expired while only the active pod ran is reported with one last_seen
// by a standby started after it lapsed. The session bucket is made with a 10 s lifetime, as
// startup_test.go's buckets are, so the registration expires within the row. Before INFRA-557 the
// last-seen time lived in the memory of a pod whose cache had held the session, and the standby
// reported none.
func TestTwoPodsReportALapsedHoldersLastSeenAlike(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	publisher := mpConnectPublisher(t, uri)
	const ttl = 10 * time.Second
	sessionKV, err := publisher.client.JS().CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.SessionBucket, TTL: ttl, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create the session bucket with a %s lifetime: %v", ttl, err)
	}
	a := mpStartPod(t, binary, uri, "pod-a")
	mpWaitActive(t, a, 30*time.Second)
	const holder = "ses_mp_seen_holder"
	a.subscribe(t, fmt.Sprintf(`{"session_id":%q,"topics":[],"self_subscribed":true}`, holder))
	a.setRole(t, holder, "mp-seen-a")
	a.setRole(t, holder, "mp-seen-b")
	mpEventually(t, ttl+10*time.Second, holder+"'s registration to expire", func() bool {
		_, err := sessionKV.Get(holder)
		return errors.Is(err, natsgo.ErrKeyNotFound)
	})
	b := mpStartPod(t, binary, uri, "pod-b")
	mpWaitStandby(t, b, 30*time.Second)

	onA := a.lapsedRole(t, "mp-seen-a")
	onB := b.lapsedRole(t, "mp-seen-b")
	if onA["last_seen"] == nil {
		t.Fatalf("%s reports no last_seen for %s: %v", a.instance, holder, onA)
	}
	requireSameLapsedAnswer(t, a, b, onA, onB, "holder", "last_seen", "claim_released")
}
