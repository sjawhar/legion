package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// liveSocketRig is the API test server behind a real listener and run on a Lifetime, so Register
// builds the delivery hub and mounts GET /ws/live as production does.
type liveSocketRig struct {
	handler  http.Handler
	database *store.Store
	served   *server
	host     string
	url      string
	// stop ends the server's Lifetime, as the signal that shuts Dispatch down does.
	stop context.CancelFunc
}

// newLiveSocketRig starts the rig on options; listen, when given, wraps the server's listener.
// The Lifetime derives from the test's context, which ends before the test's cleanups wait out the
// sockets and the hub.
func newLiveSocketRig(t *testing.T, options testServerOptions, listen func(net.Listener) net.Listener) *liveSocketRig {
	t.Helper()
	lifetime, stop := context.WithCancel(t.Context())
	options.lifetime = lifetime
	handler, database, _, served := newRegisteredTestServer(t, options)
	httpServer := httptest.NewUnstartedServer(handler)
	if listen != nil {
		httpServer.Listener = listen(httpServer.Listener)
	}
	httpServer.Start()
	t.Cleanup(httpServer.Close)
	host := strings.TrimPrefix(httpServer.URL, "http://")
	return &liveSocketRig{handler: handler, database: database, served: served, host: host, url: "ws://" + host + "/ws/live", stop: stop}
}

// deliverySubscribe is a page's subscribe frame for a delivery view, as a socket test sends it.
func deliverySubscribe(sub string, view DeliveryLiveView) LiveSubscribe {
	return LiveSubscribe{Type: "subscribe", Sub: sub, View: LiveView{DeliveryLiveView: view}}
}

func asUser(login string) http.Header { return http.Header{"X-Dispatch-User": {login}} }

func withBearer(token string) http.Header {
	return http.Header{"Authorization": {"Bearer " + token}}
}

