package natsauth_test

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/natsauth"
)

// A hub that stops answering without closing the connection is noticed within seconds, because
// Connect itself pings every second and takes the connection for lost at the third unanswered ping
// (detectSilentCuts). The daemon opens one NATS connection, and Connect is the one place it opens
// it, so the pings belong to Connect rather than to the options its caller passes: this test
// connects as the daemon's workflow does and so covers the production path, not the wiring. With
// nats.go's own defaults the daemon reads the connection as connected for minutes into such a cut,
// and reads no event in them.
//
// fakeServer with nothing to write after the handshake is that hub: it answers the handshake PING
// and then reads every later line, the client's pings included, and answers none, with the socket
// open and no reset.
func TestAHubThatStopsAnsweringIsNoticedWithinSeconds(t *testing.T) {
	var out lockedBuffer
	conn, err := natsauth.Connect([]string{fakeServer(t, "")}, "",
		natsauth.LogEvents(slog.New(slog.NewTextHandler(&out, nil))), natsauth.ReconnectForever())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	// The server answered the handshake and nothing after it, so it is quiet from here.
	quiet := time.Now()
	const within = 5 * time.Second
	for {
		connected, logged := conn.IsConnected(), strings.Contains(out.String(), "NATS connection lost")
		if !connected && logged {
			break
		}
		if waited := time.Since(quiet); waited > within {
			t.Fatalf("the connection was not reported lost within %s of the server going quiet: IsConnected = %t, the warning logged = %t; log: %s",
				waited, connected, logged, out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("the cut was noticed %s after the server went quiet", time.Since(quiet))
}
