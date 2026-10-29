//go:build multiprocess

// Two listener processes for one machine id, which is what a machine runs once it has an active
// pod and a standby (INFRA-557). Every row here starts real cmd/listener binaries against one
// in-process NATS server, each behind a listenerRelay of its own, and drives them over their real
// HTTP API and the real bus, because each thing it checks is a property of whole processes: which
// of two processes delivers from the machine's durable, what happens to the messages one process
// holds when it is stopped, killed or cut off, and what the other answers meanwhile. Port sessions
// are one fake serving port (mpServingPort) that counts each completed prompt_async by event id;
// the machine id is arbitrary (box-a). Every pod connects as the machine's nkey user, holding
// exactly the grant envoy-infra renders for it (mpStartNATS), so a permissions gap fails the row;
// the row's own clients connect as the server's admin.
//
// The rows use nothing but the binary cmdtest.Build builds, its environment, its HTTP API, NATS,
// the relay in reconnect_shutdown_test.go and APIs of the internal packages that a build before
// INFRA-557 (819dc283) has too, but for testnats' grant fixtures (grant.go and testdata/), so this
// file compiles there with that relay, cmdtest and those beside it and each row shows how that
// build fails. The few places a row has to recognise such a build (a /healthz without "role") say
// so.
//
// Its own build tag, and its own CI job, for the reason cmd/dispatch/multiprocess_test.go has
// its: a row starts two processes and waits out a lease, an ack wait or a 140 s standby, tens of
// seconds to minutes each, which go test's default per-package deadline in the unit lane cannot
// hold.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	// mpMachine is every row's machine id: any machine's, not Dispatch's.
	mpMachine = "box-a"
	// mpToken is the /v1 bearer every pod of a row is given.
	mpToken = "multiprocess-listener-token"
	// mpPoll is how often a row reads a pod's /healthz or /v1/sessions while it watches it.
	mpPoll = 250 * time.Millisecond
	// mpGracefulBound and mpHardBound are the take-over bounds (architect, 2026-10-27): after a
	// graceful stop every message reaches its session within 10 s of its publish; after a SIGKILL
	// or a lost node within 15 s, in per-session publish order.
	mpGracefulBound = 10 * time.Second
	mpHardBound     = 15 * time.Second
	// mpQuiet is how long a row watches for a repeat after every message arrived: past the ack wait
	// (6 s), so a message a pod held without acking and that came back would have arrived.
	mpQuiet = 8 * time.Second
)

// mpHTTP is every row's HTTP client. Its timeout is the 10 s every answer a row asserts on must
// come within.
var mpHTTP = &http.Client{Timeout: 10 * time.Second}

// mpPod is one listener process of the machine, behind its own relay to NATS.
type mpPod struct {
	*cmdtest.Process
	instance string
	port     int
	relay    *listenerRelay
}

// mpStartPod starts binary as instance of mpMachine, on a loopback port the kernel chose and the
// test holds until the pod serves on it (cmdtest.HoldPort, inherited as ENVOY_LISTEN_FD), reaching
// the NATS at natsURL, which mpStartNATS started, through a relay of its own as mpMachine's
// granted user, with env added to its environment. The pod is killed when the test ends, and its
// output's tail is logged when the test failed.
func mpStartPod(t *testing.T, binary, natsURL, instance string, env ...string) *mpPod {
	t.Helper()
	seed, granted := mpSeeds.Load(natsURL)
	if !granted {
		t.Fatalf("%s is no NATS server mpStartNATS started: every pod connects as its machine's granted user", natsURL)
	}
	server, err := url.Parse(natsURL)
	if err != nil {
		t.Fatalf("parse %s: %v", natsURL, err)
	}
	server.User = nil
	relay := startListenerRelay(t, server.String())
	held := cmdtest.HoldPort(t)
	cmd := exec.Command(binary)
	cmd.Env = append([]string{
		"PORT=" + strconv.Itoa(held.Port),
		"ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_MACHINE_ID=" + mpMachine,
		"ENVOY_INSTANCE_ID=" + instance,
		"NATS_URLS=" + relay.relayed,
		"NATS_NKEY_SEED=" + seed.(string),
		"ENVOY_API_TOKEN=" + mpToken,
	}, env...)
	held.Pass(cmd, "ENVOY_LISTEN_FD")
	pod := &mpPod{Process: cmdtest.Start(t, "the listener "+instance, cmd), instance: instance, port: held.Port, relay: relay}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s's output, but for each delivery's lines (last 150 lines):\n%s", instance, mpTail(mpLinesWithout(pod.Output.String()), 150))
		}
	})
	return pod
}

// mpSeeds maps the URL each row's own clients reach its NATS server at (mpStartNATS) to the seed
// of the nkey user every pod of the row connects as.
var mpSeeds sync.Map

// mpNATS starts the row's NATS server with the listener's grant (mpStartNATS) and returns the URL
// the row's own clients use.
func mpNATS(t *testing.T) string {
	t.Helper()
	_, uri := mpStartNATS(t, testnats.ListenerGrant)
	return uri
}

// mpStartNATS starts a row's NATS server, where every pod the row starts connects as mpMachine's
// nkey user holding exactly the grant grant reads for mpMachine: testnats.ListenerGrant, the grant
// envoy-infra renders for a listener, unless the row's pods need another role's (testnats.HostGrant
// for a pod with ENVOY_SUBSCRIBE_ALL_TOPICS, which only a host's grant allows). It returns the
// server and the URL the row's own clients (a publisher, a reader) use, as the server's admin,
// which mpStartPod takes.
func mpStartNATS(t *testing.T, grant func(testing.TB, string) testnats.Grant) (*testnats.Server, string) {
	t.Helper()
	seed, public := testnats.User(t)
	granted := testnats.StartGranted(t, testnats.GrantedUser{Public: public, Grant: grant(t, mpMachine)})
	mpSeeds.Store(granted.AdminURL, seed)
	t.Cleanup(func() { mpSeeds.Delete(granted.AdminURL) })
	return granted.Server, granted.AdminURL
}

// mpLinesWithout is output without the two lines the listener logs for every delivery, which would
// bury the rest.
func mpLinesWithout(output string) string {
	var kept []string
	for line := range strings.SplitSeq(output, "\n") {
		if !strings.Contains(line, `"msg":"listener received"`) && !strings.Contains(line, `"msg":"listener agent delivered"`) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// mpStartPair starts pod-a, waits for it to be the machine's active pod, then starts pod-b and
// waits for it to be its standby, both with env added to their environment.
func mpStartPair(t *testing.T, binary, natsURL string, env ...string) (a, b *mpPod) {
	t.Helper()
	a = mpStartPod(t, binary, natsURL, "pod-a", env...)
	mpWaitActive(t, a, 30*time.Second)
	b = mpStartPod(t, binary, natsURL, "pod-b", env...)
	mpWaitStandby(t, b, 30*time.Second)
	return a, b
}

// mpAnswer is one HTTP answer, or the error that stood for it, and how long it took.
type mpAnswer struct {
	status int
	header http.Header
	body   string
	err    error
	took   time.Duration
}

// call sends method path with body to the pod as the rows' /v1 caller.
func (p *mpPod) call(method, path, body string) mpAnswer {
	start := time.Now()
	request, err := http.NewRequest(method, "http://127.0.0.1:"+strconv.Itoa(p.port)+path, strings.NewReader(body))
	if err != nil {
		return mpAnswer{err: err}
	}
	request.Header.Set("Authorization", "Bearer "+mpToken)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := mpHTTP.Do(request)
	if err != nil {
		return mpAnswer{err: err, took: time.Since(start)}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	return mpAnswer{status: response.StatusCode, header: response.Header, body: string(data), err: err, took: time.Since(start)}
}

// health reads /healthz: its status and its JSON body, status 0 when the pod did not answer.
func (p *mpPod) health() (int, map[string]any) {
	answer := p.call(http.MethodGet, "/healthz", "")
	if answer.err != nil {
		return 0, nil
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(answer.body), &body)
	return answer.status, body
}

// active reports whether the pod says it delivers from the machine's durable: 200 healthy with
// role active. A build before the lease reports no role; its healthy pod is the one bound to the
// durable, so it counts as active.
func (p *mpPod) active() bool {
	status, body := p.health()
	return status == http.StatusOK && body["status"] == "healthy" && (body["role"] == "active" || body["role"] == nil)
}

// standby reports whether the pod is a ready standby: 200 with role standby and nothing it waits
// for to receive ("receive" names a lease taken and not received on yet). A build before the lease
// has no standby; the nearest it has, a pod waiting to bind the durable another pod holds, answers
// 200 "starting" with /v1 already served, which counts here so a row reaches its fault there too.
func (p *mpPod) standby() bool {
	status, body := p.health()
	if status != http.StatusOK {
		return false
	}
	if body["role"] == "standby" {
		return body["receive"] == nil
	}
	return body["role"] == nil && body["status"] == "starting" && p.call(http.MethodGet, "/v1/sessions", "").status == http.StatusOK
}

func mpWaitActive(t *testing.T, p *mpPod, within time.Duration) {
	t.Helper()
	mpEventually(t, within, p.instance+" to be the machine's active pod", p.active)
}

func mpWaitStandby(t *testing.T, p *mpPod, within time.Duration) {
	t.Helper()
	mpEventually(t, within, p.instance+" to be the machine's standby", p.standby)
}

// mpEventually polls cond every 100 ms and fails the test when it is not true within the bound.
func mpEventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(within); !cond(); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %s", within, what)
		}
	}
}