// sockets waits for exactly n sockets open on the server and answers them.
func (rig *liveSocketRig) sockets(t *testing.T, n int) []*liveSocket {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		open := rig.served.liveSockets.all()
		if len(open) == n {
			return open
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d live sockets open on the server, want %d", len(open), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// socketClient is one page's GET /ws/live connection. Once it reads, every message it is sent
// lands in frames, and ended closes when the connection ends, with err saying how and endedAt when.
type socketClient struct {
	conn    *gws.Conn
	frames  chan []byte
	ended   chan struct{}
	err     error
	endedAt time.Time
}

// open dials the socket, failing the test unless the upgrade succeeds; it reads nothing until read.
func (rig *liveSocketRig) open(t *testing.T, dialer *gws.Dialer, header http.Header) *socketClient {
	t.Helper()
	return openLiveSocketAt(t, dialer, rig.url, header)
}

// openLiveSocketAt dials the live socket at url, failing the test unless the upgrade succeeds; it
// reads nothing until read.
func openLiveSocketAt(t *testing.T, dialer *gws.Dialer, url string, header http.Header) *socketClient {
	t.Helper()
	conn, response, err := dialer.Dial(url, header)
	if err != nil {
		status := "no response"
		if response != nil {
			status = response.Status
		}
		t.Fatalf("open %s: %v (%s)", url, err, status)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &socketClient{conn: conn, frames: make(chan []byte, 4096), ended: make(chan struct{})}
}

// dial opens the socket and reads it.
func (rig *liveSocketRig) dial(t *testing.T, header http.Header) *socketClient {
	t.Helper()
	return rig.open(t, gws.DefaultDialer, header).read()
}

// refused dials the socket and answers the status Dispatch refused the upgrade with.
func (rig *liveSocketRig) refused(t *testing.T, header http.Header) int {
	t.Helper()
	conn, response, err := gws.DefaultDialer.Dial(rig.url, header)
	if err == nil {
		_ = conn.Close()
		t.Fatal("the upgrade succeeded; want it refused")
	}
	if response == nil {
		t.Fatalf("dial: %v, with no response", err)
	}
	return response.StatusCode
}

// read starts reading the socket: its ping handler answers Dispatch's pings only while it reads.
func (c *socketClient) read() *socketClient {
	go func() {
		defer close(c.ended)
		for {
			kind, data, err := c.conn.ReadMessage()
			if err != nil {
				c.err = err
				c.endedAt = time.Now()
				return
			}
			if kind != gws.TextMessage {
				c.err = fmt.Errorf("a message of kind %d", kind)
				c.endedAt = time.Now()
				return
			}
			c.frames <- data
		}
	}()
	return c
}

func (c *socketClient) send(t *testing.T, frame any) {
	t.Helper()
	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("encode %T: %v", frame, err)
	}
	c.sendText(t, data)
}

func (c *socketClient) sendText(t *testing.T, data []byte) {
	t.Helper()
	if err := c.conn.WriteMessage(gws.TextMessage, data); err != nil {
		t.Fatalf("send %.120s: %v", data, err)
	}
}

// take is the next message within within, failing the test when none comes or the socket ends.
func (c *socketClient) take(t *testing.T, within time.Duration, what string) []byte {
	t.Helper()
	return c.takeBy(t, time.Now().Add(within), within, what)
}

// takeBy is take for a wait that began earlier: deadline ends it, and within, the whole wait, is
// what its failure names.
func (c *socketClient) takeBy(t *testing.T, deadline time.Time, within time.Duration, what string) []byte {
	t.Helper()
	select {
	case data := <-c.frames:
		return data
	case <-c.ended:
		// The reader queues every message before it ends.
		select {
		case data := <-c.frames:
			return data
		default:
		}
		t.Fatalf("the socket ended (%v) waiting for %s", c.err, what)
	case <-time.After(time.Until(deadline)):
		t.Fatalf("no frame within %s, waiting for %s", within, what)
	}
	return nil
}

// next is the next frame but a heartbeat, decoded by its type.
func (c *socketClient) next(t *testing.T, within time.Duration, what string) any {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		frame := decodeSocketFrame(t, c.takeBy(t, deadline, within, what))
		if _, beat := frame.(LiveHeartbeat); !beat {
			return frame
		}
	}
}

func (c *socketClient) snapshot(t *testing.T, within time.Duration, sub string) DeliveryLiveSnapshot {
	t.Helper()
	frame := c.next(t, within, "the snapshot of "+sub)
	snapshot, ok := frame.(DeliveryLiveSnapshot)
	if !ok || snapshot.Sub != sub {
		t.Fatalf("frame = %+v, want the snapshot of %s", frame, sub)
	}
	return snapshot
}

func (c *socketClient) delta(t *testing.T, within time.Duration, sub string) DeliveryLiveDelta {
	t.Helper()
	frame := c.next(t, within, "a delta of "+sub)
	delta, ok := frame.(DeliveryLiveDelta)
	if !ok || delta.Sub != sub {
		t.Fatalf("frame = %+v, want a delta of %s", frame, sub)
	}
	return delta
}

func (c *socketClient) refusal(t *testing.T, within time.Duration, sub, code string) LiveError {
	t.Helper()
	frame := c.next(t, within, code+" for "+strconv.Quote(sub))
	refusal, ok := frame.(LiveError)
	if !ok || refusal.Sub != sub || refusal.Code != code {
		t.Fatalf("frame = %+v, want %s for %q", frame, code, sub)
	}
	return refusal
}

// refusals reads frames within within until each of subs has been refused with code, in any order,
// failing on any other frame but a heartbeat.
func (c *socketClient) refusals(t *testing.T, within time.Duration, code string, subs ...string) {
	t.Helper()
	deadline := time.Now().Add(within)
	waiting := make(map[string]bool, len(subs))
	for _, sub := range subs {
		waiting[sub] = true
	}
	for len(waiting) > 0 {
		switch frame := decodeSocketFrame(t, c.takeBy(t, deadline, within, code+" for each of "+strings.Join(subs, ", "))).(type) {
		case LiveHeartbeat:
		case LiveError:
			if frame.Code != code || !waiting[frame.Sub] {
				t.Fatalf("frame = %+v, want %s for one of %v not yet refused", frame, code, subs)
			}
			delete(waiting, frame.Sub)
		default:
			t.Fatalf("frame = %+v, want %s for one of %v", frame, code, subs)
		}
	}
}

// event is the next frame but a heartbeat, which must be one of sub's events.
func (c *socketClient) event(t *testing.T, within time.Duration, sub string) LiveEventFrame {
	t.Helper()
	frame := c.next(t, within, "an event of "+sub)
	event, ok := frame.(LiveEventFrame)
	if !ok || event.Sub != sub {
		t.Fatalf("frame = %+v, want an event of %s", frame, sub)
	}
	return event
}

// quiet fails when the socket sends any frame but a heartbeat, or ends, within within.
func (c *socketClient) quiet(t *testing.T, within time.Duration, what string) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case data := <-c.frames:
			if _, beat := decodeSocketFrame(t, data).(LiveHeartbeat); !beat {
				t.Fatalf("%s: the socket sent %.200s", what, data)
			}
		case <-c.ended:
			t.Fatalf("%s: the socket ended (%v)", what, c.err)
		case <-deadline:
			return
		}
	}
}

// closeError waits within within for the socket to end, passing over the messages it is sent
// first, and answers the close frame Dispatch ended it with.
func (c *socketClient) closeError(t *testing.T, within time.Duration) *gws.CloseError {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case <-c.frames:
		case <-c.ended:
			var closed *gws.CloseError
			if !errors.As(c.err, &closed) {
				t.Fatalf("the socket ended with %v, which carries no close code", c.err)
			}
			return closed
		case <-deadline:
			t.Fatalf("the socket was still open after %s", within)
		}
	}
}

// closeCode waits within within for the socket to end, counting the messages it is sent first, and
// answers the close code Dispatch ended it with: 1006 for an end with no close frame, which gorilla
// reads from the connection's end, and 0 for an error with no code, such as a reset.
func (c *socketClient) closeCode(t *testing.T, within time.Duration) (code, messages int) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case <-c.frames:
			messages++
		case <-c.ended:
			messages += len(c.frames)
			var closed *gws.CloseError
			if errors.As(c.err, &closed) {
				return closed.Code, messages
			}
			t.Logf("the socket ended with an error that carries no close code: %v", c.err)
			return 0, messages
		case <-deadline:
			t.Fatalf("the socket was still open after %s", within)
		}
	}
}

