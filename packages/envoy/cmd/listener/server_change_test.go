//go:build serverchange

// The acceptance soak of INFRA-547: a message sent through an Envoy listener's POST
// /v1/messages/publish reaches its recipient, or its sender gets an error, while the NATS servers
// of the hub change under the recipient. It runs the real envoy-listener binary twice, the holder's
// listener and the sender's, over a three-server cluster in this process (testnats.StartCluster),
// and a session stand-in that holds its own NATS connection, as an Oh My Pi or Claude Code session
// does, through gates (testnats.StartGate) the test can close. While three senders publish a role
// message, an ad-hoc topic message and a captured topic message every 250 ms for at least a minute,
// it moves the stand-in to another server by a lame-duck shutdown or a forced kill of the server it
// is on, holding it off the hub for the window a plugin's reconnect wait leaves.
//
// Its own build tag, and its own CI job (listener-server-change), because one run sends for a
// minute and then waits out each server's restart and the JetStream catch-up after it, past what
// envoy-go's unit step holds under go test's ten-minute package deadline.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	serverChangeSession  = "ses_server_change_standin"
	serverChangeRole     = "server-change-holder"
	serverChangeAdHoc    = "notifications.team.server-change"
	serverChangeCaptured = "notifications.github.legion-test.server_change.pr.1"
	serverChangeReplicas = 3

	// serverChangeSendInterval is how often each sender sends one numbered message.
	serverChangeSendInterval = 250 * time.Millisecond
	// serverChangeMinimumSending is the least time the senders send for, as the spec asks.
	serverChangeMinimumSending = time.Minute
	// serverChangeDisruptionsOfEachKind is the least number of lame ducks, and of kills, a run makes.
	serverChangeDisruptionsOfEachKind = 2
	// serverChangeGateWindow is how long the stand-in is kept off the hub after its connection
	// drops: the window a plugin's 2 s reconnect wait (reconnectTimeWait in
	// packages/pi-envoy/extensions/envoy.ts) leaves before its next attempt.
	serverChangeGateWindow = 1500 * time.Millisecond
	// serverChangeReconnectWait is that plugin reconnect wait, which the stand-in keeps too.
	serverChangeReconnectWait = 2 * time.Second
	// serverChangeQuiet is the sending after each recovery, and before the first disruption, in which
	// every message must be answered 200 and received (assertion 4).
	serverChangeQuiet = 3 * time.Second
	// serverChangeTail is how long the run waits, once every call has finished, for late frames and
	// the replay of the last reconnect.
	serverChangeTail = 5 * time.Second
	// serverChangeHeartbeat is how often the stand-in registers again, as a plugin heartbeats.
	serverChangeHeartbeat = 30 * time.Second

	// serverChangeAttemptTimeout and serverChangeRetryDelay are envoy-client's publish attempt and
	// the wait before its one retry (DEFAULT_TIMEOUT_MS and RETRY_DELAY_MS in
	// packages/envoy-client/src/transport.ts).
	serverChangeAttemptTimeout = 5 * time.Second
	serverChangeRetryDelay     = 250 * time.Millisecond
	// serverChangeReplayTimeout is envoy-client's replay call timeout (REPLAY_TIMEOUT_MS).
	serverChangeReplayTimeout = 10 * time.Second
	// serverChangeReplayMaxMessages is the most messages the stand-in asks one replay call for, the
	// listener's own cap (replayMaxMessages).
	serverChangeReplayMaxMessages = 500

	// serverChangeRecovery bounds each wait of the run for the cluster, the stand-in or a listener:
	// a member restarting and catching up its streams, a reconnect, a sweep.
	serverChangeRecovery = time.Minute
)

// The three kinds of message a run sends, one sender each, indexing serverChangeKinds.
const (
	kindRole = iota
	kindAdHoc
	kindCaptured
	kindCount
)

// serverChangeKinds names each kind and the topic its sender publishes to: a role topic the stand-in
// holds, an ad-hoc topic no stream subject captures (bus.UsesCoreTransport), and a captured topic
// the stream stores and POST /v1/replay serves to a session that registered it.
var serverChangeKinds = [kindCount]struct{ name, topic string }{
	{"role", contracts.RoleTopicPrefix + serverChangeRole},
	{"ad-hoc", serverChangeAdHoc},
	{"captured", serverChangeCaptured},
}

// soakMessage is one numbered message a sender sent, as one envoy-client publish call: a first
// attempt and, on a 5xx or a transport error, one retry under the same idempotency key.
type soakMessage struct {
	kind, number int
	sentAt       time.Time
	// done closes once the call has its outcome; failure is written before it closes.
	done chan struct{}
	// failure is what the sender was told when the call failed, its retry included: an HTTP status
	// and body, or a transport error. It is "" for a call answered 200.
	failure string
}

