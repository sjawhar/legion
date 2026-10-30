package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The role lane's forwarder publishes each holder's forwards in the order the handler handed them
// over and never waits for one forward's outcome before publishing the next: while every wait for
// one holder's forwards is held, that holder's later forwards are still published, in order, and
// another holder's forwards are published and settled.
func TestRoleLaneForwarderPublishesEachHoldersForwardsInOrderWithoutWaitingForOutcomes(t *testing.T) {
	const perHolder = 50
	harness := newListenerDeliveryHarness(t, nil)
	silent, live := contracts.AgentSubject("ses_forwarder_silent"), contracts.AgentSubject("ses_forwarder_live")
	release := make(chan struct{})
	var mu sync.Mutex
	published := map[string][]string{}
	var settled atomic.Int32
	cfg := harness.config
	cfg.forwardRole = func(subject string, item contracts.Envelope, _ time.Duration) (func() error, error) {
		mu.Lock()
		published[subject] = append(published[subject], item.EventID)
		mu.Unlock()
		if subject == silent {
			return func() error { <-release; return bus.ErrForwardNotStored }, nil
		}
		return func() error { settled.Add(1); return nil }, nil
	}
	forwarder := newRoleForwarder(cfg)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	handed := map[string][]string{}
	for i := range perHolder {
		for _, sessionID := range []string{"ses_forwarder_silent", "ses_forwarder_live"} {
			item := listenerTestEnvelope(contracts.RoleTopicPrefix+"forwarder-"+sessionID, fmt.Sprintf("forwarder-%s-%d", sessionID, i))
			handed[contracts.AgentSubject(sessionID)] = append(handed[contracts.AgentSubject(sessionID)], item.EventID)
			forwarder.enqueue(roleForward{item: item, sessionID: sessionID})
		}
	}
	waitFor(t, 10*time.Second, "every forward published and the live holder's settled", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(published[silent]) == perHolder && len(published[live]) == perHolder && settled.Load() == perHolder
	})
	mu.Lock()
	for _, subject := range []string{silent, live} {
		if !slices.Equal(published[subject], handed[subject]) {
			mu.Unlock()
			t.Fatalf("forwards to %s published in the order %v, want the order handed over %v", subject, published[subject], handed[subject])
		}
	}
	mu.Unlock()
	if outstanding := forwarder.outstanding(); outstanding != perHolder {
		t.Fatalf("%d forwards outstanding while the held waits are held, want its %d", outstanding, perHolder)
	}

	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !forwarder.wait(ctx) {
		t.Fatalf("%d forwards still outstanding after the held waits ended", forwarder.outstanding())
	}
	if logs := harness.logs.String(); strings.Count(logs, `"msg":"listener role forward failed"`) != perHolder || strings.Count(logs, `"msg":"listener role forwarded"`) != perHolder {
		t.Fatalf("the forwarder reported %d failed forwards and %d delivered, want %d of each:\n%s",
			strings.Count(logs, `"msg":"listener role forward failed"`), strings.Count(logs, `"msg":"listener role forwarded"`), perHolder, logs)
	}
}

// startRoleLaneListeners starts one listener binary per machine id against uri, with env added to
// each one's environment, and waits until every one is healthy. Each one's output is logged when the
// test fails.
func startRoleLaneListeners(t *testing.T, uri string, env []string, machineIDs ...string) []*listenerProcess {
	t.Helper()
	binary := cmdtest.Build(t, "envoy-listener")
	listeners := make([]*listenerProcess, len(machineIDs))
	for i, machineID := range machineIDs {
		listeners[i] = startListenerProcess(t, binary, uri, machineID, env...)
	}
	t.Cleanup(func() {
		if t.Failed() {
			for i, listener := range listeners {
				t.Logf("listener %s:\n%s", machineIDs[i], listener.Output.String())
			}
		}
	})
	for _, listener := range listeners {
		listener.waitHealthy(t)
	}
	return listeners
}

// claimRoleThrough registers a session that takes its own messages off the bus, as Oh My Pi does,
// through listener, has it claim role, and returns its id.
func claimRoleThrough(t *testing.T, listener *listenerProcess, role string) string {
	t.Helper()
	sessionID := "ses_role_lane_" + id.New()
	postListener(t, listener.port, "/v1/interests/subscribe", `{"session_id":"`+sessionID+`","topics":[],"self_subscribed":true}`)
	postListener(t, listener.port, "/v1/roles/set", `{"session_id":"`+sessionID+`","role":"`+role+`"}`)
	return sessionID
}