// throughputWait bounds a wait whose answer comes only once megabytes have crossed the socket and
// been parsed, which takes as long as the loaded machine takes. It is the smaller of
// throughputWaitCap and what the test binary's deadline leaves after throughputWaitMargin, so a
// server that never answers fails the test by name, and leaves the binary's timeout to the
// package's later tests. A deadline that leaves no more than throughputWaitMargin to begin with
// fails once, by name: that reads like a misconfigured run, not a server that stopped answering.
// A wait that needs no bulk read keeps a fixed 10 s bound.
func throughputWait(t *testing.T) time.Duration {
	t.Helper()
	deadline, bounded := t.Deadline()
	if !bounded {
		return throughputWaitCap
	}
	left := time.Until(deadline)
	if left <= throughputWaitMargin {
		t.Fatalf("the test binary's deadline leaves %s, not more than throughputWaitMargin (%s): this looks like a misconfigured run; raise -timeout", left.Round(time.Millisecond), throughputWaitMargin)
	}
	return min(left-throughputWaitMargin, throughputWaitCap)
}

const (
	// throughputWaitCap is above the slowest run of these tests measured (17.9 s, with the client's
	// writes throttled to 512 KiB/s) and the slowest loopback transfer of 8.5 MB measured on the
	// development machine (54 s, through 16 KiB socket buffers). A test that waits on it builds its
	// rig with it as the heartbeat: a frame holds the client's write lock until it is written, and
	// the client's ping handler waits 1 s for that lock before it drops the pong, so a slow transfer
	// answers no ping, and the server's pong deadline, twice the heartbeat, must outlast the wait.
	throughputWaitCap = 2 * time.Minute
	// throughputWaitMargin is what throughputWait leaves of the test binary's deadline for the
	// failed test's cleanups: the socket rig's took at most 26 ms after a wait that timed out.
	throughputWaitMargin = 30 * time.Second
)

// open reports whether the socket has not ended.
func (c *socketClient) open() bool {
	select {
	case <-c.ended:
		return false
	default:
		return true
	}
}

// decodeSocketFrame decodes one frame Dispatch sent as its type's struct, refusing a member the type
// does not declare.
func decodeSocketFrame(t *testing.T, data []byte) any {
	t.Helper()
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		t.Fatalf("a frame that is not a JSON object: %.200s: %v", data, err)
	}
	switch head.Type {
	case "snapshot":
		return decodeSocketFrameAs[DeliveryLiveSnapshot](t, data)
	case "delta":
		return decodeSocketFrameAs[DeliveryLiveDelta](t, data)
	case "freshness":
		return decodeSocketFrameAs[DeliveryLiveFreshness](t, data)
	case "error":
		return decodeSocketFrameAs[LiveError](t, data)
	case "heartbeat":
		return decodeSocketFrameAs[LiveHeartbeat](t, data)
	case "events_start":
		return decodeSocketFrameAs[LiveEventsStart](t, data)
	case "event":
		return decodeSocketFrameAs[LiveEventFrame](t, data)
	case "agent":
		return decodeSocketFrameAs[LiveAgentFrame](t, data)
	case "ended":
		return decodeSocketFrameAs[LiveEnded](t, data)
	}
	t.Fatalf("a frame of no known type: %.200s", data)
	return nil
}

func decodeSocketFrameAs[T any](t *testing.T, data []byte) T {
	t.Helper()
	var frame T
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&frame); err != nil {
		t.Fatalf("decode %T from %.200s: %v", frame, data, err)
	}
	return frame
}

// mintAgentToken is a personal agent token alice mints.
func mintAgentToken(t *testing.T, handler http.Handler) agentTokenResponse {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "delivery-probe"}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("mint an agent token: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[agentTokenResponse](t, response)
}

// hubConnCount is how many sockets hold a subscription on the hub.
func hubConnCount(hub *deliveryHub) int {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return len(hub.conns)
}

// smallSendBuffers shrinks the send buffer of every connection the server accepts, so a socket whose
// page stops reading blocks its writer after kilobytes rather than the megabytes the kernel tunes to.
type smallSendBuffers struct{ net.Listener }

func (l smallSendBuffers) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetWriteBuffer(4096)
	}
	return conn, err
}

// smallReadDialer dials with a small receive buffer, so a page that stops reading holds back the
// socket's writer after kilobytes; beside smallSendBuffers it is how a test stalls the writer.
func smallReadDialer() *gws.Dialer {
	return &gws.Dialer{HandshakeTimeout: 5 * time.Second, NetDial: func(network, address string) (net.Conn, error) {
		conn, err := net.Dial(network, address)
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetReadBuffer(4096)
		}
		return conn, err
	}}
}