// soakFrame is one copy of a message the stand-in received, live on a subscription or from a
// replay.
type soakFrame struct {
	kind, number int
	envelope     contracts.Envelope
	subject      string
	at           time.Time
	replay       bool
}

// soakOutage is one time the stand-in's connection was down: from its drop to its reconnect, and
// the server it reconnected to.
type soakOutage struct {
	dropped, reconnected time.Time
	server               string
}

// soakDisruption is one lame duck or kill of the server the stand-in was on (target), and the
// outage it caused the stand-in.
type soakDisruption struct {
	kind   string
	target string
	outage soakOutage
}

// serverChangeSoak is one run: the cluster, the two listeners, the stand-in's gates and connection,
// and everything the run records for its assertions.
type serverChangeSoak struct {
	cluster *testnats.Cluster
	holder  *listenerProcess
	sender  *listenerProcess
	gates   []*testnats.Gate
	standIn *natsgo.Conn
	// js reads the stream and buckets' cluster state on a connection of the test's own.
	js     natsgo.JetStreamContext
	client *http.Client
	// stopped closes when the test ends, so no background loop outlives it.
	stopped chan struct{}

	// dropped and reconnected carry the stand-in's connection events to the disruption waiting for
	// them; each holds one, and a disruption empties both before it starts.
	dropped     chan time.Time
	reconnected chan string
	// sweepDue wakes the sweeper after a reconnect. cursors is the sweeper's alone once it starts.
	sweepDue chan struct{}
	cursors  map[string]uint64
	calls    sync.WaitGroup

	mu                 sync.Mutex
	messages           []*soakMessage
	frames             []soakFrame
	strays             []string
	outages            []soakOutage
	disruptions        []soakDisruption
	quiet              [][2]time.Time
	droppedRoleReplays int
	sweeps             int
	sweepQueued        bool
	sweepRunning       bool
	sweepFailures      []string
	heartbeatFailures  []string
}

// TestServerChange is the acceptance test of INFRA-547 (plan section "The acceptance test"). The
// stand-in registers through the holder's listener H and claims the role there; the senders publish
// through the sender's listener S, so every message crosses the hub. Each disruption closes the
// stand-in's gates, lame-ducks or kills the server it is on, opens the gates 1.5 s after its
// connection drops, restarts the server, and waits for the cluster to be whole again before 3 s of
// quiet sending. The assertions then say, by kind and message number, that no message was lost
// without its sender's error, that each repeat carried its first copy's id, that the run did the
// moves it claims, and that the route stays live when nothing moves.
func TestServerChange(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	binary := cmdtest.Build(t, "envoy-listener")
	urls := cluster.URLs()
	replicas := "ENVOY_NATS_REPLICAS=" + strconv.Itoa(serverChangeReplicas)
	// Each listener lists the members in another order, so the two do not start on one member.
	holder := startListenerProcess(t, binary, strings.Join([]string{urls[0], urls[1], urls[2]}, ","), "holder-host", replicas)
	holder.waitHealthy(t)
	sender := startListenerProcess(t, binary, strings.Join([]string{urls[2], urls[0], urls[1]}, ","), "sender-host", replicas)
	sender.waitHealthy(t)

	checks, err := natsgo.Connect(cluster.URL(), natsgo.Timeout(time.Second), natsgo.MaxReconnects(-1), natsgo.ReconnectWait(100*time.Millisecond))
	if err != nil {
		t.Fatalf("connect the test's own client to the cluster: %v", err)
	}
	t.Cleanup(checks.Close)
	js, err := checks.JetStream(natsgo.MaxWait(2 * time.Second))
	if err != nil {
		t.Fatalf("open JetStream on the test's own client: %v", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 64
	s := &serverChangeSoak{
		cluster:     cluster,
		holder:      holder,
		sender:      sender,
		js:          js,
		client:      &http.Client{Transport: transport},
		stopped:     make(chan struct{}),
		dropped:     make(chan time.Time, 1),
		reconnected: make(chan string, 1),
		sweepDue:    make(chan struct{}, 1),
		cursors:     map[string]uint64{},
	}
	stop := sync.OnceFunc(func() { close(s.stopped) })
	t.Cleanup(stop)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("holder listener H, last lines:\n%s", lastLines(holder.Output.String(), 300))
			t.Logf("sender listener S, last lines:\n%s", lastLines(sender.Output.String(), 300))
		}
	})
	s.awaitReplicated(t)
	if bus.UsesCoreTransport(serverChangeCaptured) || !bus.UsesCoreTransport(serverChangeAdHoc) {
		t.Fatalf("the captured topic %s must be stored by the stream and the ad-hoc topic %s must not", serverChangeCaptured, serverChangeAdHoc)
	}

	s.startStandIn(t)
	s.awaitStandInSeenBySender(t)

	stopSending := make(chan struct{})
	var senders sync.WaitGroup
	sendStart := time.Now()
	for kind := range kindCount {
		senders.Go(func() { s.sendEvery(kind, stopSending) })
	}
	s.quietSending(t)
	disruptions := map[bool]int{}
	for round := 0; time.Since(sendStart) < serverChangeMinimumSending ||
		disruptions[true] < serverChangeDisruptionsOfEachKind || disruptions[false] < serverChangeDisruptionsOfEachKind; round++ {
		lameDuck := round%2 == 0
		s.disrupt(t, lameDuck)
		disruptions[lameDuck]++
		s.quietSending(t)
	}
	close(stopSending)
	senders.Wait()
	sending := time.Since(sendStart)
	finished := make(chan struct{})
	go func() {
		s.calls.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(2*serverChangeAttemptTimeout + serverChangeRetryDelay + 5*time.Second):
		t.Fatalf("publish calls were still running past envoy-client's two attempts after the last send")
	}
	// The plan names this wait: late frames and the replay of the last reconnect land in it.
	time.Sleep(serverChangeTail)
	s.awaitSweepIdle(t)
	stop()

	t.Logf("sent for %s with %d disruptions", sending.Round(time.Millisecond), len(s.disruptions))
	s.assert(t)
}

