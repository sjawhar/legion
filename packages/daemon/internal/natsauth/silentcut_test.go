package natsauth_test

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/natsauth"
)

// silentServer starts a NATS server that admits one client and answers its handshake PING, and
// then answers nothing at all with the socket open: a hub whose packets stop, with no close and no
// reset. It returns the server's URL.
func silentServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.WriteString(c, `INFO {"server_id":"fake","version":"2.10.0","proto":1,"max_payload":1048576}`+"\r\n")
		lines := bufio.NewReader(c)
		for {
			line, err := lines.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "PING") {
				break
			}
		}
		_, _ = io.WriteString(c, "PONG\r\n")
		// Every later line, the client's pings included, is read and left unanswered.
		_, _ = io.Copy(io.Discard, c)
	}()
	return "nats://" + ln.Addr().String()
}

// A hub that stops answering without closing the connection is noticed within seconds, because
// Connect itself pings every second and takes the connection for lost at the third unanswered ping
// (detectSilentCuts). The daemon opens one NATS connection, and Connect is the one place it opens
// it, so the pings belong to Connect rather than to the options its caller passes: this test
// connects as the daemon's workflow does and so covers the production path, not the wiring. With
// nats.go's own defaults the daemon reads the connection as connected for minutes into such a cut,
// and reads no event in them.
func TestAHubThatStopsAnsweringIsNoticedWithinSeconds(t *testing.T) {
	var out lockedBuffer
	conn, err := natsauth.Connect([]string{silentServer(t)}, "",
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
