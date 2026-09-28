package bus

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// denyThenDrop serves one NATS client: it answers the handshake, then answers the first publish's
// flush with the server's permissions violation for subject and the PONG, and closes the
// connection and its listener, so the client starts reconnecting and cannot finish. That is what a
// server restarted just behind a denied publish looks like to the client.
func denyThenDrop(t *testing.T, subject string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		defer listener.Close()
		fmt.Fprintf(conn, "INFO {\"server_id\":\"fake\",\"version\":\"2.10.29\",\"proto\":1,\"headers\":true,\"max_payload\":1048576}\r\n")
		lines := bufio.NewReader(conn)
		published := false
		for {
			line, err := lines.ReadString('\n')
			if err != nil {
				return
			}
			switch {
			case strings.HasPrefix(line, "PUB "):
				if _, err := lines.ReadString('\n'); err != nil {
					return
				}
				published = true
			case strings.HasPrefix(line, "PING") && !published:
				fmt.Fprint(conn, "PONG\r\n")
			case strings.HasPrefix(line, "PING"):
				fmt.Fprintf(conn, "-ERR 'Permissions Violation for Publish to %q'\r\nPONG\r\n", subject)
				return
			}
		}
	}()
	return "nats://" + listener.Addr().String()
}

// A denied publish whose connection drops right behind the flush's PONG is not confirmed: the
// reconnect clears the recorded violation before it has counted itself, so the connection's last
// error and reconnect count read exactly as they did before the publish.
func TestConfirmPublishedRefusesADenialAReconnectCleared(t *testing.T) {
	subject := "notifications.role.reviewer"
	var once sync.Once
	disconnected := make(chan struct{})
	conn, err := nats.Connect(denyThenDrop(t, subject),
		nats.ReconnectWait(time.Hour),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(*nats.Conn, error) { once.Do(func() { close(disconnected) }) }),
	)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	before := observe(conn)
	if err := conn.Publish(subject, []byte("{}")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := conn.FlushTimeout(5 * time.Second); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// nats.go queues the disconnect callback after the reconnect has cleared the last error
	// (doReconnect), so once it runs the violation is gone and the reconnect has not counted.
	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("the client never saw the server drop the connection")
	}
	if err := confirmPublished(conn, subject, before); err == nil {
		t.Fatalf("a denied publish read as accepted: last error %v, reconnecting %t", conn.LastError(), conn.IsReconnecting())
	}
}