// startStandIn connects the session stand-in through one gate per member, subscribes it to its
// agent subject and both topics as the plugins do, registers it through H with the topics,
// claims the role there, starts its cursors at the stream's end, and starts its heartbeat and its
// replay sweeper.
func (s *serverChangeSoak) startStandIn(t *testing.T) {
	t.Helper()
	gateURLs := make([]string, len(s.cluster.Servers))
	for i, server := range s.cluster.Servers {
		gate := testnats.StartGate(t, server)
		s.gates = append(s.gates, gate)
		gateURLs[i] = gate.URL()
	}
	conn, err := natsgo.Connect(strings.Join(gateURLs, ","),
		natsgo.Name("server-change stand-in"),
		natsgo.Timeout(time.Second),
		natsgo.MaxReconnects(-1),
		natsgo.ReconnectWait(serverChangeReconnectWait),
		natsgo.DisconnectErrHandler(func(*natsgo.Conn, error) { s.onDrop(time.Now()) }),
		natsgo.ReconnectHandler(func(c *natsgo.Conn) { s.onReconnect(time.Now(), c.ConnectedServerName()) }),
	)
	if err != nil {
		t.Fatalf("connect the stand-in through its gates: %v", err)
	}
	t.Cleanup(conn.Close)
	s.standIn = conn
	agentSubject := contracts.AgentSubject(serverChangeSession)
	for _, subject := range []string{agentSubject, serverChangeAdHoc, serverChangeCaptured} {
		if _, err := conn.Subscribe(subject, s.receive); err != nil {
			t.Fatalf("the stand-in subscribe %s: %v", subject, err)
		}
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the stand-in's subscriptions: %v", err)
	}

	registration, err := json.Marshal(subscribeBody{
		SessionID:      serverChangeSession,
		Dir:            "/server-change",
		Topics:         []string{agentSubject, serverChangeAdHoc, serverChangeCaptured},
		Title:          "server-change stand-in",
		SelfSubscribed: true,
	})
	if err != nil {
		t.Fatalf("marshal the stand-in's registration: %v", err)
	}
	postListener(t, s.holder.port, "/v1/interests/subscribe", string(registration))
	postListener(t, s.holder.port, "/v1/roles/set", `{"session_id":"`+serverChangeSession+`","role":"`+serverChangeRole+`"}`)
	go s.heartbeat(registration)

	// A replay without a cursor answers the stream's end, where the plugins start one. The answer
	// also shows #2228's replay serves both subjects to this session, the captured topic included.
	var answer replayResponse
	status, body, err := s.call(s.holder.port, http.MethodPost, "/v1/replay", replayRequest{
		SessionID: serverChangeSession,
		Subjects:  []replaySubjectRequest{{Subject: serverChangeCaptured}, {Subject: agentSubject}},
	}, serverChangeReplayTimeout)
	if err != nil || status != http.StatusOK || json.Unmarshal(body, &answer) != nil {
		t.Fatalf("start the stand-in's replay cursors: status %d, %s, %v", status, body, err)
	}
	for _, subject := range answer.Subjects {
		if subject.Refused != "" {
			t.Fatalf("POST /v1/replay refused %s for the stand-in: %s", subject.Subject, subject.Refused)
		}
		s.cursors[subject.Subject] = subject.NextAfter
	}
	if len(s.cursors) != 2 {
		t.Fatalf("POST /v1/replay answered %d subjects, want the two asked for: %s", len(answer.Subjects), body)
	}
	go s.sweeper()
}