// kill sends the pod SIGKILL and returns when it was sent.
func (p *mpPod) kill(t *testing.T) time.Time {
	t.Helper()
	killed := time.Now()
	if err := p.Cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL %s: %v", p.instance, err)
	}
	return killed
}

// registerPort registers sessionID through the pod as the OpenCode plugin does, served on the
// serving port at 127.0.0.1 and subscribed to topics besides its own inbox, and requires a 200.
func (p *mpPod) registerPort(t *testing.T, serving *mpServingPort, sessionID string, topics ...string) {
	t.Helper()
	topicsJSON, err := json.Marshal(append([]string{}, topics...))
	if err != nil {
		t.Fatalf("encode %s's topics: %v", sessionID, err)
	}
	p.subscribe(t, fmt.Sprintf(`{"session_id":%q,"topics":%s,"port":%d,"host":"127.0.0.1"}`, sessionID, topicsJSON, serving.port))
}

// subscribe posts body to the pod's /v1/interests/subscribe, a registration or its heartbeat, and
// requires a 200.
func (p *mpPod) subscribe(t *testing.T, body string) {
	t.Helper()
	if answer := p.call(http.MethodPost, "/v1/interests/subscribe", body); answer.status != http.StatusOK {
		t.Fatalf("subscribe %s through %s: status %d %s %v", body, p.instance, answer.status, answer.body, answer.err)
	}
}

// mpEventMark carries a message's event id in the text of the prompt its session receives.
var mpEventMark = regexp.MustCompile(`mp-event=([A-Za-z0-9_-]+)`)

// mpPrompt is one prompt a session took: the event id it carried, and when the serving port
// answered it.
type mpPrompt struct {
	event string
	at    time.Time
}

// mpServingPort is the fake serve process every port session of a row lives on. It answers each
// POST /session/<id>/prompt_async 204 and records it, under the session, once the answer is
// written. A prompt held (hold) is not answered until the listener gives up on it, and is not
// recorded: the session never took it. A prompt to a failing session (fail) is answered 500 and
// counted apart: the session never took it either.
type mpServingPort struct {
	port int

	mu         sync.Mutex
	prompts    map[string][]mpPrompt
	holds      map[string]chan struct{}
	failing    map[string]bool
	failed     map[string]int
	unexpected []string
	stopping   chan struct{}
}

func mpStartServingPort(t *testing.T) *mpServingPort {
	t.Helper()
	serving := &mpServingPort{prompts: map[string][]mpPrompt{}, holds: map[string]chan struct{}{}, failing: map[string]bool{}, failed: map[string]int{}, stopping: make(chan struct{})}
	server := httptest.NewServer(http.HandlerFunc(serving.serve))
	t.Cleanup(server.Close)
	// Runs before server.Close, which waits for every held prompt to return.
	t.Cleanup(func() { close(serving.stopping) })
	t.Cleanup(func() {
		serving.mu.Lock()
		defer serving.mu.Unlock()
		if len(serving.unexpected) > 0 {
			t.Errorf("the serving port was sent what no session expects: %q", serving.unexpected)
		}
	})
	port, err := strconv.Atoi(server.URL[strings.LastIndex(server.URL, ":")+1:])
	if err != nil {
		t.Fatalf("serving port of %s: %v", server.URL, err)
	}
	serving.port = port
	return serving
}

func (s *mpServingPort) serve(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := strings.CutPrefix(r.URL.Path, "/session/")
	sessionID, isPrompt := strings.CutSuffix(sessionID, "/prompt_async")
	var body struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}
	decodeErr := json.NewDecoder(r.Body).Decode(&body)
	var text strings.Builder
	for _, part := range body.Parts {
		text.WriteString(part.Text)
	}
	mark := mpEventMark.FindStringSubmatch(text.String())
	if r.Method != http.MethodPost || !ok || !isPrompt || decodeErr != nil || mark == nil {
		s.mu.Lock()
		s.unexpected = append(s.unexpected, r.Method+" "+r.URL.Path+" "+text.String())
		s.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}
	event := mark[1]
	s.mu.Lock()
	held, hold := s.holds[sessionID]
	delete(s.holds, sessionID)
	failing := s.failing[sessionID]
	if failing && !hold {
		s.failed[sessionID]++
	}
	s.mu.Unlock()
	if hold {
		close(held)
		select {
		case <-r.Context().Done():
		case <-s.stopping:
		}
		return
	}
	if failing {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	s.mu.Lock()
	s.prompts[sessionID] = append(s.prompts[sessionID], mpPrompt{event: event, at: time.Now()})
	s.mu.Unlock()
}

// hold holds the next prompt to sessionID: the serving port never answers it, until the listener
// that sent it gives up or goes away. The returned channel closes when that prompt arrives.
func (s *mpServingPort) hold(sessionID string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	arrived := make(chan struct{})
	s.holds[sessionID] = arrived
	return arrived
}

// fail answers every prompt to sessionID 500 from now on, so the listener counts each a failed
// prompt and NAKs its message for a retry 30 s later.
func (s *mpServingPort) fail(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing[sessionID] = true
}

// failures is how many prompts to sessionID the serving port answered 500.
func (s *mpServingPort) failures(sessionID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed[sessionID]
}

// counts is how many prompts the serving port answered for each event id.
func (s *mpServingPort) counts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	for _, prompts := range s.prompts {
		for _, prompt := range prompts {
			counts[prompt.event]++
		}
	}
	return counts
}

// promptsTo is how many prompts the serving port answered for sessionID.
func (s *mpServingPort) promptsTo(sessionID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.prompts[sessionID])
}

// first is when event's first prompt was answered, and whether it was.
func (s *mpServingPort) first(sessionID, event string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, prompt := range s.prompts[sessionID] {
		if prompt.event == event {
			return prompt.at, true
		}
	}
	return time.Time{}, false
}

// order is the event ids sessionID took, each at its first prompt, in the order it took them.
func (s *mpServingPort) order(sessionID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var order []string
	for _, prompt := range s.prompts[sessionID] {
		if !slices.Contains(order, prompt.event) {
			order = append(order, prompt.event)
		}
	}
	return order
}

func (s *mpServingPort) tookAll(messages []mpMessage) bool {
	counts := s.counts()
	for _, m := range messages {
		if counts[m.event] == 0 {
			return false
		}
	}
	return true
}