// roleLaneFrame is a forward a holder stand-in received, with the time it arrived.
type roleLaneFrame struct {
	item    contracts.Envelope
	arrived time.Time
}

// receiveRoleForwards stands in for sessionID's agent pump on conn: it hands every forward it
// receives to the returned channel, with its arrival time, and answers nothing, as a session
// answers no forward (startRoleForward).
func receiveRoleForwards(t *testing.T, conn *natsgo.Conn, sessionID string) <-chan roleLaneFrame {
	t.Helper()
	frames := make(chan roleLaneFrame, 1024)
	subscription, err := conn.Subscribe(contracts.AgentSubject(sessionID), func(msg *natsgo.Msg) {
		arrived := time.Now()
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err != nil {
			t.Errorf("decode a forward to %s: %v", sessionID, err)
			return
		}
		frames <- roleLaneFrame{item: item, arrived: arrived}
	})
	if err != nil {
		t.Fatalf("subscribe as %s: %v", sessionID, err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the subscription as %s: %v", sessionID, err)
	}
	return frames
}

// publishRoleMessage publishes a new role message on role's lane over core NATS, as a session on its
// own connection does, and returns it.
func publishRoleMessage(t *testing.T, conn *natsgo.Conn, role, message string) contracts.Envelope {
	t.Helper()
	item := newEnvelope("agent", contracts.RoleTopicPrefix+role, message)
	data, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal a role message: %v", err)
	}
	if err := conn.Publish(item.Topic, data); err != nil {
		t.Fatalf("publish a role message on %s: %v", item.Topic, err)
	}
	return item
}

// gatherForwards returns the forwards the holder receives until n have arrived and quiet then passes
// with no other, or within passes: every one that arrived, so a caller reports a shortfall or a
// surplus with the rest of what it measured.
func gatherForwards(frames <-chan roleLaneFrame, n int, within, quiet time.Duration) []roleLaneFrame {
	got := make([]roleLaneFrame, 0, n)
	deadline := time.After(within)
	for len(got) < n {
		select {
		case frame := <-frames:
			got = append(got, frame)
		case <-deadline:
			return got
		}
	}
	for {
		select {
		case frame := <-frames:
			got = append(got, frame)
		case <-time.After(quiet):
			return got
		}
	}
}

// requireOncePerMessage fails the test unless frames carry each of published exactly once, each as
// its role forward.
func requireOncePerMessage(t *testing.T, frames []roleLaneFrame, published []contracts.Envelope) {
	t.Helper()
	want := map[string]bool{}
	for _, item := range published {
		want[item.EventID] = true
	}
	seen := map[string]int{}
	for _, frame := range frames {
		if !want[frame.item.EventID] || frame.item.DedupeKey != roleForwardDedupePrefix+"live-topics-test."+frame.item.EventID {
			t.Fatalf("the holder received event %s dedupe key %q, not the role forward of a published message", frame.item.EventID, frame.item.DedupeKey)
		}
		seen[frame.item.EventID]++
	}
	for event, count := range seen {
		if count != 1 {
			t.Fatalf("the holder received event %s %d times, want once", event, count)
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("the holder received %d of the %d published messages", len(seen), len(want))
	}
}

// roleForwardsLogged counts the forwards the listeners logged as delivered on role's lane.
func roleForwardsLogged(role string, listeners ...*listenerProcess) int {
	total := 0
	for _, listener := range listeners {
		total += countRecords(listener, "listener role forwarded", func(record map[string]any) bool {
			return record["topic"] == contracts.RoleTopicPrefix+role
		})
	}
	return total
}

// settledRoleForwards waits, up to 10 s, until the listeners logged want forwards on role's lane,
// then a second more for any past it, and returns how many they logged.
func settledRoleForwards(role string, want int, listeners ...*listenerProcess) int {
	for deadline := time.Now().Add(10 * time.Second); roleForwardsLogged(role, listeners...) < want && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(time.Second)
	return roleForwardsLogged(role, listeners...)
}

// storedForwards counts the messages the notification stream holds on subject.
func storedForwards(t *testing.T, client *bus.Client, subject string) uint64 {
	t.Helper()
	info, err := client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
	if err != nil {
		t.Fatalf("read the stream's messages on %s: %v", subject, err)
	}
	return info.State.Subjects[subject]
}

// metricValue reads the value the listener's /metrics reports for the unlabelled metric name.
func metricValue(t *testing.T, listener *listenerProcess, name string) int64 {
	t.Helper()
	response, err := http.Get("http://127.0.0.1:" + strconv.Itoa(listener.port) + "/metrics")
	if err != nil {
		t.Fatalf("scrape /metrics: %v", err)
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		if value, ok := strings.CutPrefix(scanner.Text(), name+" "); ok {
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				t.Fatalf("metric %s = %q: %v", name, value, err)
			}
			return parsed
		}
	}
	t.Fatalf("/metrics reports no %s", name)
	return 0
}