// awaitStandInSeenBySender waits until S, the listener the senders call, lists the stand-in with
// both topics and names it the role's holder, so the first message is not refused for a
// registration S has not read yet.
func (s *serverChangeSoak) awaitStandInSeenBySender(t *testing.T) {
	t.Helper()
	waitFor(t, serverChangeRecovery, "S to list the stand-in with both topics and name it the role's holder", func() bool {
		status, body := callListener(t, s.sender.port, http.MethodGet, "/v1/sessions", "")
		var sessions []sessionInfo
		if status != http.StatusOK || json.Unmarshal([]byte(body), &sessions) != nil {
			return false
		}
		listed := slices.ContainsFunc(sessions, func(info sessionInfo) bool {
			return info.SessionID == serverChangeSession &&
				slices.Contains(info.Topics, serverChangeAdHoc) && slices.Contains(info.Topics, serverChangeCaptured)
		})
		if !listed {
			return false
		}
		status, body = callListener(t, s.sender.port, http.MethodGet, "/v1/roles/"+serverChangeRole, "")
		var role struct {
			Holder string `json:"holder"`
		}
		return status == http.StatusOK && json.Unmarshal([]byte(body), &role) == nil && role.Holder == serverChangeSession
	})
}

// receive is the stand-in's handler on every subscription. A frame on its agent subject whose
// envelope topic is another topic is a forward awaiting the session's receipt, answered empty as
// envoy-client's expectsLaneReceipt says; then the frame is recorded as a live copy.
func (s *serverChangeSoak) receive(msg *natsgo.Msg) {
	at := time.Now()
	var envelope contracts.Envelope
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		s.mu.Lock()
		s.strays = append(s.strays, fmt.Sprintf("live frame on %s is no envelope: %v", msg.Subject, err))
		s.mu.Unlock()
		return
	}
	agentSubject := contracts.AgentSubject(serverChangeSession)
	if msg.Subject == agentSubject && msg.Reply != "" && envelope.Topic != "" && envelope.Topic != agentSubject {
		_ = msg.Respond(nil)
	}
	s.record(envelope, msg.Subject, at, false)
}