// awaitAll waits until every message was prompted to its session at least once.
func (s *mpServingPort) awaitAll(t *testing.T, messages []mpMessage, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !s.tookAll(messages) {
		if time.Now().After(deadline) {
			counts := s.counts()
			var missing []string
			for _, m := range messages {
				if counts[m.event] == 0 {
					missing = append(missing, m.event+" to "+m.session)
				}
			}
			t.Fatalf("%d of %d messages never reached their session within %s: %v", len(missing), len(messages), within, missing)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// requireOnce fails the test unless every message was prompted exactly once.
func (s *mpServingPort) requireOnce(t *testing.T, messages []mpMessage) {
	t.Helper()
	counts := s.counts()
	var wrong []string
	for _, m := range messages {
		if counts[m.event] != 1 {
			wrong = append(wrong, fmt.Sprintf("%s to %s prompted %d times", m.event, m.session, counts[m.event]))
		}
	}
	if len(wrong) > 0 {
		t.Fatalf("%d of %d messages were not prompted exactly once: %v", len(wrong), len(messages), wrong)
	}
}

// requireKnown fails the test when a session was prompted with an event id no message carried: a
// repeat is only ever of a message, with its own event id.
func (s *mpServingPort) requireKnown(t *testing.T, messages []mpMessage) {
	t.Helper()
	known := map[string]bool{}
	for _, m := range messages {
		known[m.event] = true
	}
	for event := range s.counts() {
		if !known[event] {
			t.Fatalf("a session was prompted with event %s, which no message carried", event)
		}
	}
}

// requireOrder fails the test unless each session took its messages, each at its first prompt,
// in the order they were published.
func (s *mpServingPort) requireOrder(t *testing.T, messages []mpMessage) {
	t.Helper()
	want := map[string][]mpMessage{}
	for _, m := range messages {
		want[m.session] = append(want[m.session], m)
	}
	for sessionID, sent := range want {
		events := make([]string, len(sent))
		published := map[string]time.Time{}
		for index, m := range sent {
			events[index] = m.event
			published[m.event] = m.published
		}
		var got []string
		for _, event := range s.order(sessionID) {
			if slices.Contains(events, event) {
				got = append(got, event)
			}
		}
		if slices.Equal(got, events) {
			continue
		}
		describe := func(event string) string {
			at, _ := s.first(sessionID, event)
			return fmt.Sprintf("%s (published %s, prompted %s after the session's first publish)", event,
				published[event].Sub(sent[0].published).Round(time.Millisecond), at.Sub(sent[0].published).Round(time.Millisecond))
		}
		for index := range min(len(got), len(events)) {
			if got[index] != events[index] {
				t.Fatalf("%s took %s before %s, out of the publish order; it took %v, want %v",
					sessionID, describe(got[index]), describe(events[index]), got, events)
			}
		}
		t.Fatalf("%s took its messages in the order %v, want the publish order %v", sessionID, got, events)
	}
}

// requireWithin fails the test unless every message's first prompt came within bound of its
// publish or of since, whichever is later.
func (s *mpServingPort) requireWithin(t *testing.T, messages []mpMessage, bound time.Duration, since time.Time) {
	t.Helper()
	var late []string
	var slowest time.Duration
	for _, m := range messages {
		from := m.published
		if since.After(from) {
			from = since
		}
		at, ok := s.first(m.session, m.event)
		if !ok || at.Sub(from) > bound {
			late = append(late, fmt.Sprintf("%s to %s after %s", m.event, m.session, at.Sub(from).Round(time.Millisecond)))
		}
		slowest = max(slowest, at.Sub(from))
	}
	if len(late) > 0 {
		t.Fatalf("%d of %d messages reached their session later than %s: %v", len(late), len(messages), bound, late)
	}
	t.Logf("all %d messages reached their session within %s, the slowest after %s", len(messages), bound, slowest.Round(time.Millisecond))
}

// mpMessage is one message a row published to a session.
type mpMessage struct {
	event     string
	session   string
	published time.Time
}

// mpPublisher publishes messages to port sessions from a bus client of the test's own, straight to
// NATS, as a session holding its own connection does: to a session's inbox, or on a topic one
// session subscribes to (mpTarget). Its lock is held across each publish, so quiesce can put a
// fault between two publishes.
type mpPublisher struct {
	t        *testing.T
	client   *bus.Client
	consumer string

	mu   sync.Mutex
	sent []mpMessage
}

func mpConnectPublisher(t *testing.T, natsURL string) *mpPublisher {
	t.Helper()
	client, err := bus.Connect([]string{natsURL})
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(client.Close)
	return &mpPublisher{t: t, client: client, consumer: bus.ListenerDurable(mpMachine)}
}

// mpTarget is a topic a row publishes on and the one session that receives what is published there:
// a session's own inbox (mpInbox), or a topic the session subscribed to.
type mpTarget struct {
	topic   string
	session string
}

func mpInbox(sessionID string) mpTarget {
	return mpTarget{topic: contracts.AgentSubject(sessionID), session: sessionID}
}

// publish publishes one message to sessionID's inbox and returns it once the stream stored it.
func (p *mpPublisher) publish(sessionID string) (mpMessage, error) {
	return p.publishOn(mpInbox(sessionID))
}

// publishOn publishes one message on target's topic and returns it once it was published: stored,
// on a subject this test binary's build compiles into the stream, else sent over core NATS, as
// bus.Client sends every topic.
func (p *mpPublisher) publishOn(target mpTarget) (mpMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	event := fmt.Sprintf("evt-mp-%04d", len(p.sent)+1)
	item := contracts.Envelope{
		EventID:        event,
		Source:         "agent",
		SourceEventID:  event,
		Topic:          target.topic,
		DedupeKey:      "multiprocess." + event,
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "multiprocess message mp-event=" + event,
		TraceID:        "trace-" + event,
	}
	if err := p.client.Publish(item); err != nil {
		return mpMessage{}, fmt.Errorf("publish %s on %s for %s: %w", event, target.topic, target.session, err)
	}
	m := mpMessage{event: event, session: target.session, published: time.Now()}
	p.sent = append(p.sent, m)
	return m, nil
}

func (p *mpPublisher) mustPublish(t *testing.T, sessionID string) mpMessage {
	t.Helper()
	m, err := p.publish(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// every publishes one message every interval, to each of sessions' inboxes in turn, until the
// returned func runs (or the test ends), which returns once the last publish has.
func (p *mpPublisher) every(interval time.Duration, sessions ...string) (stop func()) {
	targets := make([]mpTarget, len(sessions))
	for index, sessionID := range sessions {
		targets[index] = mpInbox(sessionID)
	}
	return p.everyOn(interval, targets...)
}

// everyOn is every over targets: one message every interval, on each target's topic in turn.
func (p *mpPublisher) everyOn(interval time.Duration, targets ...mpTarget) (stop func()) {
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for index := 0; ; index++ {
			if _, err := p.publishOn(targets[index%len(targets)]); err != nil {
				p.t.Error(err)
				return
			}
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	}()
	stop = sync.OnceFunc(func() {
		close(done)
		<-stopped
	})
	p.t.Cleanup(stop)
	return stop
}

// since is every message published at or after from.
func (p *mpPublisher) since(from time.Time) []mpMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	var messages []mpMessage
	for _, m := range p.sent {
		if !m.published.Before(from) {
			messages = append(messages, m)
		}
	}
	return messages
}

func (p *mpPublisher) all() []mpMessage {
	return p.since(time.Time{})
}

// quiesce runs fault between two publishes, once every message published so far has reached its
// session and the machine's durable holds none unacknowledged: a fault then cuts no delivery in
// flight, whose outcome the row would otherwise have to allow for.
func (p *mpPublisher) quiesce(t *testing.T, serving *mpServingPort, fault func()) time.Time {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	sent := slices.Clone(p.sent)
	mpEventually(t, 10*time.Second, "every message published so far delivered and acknowledged", func() bool {
		if !serving.tookAll(sent) {
			return false
		}
		info, err := p.client.JS().ConsumerInfo(bus.Stream, p.consumer)
		return err == nil && info.NumAckPending == 0
	})
	at := time.Now()
	fault()
	return at
}

// ackPending is how many messages the machine's durable has handed out and not had acknowledged.
func (p *mpPublisher) ackPending() int {
	info, err := p.client.JS().ConsumerInfo(bus.Stream, p.consumer)
	if err != nil {
		return -1
	}
	return info.NumAckPending
}

// mpSessionsWatch polls a pod's GET /v1/sessions every mpPoll, recording whether each answer was
// 200 and listed every session it wants.
type mpSessionsWatch struct {
	pod  *mpPod
	want []string
	halt func()

	mu    sync.Mutex
	polls []mpSessionsPoll
}

type mpSessionsPoll struct {
	at     time.Time
	ok     bool
	answer string
}

func mpWatchSessions(t *testing.T, pod *mpPod, want ...string) *mpSessionsWatch {
	t.Helper()
	w := &mpSessionsWatch{pod: pod, want: want}
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(mpPoll)
		defer ticker.Stop()
		for {
			w.poll()
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	}()
	w.halt = sync.OnceFunc(func() {
		close(done)
		<-stopped
	})
	t.Cleanup(w.halt)
	return w
}

func (w *mpSessionsWatch) poll() {
	at := time.Now()
	answer := w.pod.call(http.MethodGet, "/v1/sessions", "")
	ok := answer.err == nil && answer.status == http.StatusOK
	if ok {
		var listed []struct {
			SessionID string `json:"session_id"`
		}
		ok = json.Unmarshal([]byte(answer.body), &listed) == nil
		for _, sessionID := range w.want {
			ok = ok && slices.ContainsFunc(listed, func(s struct {
				SessionID string `json:"session_id"`
			}) bool {
				return s.SessionID == sessionID
			})
		}
	}
	summary := fmt.Sprintf("%d %s", answer.status, mpTail(answer.body, 1))
	if answer.err != nil {
		summary = answer.err.Error()
	}
	w.mu.Lock()
	w.polls = append(w.polls, mpSessionsPoll{at: at, ok: ok, answer: summary})
	w.mu.Unlock()
}

// requireServed fails the test unless the pod listed every wanted session within 10 s of the
// take-over and in every poll from then on, each answer within the client's 10 s. It watches on
// until a second past those 10 s, then stops the watch.
func (w *mpSessionsWatch) requireServed(t *testing.T, takeover time.Time) {
	t.Helper()
	served := takeover.Add(10 * time.Second)
	mpEventually(t, time.Until(served)+15*time.Second, "GET /v1/sessions polls past 10 s after the take-over", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.polls) > 0 && w.polls[len(w.polls)-1].at.After(served.Add(time.Second))
	})
	w.halt()
	w.mu.Lock()
	defer w.mu.Unlock()
	firstOK := time.Time{}
	after := 0
	for _, poll := range w.polls {
		if poll.at.Before(takeover) {
			continue
		}
		if poll.ok && firstOK.IsZero() {
			firstOK = poll.at
		}
		if !poll.at.Before(served) {
			after++
			if !poll.ok {
				t.Fatalf("GET /v1/sessions on %s %s after the take-over: %s, want 200 listing %v", w.pod.instance, poll.at.Sub(takeover).Round(time.Millisecond), poll.answer, w.want)
			}
		}
	}
	if firstOK.IsZero() || firstOK.After(served) {
		t.Fatalf("GET /v1/sessions on %s listed %v first at %v after the take-over, want within 10 s", w.pod.instance, w.want, firstOK.Sub(takeover))
	}
	if after == 0 {
		t.Fatalf("GET /v1/sessions on %s was not polled past 10 s after the take-over", w.pod.instance)
	}
	t.Logf("GET /v1/sessions on %s: listed %v %s after the take-over, and in all %d polls from 10 s on", w.pod.instance, w.want, firstOK.Sub(takeover).Round(time.Millisecond), after)
}

// mpTail is the last n lines of text.
func mpTail(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// A reconnect in place: the active pod's link to NATS is cut for 7 s, past the lease's 5 s, then
// it reconnects to the same server, with a message published every 100 ms throughout. The standby
// takes over while the active pod is away, and once it is back only one of them delivers: every
// message reaches its session exactly once. Before INFRA-557 the standby binds the durable once
// the server drops the cut pod's interest, and the cut pod's nats.go sends its subscription again
// on the reconnect, so both deliver and messages arrive twice.
func TestTwoPodsReconnectInPlaceDeliversEachMessageOnce(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartServingPort(t)
	sessions := []string{"ses_mp_reconnect_one", "ses_mp_reconnect_two"}
	for _, sessionID := range sessions {
		a.registerPort(t, serving, sessionID)
	}
	watch := mpWatchSessions(t, b, sessions...)
	publisher := mpConnectPublisher(t, uri)

	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, 10*time.Second, "the first messages to arrive", func() bool { return len(publisher.all()) >= 20 })
	const outage = 7 * time.Second
	reconnects := mainClientReconnects(a.Output.String())
	cut := publisher.quiesce(t, serving, func() { a.relay.cut(outage) })
	mpEventually(t, outage+30*time.Second, a.instance+" to reconnect after the cut", func() bool {
		return mainClientReconnects(a.Output.String()) > reconnects
	})
	time.Sleep(5 * time.Second) // keep publishing past the reconnect
	stop()

	messages := publisher.all()
	serving.awaitAll(t, messages, 30*time.Second)
	time.Sleep(mpQuiet)
	serving.requireOnce(t, messages)
	actives := 0
	for _, pod := range []*mpPod{a, b} {
		if pod.active() {
			actives++
		}
	}
	if actives != 1 {
		t.Fatalf("%d pods report themselves active after the reconnect, want 1", actives)
	}
	watch.requireServed(t, cut)
	t.Logf("%d messages, each prompted once across a %s cut of %s", len(messages), outage, a.instance)
}

// A short drop: the active pod's link is cut for 2 s, under the lease's 5 s. It reconnects and
// renews in time, so it stays the machine's active pod and the standby never delivers: every
// message reaches its session exactly once and within 10 s of its publish (a ledger call that
// failed while the pod's state connection reconnected once cost a message 30 s), and the standby
// never reports itself anything but a standby. Before INFRA-557 the standby may bind in the gap,
// and the reconnected pod's nats.go then subscribes beside it.
func TestTwoPodsShortDropKeepsTheActivePod(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartServingPort(t)
	sessions := []string{"ses_mp_drop_one", "ses_mp_drop_two"}
	for _, sessionID := range sessions {
		a.registerPort(t, serving, sessionID)
	}
	publisher := mpConnectPublisher(t, uri)

	var notStandby []string
	done, watched := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		for {
			if !b.standby() {
				_, body := b.health()
				notStandby = append(notStandby, fmt.Sprint(body))
			}
			select {
			case <-done:
				return
			case <-time.After(mpPoll):
			}
		}
	}()
	halt := sync.OnceFunc(func() { close(done); <-watched })
	t.Cleanup(halt)

	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, 10*time.Second, "the first messages to arrive", func() bool { return len(publisher.all()) >= 20 })
	const outage = 2 * time.Second
	reconnects := mainClientReconnects(a.Output.String())
	cut := publisher.quiesce(t, serving, func() { a.relay.cut(outage) })
	mpEventually(t, outage+30*time.Second, a.instance+" to reconnect after the cut", func() bool {
		return mainClientReconnects(a.Output.String()) > reconnects
	})
	reconnected := time.Since(cut)
	time.Sleep(5 * time.Second) // keep publishing past the reconnect
	stop()

	messages := publisher.all()
	stopped := time.Now()
	serving.awaitAll(t, messages, 30*time.Second)
	t.Logf("%s reconnected %s after the cut; the last of %d messages arrived %s after the last publish",
		a.instance, reconnected.Round(time.Millisecond), len(messages), time.Since(stopped).Round(time.Millisecond))
	serving.requireWithin(t, messages, mpGracefulBound, time.Time{})
	time.Sleep(mpQuiet)
	halt()
	serving.requireOnce(t, messages)
	if len(notStandby) > 0 {
		t.Fatalf("%s was not a standby in %d polls across %s's short drop: %v", b.instance, len(notStandby), a.instance, notStandby)
	}
	if strings.Contains(b.Output.String(), "durable receiving") {
		t.Fatalf("%s received the durable across %s's short drop", b.instance, a.instance)
	}
	if !a.active() {
		t.Fatalf("%s is not the active pod after its short drop", a.instance)
	}
}

