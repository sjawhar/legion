//go:build multiprocess

// Two listener processes for one machine id, which is what a machine runs once it has an active
// pod and a standby (INFRA-557). This file is the suite's harness; its rows are in
// multiprocess_takeover_test.go, multiprocess_state_test.go and multiprocess_handback_test.go,
// under the same build tag. Every row of the suite starts real cmd/listener binaries against one
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
// INFRA-557 (819dc283) has too, but for testnats' grant fixtures (grant.go and testdata/), so
// these files compile there with that relay, cmdtest and those beside it and each row shows how
// that build fails. The few places a row has to recognise such a build (a /healthz without
// "role") say so.
//
// Its own build tag, and its own CI job, for the reason cmd/dispatch/multiprocess_test.go has
// its: a row starts two processes and waits out a lease, an ack wait or a 140 s standby, tens of
// seconds to minutes each, which go test's default per-package deadline in the unit lane cannot
// hold.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
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

// mpPod is one listener process of the machine, behind its own relay to NATS: a listenerRelay,
// which fails the whole link (mpStartPod), or an mpLaneRelay, which fails one of the pod's
// connections and leaves the others (mpStartPodBehindLanes); the other is nil.
type mpPod struct {
	*cmdtest.Process
	instance string
	port     int
	relay    *listenerRelay
	lanes    *mpLaneRelay
}

// mpStartPod starts binary as instance of mpMachine, on a loopback port the kernel chose and the
// test holds until the pod serves on it (cmdtest.HoldPort, inherited as ENVOY_LISTEN_FD), reaching
// the NATS at natsURL, which mpStartNATS started, through a relay of its own as mpMachine's
// granted user, with env added to its environment. The pod is killed when the test ends, and its
// output's tail is logged when the test failed.
func mpStartPod(t *testing.T, binary, natsURL, instance string, env ...string) *mpPod {
	t.Helper()
	relay := startListenerRelay(t, mpServerURL(t, natsURL))
	pod := mpStartPodAt(t, binary, natsURL, relay.relayed, instance, env...)
	pod.relay = relay
	return pod
}

// mpStartPodBehindLanes starts instance as mpStartPod does, behind an mpLaneRelay rather than a
// listenerRelay: a row that fails one of the pod's NATS connections alone - its receive
// connection while its lease connection renews on - cuts it by name there.
func mpStartPodBehindLanes(t *testing.T, binary, natsURL, instance string, env ...string) *mpPod {
	t.Helper()
	lanes := startMPLaneRelay(t, mpServerURL(t, natsURL))
	pod := mpStartPodAt(t, binary, natsURL, lanes.relayed, instance, env...)
	pod.lanes = lanes
	return pod
}

// mpServerURL is natsURL without the admin user every row's own clients connect as: what a relay
// dials, since each pod connects as mpMachine's granted nkey user instead.
func mpServerURL(t *testing.T, natsURL string) string {
	t.Helper()
	server, err := url.Parse(natsURL)
	if err != nil {
		t.Fatalf("parse %s: %v", natsURL, err)
	}
	server.User = nil
	return server.String()
}

// mpStartPodAt starts the pod of mpStartPod, dialling NATS at dial, the address of the relay the
// caller put in front of the server natsURL names.
func mpStartPodAt(t *testing.T, binary, natsURL, dial, instance string, env ...string) *mpPod {
	t.Helper()
	seed, granted := mpSeeds.Load(natsURL)
	if !granted {
		t.Fatalf("%s is no NATS server mpStartNATS started: every pod connects as its machine's granted user", natsURL)
	}
	held := cmdtest.HoldPort(t)
	cmd := exec.Command(binary)
	cmd.Env = append([]string{
		"PORT=" + strconv.Itoa(held.Port),
		"ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_MACHINE_ID=" + mpMachine,
		"ENVOY_INSTANCE_ID=" + instance,
		"NATS_URLS=" + dial,
		"NATS_NKEY_SEED=" + seed.(string),
		"ENVOY_API_TOKEN=" + mpToken,
	}, env...)
	held.Pass(cmd, "ENVOY_LISTEN_FD")
	pod := &mpPod{Process: cmdtest.Start(t, "the listener "+instance, cmd), instance: instance, port: held.Port}
	mpJournalOf(t).pod(pod)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s's output, but for each delivery's lines (last 150 lines):\n%s", instance, mpTail(mpLinesWithout(pod.Output.String()), 150))
		}
	})
	return pod
}

