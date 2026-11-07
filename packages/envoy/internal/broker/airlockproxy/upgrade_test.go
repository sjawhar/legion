// packages/envoy/internal/broker/airlockproxy/upgrade_test.go
//
// A WebSocket upgrade on a bound host: the swap reaches the upgrade request's own headers, the
// Upgrade and Connection headers survive it, and once the service answers 101 the two
// connections are joined and bytes pass both ways until either side closes. Two things end such
// a connection: the idle limit, which is the command's own connection's and so one the proxy
// already bounds, and the grant behind it, which no request on that connection reads again - so
// the proxy reads it for the connection itself and closes the connection when the grant stops
// carrying it.
package airlockproxy

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/airlock/airlocktest"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// upgradePath is the path these rows upgrade at: the stand-in service answers an upgrade
// there and its own body everywhere else, as S does in the broker's local stack.
const upgradePath = "/v1/socket"

// upgradeStandIn makes the stand-in service answer 101 at that path and then speak the little
// line protocol these rows pass frames with: one line in, one line out. It is airlocktest's
// own upgrade stand, the one cmd/dev-airlock-stands installs on S, so these rows and a check
// script drive the one answer.
func upgradeStandIn(t *testing.T, r *rig) {
	t.Helper()
	r.answerWith(airlocktest.NewUpgradeStand(airlocktest.UpgradeOptions{Path: upgradePath}).Answer)
}

// upgradeThrough opens a connection to the bound host through the proxy, sends the upgrade
// request on it, and answers the 101 and the connection the frames pass on.
func upgradeThrough(t *testing.T, r *rig) (*http.Response, net.Conn, *bufio.Reader) {
	t.Helper()
	answer, conn, _ := r.connectRaw(net.JoinHostPort(boundHost, "443"), basic(workerPlaceholder))
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("the CONNECT answered %d; want 200", answer.StatusCode)
	}
	// The proxy's own certificate, which this command trusts as a released one does.
	decrypted := tls.Client(conn, &tls.Config{RootCAs: r.trust, ServerName: boundHost})
	if err := decrypted.Handshake(); err != nil {
		t.Fatalf("the TLS handshake with the proxy: %v", err)
	}
	t.Cleanup(func() { decrypted.Close() })
	request := "GET " + upgradePath + " HTTP/1.1\r\n" +
		"Host: " + boundHost + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Authorization: Bearer " + workerPlaceholder + "\r\n\r\n"
	if _, err := decrypted.Write([]byte(request)); err != nil {
		t.Fatalf("write the upgrade request: %v", err)
	}
	reader := bufio.NewReader(decrypted)
	upgraded, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read the answer to the upgrade: %v", err)
	}
	return upgraded, decrypted, reader
}

// TestAWebSocketUpgradeIsSwappedAndThenJoined: an upgrade is a request like any other until the
// service answers 101 - so the value reaches the service in the header the entry names, and the
// Upgrade and Connection headers survive the swap - and after the 101 the two connections are
// one and the frames cross it both ways.
func TestAWebSocketUpgradeIsSwappedAndThenJoined(t *testing.T) {
	r := newRig(t, Options{})
	upgradeStandIn(t, r)

	upgraded, conn, reader := upgradeThrough(t, r)
	if upgraded.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("the upgrade answered %d; want 101", upgraded.StatusCode)
	}
	if got := upgraded.Header.Get("Upgrade"); got != "websocket" {
		t.Errorf("the answer's Upgrade = %q; want websocket", got)
	}

	seen := r.requests()
	if len(seen) != 1 {
		t.Fatalf("the service saw %d requests; want the upgrade", len(seen))
	}
	if got, want := seen[0].Header.Get("Authorization"), "Bearer "+workerValue; got != want {
		t.Fatalf("the service saw Authorization %q; want %q: the swap reaches an upgrade's own headers", got, want)
	}
	if got := seen[0].Header.Get("Upgrade"); got != "websocket" {
		t.Errorf("the service saw Upgrade %q; want websocket: the header has to survive the rewrite", got)
	}
	if got := seen[0].Header.Get("Connection"); !strings.Contains(strings.ToLower(got), "upgrade") {
		t.Errorf("the service saw Connection %q; want it to name Upgrade", got)
	}

	if _, err := conn.Write([]byte("a frame from the command\n")); err != nil {
		t.Fatalf("write a frame to the joined connection: %v", err)
	}
	frame, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read the service's frame: %v", err)
	}
	if want := "the service read a frame from the command\n"; frame != want {
		t.Fatalf("the service's frame is %q; want %q: the bytes pass both ways", frame, want)
	}
}