// Two pods of the machine started at the same moment, 20 times, each time on a NATS account of its
// own: once both are ready exactly one reports itself active and the other a standby, and each
// message reaches its session exactly once. Before INFRA-557 nats.go's push-bound check is
// check-then-act, and two pods started together both bind the durable, so both report themselves
// healthy, the active pod's answer then (and both deliver). The row stops at the first start that
// fails.
func TestTwoPodsStartedTogetherElectOneActivePod(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	for start := range 20 {
		passed := t.Run(fmt.Sprintf("start %d", start+1), func(t *testing.T) {
			uri := mpNATS(t)
			pods := []*mpPod{mpStartPod(t, binary, uri, "pod-a"), mpStartPod(t, binary, uri, "pod-b")}
			mpEventually(t, 30*time.Second, "both pods to be ready", func() bool {
				return (pods[0].active() || pods[0].standby()) && (pods[1].active() || pods[1].standby())
			})
			// Read again, a few times, so a second pod that took the lease a moment later shows.
			for range 4 {
				var actives []string
				for _, pod := range pods {
					if pod.active() {
						actives = append(actives, pod.instance)
					}
				}
				if len(actives) != 1 {
					t.Fatalf("%d pods report themselves active (%v), want exactly 1", len(actives), actives)
				}
				time.Sleep(mpPoll)
			}
			serving := mpStartServingPort(t)
			const sessionID = "ses_mp_together"
			pods[1].registerPort(t, serving, sessionID)
			publisher := mpConnectPublisher(t, uri)
			for range 5 {
				publisher.mustPublish(t, sessionID)
			}
			messages := publisher.all()
			serving.awaitAll(t, messages, 10*time.Second)
			time.Sleep(time.Second)
			serving.requireOnce(t, messages)
		})
		if !passed {
			break
		}
	}
}

// SIGKILL of the active pod while it has a message in hand, on a port session T whose prompt never
// answers, and a message for session S queued behind it. The standby takes over: S's message and
// every message published after the kill reach their sessions within 15 s, each session's in the
// order they were published, and the standby lists the registered sessions throughout. Before
// INFRA-557 the durable's 60 s ack wait holds S's message with the killed pod (and any message the
// server pushed to it as it died), so those come after the newer ones, and late.
func TestTwoPodsSIGKILLHandsOverInOrderWithin15s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartServingPort(t)
	const blocked, queued, other = "ses_mp_kill_t", "ses_mp_kill_s", "ses_mp_kill_u"
	for _, sessionID := range []string{blocked, queued, other} {
		a.registerPort(t, serving, sessionID)
	}
	watch := mpWatchSessions(t, b, blocked, queued, other)
	publisher := mpConnectPublisher(t, uri)

	inHand := serving.hold(blocked)
	publisher.mustPublish(t, blocked)
	select {
	case <-inHand:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never prompted %s", a.instance, blocked)
	}
	behind := publisher.mustPublish(t, queued)
	mpEventually(t, 10*time.Second, a.instance+" to hold the queued message too", func() bool { return publisher.ackPending() >= 2 })

	killed := a.kill(t)
	stop := publisher.every(100*time.Millisecond, queued, other)
	time.Sleep(3 * time.Second) // publish for a while after the kill
	stop()

	messages := append([]mpMessage{behind}, publisher.since(killed)...)
	serving.awaitAll(t, messages, mpHardBound+5*time.Second)
	serving.requireWithin(t, messages, mpHardBound, killed)
	serving.requireOrder(t, messages)
	time.Sleep(time.Second)
	serving.requireOnce(t, messages)
	mpWaitActive(t, b, time.Second)
	watch.requireServed(t, killed)
}