// mpReceiveConnection is the client name of a pod's main NATS connection, the one the machine's
// durable is received on (`bus.Dial`, cmd/listener/main.go): every connection a pod opens names
// itself in its CONNECT frame, and the delivery lease's own connection, `envoy-listener-lease
// <instance>`, is a second socket to the same server under a name of its own. An mpLaneRelay
// tells them apart by that name.
func mpReceiveConnection(instance string) string { return "envoy-listener " + instance }

// mpConnectName reads the client name out of a CONNECT frame's JSON.
var mpConnectName = regexp.MustCompile(`"name":"([^"]*)"`)

// mpLaneRelay stands between one pod and its NATS server and tells the pod's connections apart
// by the client name in each CONNECT frame (mpReceiveConnection), so a row can fail one of them
// and leave the rest: a holder whose receive connection is lost while its lease connection
// renews on is the fault cut answers. Every connection is forwarded until cut names it.
type mpLaneRelay struct {
	listener net.Listener
	target   string
	relayed  string
	done     chan struct{}
	closing  sync.Once

	mu sync.Mutex
	// cuts holds one open gate per client name that is cut: every copy of such a connection waits
	// for it to close, which healing does.
	cuts  map[string]chan struct{}
	links []*mpLaneLink
	// blackhole, while open, holds every connection the pod opens from then on before its first
	// byte in either direction, as a route that drops packets leaves a dial: the TCP connection is
	// accepted, and nats.go waits out its connect timeout for an INFO that never comes.
	blackhole chan struct{}
	// drops holds, per client name, the subject prefix of the requests the relay drops on that
	// name's connections (dropRequests).
	drops map[string]string
}

// mpLaneLink is one connection through the relay: the pod's socket, the server's, and the client
// name the pod's CONNECT frame carried, which is empty until the relay has read it.
type mpLaneLink struct {
	client, server net.Conn

	mu   sync.Mutex
	name string
	seen []byte
	// pending is what the pod has sent that does not yet make a whole protocol frame (forward).
	pending []byte
}

func startMPLaneRelay(t *testing.T, uri string) *mpLaneRelay {
	t.Helper()
	server, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse NATS URL %q: %v", uri, err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the lane relay: %v", err)
	}
	relayed := *server
	relayed.Host = listener.Addr().String()
	relay := &mpLaneRelay{
		listener: listener,
		target:   server.Host,
		relayed:  relayed.String(),
		done:     make(chan struct{}),
		cuts:     map[string]chan struct{}{},
	}
	t.Cleanup(relay.close)
	go relay.serve()
	return relay
}

// cut takes the pod's connection named name down and keeps it down: the relay stops moving its
// bytes in either direction, every socket left open as a lost node leaves them, and closes each
// connection the pod opens under that name from its CONNECT frame on, as a refused dial does, so
// nats.go's reconnects never land. The pod's other connections are untouched. The returned func
// heals it, however often it is called.
func (r *mpLaneRelay) cut(name string) (heal func()) {
	r.mu.Lock()
	gate, cutting := r.cuts[name]
	if !cutting {
		gate = make(chan struct{})
		r.cuts[name] = gate
	}
	r.mu.Unlock()
	return sync.OnceFunc(func() {
		r.mu.Lock()
		owned := r.cuts[name] == gate
		if owned {
			delete(r.cuts, name)
		}
		r.mu.Unlock()
		if owned {
			close(gate)
		}
	})
}

func (r *mpLaneRelay) serve() {
	for {
		client, err := r.listener.Accept()
		if err != nil {
			return
		}
		go r.relay(client)
	}
}