// Two listeners share the role lane's queue group, so each role message is forwarded by one of
// them: the holder receives each of 100 role messages once, the stream stores one forward per
// message on the holder's agent subject, and the two listeners forwarded 100 in all. In machine
// groups each listener forwards every message, 200 in all, and the holder receives each twice.
func TestRoleLaneForwardsAndStoresEachRoleMessageOnceAcrossTwoListeners(t *testing.T) {
	const messages = 100
	uri := testnats.URL(t)
	listeners := startRoleLaneListeners(t, uri, nil, "role-lane-once-a", "role-lane-once-b")
	role := "role-lane-once-" + id.New()
	holderID := claimRoleThrough(t, listeners[0], role)
	client := connectPublisher(t, uri)
	holderFrames := receiveRoleForwards(t, client.Conn, holderID)

	published := make([]contracts.Envelope, 0, messages)
	for i := range messages {
		published = append(published, publishRoleMessage(t, client.Conn, role, "role lane message "+strconv.Itoa(i)))
	}
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the role messages: %v", err)
	}
	frames := gatherForwards(holderFrames, messages, 10*time.Second, time.Second)
	forwards := settledRoleForwards(role, messages, listeners...)
	stored := storedForwards(t, client, contracts.AgentSubject(holderID))
	t.Logf("%d role messages: the holder received %d forwards, the listeners logged %d, the stream stores %d", messages, len(frames), forwards, stored)
	if len(frames) != messages || forwards != messages || stored != messages {
		t.Fatalf("%d role messages: the holder received %d forwards, the listeners logged %d, the stream stores %d; want %d of each", messages, len(frames), forwards, stored, messages)
	}
	requireOncePerMessage(t, frames, published)
}

// The role lane forwards off its handler, so one role's messages do not wait behind another's:
// across two listeners, two roles take 40 messages a second between them for 10 s, and each of
// the busier role's messages reaches its holder within 1 s of its publish at the 99th percentile.
// The role lane queues what it has not handled rather than dropping it, so every message arrives
// and neither listener reports a slow consumer. A handler that forwarded inline, waiting for the
// stream's acknowledgement of each message before reading the next, stalls behind every one.
func TestRoleLaneForwardsOneRoleWithoutDelayingAnother(t *testing.T) {
	const (
		rate     = 40
		duration = 10 * time.Second
		messages = rate * int(duration/time.Second)
	)
	uri := testnats.URL(t)
	listeners := startRoleLaneListeners(t, uri, nil, "role-lane-busy-a", "role-lane-busy-b")
	otherRole, liveRole := "role-lane-other-"+id.New(), "role-lane-live-"+id.New()
	otherID := claimRoleThrough(t, listeners[0], otherRole)
	liveID := claimRoleThrough(t, listeners[1], liveRole)
	holders := connectPublisher(t, uri)
	otherFrames := receiveRoleForwards(t, holders.Conn, otherID)
	liveFrames := receiveRoleForwards(t, holders.Conn, liveID)
	publisher := testnats.Connect(t, uri)
	t.Cleanup(publisher.Close)

	publishedAt := map[string]time.Time{}
	published := make([]contracts.Envelope, 0, messages)
	ticker := time.NewTicker(time.Second / rate)
	for i := range messages {
		<-ticker.C
		if i%4 == 0 {
			publishRoleMessage(t, publisher, otherRole, "for the other holder "+strconv.Itoa(i))
		}
		item := newEnvelope("agent", contracts.RoleTopicPrefix+liveRole, "for the live holder "+strconv.Itoa(i))
		data, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("marshal a role message: %v", err)
		}
		publishedAt[item.EventID] = time.Now()
		if err := publisher.Publish(item.Topic, data); err != nil {
			t.Fatalf("publish a role message: %v", err)
		}
		published = append(published, item)
	}
	ticker.Stop()

	frames := gatherForwards(liveFrames, messages, 10*time.Second, time.Second)
	latencies := make([]time.Duration, 0, messages)
	for _, frame := range frames {
		latencies = append(latencies, frame.arrived.Sub(publishedAt[frame.item.EventID]))
	}
	// A message that never arrived counts as later than any that did.
	const notReceived = time.Duration(math.MaxInt64)
	for range messages - len(frames) {
		latencies = append(latencies, notReceived)
	}
	slices.Sort(latencies)
	describe := func(latency time.Duration) string {
		if latency == notReceived {
			return "not received"
		}
		return latency.String()
	}
	p50 := latencies[len(latencies)/2]
	p99 := latencies[int(math.Ceil(0.99*float64(len(latencies))))-1]
	slowest := time.Duration(0)
	if received := min(len(frames), messages); received > 0 {
		slowest = latencies[received-1]
	}
	t.Logf("%d messages, %d received: forward latency p50 %s, p99 %s, slowest received %s", messages, len(frames), describe(p50), describe(p99), slowest)
	if p99 >= time.Second {
		t.Fatalf("forward p99 %s (%d of %d received) while another role took its own messages, want under 1 s", describe(p99), len(frames), messages)
	}
	requireOncePerMessage(t, frames, published)
	if len(otherFrames) == 0 {
		t.Fatal("the other role's holder received no forward; the test proves nothing about two roles at once")
	}
	for i, listener := range listeners {
		if slow := metricValue(t, listener, "envoy_role_lane_slow_consumers"); slow != 0 {
			t.Fatalf("listener %d reports %d slow-consumer errors on its role lane, want 0", i, slow)
		}
		if countRecords(listener, "role lane slow consumers", func(map[string]any) bool { return true }) != 0 {
			t.Fatalf("listener %d logged a role lane slow consumer:\n%s", i, listener.Output.String())
		}
	}
}