// pollUntil polls condition every 10 ms for up to within, failing the test with what when it
// never holds.
func pollUntil(t *testing.T, within time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// streamFull reports whether socket's stream queue holds all it can.
func streamFull(socket *liveSocket) bool {
	return cap(socket.stream) > 0 && len(socket.stream) == cap(socket.stream)
}

func TestTheDeliverySocketAnswersASubscribeWithASnapshot(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	seedHubSettings(t, rig.database)
	now := time.Now().UTC()
	upsertPR(t, rig.database, hubPR(1, now.Add(-3*time.Hour)))

	client := rig.dial(t, asUser("alice"))
	client.send(t, deliverySubscribe("t", timelineRange(now.Add(-48*time.Hour), now, "")))
	snapshot := client.snapshot(t, 10*time.Second, "t")
	requireSameBody(t, "the socket's snapshot", snapshot.Body, getBody(t, rig.handler, windowTarget(t, deliveryLiveTimeline, snapshot.Body, "")))
	if _, ok := snapshot.Revs.Items["prs"]["acme/widgets#1"]; !ok {
		t.Fatalf("the snapshot's revisions %v hold no revision of acme/widgets#1", snapshot.Revs.Items)
	}

	// A merge after the snapshot reaches the page as a delta that lands on the GET route's body.
	upsertPR(t, rig.database, hubPR(2, now.Add(-2*time.Hour)))
	delta := client.delta(t, 10*time.Second, "t")
	if got := liveUpsertIDs(t, delta, "prs"); !slices.Equal(got, []string{"acme/widgets#2"}) {
		t.Fatalf("the merge's delta upserts %v, want acme/widgets#2", got)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, snapshot.Body, delta, "")
}

func TestAnUnauthenticatedUpgradeIsRefused(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	for name, header := range map[string]http.Header{
		"no credential":     nil,
		"an unknown bearer": withBearer("not-a-token"),
	} {
		if status := rig.refused(t, header); status != http.StatusUnauthorized {
			t.Errorf("%s: the upgrade answered %d, want 401", name, status)
		}
	}
	if open := rig.served.liveSockets.all(); len(open) != 0 {
		t.Fatalf("%d sockets open after refused upgrades", len(open))
	}
}

func TestAForeignOriginIsRefused(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	foreign := asUser("alice")
	foreign.Set("Origin", "https://other.example")
	if status := rig.refused(t, foreign); status != http.StatusForbidden {
		t.Fatalf("another site's page opening the socket with alice's credential was answered %d, want 403", status)
	}

	// The page's own origin opens it.
	own := asUser("alice")
	own.Set("Origin", "http://"+rig.host)
	client := rig.dial(t, own)
	client.send(t, LiveUnsubscribe{Type: "unsubscribe"})
	client.refusal(t, 3*time.Second, "", "INVALID_FRAME")
}

func TestABearerMayReadTheSocket(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	seedHubSettings(t, rig.database)
	minted := mintAgentToken(t, rig.handler)
	for name, token := range map[string]string{"the shared token": "agent-token", "a personal token": minted.Token} {
		t.Run(name, func(t *testing.T) {
			client := rig.dial(t, withBearer(token))
			client.send(t, deliverySubscribe("p", presetView(deliveryLiveTimeline, 7, "")))
			client.snapshot(t, 10*time.Second, "p")
		})
	}
}

// A bearer revoked while its socket is open loses the socket at the next heartbeat's resolution, as
// the document socket and the event stream do: the request's context ends, and the socket closes
// with 1008. The heartbeat is a second, so the client's pong, which gorilla's default handler
// writes, has a heartbeat to meet the pong deadline on a loaded runner: the socket must last until
// the revocation, and a pong timeout would end it first.
func TestARevokedBearerClosesItsSocketWithPolicyViolation(t *testing.T) {
	const heartbeat = time.Second
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: heartbeat}, nil)
	minted := mintAgentToken(t, rig.handler)
	client := rig.dial(t, withBearer(minted.Token))

	time.Sleep(3 * heartbeat)
	if !client.open() {
		t.Fatalf("the socket of a token that resolves ended: %v", client.err)
	}
	if response := dispatchRequest(t, rig.handler, http.MethodDelete, "/api/v1/me/agent-tokens/"+minted.ID, nil, "alice"); response.Code != http.StatusNoContent {
		t.Fatalf("revoke the token: status=%d body=%s", response.Code, response.Body.String())
	}
	if code, _ := client.closeCode(t, heartbeat+3*time.Second); code != gws.ClosePolicyViolation {
		t.Fatalf("the revoked token's socket closed with %d, want 1008", code)
	}
	rig.sockets(t, 0)
}