// relay copies what the pod sends to the server, and starts the copy of what the server sends
// back. It reads the client name out of the CONNECT frame as it passes, and closes a connection
// opened under a name that is cut.
func (r *mpLaneRelay) relay(client net.Conn) {
	server, err := net.Dial("tcp", r.target)
	if err != nil {
		_ = client.Close()
		return
	}
	link := &mpLaneLink{client: client, server: server}
	r.mu.Lock()
	r.links = append(r.links, link)
	r.mu.Unlock()
	defer func() { _ = server.Close() }()
	go r.toClient(link)
	buffer := make([]byte, 32*1024)
	for {
		// Waiting before the read as well as before the write leaves a cut connection's sockets
		// as a lost node leaves them: the relay never reads the pod's close, so the server keeps
		// the connection, and with it the durable's push binding.
		r.pass(link)
		n, err := client.Read(buffer)
		if n > 0 {
			if name, learned := link.named(buffer[:n]); learned && r.gate(name) != nil {
				return
			}
			r.pass(link)
			if out := link.forward(buffer[:n], r.dropFor(link.lane())); len(out) > 0 {
				if _, writeErr := server.Write(out); writeErr != nil {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// toClient copies what the server sends to the pod, and nothing while the connection is cut.
func (r *mpLaneRelay) toClient(link *mpLaneLink) {
	defer func() { _ = link.client.Close() }()
	buffer := make([]byte, 32*1024)
	for {
		r.pass(link)
		n, err := link.server.Read(buffer)
		if n > 0 {
			r.pass(link)
			if _, writeErr := link.client.Write(buffer[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// pass waits while the link's connection is cut, and returns at once while it is not, once it is
// healed, or once the relay is closed.
func (r *mpLaneRelay) pass(link *mpLaneLink) {
	if link.lane() == "" {
		r.mu.Lock()
		hole := r.blackhole
		r.mu.Unlock()
		if hole != nil {
			select {
			case <-hole:
			case <-r.done:
			}
		}
	}
	gate := r.gate(link.lane())
	if gate == nil {
		return
	}
	select {
	case <-gate:
	case <-r.done:
	}
}

// blackholeNew holds every connection the pod opens from now on before its first byte, until the
// returned func runs (pass). Connections already open are untouched.
func (r *mpLaneRelay) blackholeNew() (heal func()) {
	hole := make(chan struct{})
	r.mu.Lock()
	r.blackhole = hole
	r.mu.Unlock()
	return sync.OnceFunc(func() {
		r.mu.Lock()
		if r.blackhole == hole {
			r.blackhole = nil
		}
		r.mu.Unlock()
		close(hole)
	})
}

// dropRequests drops, from now on, every PUB or HPUB frame the pod sends on a connection named
// name whose subject starts with prefix, as a server that never answers those requests leaves
// them: the requester waits out its own timeout. Every other frame passes. The returned func
// stops the dropping, however often it is called.
func (r *mpLaneRelay) dropRequests(name, prefix string) (stop func()) {
	r.mu.Lock()
	if r.drops == nil {
		r.drops = map[string]string{}
	}
	r.drops[name] = prefix
	r.mu.Unlock()
	return sync.OnceFunc(func() {
		r.mu.Lock()
		if r.drops[name] == prefix {
			delete(r.drops, name)
		}
		r.mu.Unlock()
	})
}

// dropFor is the subject prefix dropped on connections named name, empty when none is.
func (r *mpLaneRelay) dropFor(name string) string {
	if name == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.drops[name]
}

// forward takes what the pod sent and returns the whole protocol frames of it to send on, without
// each PUB or HPUB frame whose subject starts with drop when drop is not empty. A frame not yet
// whole waits in pending for the rest, so the frames stay whole whenever dropping starts.
func (l *mpLaneLink) forward(data []byte, drop string) []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending = append(l.pending, data...)
	var out []byte
	for {
		end := bytes.Index(l.pending, []byte("\r\n"))
		if end < 0 {
			return out
		}
		size := end + 2
		fields := strings.Fields(string(l.pending[:end]))
		if len(fields) >= 3 && (strings.EqualFold(fields[0], "PUB") || strings.EqualFold(fields[0], "HPUB")) {
			total, err := strconv.Atoi(fields[len(fields)-1])
			if err == nil {
				size += total + 2
			}
			if len(l.pending) < size {
				return out
			}
			if drop != "" && strings.HasPrefix(fields[1], drop) {
				l.pending = l.pending[size:]
				continue
			}
		}
		out = append(out, l.pending[:size]...)
		l.pending = l.pending[size:]
	}
}

// gate is the open gate of a cut connection name, nil while connections under that name pass.
func (r *mpLaneRelay) gate(name string) chan struct{} {
	if name == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cuts[name]
}

// named records what the pod has sent until its CONNECT frame names the client, and reports that
// name and whether this read is the one that completed it.
func (l *mpLaneLink) named(data []byte) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.name != "" {
		return l.name, false
	}
	l.seen = append(l.seen, data...)
	match := mpConnectName.FindSubmatch(l.seen)
	if match == nil {
		return "", false
	}
	l.name, l.seen = string(match[1]), nil
	return l.name, true
}

// lane is the client name of the link's connection, empty until its CONNECT frame was read.
func (l *mpLaneLink) lane() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.name
}

func (r *mpLaneRelay) close() {
	r.closing.Do(func() {
		close(r.done)
		_ = r.listener.Close()
	})
	r.mu.Lock()
	links := r.links
	r.links = nil
	r.mu.Unlock()
	for _, link := range links {
		_ = link.client.Close()
		_ = link.server.Close()
	}
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
// counted apart: the session never took it either. Every prompt it sees, whichever of those it
// is, is recorded in the row's delivery journal (mpJournal), which a failing row prints.
type mpServingPort struct {
	port    int
	journal *mpJournal

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
	serving := &mpServingPort{journal: mpJournalOf(t), prompts: map[string][]mpPrompt{}, holds: map[string]chan struct{}{}, failing: map[string]bool{}, failed: map[string]int{}, stopping: make(chan struct{})}
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
		s.journal.receipt(mpReceipt{session: sessionID, at: time.Now(), answer: mpPromptUnexpected})
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
		s.journal.receipt(mpReceipt{session: sessionID, event: event, at: time.Now(), answer: mpPromptHeld})
		close(held)
		select {
		case <-r.Context().Done():
		case <-s.stopping:
		}
		return
	}
	if failing {
		s.journal.receipt(mpReceipt{session: sessionID, event: event, at: time.Now(), answer: mpPromptRefused})
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	taken := time.Now()
	s.mu.Lock()
	s.prompts[sessionID] = append(s.prompts[sessionID], mpPrompt{event: event, at: taken})
	s.mu.Unlock()
	s.journal.receipt(mpReceipt{session: sessionID, event: event, at: taken, answer: mpPromptTaken})
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
	mpJournalOf(t).readsFrom(natsURL)
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
	mpJournalOf(p.t).publish(m, target.topic)
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

// The row's delivery journal.
//
// Every row of this suite publishes tens to hundreds of messages and asserts that each one
// reached its session. When one did not, the row's output said which message was missing and
// nothing else: the two lines a listener logs for every delivery are stripped out of the pods'
// own output (mpLinesWithout), since a hundred messages' worth of them would bury everything
// else, so a run that lost a message could not be told from one that was merely late with it.
//
// The journal is what the harness saw of each message: the publish, every prompt the serving
// port answered for it, and - for a message the port did not take exactly once - the sequence
// the stream holds it under, what the machine's durable has done with it, and the lines each pod
// logged that name it, which is where an acknowledgement shows. A pod that logged
// "listener agent delivered" prompted the message and acked it; a message the durable counts
// neither unacknowledged nor waiting, that no pod ever logged, never reached a pod at all.
//
// That last reading is what this exists for. A message the stream stored, that no pod of the
// machine ever logged and that the durable holds nothing for, was pushed by nats-server into the
// durable's deliver subject in the moment after its only subscriber's connection went: the
// server counts such a push undone, and through v2.14.x it counted it below zero, so the message
// left the pending list at its ack wait with no redelivery (the same behaviour receive.go's
// hand-back keeps a subscriber for; v2.10.29 consumer.go didNotDeliver, decDeliveryCount and
// hasMaxDeliveries). The listener requires nats-server v2.15.0 or later, whose decDeliveryCount
// leaves a first delivery's count alone, so on the server these tests link such a message comes
// back one ack wait later and this reading should find none (durable_backlog_cut_test.go).
// The durable's own count of redeliveries rises by one for each message it loses that way.
//
// It is printed when the row fails, and only then: a row that passes is as quiet as before.

// mpPromptTaken and the three beside it are what the serving port answered one prompt, as the
// journal records it (mpServingPort.serve).
const (
	mpPromptTaken      = "204"
	mpPromptHeld       = "held"
	mpPromptRefused    = "500"
	mpPromptUnexpected = "unexpected"
)

// mpJournalScanCap bounds the stream sequences a failing row reads back, so a row that published
// more than any of this suite's do spends no longer in its cleanup.
const mpJournalScanCap = 2000

// mpJournalPodLines bounds how many messages the journal quotes the pods' own lines for, and how
// many lines of one pod it quotes for each: a row that lost every message says so once rather
// than in a thousand lines.
const (
	mpJournalPodLines    = 20
	mpJournalPodMessages = 10
)

// mpPublication is one message the row published, with the topic it went on.
type mpPublication struct {
	message mpMessage
	topic   string
}

// mpReceipt is one prompt the serving port saw and what it answered (mpPromptTaken and the three
// beside it). event is empty for a request no session expects, which carries no event id.
type mpReceipt struct {
	session string
	event   string
	at      time.Time
	answer  string
}

// mpJournal is one row's delivery journal (see above).
type mpJournal struct {
	started time.Time

	mu        sync.Mutex
	published []mpPublication
	receipts  []mpReceipt
	pods      []*mpPod
	// natsURL is the row's NATS server as its own clients reach it, which the journal reads the
	// stream and the durable through at the end (mpStreamReads). It is the publisher's
	// (mpConnectPublisher), not a connection of its own, because a cleanup runs after the
	// publisher's client was closed and before the server is stopped.
	natsURL string
}

// mpJournals is each row's journal, keyed on its *testing.T: the harness's pieces are made one at
// a time by the row - its pods, its serving port, its publisher - and share no object but the
// test itself. A row whose pods live in a t.Run keeps the subtest's journal, with the subtest.
var mpJournals sync.Map

// mpJournalOf is t's journal, made on the first call. Its dump is registered before every pod's
// own output is, so it is printed after them: the last thing a failing row says.
func mpJournalOf(t *testing.T) *mpJournal {
	if held, ok := mpJournals.Load(t); ok {
		return held.(*mpJournal)
	}
	held, loaded := mpJournals.LoadOrStore(t, &mpJournal{started: time.Now()})
	journal := held.(*mpJournal)
	if !loaded {
		t.Cleanup(func() { mpJournals.Delete(t) })
		t.Cleanup(func() {
			if t.Failed() {
				journal.dump(t)
			}
		})
	}
	return journal
}

func (j *mpJournal) publish(m mpMessage, topic string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.published = append(j.published, mpPublication{message: m, topic: topic})
}

func (j *mpJournal) receipt(r mpReceipt) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.receipts = append(j.receipts, r)
}

func (j *mpJournal) pod(p *mpPod) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.pods = append(j.pods, p)
}

// readsFrom names the NATS server the journal reads the stream and the durable from.
func (j *mpJournal) readsFrom(natsURL string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.natsURL = natsURL
}

// into is how long after the row's first harness call at came.
func (j *mpJournal) into(at time.Time) string {
	return "+" + at.Sub(j.started).Round(time.Millisecond).String()
}

// dump prints the journal. The row's own failure says which message it is about; this says where
// that message went.
func (j *mpJournal) dump(t *testing.T) {
	j.mu.Lock()
	published := slices.Clone(j.published)
	receipts := slices.Clone(j.receipts)
	pods := slices.Clone(j.pods)
	natsURL := j.natsURL
	j.mu.Unlock()
	if len(published) == 0 && len(receipts) == 0 {
		return
	}
	took := map[string][]mpReceipt{}
	for _, receipt := range receipts {
		took[receipt.event] = append(took[receipt.event], receipt)
	}
	var inQuestion []mpPublication
	for _, publication := range published {
		taken := 0
		for _, receipt := range took[publication.message.event] {
			if receipt.answer == mpPromptTaken {
				taken++
			}
		}
		if taken != 1 {
			inQuestion = append(inQuestion, publication)
		}
	}
	stored, durable := mpStreamReads(natsURL)
	var report strings.Builder
	fmt.Fprintf(&report, "the row's delivery journal: %d messages published, %d prompts at the serving port, %d messages the port did not take exactly once\n  the machine's durable: %s",
		len(published), len(receipts), len(inQuestion), durable)
	for _, publication := range published {
		event := publication.message.event
		fmt.Fprintf(&report, "\n  %s to %s: published %s on %s, %s; %s",
			event, publication.message.session, j.into(publication.message.published), publication.topic,
			mpStoredAs(stored, event), j.prompts(took[event]))
	}
	known := map[string]bool{}
	for _, publication := range published {
		known[publication.message.event] = true
	}
	for _, receipt := range receipts {
		if !known[receipt.event] {
			fmt.Fprintf(&report, "\n  a prompt to %s for %q, which the row published nothing for: %s (%s)",
				receipt.session, receipt.event, j.into(receipt.at), receipt.answer)
		}
	}
	for index, publication := range inQuestion {
		if index == mpJournalPodMessages {
			fmt.Fprintf(&report, "\n  and %d more messages the port did not take exactly once, whose lines are not quoted", len(inQuestion)-index)
			break
		}
		event := publication.message.event
		fmt.Fprintf(&report, "\n  what the pods logged for %s:", event)
		for _, pod := range pods {
			lines := mpLinesNaming(pod.Output.String(), event)
			if len(lines) == 0 {
				fmt.Fprintf(&report, "\n    %s logged no line that names it", pod.instance)
				continue
			}
			for _, line := range lines {
				fmt.Fprintf(&report, "\n    %s: %s", pod.instance, line)
			}
		}
	}
	t.Log(report.String())
}

// prompts is what the serving port answered for one message, in the order it answered.
func (j *mpJournal) prompts(receipts []mpReceipt) string {
	if len(receipts) == 0 {
		return "the serving port saw no prompt for it"
	}
	answers := make([]string, len(receipts))
	for index, receipt := range receipts {
		answers[index] = fmt.Sprintf("prompted %s (%s)", j.into(receipt.at), receipt.answer)
	}
	return strings.Join(answers, ", ")
}

// mpStoredAs says what the stream holds the event under.
func mpStoredAs(stored map[string]uint64, event string) string {
	if seq, ok := stored[event]; ok {
		return fmt.Sprintf("stored at stream sequence %d", seq)
	}
	if stored == nil {
		return "the stream was not read"
	}
	return "no sequence of the stream holds it"
}

// mpLinesNaming is the lines of a pod's output that name event, at most mpJournalPodLines of
// them. These are mostly the two lines a listener logs for every delivery, which the pod's own
// output is stripped of (mpLinesWithout).
func mpLinesNaming(output, event string) []string {
	var named []string
	for line := range strings.SplitSeq(output, "\n") {
		if !strings.Contains(line, event) {
			continue
		}
		if len(named) == mpJournalPodLines {
			named = append(named, "... and more lines name it")
			break
		}
		named = append(named, line)
	}
	return named
}

// mpStreamReads is what the stream and the machine's durable hold at the end of a failing row:
// the stream sequence of every event the stream still holds, by event id, and one line describing
// the durable. A message the serving port never took that the stream holds, that the durable
// counts neither unacknowledged nor waiting, and that no pod ever logged, was dropped between the
// two rather than delayed. It connects a client of its own, since a cleanup runs after the
// publisher's was closed.
//
// A row whose own fault left the server down - one that stopped it and failed before it was back -
// is answered by the dial below rather than by bus.Connect, whose ten attempts would hold the
// cleanup of an already failing row for a minute.
func mpStreamReads(natsURL string) (map[string]uint64, string) {
	if natsURL == "" {
		return nil, "not read: the row connected no publisher"
	}
	server, err := url.Parse(natsURL)
	if err != nil {
		return nil, fmt.Sprintf("not read: parse %s: %v", natsURL, err)
	}
	probe, err := net.DialTimeout("tcp", server.Host, time.Second)
	if err != nil {
		return nil, fmt.Sprintf("not read: %s did not answer: %v", server.Host, err)
	}
	_ = probe.Close()
	client, err := bus.Connect([]string{natsURL})
	if err != nil {
		return nil, fmt.Sprintf("not read: connect %s: %v", natsURL, err)
	}
	defer client.Close()
	consumer := bus.ListenerDurable(mpMachine)
	state := ""
	if info, err := client.JS().ConsumerInfo(bus.Stream, consumer); err != nil {
		state = fmt.Sprintf("%s: %v", consumer, err)
	} else {
		state = fmt.Sprintf("%s: delivered to stream sequence %d, ack floor %d, %d unacknowledged, %d redelivered, %d waiting",
			consumer, info.Delivered.Stream, info.AckFloor.Stream, info.NumAckPending, info.NumRedelivered, info.NumPending)
	}
	info, err := client.JS().StreamInfo(bus.Stream)
	if err != nil {
		return nil, state + fmt.Sprintf("; the stream was not read: %v", err)
	}
	events := map[string]uint64{}
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq && seq < info.State.FirstSeq+mpJournalScanCap; seq++ {
		raw, err := client.JS().GetMsg(bus.Stream, seq)
		if err != nil {
			continue
		}
		var item contracts.Envelope
		if json.Unmarshal(raw.Data, &item) == nil && item.EventID != "" {
			events[item.EventID] = seq
		}
	}
	return events, state
}
