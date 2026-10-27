//go:build multiprocess

// Two listener processes for one machine id, which is what a machine runs once it has an active
// pod and a standby (INFRA-557). Every row here starts real cmd/listener binaries against one
// in-process NATS server, each behind a listenerRelay of its own, and drives them over their real
// HTTP API and the real bus, because each thing it checks is a property of whole processes: which
// of two processes delivers from the machine's durable, what happens to the messages one process
// holds when it is stopped, killed or cut off, and what the other answers meanwhile. Port sessions
// are one fake serving port (mpServingPort) that counts each completed prompt_async by event id;
// the machine id is arbitrary (box-a).
//
// The rows use nothing but the binary cmdtest.Build builds, its environment, its HTTP API, NATS,
// the relay in reconnect_shutdown_test.go and APIs of the internal packages that a build before
// INFRA-557 (819dc283) has too, so this file compiles there with that relay and cmdtest beside
// it and each row shows how that build fails. The few places a row has to recognise such a build
// (a /healthz without "role") say so.
//
// Its own build tag, and its own CI job, for the reason cmd/dispatch/multiprocess_test.go has
// its: a row starts two processes and waits out a lease, an ack wait or a 140 s standby, tens of
// seconds to minutes each, which go test's default per-package deadline in the unit lane cannot
// hold.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

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

// mpStartPod starts binary as instance of mpMachine, on a free port, reaching the NATS at natsURL
// through a relay of its own, with env added to its environment. The pod is killed when the test
// ends, and its output's tail is logged when the test failed.
func mpStartPod(t *testing.T, binary, natsURL, instance string, env ...string) *mpPod {
	t.Helper()
	relay := startListenerRelay(t, natsURL)
	port := cmdtest.FreeTCPPort(t)
	cmd := exec.Command(binary)
	cmd.Env = append([]string{
		"PORT=" + strconv.Itoa(port),
		"ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_MACHINE_ID=" + mpMachine,
		"ENVOY_INSTANCE_ID=" + instance,
		"NATS_URLS=" + relay.relayed,
		"ENVOY_API_TOKEN=" + mpToken,
	}, env...)
	pod := &mpPod{Process: cmdtest.Start(t, "the listener "+instance, cmd), instance: instance, port: port, relay: relay}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s's output, but for each delivery's lines (last 150 lines):\n%s", instance, mpTail(mpLinesWithout(pod.Output.String()), 150))
		}
	})
	return pod
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
// waits for it to be its standby.
func mpStartPair(t *testing.T, binary, natsURL string) (a, b *mpPod) {
	t.Helper()
	a = mpStartPod(t, binary, natsURL, "pod-a")
	mpWaitActive(t, a, 30*time.Second)
	b = mpStartPod(t, binary, natsURL, "pod-b")
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
// serving port at 127.0.0.1, and requires a 200.
func (p *mpPod) registerPort(t *testing.T, serving *mpServingPort, sessionID string) {
	t.Helper()
	body := fmt.Sprintf(`{"session_id":%q,"topics":[],"port":%d,"host":"127.0.0.1"}`, sessionID, serving.port)
	if answer := p.call(http.MethodPost, "/v1/interests/subscribe", body); answer.status != http.StatusOK {
		t.Fatalf("register %s through %s: status %d %s %v", sessionID, p.instance, answer.status, answer.body, answer.err)
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
// recorded: the session never took it.
type mpServingPort struct {
	port int

	mu         sync.Mutex
	prompts    map[string][]mpPrompt
	holds      map[string]chan struct{}
	unexpected []string
	stopping   chan struct{}
}

func mpStartServingPort(t *testing.T) *mpServingPort {
	t.Helper()
	serving := &mpServingPort{prompts: map[string][]mpPrompt{}, holds: map[string]chan struct{}{}, stopping: make(chan struct{})}
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
	s.mu.Unlock()
	if hold {
		close(held)
		select {
		case <-r.Context().Done():
		case <-s.stopping:
		}
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

// mpPublisher publishes agent messages to port sessions from a bus client of the test's own,
// straight to NATS, as a session holding its own connection does. Its lock is held across each
// publish, so quiesce can put a fault between two publishes.
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

// publish publishes one message to sessionID and returns it once the stream stored it.
func (p *mpPublisher) publish(sessionID string) (mpMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	event := fmt.Sprintf("evt-mp-%04d", len(p.sent)+1)
	item := contracts.Envelope{
		EventID:        event,
		Source:         "agent",
		SourceEventID:  event,
		Topic:          contracts.AgentSubject(sessionID),
		DedupeKey:      "multiprocess." + event,
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "multiprocess message mp-event=" + event,
		TraceID:        "trace-" + event,
	}
	if err := p.client.Publish(item); err != nil {
		return mpMessage{}, fmt.Errorf("publish %s to %s: %w", event, sessionID, err)
	}
	m := mpMessage{event: event, session: sessionID, published: time.Now()}
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

// every publishes one message every interval, to each of sessions in turn, until the returned
// func runs (or the test ends), which returns once the last publish has.
func (p *mpPublisher) every(interval time.Duration, sessions ...string) (stop func()) {
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for index := 0; ; index++ {
			if _, err := p.publish(sessions[index%len(sessions)]); err != nil {
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
	uri := testnats.URL(t)
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
	uri := testnats.URL(t)
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
			uri := testnats.URL(t)
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
	uri := testnats.URL(t)
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
	uri := testnats.URL(t)
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
	uri := testnats.URL(t)
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
	uri := testnats.URL(t)
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
	server, uri := testnats.Start(t)
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
			uri := testnats.URL(t)
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
	server, uri := testnats.Start(t)
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
	uri := testnats.URL(t)
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

// mpLineWith is the first line of output containing text, or "".
func mpLineWith(output, text string) string {
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, text) {
			return line
		}
	}
	return ""
}