func TestAMalformedFrameIsAnErrorFrameNotAClose(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	client := rig.dial(t, asUser("alice"))
	for _, test := range []struct {
		name   string
		frame  string
		binary bool
		sub    string
		code   string
		names  string
	}{
		{name: "a subscribe without a view", frame: `{"type":"subscribe","sub":"a"}`, sub: "a", code: "INVALID_FRAME"},
		{name: "a subscribe without a sub", frame: `{"type":"subscribe"}`, code: "INVALID_FRAME"},
		{name: "a member no frame declares", frame: `{"type":"subscribe","sub":"b","view":{"kind":"pipeline"},"since":1}`, sub: "b", code: "INVALID_FRAME"},
		{name: "an unknown type", frame: `{"type":"resubscribe","sub":"c"}`, sub: "c", code: "INVALID_FRAME"},
		{name: "an unsubscribe without a sub", frame: `{"type":"unsubscribe"}`, code: "INVALID_FRAME"},
		{name: "text that is not JSON", frame: `subscribe`, code: "INVALID_FRAME"},
		{name: "two JSON values", frame: `{"type":"unsubscribe","sub":"d"} {}`, code: "INVALID_FRAME"},
		{name: "a binary message", frame: `{"type":"unsubscribe","sub":"e"}`, binary: true, code: "INVALID_FRAME"},
		{name: "a NUL in the sub", frame: `{"type":"unsubscribe","sub":"f\u0000"}`, code: "NUL_CHARACTER", names: "sub "},
		{name: "a NUL in the query", frame: `{"type":"subscribe","sub":"g","view":{"kind":"timeline","window":{"preset":7},"query":"repo=a%00b"}}`, sub: "g", code: "NUL_CHARACTER", names: "query parameter repo "},
	} {
		kind := gws.TextMessage
		if test.binary {
			kind = gws.BinaryMessage
		}
		if err := client.conn.WriteMessage(kind, []byte(test.frame)); err != nil {
			t.Fatalf("%s: send: %v", test.name, err)
		}
		refusal := client.refusal(t, 3*time.Second, test.sub, test.code)
		if !strings.HasPrefix(refusal.Message, test.names) || strings.ContainsRune(refusal.Message, 0) {
			t.Fatalf("%s: refusal %q, want one naming %q with no NUL in it", test.name, refusal.Message, test.names)
		}
	}

	// The socket is still open, and the hub answers its next subscribe.
	client.send(t, deliverySubscribe("h", presetView(deliveryLiveTimeline, 7, "")))
	client.refusal(t, 10*time.Second, "h", "DELIVERY_NOT_CONFIGURED")
}

func TestAFrameOverTheReadLimitClosesTheSocket(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: throughputWaitCap}, nil)
	// A write buffer larger than either message sends each as one frame, as Firefox sends a message.
	dialer := *gws.DefaultDialer
	dialer.WriteBufferSize = 10 << 20
	client := rig.open(t, &dialer, asUser("alice")).read()

	// A resume's revisions may run to megabytes: a frame under 8 MiB is read, and this one is answered
	// for its view, which names no kind. The answer comes only once all 7 MiB have arrived and been
	// parsed, so its wait is throughputWait, and so is the write's: it is one frame, which only the
	// server's pong deadline, twice the heartbeat, would otherwise end if the transfer stalled.
	revisions := strings.Repeat("0", 7<<20)
	subscribe := []byte(`{"type":"subscribe","sub":"r","view":{"kind":"bogus"},"resume":{"seq":"0","revs":{"items":{},"maps":{},"counts":{},"fields":{"f":"` + revisions + `"}}}}`)
	written := throughputWait(t)
	_ = client.conn.SetWriteDeadline(time.Now().Add(written))
	if err := client.conn.WriteMessage(gws.TextMessage, subscribe); err != nil {
		t.Fatalf("write the 7 MiB subscribe frame within throughputWait (%s): %v", written, err)
	}
	_ = client.conn.SetWriteDeadline(time.Time{})
	client.refusal(t, throughputWait(t), "r", "INVALID_FRAME")

	// One over it closes the socket with 1009. gorilla compares a frame's declared length with the
	// limit as it reads the frame's header, so Dispatch answers before reading any of the 9 MiB.
	oversized := []byte(`{"type":"unsubscribe","sub":"` + strings.Repeat("a", 9<<20) + `"}`)
	go func() { _ = client.conn.WriteMessage(gws.TextMessage, oversized) }()
	if code, _ := client.closeCode(t, 10*time.Second); code != gws.CloseMessageTooBig {
		t.Fatalf("a 9 MiB frame closed the socket with %d, want 1009", code)
	}
	rig.sockets(t, 0)
}

// Chrome on desktop sends a message over 64 KiB in frames of at most 131,000 bytes, the capacity of
// the data pipe its renderer fills for the network service (services/network/websocket.cc). The read
// limit counts a message's frames together: the frame that takes the message past 8 MiB closes the
// socket with 1009, though the message never finishes.
func TestAMessageOverTheReadLimitInChromesFramesClosesTheSocket(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: throughputWaitCap}, nil)
	const frame = 131000
	dialer := *gws.DefaultDialer
	dialer.WriteBufferSize = frame
	client := rig.open(t, &dialer, asUser("alice")).read()

	// 65 frames and a byte: the writer sends a full frame once the next byte needs its buffer, so
	// the byte sends the 65th frame, which takes the message past the limit. No last frame follows.
	writer, err := client.conn.NextWriter(gws.TextMessage)
	if err != nil {
		t.Fatalf("open a message: %v", err)
	}
	go func() { _, _ = writer.Write(bytes.Repeat([]byte("a"), 65*frame+1)) }()
	// Dispatch reads the 64 frames under the limit before the 65th's header, so the wait is
	// throughputWait.
	if code, _ := client.closeCode(t, throughputWait(t)); code != gws.CloseMessageTooBig {
		t.Fatalf("65 frames of 131,000 bytes closed the socket with %d, want 1009", code)
	}
	rig.sockets(t, 0)
}

