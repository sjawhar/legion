package bus

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// denyThenDrop serves a NATS client: it answers the handshake, then answers the first publish's
// flush with the server's permissions violation for subject and the PONG, and closes the
// connection. That is what a server restarted just behind a denied publish looks like to the
// client. With reconnectable false it closes its listener too, so the client stays reconnecting;
// with it true it serves the client's next connection, so the reconnect completes.
func denyThenDrop(t *testing.T, subject string, reconnectable bool) string {
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
		serveFake(conn, subject)
		if !reconnectable {
			listener.Close()
			return
		}
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveFake(conn, "")
		}
	}()
	return "nats://" + listener.Addr().String()
}

// serveFake speaks just enough of the NATS protocol to one client: INFO, then a PONG for every
// PING. When deny names a subject, the first publish's flush is answered with the permissions
// violation for it and the PONG, and the connection is closed.
func serveFake(conn net.Conn, deny string) {
	defer conn.Close()
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
		case strings.HasPrefix(line, "PING") && published && deny != "":
			fmt.Fprintf(conn, "-ERR 'Permissions Violation for Publish to %q'\r\nPONG\r\n", deny)
			return
		case strings.HasPrefix(line, "PING"):
			fmt.Fprint(conn, "PONG\r\n")
		}
	}
}

// A denied publish whose connection drops right behind the flush's PONG is not confirmed: the
// reconnect clears the recorded violation before it has counted itself, so the connection's last
// error and reconnect count read exactly as they did before the publish.
func TestConfirmPublishedRefusesADenialAReconnectCleared(t *testing.T) {
	subject := "notifications.role.reviewer"
	var once sync.Once
	disconnected := make(chan struct{})
	conn, err := nats.Connect(denyThenDrop(t, subject, false),
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
	if err := confirmPublished(conn, subject, before); !errors.Is(err, errPublishUnconfirmed) {
		t.Fatalf("a denied publish read as %v, want unconfirmed: last error %v, reconnecting %t",
			err, conn.LastError(), conn.IsReconnecting())
	}
}

// A denied publish whose connection drops behind the flush's PONG and reconnects before the check
// is not confirmed either: the connection reads as connected again with no error, and only its
// reconnect count shows the violation may have gone with the old connection.
func TestConfirmPublishedRefusesADenialACompletedReconnectCleared(t *testing.T) {
	subject := "notifications.role.reviewer"
	var once sync.Once
	reconnected := make(chan struct{})
	conn, err := nats.Connect(denyThenDrop(t, subject, true),
		nats.ReconnectWait(10*time.Millisecond),
		nats.MaxReconnects(-1),
		nats.ReconnectHandler(func(*nats.Conn) { once.Do(func() { close(reconnected) }) }),
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
	// nats.go queues the reconnect callback once the new connection is marked connected.
	select {
	case <-reconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("the client never reconnected")
	}
	if err := confirmPublished(conn, subject, before); !errors.Is(err, errPublishUnconfirmed) {
		t.Fatalf("a denied publish read as %v, want unconfirmed: before %+v, after %+v", err, before, observe(conn))
	}
}

// sliceError is an error whose value cannot be compared: its interface field holds a slice, though
// its type reads as comparable.
type sliceError struct{ cause any }

func (sliceError) Error() string { return "slice error" }

// sameError never panics on a value that cannot be compared, and counts it as a change.
func TestSameErrorTreatsAValueThatCannotBeComparedAsChanged(t *testing.T) {
	if sameError(sliceError{[]int{1}}, sliceError{[]int{1}}) {
		t.Fatal("two incomparable error values read as the same error")
	}
	shared := errors.New("shared")
	if !sameError(shared, shared) {
		t.Fatal("one error value read as a change")
	}
}