// On a hub whose grant predates the shared queue group, each listener subscribes the role lane in
// its machine's group, so each of two listeners on two machine ids forwards every role message:
// the holder receives each live twice, and the stream still stores one forward per message, since
// each forward carries the message's dedupe key and the holder's subject as its Nats-Msg-Id.
func TestRoleLaneRefusedSharedQueueFallsBackToMachineGroupsAndStoresOneCopy(t *testing.T) {
	const messages = 20
	machines := []string{"role-lane-grant-a", "role-lane-grant-b"}
	seed, public := testnats.User(t)
	allow := []string{"_INBOX.>", contracts.AgentTopicPrefix + ">"}
	for _, machine := range machines {
		allow = append(allow, contracts.RoleTopicPrefix+"> "+bus.ListenerQueue(machine))
	}
	_, uri := testnats.StartConfig(t, testnats.SubscribeGrantConfig(public, allow...), []string{"-js"}, false)
	t.Setenv("NATS_NKEY_SEED", seed)
	listeners := startRoleLaneListeners(t, uri, []string{"NATS_NKEY_SEED=" + seed}, machines...)
	for i, listener := range listeners {
		queue := bus.ListenerQueue(machines[i])
		if countRecords(listener, "role lane subscribed", func(record map[string]any) bool { return record["queue"] == queue }) != 1 {
			t.Fatalf("listener %s did not subscribe the role lane in its machine's group %s:\n%s", machines[i], queue, listener.Output.String())
		}
	}
	role := "role-lane-grant-" + id.New()
	holderID := claimRoleThrough(t, listeners[0], role)
	client := connectPublisher(t, uri)
	frames := receiveRoleForwards(t, client.Conn, holderID)

	published := make([]contracts.Envelope, 0, messages)
	for i := range messages {
		published = append(published, publishRoleMessage(t, client.Conn, role, "machine group message "+strconv.Itoa(i)))
	}
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the role messages: %v", err)
	}
	received := gatherForwards(frames, 2*messages, 10*time.Second, time.Second)
	perEvent := map[string]int{}
	for _, frame := range received {
		perEvent[frame.item.EventID]++
	}
	for _, item := range published {
		if perEvent[item.EventID] != 2 {
			t.Fatalf("the holder received event %s %d times live, want once from each listener (%d forwards for %d messages)", item.EventID, perEvent[item.EventID], len(received), messages)
		}
	}
	if forwards := settledRoleForwards(role, 2*messages, listeners...); forwards != 2*messages {
		t.Fatalf("the listeners logged %d forwards, want %d, every message from each", forwards, 2*messages)
	}
	if stored := storedForwards(t, client, contracts.AgentSubject(holderID)); stored != messages {
		t.Fatalf("the stream stores %d forwards on the holder's agent subject, want one per message, %d", stored, messages)
	}
}