func TestAWindowWiderThanTheMaximumIsAnErrorFrame(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	seedHubSettings(t, rig.database)
	client := rig.dial(t, asUser("alice"))
	to := time.Now().UTC().Truncate(time.Second)

	client.send(t, deliverySubscribe("wide", timelineRange(to.Add(-maxDeliveryWindow-time.Second), to, "")))
	if refusal := client.refusal(t, 3*time.Second, "wide", "INVALID_QUERY"); refusal.Message != "from..to must not span more than 84 days" {
		t.Fatalf("refusal %q, want the GET route's", refusal.Message)
	}

	// The widest window the GET route answers is answered too, on the same socket.
	client.send(t, deliverySubscribe("widest", timelineRange(to.Add(-maxDeliveryWindow), to, "")))
	client.snapshot(t, 10*time.Second, "widest")
}

func TestASlowReaderIsClosedNotBuffered(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, func(listener net.Listener) net.Listener { return smallSendBuffers{listener} })
	client := rig.open(t, smallReadDialer(), asUser("alice"))

	// Every frame is answered with an error frame echoing its 4 KiB sub, and the page reads none of
	// them: the socket's writer blocks, its queue fills, and the next frame overflows it.
	const sent = 300
	sub := strings.Repeat("s", 4096)
	for n := range sent {
		client.sendText(t, []byte(`{"type":"resubscribe","sub":"`+sub+strconv.Itoa(n)+`"}`))
	}
	socket := rig.sockets(t, 1)[0]
	deadline := time.Now().Add(5 * time.Second)
	for !socket.overflowed() {
		if time.Now().After(deadline) {
			t.Fatalf("the socket's %d-frame queue never overflowed", liveSocketQueue)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Once the page reads again it is sent what the connection already carried, then 1013, and not
	// the answers queued behind the overflow.
	code, messages := client.read().closeCode(t, 15*time.Second)
	if code != gws.CloseTryAgainLater {
		t.Fatalf("the slow page's socket closed with %d, want 1013", code)
	}
	if messages >= sent {
		t.Fatalf("the slow page was sent %d of %d answers: the socket buffered them", messages, sent)
	}
	rig.sockets(t, 0)
}

func TestShutdownClosesEverySocketWithServiceRestart(t *testing.T) {
	events := newFakeLiveKind(0, 0)
	rig := newLiveSocketRig(t, testServerOptions{liveKinds: map[string]liveKind{liveEventsView: events.kind(true)}}, nil)
	seedHubSettings(t, rig.database)
	person := rig.dial(t, asUser("alice"))
	agent := rig.dial(t, withBearer("agent-token"))
	person.send(t, deliverySubscribe("t", presetView(deliveryLiveTimeline, 7, "")))
	person.snapshot(t, 10*time.Second, "t")
	agent.send(t, liveSubscribe("e", eventsView(), ""))
	running := events.next(t, 5*time.Second, "the agent's events subscription")

	rig.stop()
	for name, client := range map[string]*socketClient{"the person's": person, "the agent's": agent} {
		if code, _ := client.closeCode(t, 5*time.Second); code != gws.CloseServiceRestart {
			t.Errorf("%s socket closed with %d, want 1012", name, code)
		}
	}
	rig.served.liveSockets.wait()
	rig.served.deliveryHub.wait()
	if held := hubConnCount(rig.served.deliveryHub); held != 0 {
		t.Fatalf("the hub holds subscriptions of %d closed sockets", held)
	}
	// The socket's wait covers the subscriptions it ran: the events subscription stopped once, and
	// its goroutine had returned before the wait did.
	select {
	case <-running.done:
	default:
		t.Fatal("the socket's wait returned with its events subscription still running")
	}
	if stops := running.stops.Load(); stops != 1 {
		t.Fatalf("the events subscription was stopped %d times, want once", stops)
	}
}

func TestASocketPastItsAgeClosesWithMaxAge(t *testing.T) {
	const age = 200 * time.Millisecond
	rig := newLiveSocketRig(t, testServerOptions{liveSocketAge: func() time.Duration { return age }}, nil)
	opened := time.Now()
	client := rig.dial(t, asUser("alice"))
	closed := client.closeError(t, age+3*time.Second)
	if closed.Code != contracts.LiveSocketMaxAgeClose || closed.Text != "max age" {
		t.Fatalf("the socket past its age closed with %d %q, want %d \"max age\", which a page answers by reopening at once", closed.Code, closed.Text, contracts.LiveSocketMaxAgeClose)
	}
	if lived := client.endedAt.Sub(opened); lived < age {
		t.Fatalf("the socket closed %s after it opened, before its age (%s)", lived, age)
	}
	rig.sockets(t, 0)
}

func TestEachSocketsAgeFallsInItsWindow(t *testing.T) {
	served := &server{}
	drawn := map[time.Duration]struct{}{}
	for range 1000 {
		age := served.liveSocketAge()
		if age < contracts.LiveSocketMinAge || age >= contracts.LiveSocketMaxAge {
			t.Fatalf("a socket's age %s is outside [%s, %s)", age, contracts.LiveSocketMinAge, contracts.LiveSocketMaxAge)
		}
		drawn[age] = struct{}{}
	}
	// Drawn at random, so the sockets a deploy opened together do not all close together.
	if len(drawn) < 990 {
		t.Fatalf("1,000 sockets drew %d distinct ages, want them drawn at random", len(drawn))
	}
	fixed := &server{deps: Deps{LiveSocketAge: func() time.Duration { return time.Minute }}}
	if age := fixed.liveSocketAge(); age != time.Minute {
		t.Fatalf("with Deps.LiveSocketAge set, a socket's age is %s, want its %s", age, time.Minute)
	}
}

// A test hook closes every open socket, a person's and a bearer's, with its code, and the page's
// reconnect is served.
func requireTheHookClosesEverySocket(t *testing.T, hook string, code int, reason string) {
	t.Helper()
	rig := newLiveSocketRig(t, testServerOptions{testHooks: true}, nil)
	person := rig.dial(t, asUser("alice"))
	agent := rig.dial(t, withBearer("agent-token"))
	rig.sockets(t, 2)

	if response := dispatchRequest(t, rig.handler, http.MethodPost, hook, nil, "alice"); response.Code != http.StatusOK {
		t.Fatalf("POST %s: status=%d body=%s", hook, response.Code, response.Body.String())
	}
	for name, client := range map[string]*socketClient{"the person's": person, "the agent's": agent} {
		if closed := client.closeError(t, 5*time.Second); closed.Code != code || closed.Text != reason {
			t.Errorf("%s socket closed with %d %q, want %d %q", name, closed.Code, closed.Text, code, reason)
		}
	}

	again := rig.dial(t, asUser("alice"))
	again.send(t, LiveUnsubscribe{Type: "unsubscribe"})
	again.refusal(t, 3*time.Second, "", "INVALID_FRAME")
}

func TestTheRotateHookClosesEverySocketWithMaxAge(t *testing.T) {
	requireTheHookClosesEverySocket(t, "/api/v1/live/_test/rotate", contracts.LiveSocketMaxAgeClose, "max age")
}

func TestTheDisconnectHookClosesEverySocketWithServiceRestart(t *testing.T) {
	requireTheHookClosesEverySocket(t, "/api/v1/live/_test/disconnect", gws.CloseServiceRestart, "")
}

func TestTheLiveTestHooksAreMountedOnlyUnderTestHooks(t *testing.T) {
	t.Run("without test hooks", func(t *testing.T) {
		rig := newLiveSocketRig(t, testServerOptions{}, nil)
		client := rig.dial(t, asUser("alice"))
		for _, hook := range []string{"/api/v1/live/_test/disconnect", "/api/v1/live/_test/rotate"} {
			if response := dispatchRequest(t, rig.handler, http.MethodPost, hook, nil, "alice"); response.Code != http.StatusNotFound {
				t.Fatalf("POST %s without DISPATCH_TEST_HOOKS: status=%d, want 404", hook, response.Code)
			}
		}
		client.send(t, LiveUnsubscribe{Type: "unsubscribe"})
		client.refusal(t, 3*time.Second, "", "INVALID_FRAME")
	})
	t.Run("the delivery socket's hook is gone", func(t *testing.T) {
		rig := newLiveSocketRig(t, testServerOptions{testHooks: true}, nil)
		if response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/delivery/_test/disconnect", nil, "alice"); response.Code != http.StatusNotFound {
			t.Fatalf("POST /api/v1/delivery/_test/disconnect: status=%d, want 404", response.Code)
		}
	})
}