// TestTheIdleLimitEndsAQuietUpgradedConnection: the joined connection is the command's own, so
// the limit that ends a quiet tunnel ends a quiet upgrade too - a WebSocket nobody is speaking
// on is state the broker holds for nobody.
func TestTheIdleLimitEndsAQuietUpgradedConnection(t *testing.T) {
	const idle = 400 * time.Millisecond
	r := newRig(t, Options{Idle: idle})
	upgradeStandIn(t, r)

	began := time.Now()
	upgraded, conn, reader := upgradeThrough(t, r)
	if upgraded.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("the upgrade answered %d; want 101", upgraded.StatusCode)
	}
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set the read deadline: %v", err)
	}
	// The read ends when the proxy closes the joined connection, which is the end of the
	// stream and no error of its own; a connection the limit missed would sit here until the
	// deadline above and answer a timeout instead.
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("the joined connection was not closed by the idle limit: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("the joined connection carried %q after the upgrade; want nothing", rest)
	}
	if quiet := time.Since(began); quiet < idle {
		t.Fatalf("the joined connection closed after %v; want it to outlive the idle limit %v", quiet, idle)
	}
}

// watchFast is Options.Watch for the rows below: short enough that a row does not wait out the
// real interval, long enough that a handful of them are a handful of reads and not a busy loop.
const watchFast = 20 * time.Millisecond

// frameStand is the stand-in service the rows below pass frames to. airlocktest's own stand
// answers one frame and lets its connection go, which is the join ending by itself; a row about
// a grant that ends under an open join needs a service that keeps reading, and it needs to read
// back which frames reached that service, since the frame a closed connection did not carry is
// what says it was closed.
type frameStand struct {
	mu     sync.Mutex
	frames []string
}

// Answer is airlocktest.Answer: 101 at upgradePath, and then one line back for each line in, as
// airlocktest's own stand answers its one.
func (f *frameStand) Answer(w http.ResponseWriter, _ *http.Request, seen airlocktest.Request) bool {
	if seen.Path != upgradePath {
		return false
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "the stand-in service cannot be taken over", http.StatusInternalServerError)
		return true
	}
	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, "the stand-in service could not hijack: "+err.Error(), http.StatusInternalServerError)
		return true
	}
	defer conn.Close()
	if _, err := buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		return true
	}
	if err := buffered.Flush(); err != nil {
		return true
	}
	for {
		line, err := buffered.ReadString('\n')
		if err != nil {
			return true
		}
		f.mu.Lock()
		f.frames = append(f.frames, strings.TrimSpace(line))
		f.mu.Unlock()
		if _, err := buffered.WriteString("the service read " + strings.TrimSpace(line) + "\n"); err != nil {
			return true
		}
		if err := buffered.Flush(); err != nil {
			return true
		}
	}
}

// read is every frame the service has read, as a copy of its own: a row reads them while the
// service is still being sent frames.
func (f *frameStand) read() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.frames)
}

// joinedThrough upgrades through the proxy to a frameStand of its own and answers that stand
// and the joined connection, with the 101 already read.
func joinedThrough(t *testing.T, r *rig) (*frameStand, net.Conn, *bufio.Reader) {
	t.Helper()
	stand := &frameStand{}
	r.answerWith(stand.Answer)
	upgraded, conn, reader := upgradeThrough(t, r)
	if upgraded.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("the upgrade answered %d; want 101", upgraded.StatusCode)
	}
	return stand, conn, reader
}

// frame writes one frame on a joined connection and reads the service's answer to it, which is
// what says the join is carrying bytes.
func frame(t *testing.T, conn net.Conn, reader *bufio.Reader, line string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set the read deadline: %v", err)
	}
	if _, err := conn.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("write %q to the joined connection: %v", line, err)
	}
	answer, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read the service's answer to %q: %v", line, err)
	}
	if want := "the service read " + line + "\n"; answer != want {
		t.Fatalf("the service answered %q; want %q", answer, want)
	}
}

// waitClosed reads the joined connection until it ends, which is what the proxy's close looks
// like from the command's side. A connection the close missed sits here until the deadline and
// answers a timeout instead, which fails the row.
func waitClosed(t *testing.T, conn net.Conn, reader *bufio.Reader) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set the read deadline: %v", err)
	}
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("the joined connection was not closed: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("the joined connection carried %q before it closed; want nothing", rest)
	}
}