// Node loss: the active pod's link freezes, its sockets left open, so the server keeps its
// subscription as it does for a node that vanished. The messages published during the freeze
// reach their sessions through the standby within 15 s, each session's in order; once the link
// thaws, the old pod delivers none of them again. Before INFRA-557 the server keeps the frozen
// pod's binding for minutes and the standby cannot bind.
func TestTwoPodsNodeLossHandsOverInOrderWithin15s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartServingPort(t)
	sessions := []string{"ses_mp_loss_one", "ses_mp_loss_two"}
	for _, sessionID := range sessions {
		a.registerPort(t, serving, sessionID)
	}
	watch := mpWatchSessions(t, b, sessions...)
	publisher := mpConnectPublisher(t, uri)

	var thaw func()
	frozen := publisher.quiesce(t, serving, func() { thaw = a.relay.freeze() })
	t.Cleanup(thaw)
	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, mpHardBound, b.instance+" to take over", b.active)
	time.Sleep(2 * time.Second) // publish on past the take-over
	stop()

	messages := publisher.since(frozen)
	serving.awaitAll(t, messages, mpHardBound+5*time.Second)
	serving.requireWithin(t, messages, mpHardBound, frozen)
	serving.requireOrder(t, messages)
	serving.requireOnce(t, messages)

	thaw()
	time.Sleep(mpQuiet)
	serving.requireOnce(t, messages)
	watch.requireServed(t, frozen)
}

// A graceful stop: the active pod is in hand on a prompt that never answers when it is sent
// SIGTERM, with a message published every 100 ms before and after. Every message reaches its
// session within 10 s of its publish; a message prompted twice is the same message, with its own
// event id. Before INFRA-557 the stopping pod keeps the durable bound for ~23 s, through its HTTP
// shutdown and its NATS drain, so the messages wait.
func TestTwoPodsGracefulStopDeliversWithin10s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartServingPort(t)
	const slow = "ses_mp_graceful_slow"
	sessions := []string{"ses_mp_graceful_one", "ses_mp_graceful_two"}
	for _, sessionID := range append([]string{slow}, sessions...) {
		a.registerPort(t, serving, sessionID)
	}
	watch := mpWatchSessions(t, b, append([]string{slow}, sessions...)...)
	publisher := mpConnectPublisher(t, uri)

	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, 10*time.Second, "the first messages to arrive", func() bool { return len(publisher.all()) >= 10 })
	inHand := serving.hold(slow)
	publisher.mustPublish(t, slow)
	select {
	case <-inHand:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never prompted %s", a.instance, slow)
	}
	signalled := a.Terminate(t)
	time.Sleep(6 * time.Second) // publish on through the hand-over
	stop()

	messages := publisher.all()
	serving.awaitAll(t, messages, mpGracefulBound+5*time.Second)
	serving.requireWithin(t, messages, mpGracefulBound, time.Time{})
	serving.requireKnown(t, messages)
	a.WaitExit(t, "SIGTERM")
	mpWaitActive(t, b, time.Second)
	watch.requireServed(t, signalled)
}

// A standby waits as long as the active pod runs: 140 s on, past the 135 s after which a standby
// once gave up and exited, it is still up, /healthz 200 role standby, and it lists the registered
// sessions in every poll; then a SIGTERM of the active pod hands delivery to it. Before INFRA-557
// the standby exits at 135 s.
func TestTwoPodsStandbyWaitsPast140s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartServingPort(t)
	const sessionID = "ses_mp_standby_waits"
	a.registerPort(t, serving, sessionID)
	watch := mpWatchSessions(t, b, sessionID)
	started := time.Now()

	// The standby is checked by mpPod.standby here, which a build before the lease answers too, so
	// that build fails on its exit rather than on its /healthz; the role is checked after the wait.
	for time.Since(started) < 140*time.Second {
		select {
		case <-b.Exited:
			t.Fatalf("%s exited %s into its standby", b.instance, time.Since(started).Round(time.Second))
		case <-time.After(mpPoll):
		}
		if !b.standby() {
			status, body := b.health()
			t.Fatalf("%s %s into its standby is not a standby: /healthz %d %v", b.instance, time.Since(started).Round(time.Second), status, body)
		}
	}
	if status, body := b.health(); status != http.StatusOK || body["role"] != "standby" {
		t.Fatalf("%s's /healthz after %s as a standby: %d %v, want 200 role standby", b.instance, time.Since(started).Round(time.Second), status, body)
	}

	signalled := a.Terminate(t)
	mpWaitActive(t, b, mpGracefulBound)
	publisher := mpConnectPublisher(t, uri)
	m := publisher.mustPublish(t, sessionID)
	serving.awaitAll(t, []mpMessage{m}, mpGracefulBound)
	watch.requireServed(t, started)
	t.Logf("%s took over %s after %s's SIGTERM", b.instance, time.Since(signalled).Round(time.Millisecond), a.instance)
}