// A forward the stream does not store is not delivered, though its holder received it live: with
// the agent subjects taken out of the stream, no acknowledgement comes back, and the listener
// reports the role message delivery_failed.
func TestRoleLaneForwardTheStreamDoesNotStoreIsDeliveryFailed(t *testing.T) {
	uri := testnats.URL(t)
	listener := startRoleLaneListeners(t, uri, nil, "role-lane-unstored")[0]
	role := "role-lane-unstored-" + id.New()
	holderID := claimRoleThrough(t, listener, role)
	client := connectPublisher(t, uri)
	frames := receiveRoleForwards(t, client.Conn, holderID)

	info, err := client.JS().StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	config := info.Config
	config.Subjects = slices.DeleteFunc(slices.Clone(config.Subjects), func(subject string) bool {
		return strings.HasPrefix(subject, contracts.AgentTopicPrefix)
	})
	if _, err := client.JS().UpdateStream(&config); err != nil {
		t.Fatalf("take the agent subjects out of the stream: %v", err)
	}
	exceptions, err := client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + role)
	if err != nil {
		t.Fatalf("subscribe to the role's exceptions: %v", err)
	}
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the exception subscription: %v", err)
	}

	item := publishRoleMessage(t, client.Conn, role, "a forward nothing stores")
	if live := gatherForwards(frames, 1, 5*time.Second, 0); len(live) != 1 {
		t.Fatalf("the holder received %d forwards live, want the one; the test proves nothing about a forward the stream did not acknowledge", len(live))
	}
	assertDeliveryException(t, exceptions, item, "delivery_failed")
	waitFor(t, 5*time.Second, "the listener to log the failed forward", func() bool {
		return countRecords(listener, "listener role forward failed", func(record map[string]any) bool {
			return record["event_id"] == item.EventID && strings.Contains(fmt.Sprint(record["error"]), bus.ErrForwardNotStored.Error())
		}) == 1
	})
	if roleForwardsLogged(role, listener) != 0 {
		t.Fatalf("the listener logged the unstored forward as delivered:\n%s", listener.Output.String())
	}
}

// A serving-port session holding a role has no NATS subscriber: when its entry records consumer
// delivery, the stored forward is its delivery, pushed to the port from its consumer, so the role
// message counts as delivered on the stream's acknowledgement alone, reaches the port once, and
// raises no exception.
func TestRoleLaneServingPortHolderTakesTheStoredForward(t *testing.T) {
	uri := testnats.URL(t)
	listener := startRoleLaneListeners(t, uri, nil, "role-lane-serving")[0]
	sessionID := "ses_role_lane_serving_" + id.New()
	role := "role-lane-serving-" + id.New()
	port := startScriptedPort(t, sessionID)
	registerScriptedSession(t, listener, port)
	postListener(t, listener.port, "/v1/roles/set", `{"session_id":"`+sessionID+`","role":"`+role+`"}`)
	client := connectPublisher(t, uri)
	exceptions, err := client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + role)
	if err != nil {
		t.Fatalf("subscribe to the role's exceptions: %v", err)
	}
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the exception subscription: %v", err)
	}

	item := publishRoleMessage(t, client.Conn, role, "a role message for a serving port")
	push := port.next(t, 10*time.Second)
	if push.status != http.StatusNoContent {
		t.Fatalf("the push answered %d, want 204", push.status)
	}
	requirePromptCarries(t, push.text, item)
	waitFor(t, 5*time.Second, "the listener to log the forward delivered", func() bool {
		return countRecords(listener, "listener role forwarded", func(record map[string]any) bool { return record["event_id"] == item.EventID }) == 1
	})
	// The forward window is 2 s: past it, a forward whose outcome the stream never gave would have
	// raised its exception.
	port.noPush(t, roleForwardTimeout+time.Second)
	if message, err := exceptions.NextMsg(100 * time.Millisecond); err == nil {
		t.Fatalf("the role message raised an exception: %s", message.Data)
	}
	if count := countRecords(listener, "listener role forward failed", func(map[string]any) bool { return true }); count != 0 {
		t.Fatalf("the listener logged %d failed forwards for a holder that takes its messages from the stream", count)
	}
}