// record files one received copy under its kind, read from its topic, and its number, read from
// its message text; a frame that names neither is a stray, which the run reports.
func (s *serverChangeSoak) record(envelope contracts.Envelope, subject string, at time.Time, replay bool) {
	kind := slices.IndexFunc(serverChangeKinds[:], func(k struct{ name, topic string }) bool { return k.topic == envelope.Topic })
	fields := strings.Fields(envelope.PayloadSummary)
	number := -1
	if kind >= 0 && len(fields) == 3 && fields[0] == "server-change" && fields[1] == serverChangeKinds[kind].name {
		if parsed, err := strconv.Atoi(fields[2]); err == nil {
			number = parsed
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if number < 0 {
		s.strays = append(s.strays, fmt.Sprintf("frame on %s (replay %t) with topic %q and text %q", subject, replay, envelope.Topic, envelope.PayloadSummary))
		return
	}
	s.frames = append(s.frames, soakFrame{kind: kind, number: number, envelope: envelope, subject: subject, at: at, replay: replay})
}

// onDrop records the stand-in's connection going down and tells a waiting disruption.
func (s *serverChangeSoak) onDrop(at time.Time) {
	s.mu.Lock()
	s.outages = append(s.outages, soakOutage{dropped: at})
	s.mu.Unlock()
	select {
	case s.dropped <- at:
	default:
	}
}

// onReconnect records where the stand-in's connection came back, tells a waiting disruption, and
// wakes the sweeper, as a plugin sweeps the stream after each reconnect.
func (s *serverChangeSoak) onReconnect(at time.Time, server string) {
	s.mu.Lock()
	if n := len(s.outages); n > 0 && s.outages[n-1].reconnected.IsZero() {
		s.outages[n-1].reconnected = at
		s.outages[n-1].server = server
	}
	queue := !s.sweepQueued
	s.sweepQueued = true
	s.mu.Unlock()
	select {
	case s.reconnected <- server:
	default:
	}
	if queue {
		s.sweepDue <- struct{}{}
	}
}

// sweeper runs one sweep for each reconnect, one at a time; a reconnect during a sweep queues
// another after it.
func (s *serverChangeSoak) sweeper() {
	for {
		select {
		case <-s.stopped:
			return
		case <-s.sweepDue:
		}
		s.mu.Lock()
		s.sweepQueued = false
		s.sweepRunning = true
		s.mu.Unlock()
		err := s.sweep()
		s.mu.Lock()
		s.sweepRunning = false
		s.sweeps++
		if err != nil {
			s.sweepFailures = append(s.sweepFailures, err.Error())
		}
		s.mu.Unlock()
	}
}

// sweep reads, through H's POST /v1/replay, what the stream stored for the captured topic and the
// stand-in's agent subject after its cursors, page by page until no subject has more, and retries a
// call that failed (H's own NATS connection may be moving too) until serverChangeRecovery passes.
func (s *serverChangeSoak) sweep() error {
	deadline := time.Now().Add(serverChangeRecovery)
	for {
		more, err := s.replayPage()
		switch {
		case err == nil && !more:
			return nil
		case err == nil:
			continue
		case time.Now().After(deadline):
			return fmt.Errorf("replay sweep did not finish within %s: %w", serverChangeRecovery, err)
		}
		select {
		case <-s.stopped:
			return err
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// replayPage makes one replay call from the cursors, records what it returned and moves the
// cursors, and reports whether a subject has more.
func (s *serverChangeSoak) replayPage() (bool, error) {
	maxMessages := serverChangeReplayMaxMessages
	request := replayRequest{SessionID: serverChangeSession, MaxMessages: &maxMessages}
	for _, subject := range []string{serverChangeCaptured, contracts.AgentSubject(serverChangeSession)} {
		after := s.cursors[subject]
		request.Subjects = append(request.Subjects, replaySubjectRequest{Subject: subject, After: &after})
	}
	status, body, err := s.call(s.holder.port, http.MethodPost, "/v1/replay", request, serverChangeReplayTimeout)
	if err != nil {
		return false, err
	}
	var answer replayResponse
	if status != http.StatusOK || json.Unmarshal(body, &answer) != nil {
		return false, fmt.Errorf("POST /v1/replay: status %d: %s", status, body)
	}
	at := time.Now()
	more := false
	for _, subject := range answer.Subjects {
		if subject.Refused != "" {
			return false, fmt.Errorf("POST /v1/replay refused %s: %s", subject.Subject, subject.Refused)
		}
		for _, stored := range subject.Messages {
			s.replayed(stored, at)
		}
		s.cursors[subject.Subject] = subject.NextAfter
		more = more || subject.More
	}
	return more, nil
}

// replayed records one stored message a sweep returned, but drops a role forward as the plugins'
// isRoleForward does (packages/envoy-client/src/replay.ts): role messages stay live only, so a role
// message counts as received only when it arrived live.
func (s *serverChangeSoak) replayed(stored replayMessage, at time.Time) {
	var envelope contracts.Envelope
	if err := json.Unmarshal([]byte(stored.Data), &envelope); err != nil {
		s.mu.Lock()
		s.strays = append(s.strays, fmt.Sprintf("replayed message %d on %s is no envelope: %v", stored.Seq, stored.Subject, err))
		s.mu.Unlock()
		return
	}
	if strings.HasPrefix(envelope.DedupeKey, roleForwardDedupePrefix) || strings.HasPrefix(envelope.Topic, contracts.RoleTopicPrefix) {
		s.mu.Lock()
		s.droppedRoleReplays++
		s.mu.Unlock()
		return
	}
	s.record(envelope, stored.Subject, at, true)
}

// awaitSweepIdle waits until no sweep is queued or running, so the assertions read every copy the
// last reconnect's sweep found.
func (s *serverChangeSoak) awaitSweepIdle(t *testing.T) {
	t.Helper()
	waitFor(t, serverChangeRecovery, "the stand-in's last replay sweep to finish", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.sweepQueued && !s.sweepRunning
	})
}

// heartbeat registers the stand-in again through H every serverChangeHeartbeat, as a plugin keeps
// its session and its role claim live. A heartbeat H refuses while the hub moves is recorded and
// logged, not fatal: the session stays registered for minutes past a missed one.
func (s *serverChangeSoak) heartbeat(registration []byte) {
	ticker := time.NewTicker(serverChangeHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopped:
			return
		case <-ticker.C:
		}
		status, body, err := s.call(s.holder.port, http.MethodPost, "/v1/interests/subscribe", json.RawMessage(registration), serverChangeAttemptTimeout)
		if err != nil || status != http.StatusOK {
			s.mu.Lock()
			s.heartbeatFailures = append(s.heartbeatFailures, fmt.Sprintf("status %d: %s %v", status, body, err))
			s.mu.Unlock()
		}
	}
}

// sendEvery sends one numbered message of kind every serverChangeSendInterval until stop closes or
// the test ends, each call on its own goroutine as envoy-client's callers make them.
func (s *serverChangeSoak) sendEvery(kind int, stop <-chan struct{}) {
	ticker := time.NewTicker(serverChangeSendInterval)
	defer ticker.Stop()
	for number := 1; ; number++ {
		message := &soakMessage{kind: kind, number: number, sentAt: time.Now(), done: make(chan struct{})}
		s.mu.Lock()
		s.messages = append(s.messages, message)
		s.mu.Unlock()
		s.calls.Go(func() { s.publish(message) })
		select {
		case <-stop:
			return
		case <-s.stopped:
			return
		case <-ticker.C:
		}
	}
}

// publish sends message through S's POST /v1/messages/publish as envoy-client's publish does: a
// UUID idempotency key, a 5 s timeout per attempt, and one retry after 250 ms with the same key on
// a 5xx or a transport error. Any other answer than 200 from the last attempt is the sender's
// error.
func (s *serverChangeSoak) publish(message *soakMessage) {
	defer close(message.done)
	kind := serverChangeKinds[message.kind]
	body := map[string]string{
		"source":          "agent",
		"topic":           kind.topic,
		"message":         fmt.Sprintf("server-change %s %d", kind.name, message.number),
		"idempotency_key": uuid.NewString(),
	}
	for attempt := 1; attempt <= 2; attempt++ {
		status, answer, err := s.call(s.sender.port, http.MethodPost, "/v1/messages/publish", body, serverChangeAttemptTimeout)
		switch {
		case err == nil && status == http.StatusOK:
			message.failure = ""
			return
		case err == nil:
			message.failure = fmt.Sprintf("attempt %d: %d %s", attempt, status, bytes.TrimSpace(answer))
			if status < http.StatusInternalServerError {
				return
			}
		default:
			message.failure = fmt.Sprintf("attempt %d: %v", attempt, err)
		}
		if attempt == 1 {
			time.Sleep(serverChangeRetryDelay)
		}
	}
}

// call sends one request to a listener's /v1 as the test's shared-token caller, bounded by timeout,
// and returns its status and body, or the transport error. Unlike callListener it fails no test, so
// the senders, the heartbeat and the sweeper call it from their own goroutines and count a failure
// as an outcome.
func (s *serverChangeSoak) call(port int, method, path string, body any, timeout time.Duration) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, "http://127.0.0.1:"+strconv.Itoa(port)+path, reader)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+listenerTestToken)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := s.client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, answer, nil
}