// An events subscription that keeps the stream queue full while the page reads nothing neither
// holds back the hub's frames nor overflows the hub's queue: the hub's queue fills beside it, and
// once the page reads again every hub frame is written ahead of the stream frames still queued.
func TestHubFramesAreWrittenAheadOfAFullStreamQueue(t *testing.T) {
	busy := newFakeLiveKind(-1, 512)
	rig := newLiveSocketRig(t, testServerOptions{liveKinds: map[string]liveKind{liveEventsView: busy.kind(true)}},
		func(listener net.Listener) net.Listener { return smallSendBuffers{listener} })
	client := rig.open(t, smallReadDialer(), asUser("alice"))
	client.send(t, liveSubscribe("busy", eventsView(), ""))
	running := busy.next(t, 5*time.Second, "the busy events subscription")
	socket := rig.sockets(t, 1)[0]
	waitStalled(t, socket, running)

	// Eight delivery views, each told delivery is not configured: the hub's answers queue beside
	// the full stream queue, and the socket stays open.
	const answers = 8
	subs := make([]string, answers)
	for n := range answers {
		subs[n] = "d" + strconv.Itoa(n)
		client.send(t, deliverySubscribe(subs[n], livePipelineView(nil)))
	}
	pollUntil(t, 10*time.Second, "the hub answers every delivery view", func() bool { return len(socket.queue) == answers })
	if socket.overflowed() {
		t.Fatal("the hub's queue overflowed beside a full stream queue")
	}

	// The page reads again: the stream frames the connection already carried come first, then all
	// eight hub frames together, then the stream again.
	client.read()
	deadline := time.Now().Add(10 * time.Second)
	var first LiveError
	for {
		frame := decodeSocketFrame(t, client.takeBy(t, deadline, 10*time.Second, "the first hub frame"))
		if refusal, ok := frame.(LiveError); ok {
			first = refusal
			break
		}
	}
	if first.Code != "DELIVERY_NOT_CONFIGURED" {
		t.Fatalf("the first hub frame = %+v, want DELIVERY_NOT_CONFIGURED", first)
	}
	rest := slices.DeleteFunc(slices.Clone(subs), func(sub string) bool { return sub == first.Sub })
	if len(rest) != answers-1 {
		t.Fatalf("the first hub frame refuses %q, not one of the delivery views %v", first.Sub, subs)
	}
	client.refusals(t, 3*time.Second, "DELIVERY_NOT_CONFIGURED", rest...)
	client.event(t, 5*time.Second, "busy")
	if !client.open() {
		t.Fatalf("the socket ended: %v", client.err)
	}
}