// Readiness: a listener started while its NATS is down answers /healthz 503 "starting" and stays
// up, past the ten one-second connect tries after which it once exited; once NATS is up it is
// 200 active within 10 s, and a second pod started then is 200 standby. Before INFRA-557 the
// listener answers 200 "starting" with nothing ready, then exits.
func TestTwoPodsReadinessWaitsForNATS(t *testing.T) {
	server, uri := mpStartNATS(t, testnats.ListenerGrant)
	server.Stop(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a := mpStartPod(t, binary, uri, "pod-a")
	mpEventually(t, 10*time.Second, a.instance+" to answer /healthz", func() bool { status, _ := a.health(); return status != 0 })
	for started := time.Now(); time.Since(started) < 15*time.Second; time.Sleep(mpPoll) {
		select {
		case <-a.Exited:
			t.Fatalf("%s exited while NATS was down", a.instance)
		default:
		}
		if status, body := a.health(); status != http.StatusServiceUnavailable || body["status"] != "starting" {
			t.Fatalf("%s's /healthz while NATS is down: %d %v, want 503 starting", a.instance, status, body)
		}
	}

	server.Start(t)
	upAt := time.Now()
	mpEventually(t, 10*time.Second, a.instance+" to be 200 active once NATS is up", func() bool {
		status, body := a.health()
		return status == http.StatusOK && body["role"] == "active"
	})
	t.Logf("%s was active %s after NATS came up", a.instance, time.Since(upAt).Round(time.Millisecond))
	b := mpStartPod(t, binary, uri, "pod-b")
	mpEventually(t, 30*time.Second, b.instance+" to be 200 standby", func() bool {
		status, body := b.health()
		return status == http.StatusOK && body["role"] == "standby"
	})
}

// The architect's contract for a message sent while the active pod is deleted, at the listener's
// API: the standby sits behind a 500 ms slow link, so its caches trail the bucket. While the active
// pod is stopped, by SIGTERM and by SIGKILL, sessions keep registering through it every 250 ms for
// as long as it answers, and each registered session is sent to through the standby every 250 ms,
// the first send at once, until the standby has taken over. Every send answers within 10 s, 2xx
// or a retryable 503 with Retry-After, and never 404 "no live session". The end-to-end resend is
// INFRA-562's. Before INFRA-557 the standby's cache has not seen the registration yet and answers
// 404 "no live session".
func TestTwoPodsSendThroughTheStandbyDuringATakeover(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	for _, stopping := range []struct {
		name string
		stop func(t *testing.T, p *mpPod) time.Time
	}{
		{"SIGTERM", func(t *testing.T, p *mpPod) time.Time { return p.Terminate(t) }},
		{"SIGKILL", func(t *testing.T, p *mpPod) time.Time { return p.kill(t) }},
	} {
		t.Run(stopping.name, func(t *testing.T) {
			uri := mpNATS(t)
			a, b := mpStartPair(t, binary, uri)
			b.relay.delay(500 * time.Millisecond)

			type send struct {
				session string
				answer  mpAnswer
			}
			var (
				mu         sync.Mutex
				sends      []send
				registered []string
				senders    sync.WaitGroup
			)
			stopSending := make(chan struct{})
			sendTo := func(sessionID string) {
				defer senders.Done()
				body := fmt.Sprintf(`{"target_session":%q,"message":"send during the take-over","source":"agent","source_session":"ses_mp_sender"}`, sessionID)
				for {
					answer := b.call(http.MethodPost, "/v1/messages/send", body)
					mu.Lock()
					sends = append(sends, send{session: sessionID, answer: answer})
					mu.Unlock()
					select {
					case <-stopSending:
						return
					case <-time.After(mpPoll):
					}
				}
			}
			var refusal string
			register := func(index int) bool {
				sessionID := fmt.Sprintf("ses_mp_takeover_%03d", index)
				answer := a.call(http.MethodPost, "/v1/interests/subscribe", fmt.Sprintf(`{"session_id":%q,"topics":[],"self_subscribed":true}`, sessionID))
				if answer.err != nil || answer.status != http.StatusOK {
					refusal = fmt.Sprintf("%d %s %v", answer.status, strings.TrimSpace(answer.body), answer.err)
					return false
				}
				mu.Lock()
				registered = append(registered, sessionID)
				mu.Unlock()
				senders.Add(1)
				go sendTo(sessionID)
				return true
			}

			index := 0
			for ; index < 6; index++ {
				if !register(index) {
					t.Fatalf("%s refused a registration before it was stopped: %s", a.instance, refusal)
				}
				time.Sleep(mpPoll)
			}
			mu.Lock()
			watch := mpWatchSessions(t, b, slices.Clone(registered)...)
			mu.Unlock()
			stoppedAt := stopping.stop(t, a)
			after := 0
			for ; register(index); index++ {
				after++
				time.Sleep(mpPoll)
			}
			mpEventually(t, mpHardBound, b.instance+" to take over", b.active)
			time.Sleep(time.Second) // send on past the take-over
			close(stopSending)
			senders.Wait()
			watch.requireServed(t, stoppedAt)

			mu.Lock()
			defer mu.Unlock()
			retryable := 0
			for _, s := range sends {
				answer := s.answer
				switch {
				case answer.err != nil:
					t.Fatalf("send to %s through %s: %v after %s, want an answer within 10 s", s.session, b.instance, answer.err, answer.took)
				case answer.status == http.StatusNotFound:
					t.Fatalf("send to %s through %s: 404 %s", s.session, b.instance, answer.body)
				case answer.status == http.StatusServiceUnavailable && answer.header.Get("Retry-After") != "":
					retryable++
				case answer.status < 200 || answer.status > 299:
					t.Fatalf("send to %s through %s: %d %s, want 2xx or a 503 with Retry-After", s.session, b.instance, answer.status, answer.body)
				}
			}
			t.Logf("%d sessions registered through %s, %d of them after its %s, until it answered %s; %d sends through %s, %d answered a retryable 503, %s after the stop",
				len(registered), a.instance, after, stopping.name, refusal, len(sends), b.instance, retryable, time.Since(stoppedAt).Round(time.Millisecond))
		})
	}
}

// A standby across a NATS restart registers no JetStream subscription of the durable, so its
// client's recovery has nothing to resubscribe and logs no "envoy nats recovery resubscribe
// failed"; both pods come back, one active and one a standby. Before INFRA-557 the standby's
// pending bind is a subscription the recovery retries and logs that failure for.
func TestTwoPodsStandbyAcrossANATSRestartLogsNoResubscribeFailure(t *testing.T) {
	server, uri := mpStartNATS(t, testnats.ListenerGrant)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)

	disconnects := strings.Count(b.Output.String(), "envoy nats disconnected")
	server.Stop(t)
	mpEventually(t, 10*time.Second, b.instance+" to see NATS go", func() bool {
		return strings.Count(b.Output.String(), "envoy nats disconnected") > disconnects
	})
	server.Start(t)
	mpEventually(t, 30*time.Second, "one pod active and the other a standby after the restart", func() bool {
		return (a.active() && b.standby()) || (b.active() && a.standby())
	})
	// A recovery's first attempt runs at the reconnect and the next a second later: watch past both.
	const failure = "envoy nats recovery resubscribe failed"
	for until := time.Now().Add(5 * time.Second); time.Now().Before(until); time.Sleep(mpPoll) {
		if line := mpLineWith(b.Output.String(), failure); line != "" {
			t.Fatalf("%s, the standby, logged across the NATS restart: %s", b.instance, line)
		}
	}
}

// Two pods of one machine can be told apart in their logs: every JSON record of each carries its
// own instance, and internal/bus's text lines, which keep Go's positional format, name each pod's
// NATS client. Before INFRA-557 the records carry only machine_id.
func TestTwoPodsLogTheirOwnInstance(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	// A dropped link has each pod's client log its disconnect and reconnect in text.
	a.relay.drop()
	b.relay.drop()
	for _, pod := range []*mpPod{a, b} {
		mpEventually(t, 10*time.Second, pod.instance+" to log its reconnect", func() bool {
			return mainClientReconnects(pod.Output.String()) > 0
		})
	}
	for _, pod := range []*mpPod{a, b} {
		records := 0
		for line := range strings.SplitSeq(pod.Output.String(), "\n") {
			var record map[string]any
			if json.Unmarshal([]byte(line), &record) != nil || record["msg"] == nil {
				continue
			}
			records++
			if record["instance"] != pod.instance {
				t.Fatalf("%s's record has instance %v, want %q: %s", pod.instance, record["instance"], pod.instance, line)
			}
		}
		if records == 0 {
			t.Fatalf("%s logged no JSON record", pod.instance)
		}
		textLine := regexp.MustCompile(`(?m)^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} INFO envoy nats disconnected name="envoy-listener ` + regexp.QuoteMeta(pod.instance) + `"`)
		if !textLine.MatchString(pod.Output.String()) {
			t.Fatalf("%s's bus disconnect line is not date, time, message naming the client envoy-listener %s", pod.instance, pod.instance)
		}
	}
}

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

// mpTakeoverHolder waits until one of pods logs that it took the machine's expired lease, and
// returns that pod and the other one.
func mpTakeoverHolder(t *testing.T, within time.Duration, first, second *mpPod) (holder, other *mpPod) {
	t.Helper()
	mpEventually(t, within, "a standby to take the expired lease", func() bool {
		for _, pod := range []*mpPod{first, second} {
			if strings.Contains(pod.Output.String(), `"from":"expired"`) {
				holder = pod
			}
		}
		return holder != nil
	})
	if holder == first {
		return first, second
	}
	return second, first
}

// A lost node, then a graceful stop of the pod that took its expired lease, with a second standby
// running (a rolling update, or a rescheduled pod, after a node loss). The server keeps the lost
// pod's connection, and with it the durable's push binding, until its ping timeout, minutes in
// production; the second standby takes the released lease and must deliver all the same: every
// message published since the node was lost reaches its session, and every message published after
// the SIGTERM within 10 s of it. At 24a88452 (the INFRA-557 tester's finding) the second standby
// logs "durable is push-bound by a subscriber without the lease; waiting for its binding to end"
// and delivers nothing until the lost pod's connection goes (3 min 42 s against a real
// nats-server 2.10.29), while the server redelivers each message to that connection every ack
// wait until MaxDeliver is spent, so a message published meanwhile is never delivered at all.
func TestTwoPodsGracefulStopAfterANodeLossHandsOverWithin10s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	c := mpStartPod(t, binary, uri, "pod-c")
	mpWaitStandby(t, c, 30*time.Second)
	serving := mpStartServingPort(t)
	sessions := []string{"ses_mp_lossstop_one", "ses_mp_lossstop_two"}
	for _, sessionID := range sessions {
		a.registerPort(t, serving, sessionID)
	}
	publisher := mpConnectPublisher(t, uri)

	var thaw func()
	frozen := publisher.quiesce(t, serving, func() { thaw = a.relay.freeze() })
	t.Cleanup(thaw)
	stop := publisher.every(100*time.Millisecond, sessions...)
	holder, other := mpTakeoverHolder(t, mpHardBound, b, c)
	serving.awaitAll(t, publisher.since(frozen), mpHardBound+5*time.Second)

	signalled := holder.Terminate(t)
	holder.WaitExit(t, "SIGTERM")
	time.Sleep(4 * time.Second) // publish on past the hand-over
	stop()

	serving.awaitAll(t, publisher.since(frozen), mpGracefulBound+5*time.Second)
	serving.requireWithin(t, publisher.since(signalled), mpGracefulBound, signalled)
	mpWaitActive(t, other, time.Second)
}