// quietSending lets the senders run serverChangeQuiet with nothing moving and records the window
// for assertion 4, then waits for every call of the window to finish, so the next disruption cannot
// cut one short. It fails the run when the stand-in's connection moved inside the window, since the
// window would then not be quiet.
func (s *serverChangeSoak) quietSending(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	outagesBefore := len(s.outages)
	s.mu.Unlock()
	from := time.Now()
	// The plan names this time: 3 s of quiet sending after each recovery.
	time.Sleep(serverChangeQuiet)
	to := time.Now()
	s.mu.Lock()
	moved := len(s.outages) != outagesBefore
	s.quiet = append(s.quiet, [2]time.Time{from, to})
	var window []*soakMessage
	for _, message := range s.messages {
		if !message.sentAt.Before(from) && message.sentAt.Before(to) {
			window = append(window, message)
		}
	}
	s.mu.Unlock()
	if moved || !s.standIn.IsConnected() {
		t.Fatalf("the stand-in's connection moved during a quiet window from %s to %s", from.Format(time.StampMilli), to.Format(time.StampMilli))
	}
	finished := time.After(2*serverChangeAttemptTimeout + serverChangeRetryDelay + 5*time.Second)
	for _, message := range window {
		select {
		case <-message.done:
		case <-finished:
			t.Fatalf("the %s message %d of a quiet window was still unanswered past envoy-client's two attempts", serverChangeKinds[message.kind].name, message.number)
		}
	}
}

// disrupt moves the stand-in off the server it is connected to by a lame duck or a kill of that
// server. Its gates close first, so it cannot reconnect at once; they open 1.5 s after its
// connection drops; once it has reconnected elsewhere the server starts again, and disrupt returns
// when the meta group, the stream and every bucket are whole again (three current replicas) and
// both listeners report healthy.
func (s *serverChangeSoak) disrupt(t *testing.T, lameDuck bool) {
	t.Helper()
	kind := "kill"
	if lameDuck {
		kind = "lame duck"
	}
	target := s.standIn.ConnectedServerName()
	member := s.cluster.Member(target)
	if !s.standIn.IsConnected() || member < 0 {
		t.Fatalf("before a %s the stand-in is on %q (connected %t), no member of the cluster", kind, target, s.standIn.IsConnected())
	}
	server := s.cluster.Servers[member]
	drain(s.dropped)
	drain(s.reconnected)
	for _, gate := range s.gates {
		gate.Close()
	}
	disrupted := make(chan struct{})
	go func() {
		defer close(disrupted)
		if lameDuck {
			server.LameDuck(t)
		} else {
			server.Stop(t)
		}
	}()
	var dropped time.Time
	select {
	case dropped = <-s.dropped:
	case <-time.After(serverChangeRecovery):
		t.Fatalf("the stand-in's connection to %s did not drop within %s of its %s", target, serverChangeRecovery, kind)
	}
	// The plan names this time: the stand-in is held off the hub for the window a plugin's reconnect
	// wait leaves, counted from its drop.
	time.Sleep(time.Until(dropped.Add(serverChangeGateWindow)))
	for _, gate := range s.gates {
		gate.Open()
	}
	var after string
	select {
	case after = <-s.reconnected:
	case <-time.After(serverChangeRecovery):
		t.Fatalf("the stand-in did not reconnect within %s of its gates opening after the %s of %s", serverChangeRecovery, kind, target)
	}
	select {
	case <-disrupted:
	case <-time.After(serverChangeRecovery):
		t.Fatalf("the %s of %s did not finish within %s", kind, target, serverChangeRecovery)
	}
	server.Start(t)
	s.cluster.AwaitReady(t)
	s.awaitReplicated(t)
	s.holder.waitHealthy(t)
	s.sender.waitHealthy(t)
	s.mu.Lock()
	outage := s.outages[len(s.outages)-1]
	s.disruptions = append(s.disruptions, soakDisruption{kind: kind, target: target, outage: outage})
	s.mu.Unlock()
	t.Logf("%s of %s: the stand-in dropped at %s, reconnected to %s after %s",
		kind, target, dropped.Format(time.StampMilli), after, outage.reconnected.Sub(outage.dropped).Round(time.Millisecond))
}

