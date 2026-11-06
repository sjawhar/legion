// packages/envoy/internal/broker/airlockproxy/upgrade_test.go
//
// A WebSocket upgrade on a bound host: the swap reaches the upgrade request's own headers, the
// Upgrade and Connection headers survive it, and once the service answers 101 the two
// connections are joined and bytes pass both ways until either side closes - with the idle
// limit on the joined connection, which is the command's own connection and so the one the
// proxy already bounds.
package airlockproxy

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/airlock/airlocktest"
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