// A lost node, then a graceful stop of the pod that took its expired lease with no standby
// running at all, then a pod started after it. The lost pod's connection still holds the durable's
// push binding, and the stopping pod's linger ends "not taken" a second after its release
// (lingerUntaken), so from then on that binding is the deliver subject's only subscriber: what the
// stopping pod handed back, and every message published after it drained, reaches a connection
// that acknowledges nothing and comes back only at its ack wait. The pod started then takes the
// released record, which no linger bridged to it (leaseHandBackBridge), so its term settles for an
// ack wait, 7 s, not for a hand-back's 2 s delay: every message reaches its session in the order it
// was published. At d3787812 (INFRA-557 review finding) the released record counted as a graceful
// hand-back however long it had stood, so the new pod held for 3 s and delivered the messages
// published after the stop before the older ones still due from the lost node's binding.
func TestTwoPodsGracefulStopWithNoStandbyAfterANodeLossKeepsOrder(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartServingPort(t)
	sessions := []string{"ses_mp_alone_one", "ses_mp_alone_two"}
	for _, sessionID := range sessions {
		a.registerPort(t, serving, sessionID)
	}
	publisher := mpConnectPublisher(t, uri)

	var thaw func()
	frozen := publisher.quiesce(t, serving, func() { thaw = a.relay.freeze() })
	t.Cleanup(thaw)
	stop := publisher.every(100*time.Millisecond, sessions...)
	mpWaitActive(t, b, mpHardBound)
	serving.awaitAll(t, publisher.since(frozen), mpHardBound+5*time.Second)

	// No standby is left: the lease record is released to nobody, and the lost node's binding has
	// the deliver subject to itself until a pod starts.
	b.Terminate(t)
	b.WaitExit(t, "SIGTERM")
	time.Sleep(3 * time.Second) // publish on with no pod receiving
	c := mpStartPod(t, binary, uri, "pod-c")
	mpWaitActive(t, c, 30*time.Second)
	time.Sleep(2 * time.Second) // publish on past the take-over
	stop()

	messages := publisher.all()
	serving.awaitAll(t, messages, mpHardBound+20*time.Second)
	serving.requireKnown(t, messages)
	serving.requireOrder(t, messages)
	if line := mpLineWith(c.Output.String(), "listener lease term must settle", `"cap":"7s"`); line == "" {
		t.Fatalf("%s did not hold for an ack wait after a stop no linger of its own bridged to it:\n%s",
			c.instance, mpTail(mpLinesWithout(c.Output.String()), 40))
	}
}

// A lost node, then, while the pod that took its expired lease still holds what it received for
// one ack wait, a graceful stop of that pod, with a second standby running. The lost pod is gone
// for good by then (killed, its connection closed), so no push binding outlives it. The messages
// the server pushed to the lost pod come back over one ack wait, after newer ones, some to the
// stopping holder and the rest to the second standby, which takes the released lease: each
// session must still take its messages in the order they were published, as after a hard
// take-over alone. At 24a88452 (the INFRA-557 tester's finding) the stopping holder NAKs what it
// holds in the order it received it, and the second standby delivers at once, without the hold,
// so messages published before the take-over reach their session after newer ones.
func TestTwoPodsGracefulStopDuringAHardTakeoversHoldKeepsOrder(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	c := mpStartPod(t, binary, uri, "pod-c")
	mpWaitStandby(t, c, 30*time.Second)
	serving := mpStartServingPort(t)
	sessions := []string{"ses_mp_holdstop_one", "ses_mp_holdstop_two"}
	for _, sessionID := range sessions {
		a.registerPort(t, serving, sessionID)
	}
	publisher := mpConnectPublisher(t, uri)

	var thaw func()
	publisher.quiesce(t, serving, func() { thaw = a.relay.freeze() })
	t.Cleanup(thaw)
	stop := publisher.every(100*time.Millisecond, sessions...)
	holder, other := mpTakeoverHolder(t, mpHardBound, b, c)
	a.kill(t)
	a.relay.close()             // the lost pod's connection goes, so the second standby finds no binding
	time.Sleep(2 * time.Second) // inside the new holder's hold of one ack wait and a second
	holder.Terminate(t)
	holder.WaitExit(t, "SIGTERM")
	time.Sleep(4 * time.Second) // publish on past the hand-over
	stop()

	messages := publisher.all()
	serving.awaitAll(t, messages, mpHardBound+5*time.Second)
	serving.requireKnown(t, messages)
	serving.requireOrder(t, messages)
	mpWaitActive(t, other, time.Second)
}

// A graceful stop of the active pod while messages are on the wire to it: its link delivers
// 300 ms late, so when it unsubscribes the server has already pushed it the messages of the last
// 300 ms. Per-session order must still hold across the hand-over (README "Take-over": the stopping
// pod NAKs what it has not handled, and the durable redelivers those before newer messages). At
// 2e592cc8 (the INFRA-557 tester's round-2 finding) the stopping pod drops what reaches it after
// its unsubscribe without a NAK, the standby delivers newer messages at once, and the dropped ones
// come back one ack wait (6 s) later, behind them. Without the delay the same happens under load:
// at a message every 2 ms one message in about three stops arrived 6 s late and out of order.
func TestTwoPodsGracefulStopWithMessagesOnTheWireKeepsOrder(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartServingPort(t)
	sessions := []string{"ses_mp_wirestop_one", "ses_mp_wirestop_two"}
	for _, sessionID := range sessions {
		a.registerPort(t, serving, sessionID)
	}
	a.relay.delay(300 * time.Millisecond)
	publisher := mpConnectPublisher(t, uri)

	stop := publisher.every(50*time.Millisecond, sessions...)
	time.Sleep(3 * time.Second)
	a.Terminate(t)
	a.WaitExit(t, "SIGTERM")
	time.Sleep(4 * time.Second) // publish on past the hand-over
	stop()

	messages := publisher.all()
	serving.awaitAll(t, messages, mpGracefulBound+5*time.Second)
	serving.requireKnown(t, messages)
	serving.requireWithin(t, messages, mpGracefulBound, time.Time{})
	serving.requireOrder(t, messages)
	mpWaitActive(t, b, time.Second)
}

// SIGKILL of the active pod while a prompt to session T is in flight, then more messages to T. The
// standby takes over: T must take every message, the one in flight at the kill included, in the
// order they were published and within the hard bound (architect Q2: in order within 15 s; a
// repeat with the same id is allowed). TestTwoPodsSIGKILLHandsOverInOrderWithin15s leaves the
// message in flight out of what it checks. At 39a73080 (the INFRA-557 tester's round-4 finding)
// the killed pod's ledger attempt for that message, under 35 s old, makes the new holder defer it
// as another pod's live attempt, so it reaches T 35 s after the kill, after every newer message.
// The killed pod is the lease holder whose expired record the new holder took, so its attempt is
// no live pod's.
func TestTwoPodsSIGKILLMidPromptKeepsTheSessionsOrderWithin15s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartServingPort(t)
	const blocked = "ses_mp_midprompt"
	a.registerPort(t, serving, blocked)
	publisher := mpConnectPublisher(t, uri)

	inHand := serving.hold(blocked)
	first := publisher.mustPublish(t, blocked)
	select {
	case <-inHand:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never prompted %s", a.instance, blocked)
	}
	killed := a.kill(t)
	stop := publisher.every(200*time.Millisecond, blocked)
	time.Sleep(3 * time.Second) // publish for a while after the kill
	stop()

	messages := append([]mpMessage{first}, publisher.since(killed)...)
	serving.awaitAll(t, messages, mpHardBound+30*time.Second)
	serving.requireOrder(t, messages)
	serving.requireWithin(t, messages, mpHardBound, killed)
	mpWaitActive(t, b, time.Second)
}

// mpRetryOutstanding registers on pod a a session whose prompts fail and two healthy sessions,
// named after row, and has a prompt the failing session and NAK its message for the delivery
// handler's 30 s retry, which keeps that message in the durable's pending until its delay ends. It
// returns the serving port, the publisher and the healthy sessions.
func mpRetryOutstanding(t *testing.T, uri string, a *mpPod, row string) (serving *mpServingPort, publisher *mpPublisher, healthy []string) {
	t.Helper()
	serving = mpStartServingPort(t)
	failing := "ses_mp_" + row + "_failing"
	healthy = []string{"ses_mp_" + row + "_one", "ses_mp_" + row + "_two"}
	for _, sessionID := range append([]string{failing}, healthy...) {
		a.registerPort(t, serving, sessionID)
	}
	publisher = mpConnectPublisher(t, uri)
	serving.fail(failing)
	retry := publisher.mustPublish(t, failing)
	a.awaitEventLine(t, 10*time.Second, "listener agent delivery failed", retry.event)
	time.Sleep(500 * time.Millisecond) // past the NAK that follows the failure's record
	if pending := publisher.ackPending(); pending != 1 {
		t.Fatalf("the durable has %d messages outstanding after the failed prompt, want its retry alone", pending)
	}
	return serving, publisher, healthy
}

// mpTo is the messages published to the sessions given.
func mpTo(messages []mpMessage, sessions ...string) []mpMessage {
	var to []mpMessage
	for _, m := range messages {
		if slices.Contains(sessions, m.session) {
			to = append(to, m)
		}
	}
	return to
}