// TestAJoinedConnectionIsClosedWhenItsGrantStopsCarryingIt: a joined connection is the one
// connection no later request is read on, so the read that stops every other call of a grant
// that has ended never runs for it, and the idle limit bounds nothing on a socket a service
// pings. The grant is read for the connection itself instead, and a grant that no longer
// carries it closes it: the frames stop, and a frame written after that reaches no service.
func TestAJoinedConnectionIsClosedWhenItsGrantStopsCarryingIt(t *testing.T) {
	// The grant as it reads once its operator has withheld the name this upgrade was decided
	// for, holding one other name so that the read answers a grant rather than refusing.
	withheld := testConnection()
	withheld.Names = map[string]requests.AirlockGrantName{
		"OTHER_TOKEN": {Slots: map[string]string{requests.SlotValue: requests.PlaceholderPrefix + "otherotherotherotherotherother01"}},
	}
	for _, row := range []struct {
		name   string
		end    func(f *fakeGrants)
		reason string
	}{
		{"the grant has ended", func(f *fakeGrants) {
			f.refuseConnection(workerPlaceholder, requests.ErrGrantNotLive)
		}, requests.ErrGrantNotLive.Error()},
		{"every name of the grant is withheld", func(f *fakeGrants) {
			f.refuseConnection(workerPlaceholder, requests.ErrAirlockNoNames)
		}, requests.ErrAirlockNoNames.Error()},
		{"the name this upgrade was decided for is withheld", func(f *fakeGrants) {
			f.setConnection(workerPlaceholder, withheld)
		}, workerName + " is no longer a name of this grant"},
	} {
		t.Run(row.name, func(t *testing.T) {
			const carried = "a frame while the grant carries this connection"
			r := newRig(t, Options{Watch: watchFast})
			stand, conn, reader := joinedThrough(t, r)
			frame(t, conn, reader, carried)

			row.end(r.grants)
			waitClosed(t, conn, reader)
			// The frame the round-1 probe sent after the refusal, which the service read
			// then: a closed connection carries it nowhere.
			_, _ = conn.Write([]byte("a frame after the grant stopped carrying it\n"))
			time.Sleep(5 * watchFast)
			if frames := stand.read(); !slices.Equal(frames, []string{carried}) {
				t.Fatalf("the service read %q; want the one frame from before the grant stopped carrying the connection", frames)
			}

			lines := r.logs.joins()
			if len(lines) != 1 {
				t.Fatalf("the proxy wrote %d lines for the close; want the one", len(lines))
			}
			if got := lines[0].fields["reason"]; got != row.reason {
				t.Errorf("the close logged the reason %v; want %q", got, row.reason)
			}
			if got := lines[0].fields["grant"]; got != testGrantID {
				t.Errorf("the close logged the grant %v; want %s", got, testGrantID)
			}
			for key, value := range lines[0].fields {
				if text, ok := value.(string); ok && (strings.Contains(text, workerPlaceholder) || strings.Contains(text, workerValue)) {
					t.Errorf("the close logged %s = %q; want neither the placeholder nor the value", key, text)
				}
			}
		})
	}
}

// TestAJoinedConnectionIsClosedAtItsGrantsExpiry: when a grant ends is a time the broker already
// holds, so a joined connection is closed at it without reading anything.
func TestAJoinedConnectionIsClosedAtItsGrantsExpiry(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)}
	expires := clock.read().Add(time.Hour)
	bounded := testConnection()
	bounded.Expires = &expires
	r := newRig(t, Options{Now: clock.read, Watch: watchFast})
	r.grants.setConnection(workerPlaceholder, bounded)
	stand, conn, reader := joinedThrough(t, r)
	frame(t, conn, reader, "a frame before the grant expires")

	clock.set(expires)
	waitClosed(t, conn, reader)
	if frames := stand.read(); len(frames) != 1 {
		t.Fatalf("the service read %q; want the one frame from before the expiry", frames)
	}
}

// TestAGrantReadThatFailedLeavesAJoinedConnectionAlone: a read that did not happen says nothing
// about the grant, so the connection stays open and the error goes to the log. A store down for
// a minute cutting every WebSocket the airlock carries would be an outage of its own.
func TestAGrantReadThatFailedLeavesAJoinedConnectionAlone(t *testing.T) {
	r := newRig(t, Options{Watch: watchFast})
	stand, conn, reader := joinedThrough(t, r)
	frame(t, conn, reader, "a frame while the store answers")

	before := r.grants.connectionReads()
	r.grants.refuseConnection(workerPlaceholder, databaseDown)
	// Three reads that failed, so that what this row reads back is what the watcher did with
	// them rather than a watcher that has not run yet.
	deadline := time.Now().Add(10 * time.Second)
	for r.grants.connectionReads() < before+3 {
		if time.Now().After(deadline) {
			t.Fatalf("the grant was read %d times in ten seconds; want three more than the %d before the store went down", r.grants.connectionReads(), before)
		}
		time.Sleep(watchFast)
	}
	frame(t, conn, reader, "a frame while the store is down")

	if frames := stand.read(); len(frames) != 2 {
		t.Fatalf("the service read %q; want both frames: a read that failed closes no connection", frames)
	}
	if lines := r.logs.joins(); len(lines) != 0 {
		t.Fatalf("the proxy closed the joined connection and logged %d lines for it; want none", len(lines))
	}
}