// A page that answers each ping keeps its socket, idle, across many heartbeats. The heartbeat is a
// second, so the page's first pong has a heartbeat to arrive before the pong deadline, two
// heartbeats after the socket opened, and a loaded runner that delays it most of a heartbeat still
// meets it. A page that answers no ping, which a short heartbeat ends quickly, is the next test's.
func TestPingsKeepAnIdleSocketOpen(t *testing.T) {
	const heartbeat = time.Second
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: heartbeat}, nil)
	answering := rig.open(t, gws.DefaultDialer, asUser("alice"))
	var pings atomic.Int32
	answering.conn.SetPingHandler(func(data string) error {
		pings.Add(1)
		return answering.conn.WriteControl(gws.PongMessage, []byte(data), time.Now().Add(time.Second))
	})
	answering.read()

	// Four heartbeats is twice the pong deadline, so the page still holds its socket only through
	// its pongs.
	time.Sleep(4 * heartbeat)
	if !answering.open() {
		t.Fatalf("an idle page that answers every ping lost its socket: %v", answering.err)
	}
	if got := pings.Load(); got < 3 {
		t.Fatalf("the idle page was pinged %d times in four heartbeats, want one each", got)
	}
	rig.sockets(t, 1)
}

// A page that answers no ping is closed once two heartbeats pass without a pong. It has gone or
// stopped reading, so the socket sends it no close frame: the connection ends, which gorilla and a
// browser report as 1006, and the page reconnects with backoff, as after every close but 1012
// (packages/dispatch/web/src/api/delivery-live.ts, lost). 1008 would say its caller no longer
// resolves.
func TestAPageThatAnswersNoPingLosesItsSocketWithNoCloseFrame(t *testing.T) {
	const heartbeat = 200 * time.Millisecond
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: heartbeat}, nil)
	// Where a page that answers no ping ends with the wrong close, it does so through a race in the
	// writer, so one silent page would show a regression only in some runs. Eight pages, each racing
	// on its own, make it fail nearly every run: at an even race a run misses with odds of 1 in 256.
	const silentPages = 8
	silent := make([]*socketClient, silentPages)
	opened := make([]time.Time, silentPages)
	for i := range silent {
		silent[i] = rig.open(t, gws.DefaultDialer, asUser("bob"))
		silent[i].conn.SetPingHandler(func(string) error { return nil })
		opened[i] = time.Now()
		silent[i].read()
	}

	for i, page := range silent {
		select {
		case <-page.ended:
			if waited := page.endedAt.Sub(opened[i]); waited < heartbeat || waited > 2*heartbeat+time.Second {
				t.Errorf("silent page %d's socket ended %s after it opened, want about two heartbeats (%s)", i, waited, 2*heartbeat)
			}
			var closed *gws.CloseError
			if !errors.As(page.err, &closed) || closed.Code != gws.CloseAbnormalClosure {
				t.Errorf("silent page %d's socket ended with %v, want an end with no close frame (1006)", i, page.err)
			}
		case <-time.After(2*heartbeat + 3*time.Second):
			t.Errorf("silent page %d, which answers no ping, kept its socket", i)
		}
	}
	rig.sockets(t, 0)
}

func TestAnIdleDeliverySocketIsSentAHeartbeat(t *testing.T) {
	const heartbeat = 300 * time.Millisecond
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: heartbeat}, nil)
	client := rig.dial(t, asUser("alice"))
	if data := client.take(t, heartbeat+time.Second, "the heartbeat"); string(data) != `{"type":"heartbeat"}` {
		t.Fatalf("an idle socket's first frame = %s, want {\"type\":\"heartbeat\"} alone", data)
	}
}