// A graceful stop of the active pod while a message to a session whose prompts fail waits out its
// 30 s retry delay, with a message published every 100 ms to two healthy sessions. The retry stays
// in the durable's pending until its delay ends, so the standby's settle hold never finds every
// outstanding message in hand: it ends at what the stopping pod can still have outstanding, the
// 2 s hand-back delay and a second after the subscribe, and every message to a healthy session
// published after the SIGTERM reaches it within 10 s, each session's in order. At 18779a03 (INFRA-557
// review finding) the hold runs to its 13 s cap, and those messages arrive about 14 s after the
// SIGTERM.
func TestTwoPodsGracefulStopWithARetryOutstandingDeliversWithin10s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving, publisher, healthy := mpRetryOutstanding(t, uri, a, "retrystop")

	stop := publisher.every(100*time.Millisecond, healthy...)
	mpEventually(t, 10*time.Second, "the first healthy messages to arrive", func() bool {
		sent := mpTo(publisher.all(), healthy...)
		return len(sent) >= 10 && serving.tookAll(sent[:10])
	})
	signalled := a.Terminate(t)
	time.Sleep(6 * time.Second) // publish on through the hand-over
	stop()

	messages := mpTo(publisher.all(), healthy...)
	serving.awaitAll(t, messages, mpGracefulBound+10*time.Second)
	serving.requireWithin(t, mpTo(publisher.since(signalled), healthy...), mpGracefulBound, signalled)
	serving.requireOrder(t, messages)
	serving.requireKnown(t, publisher.all())
	a.WaitExit(t, "SIGTERM")
	mpWaitActive(t, b, time.Second)
}

// Graceful stops one after another, A to B to C to D, each with a prompt in hand that the serving
// port never answers and a message to a session whose prompts fail waiting out its 30 s retry
// delay, with a message published every 100 ms to two healthy sessions. Each stop comes a second
// into the prompt, with the healthy sessions' messages queued behind it; the stopping pod cancels
// the prompt after the 3 s grace and NAKs it for a retry, so with the failing session's retry it
// holds every next pod's settle hold to its cap: every hand-over must be a graceful hand-back's,
// whose cap is 3 s, and every message to a healthy session reaches it within 10 s of its publish,
// each session's in order. At 1fe15e1b (INFRA-557 review finding) the pod that took a hand-back
// marked the record stale_binding for subscribing beside the stopping pod's linger, so from the
// second hand-over on the hold ran to the 7 s cap of a hard take-over, and the messages queued at
// the SIGTERM arrived about 11 s after their publish.
func TestTwoPodsEveryGracefulStopWithAPromptInHandAndARetryOutstandingDeliversWithin10s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving, publisher, healthy := mpRetryOutstanding(t, uri, a, "everystop")

	stop := publisher.every(100*time.Millisecond, healthy...)
	mpEventually(t, 10*time.Second, "the first healthy messages to arrive", func() bool {
		sent := mpTo(publisher.all(), healthy...)
		return len(sent) >= 10 && serving.tookAll(sent[:10])
	})
	mpGracefulStopsInTurn(t, binary, uri, serving, publisher, healthy, "everystop", a, b, "pod-c", "pod-d")
	time.Sleep(4 * time.Second) // publish on past the last hand-over
	stop()

	messages := mpTo(publisher.all(), healthy...)
	serving.awaitAll(t, messages, mpGracefulBound+10*time.Second)
	serving.requireWithin(t, messages, mpGracefulBound, time.Time{})
	serving.requireOrder(t, messages)
	serving.requireKnown(t, publisher.all())
}

// A lost node, then graceful stops one after another of the pod that took its expired lease and of
// each pod after it, B to C to D, each with a prompt in hand and a retry outstanding as in
// TestTwoPodsEveryGracefulStopWithAPromptInHandAndARetryOutstandingDeliversWithin10s. The server
// keeps the lost pod's connection, and with it the durable's push binding, until its ping timeout,
// minutes in production, so B subscribes beside it and marks the record stale_binding, which every
// later holder carries on. B's hold ends before its stop, so nothing is left with the lost pod's
// binding alone: each hand-over is a graceful hand-back's, whose cap is 3 s, and every message to a
// healthy session reaches it within 10 s of its publish, each session's in order. At 1fe15e1b
// (INFRA-557 review finding) stale_binding also stood for an ack wait owed, so after the node loss
// every hand-over's hold ran to the 7 s cap of a hard take-over, and the messages queued at each
// SIGTERM arrived about 11 s after their publish.
func TestTwoPodsEveryGracefulStopAfterANodeLossWithAPromptInHandAndARetryOutstandingDeliversWithin10s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	c := mpStartPod(t, binary, uri, "pod-c")
	mpWaitStandby(t, c, 30*time.Second)
	t.Cleanup(a.relay.freeze()) // the node is lost: its connection stays open, silent
	holder, other := mpTakeoverHolder(t, mpHardBound, b, c)
	mpWaitActive(t, holder, mpHardBound)
	serving, publisher, healthy := mpRetryOutstanding(t, uri, holder, "lossstops")

	stop := publisher.every(100*time.Millisecond, healthy...)
	mpEventually(t, 10*time.Second, "the first healthy messages to arrive", func() bool {
		sent := mpTo(publisher.all(), healthy...)
		return len(sent) >= 10 && serving.tookAll(sent[:10])
	})
	mpGracefulStopsInTurn(t, binary, uri, serving, publisher, healthy, "lossstops", holder, other, "pod-d")
	time.Sleep(4 * time.Second) // publish on past the last hand-over
	stop()

	messages := mpTo(publisher.all(), healthy...)
	serving.awaitAll(t, messages, mpGracefulBound+10*time.Second)
	serving.requireWithin(t, messages, mpGracefulBound, time.Time{})
	serving.requireOrder(t, messages)
	serving.requireKnown(t, publisher.all())
}

// mpGracefulStopsInTurn stops stopping gracefully, then each pod that takes its place in turn, with
// next the first stop's standby and each later standby a pod started then, named after standbys,
// one per stop but the first. Each stop comes once the active pod has delivered every message to
// healthy published so far, past its own settle hold, and a second into a prompt that the serving
// port never answers, to a session of row's registered for it, with healthy's messages queued
// behind it. It returns once the last stop's standby is active.
func mpGracefulStopsInTurn(t *testing.T, binary, uri string, serving *mpServingPort, publisher *mpPublisher, healthy []string, row string, stopping, next *mpPod, standbys ...string) {
	t.Helper()
	for handOver := range len(standbys) + 1 {
		sessionID := fmt.Sprintf("ses_mp_%s_slow_%d", row, handOver+1)
		stopping.registerPort(t, serving, sessionID)
		sent := mpTo(publisher.all(), healthy...)
		mpEventually(t, mpGracefulBound, stopping.instance+" to deliver what was published before its prompt in hand", func() bool {
			return serving.tookAll(sent)
		})
		inHand := serving.hold(sessionID)
		publisher.mustPublish(t, sessionID)
		select {
		case <-inHand:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s never prompted %s", stopping.instance, sessionID)
		}
		time.Sleep(time.Second) // healthy's messages queue behind the prompt in hand
		stopping.Terminate(t)
		stopping.WaitExit(t, "SIGTERM")
		mpWaitActive(t, next, mpGracefulBound)
		if handOver == len(standbys) {
			return
		}
		standby := mpStartPod(t, binary, uri, standbys[handOver])
		mpWaitStandby(t, standby, 30*time.Second)
		stopping, next = next, standby
	}
}

// SIGKILL of the active pod while a message to a session whose prompts fail waits out its 30 s
// retry delay, with a message published every 100 ms to two healthy sessions. The standby takes the
// expired lease and its settle hold never finds every outstanding message in hand: it ends at what
// the killed pod can still have outstanding, one 6 s ack wait and a second after the subscribe, and
// every message to a healthy session published after the kill reaches it within 15 s, each
// session's in order. At 18779a03 (INFRA-557 review finding) the hold runs to its 13 s cap, and
// those messages arrive about 19 s after the kill.
func TestTwoPodsSIGKILLWithARetryOutstandingHandsOverInOrderWithin15s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving, publisher, healthy := mpRetryOutstanding(t, uri, a, "retrykill")

	stop := publisher.every(100*time.Millisecond, healthy...)
	mpEventually(t, 10*time.Second, "the first healthy messages to arrive", func() bool {
		sent := mpTo(publisher.all(), healthy...)
		return len(sent) >= 10 && serving.tookAll(sent[:10])
	})
	killed := a.kill(t)
	time.Sleep(3 * time.Second) // publish on after the kill
	stop()

	messages := mpTo(publisher.all(), healthy...)
	serving.awaitAll(t, messages, mpHardBound+10*time.Second)
	serving.requireWithin(t, mpTo(publisher.since(killed), healthy...), mpHardBound, killed)
	serving.requireOrder(t, messages)
	serving.requireKnown(t, publisher.all())
	mpWaitActive(t, b, time.Second)
}