// drain empties a one-slot signal channel, so a wait reads only an event that comes after it.
func drain[T any](signal chan T) {
	select {
	case <-signal:
	default:
	}
}

// awaitReplicated waits until the notification stream and every KV bucket's stream, the three the
// listener cannot run without among them, are at three replicas with a leader and both other
// replicas current and online.
func (s *serverChangeSoak) awaitReplicated(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(serverChangeRecovery)
	var why error
	for time.Now().Before(deadline) {
		if why = s.replicated(); why == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the stream and buckets were not at %d current replicas within %s: %v", serverChangeReplicas, serverChangeRecovery, why)
}

// replicated reports why the stream or a bucket is not fully replicated yet, or nil when all are.
func (s *serverChangeSoak) replicated() error {
	names := []string{bus.Stream}
	for name := range s.js.StreamNames() {
		if strings.HasPrefix(name, "KV_") {
			names = append(names, name)
		}
	}
	for _, bucket := range []string{bus.InterestBucket, bus.RoleClaimBucket, bus.SessionBucket} {
		if !slices.Contains(names, "KV_"+bucket) {
			return fmt.Errorf("bucket %s is not listed among the streams %v", bucket, names)
		}
	}
	for _, name := range names {
		info, err := s.js.StreamInfo(name)
		if err != nil {
			return fmt.Errorf("stream %s: %w", name, err)
		}
		if info.Config.Replicas != serverChangeReplicas {
			return fmt.Errorf("stream %s is configured for %d replicas", name, info.Config.Replicas)
		}
		if info.Cluster == nil || info.Cluster.Leader == "" {
			return fmt.Errorf("stream %s has no leader: %+v", name, info.Cluster)
		}
		if len(info.Cluster.Replicas) != serverChangeReplicas-1 {
			return fmt.Errorf("stream %s led by %s has %d other replicas", name, info.Cluster.Leader, len(info.Cluster.Replicas))
		}
		for _, peer := range info.Cluster.Replicas {
			if !peer.Current || peer.Offline {
				return fmt.Errorf("stream %s replica %s is current %t, offline %t", name, peer.Name, peer.Current, peer.Offline)
			}
		}
	}
	return nil
}

// assert logs what each kind did and checks the plan's four assertions, each failure naming the
// kind and the message numbers.
func (s *serverChangeSoak) assert(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	messages := slices.Clone(s.messages)
	frames := slices.Clone(s.frames)
	strays := slices.Clone(s.strays)
	disruptions := slices.Clone(s.disruptions)
	outages := slices.Clone(s.outages)
	quiet := slices.Clone(s.quiet)
	t.Logf("replay sweeps %d, role forwards a sweep dropped %d, heartbeats H refused %d %v",
		s.sweeps, s.droppedRoleReplays, len(s.heartbeatFailures), s.heartbeatFailures)
	for _, failure := range s.sweepFailures {
		t.Errorf("a replay sweep failed: %s", failure)
	}
	s.mu.Unlock()
	for _, stray := range strays {
		t.Errorf("the stand-in received a frame of no message the senders sent: %s", stray)
	}

	type messageID struct{ kind, number int }
	copies := map[messageID][]soakFrame{}
	for _, frame := range frames {
		id := messageID{frame.kind, frame.number}
		copies[id] = append(copies[id], frame)
	}
	for id := range copies {
		slices.SortStableFunc(copies[id], func(a, b soakFrame) int { return a.at.Compare(b.at) })
	}
	// received is the receipt assertion 1 and 4 count: a live copy, or for the captured kind a copy
	// from a replay too. An ad-hoc copy a sweep found on the agent subject is that session's own
	// stored forward, but the plan counts only the captured kind's replay.
	received := func(message *soakMessage) bool {
		return slices.ContainsFunc(copies[messageID{message.kind, message.number}], func(frame soakFrame) bool {
			return !frame.replay || message.kind == kindCaptured
		})
	}

	for kind := range kindCount {
		name := serverChangeKinds[kind].name
		var sent, live, replay, onlyReplay, errored, repeats int
		var longest time.Duration
		var missing, badRepeats []string
		for _, message := range messages {
			if message.kind != kind {
				continue
			}
			sent++
			if message.failure != "" {
				errored++
			}
			got := copies[messageID{kind, message.number}]
			if len(got) > 0 {
				liveCopy := slices.ContainsFunc(got, func(frame soakFrame) bool { return !frame.replay })
				replayCopy := slices.ContainsFunc(got, func(frame soakFrame) bool { return frame.replay })
				if liveCopy {
					live++
				}
				if replayCopy {
					replay++
				}
				if !liveCopy {
					onlyReplay++
				}
				repeats += len(got) - 1
				longest = max(longest, got[0].at.Sub(message.sentAt))
			}
			// Assertion 1: no message is missing silently.
			if message.failure == "" && !received(message) {
				missing = append(missing, strconv.Itoa(message.number))
			}
			// Assertion 2: every copy after the first carries the first copy's event_id, or its
			// dedupe_key where that key names the event. A role forward's key keeps the role lane's
			// prefix on every copy, so the two keys compare as they arrived; the minted key pattern
			// admits the prefix.
			for _, repeat := range got[min(1, len(got)):] {
				first := got[0].envelope
				if repeat.envelope.EventID == first.EventID ||
					(repeat.envelope.DedupeKey == first.DedupeKey && contracts.DedupeKeyNamesTheUpstreamEvent(repeat.envelope)) {
					continue
				}
				badRepeats = append(badRepeats, fmt.Sprintf("%d (first %s %s, repeat %s %s on %s, replay %t)",
					message.number, first.EventID, first.DedupeKey, repeat.envelope.EventID, repeat.envelope.DedupeKey, repeat.subject, repeat.replay))
			}
		}
		t.Logf("%s: sent %d, received live %d, received by replay %d (only by replay %d), errored %d, repeats %d, longest delay send to first receipt %s",
			name, sent, live, replay, onlyReplay, errored, repeats, longest.Round(time.Millisecond))
		if len(missing) > 0 {
			t.Errorf("assertion 1 (no message missing silently): %s messages %s were answered 200 and never received", name, strings.Join(missing, ", "))
		}
		if len(badRepeats) > 0 {
			t.Errorf("assertion 2 (every repeat has its first id): %s messages %s", name, strings.Join(badRepeats, "; "))
		}
	}

	// Assertion 3: the run did what it claims.
	kinds := map[string]int{}
	for _, disruption := range disruptions {
		kinds[disruption.kind]++
		if disruption.outage.server == "" || disruption.outage.server == disruption.target {
			t.Errorf("assertion 3 (the run did what it claims): after the %s of %s the stand-in reconnected to %q, not another server", disruption.kind, disruption.target, disruption.outage.server)
		}
	}
	for _, kind := range []string{"lame duck", "kill"} {
		if kinds[kind] < serverChangeDisruptionsOfEachKind {
			t.Errorf("assertion 3 (the run did what it claims): %d disruptions by %s, want at least %d", kinds[kind], kind, serverChangeDisruptionsOfEachKind)
		}
	}
	for kind := range kindCount {
		var during []string
		for _, message := range messages {
			if message.kind == kind && slices.ContainsFunc(outages, func(outage soakOutage) bool {
				return !message.sentAt.Before(outage.dropped) && (outage.reconnected.IsZero() || message.sentAt.Before(outage.reconnected))
			}) {
				during = append(during, strconv.Itoa(message.number))
			}
		}
		if len(during) == 0 {
			t.Errorf("assertion 3 (the run did what it claims): no %s message was sent while the stand-in was disconnected", serverChangeKinds[kind].name)
		} else {
			t.Logf("%s messages sent while the stand-in was disconnected: %s", serverChangeKinds[kind].name, strings.Join(during, ", "))
		}
	}

	// Assertion 4: liveness when nothing moves.
	for kind := range kindCount {
		var dead []string
		for _, message := range messages {
			inQuiet := slices.ContainsFunc(quiet, func(window [2]time.Time) bool {
				return !message.sentAt.Before(window[0]) && message.sentAt.Before(window[1])
			})
			if message.kind != kind || !inQuiet {
				continue
			}
			switch {
			case message.failure != "":
				dead = append(dead, fmt.Sprintf("%d (%s)", message.number, message.failure))
			case !received(message):
				dead = append(dead, fmt.Sprintf("%d (answered 200, never received)", message.number))
			}
		}
		if len(dead) > 0 {
			t.Errorf("assertion 4 (liveness): %s messages sent in a quiet window were not answered 200 and received: %s", serverChangeKinds[kind].name, strings.Join(dead, "; "))
		}
	}
}

// lastLines is the last n lines of text, for a failure log that a whole run's output would bury.
func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}
