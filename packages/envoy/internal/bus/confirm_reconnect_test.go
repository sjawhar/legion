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
	"github.com/sjawhar/envoy/internal/contracts"
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

// answeringServer serves one NATS client and stays up: it answers every flush that follows a
// publish with the permissions violation for deny, when deny is not empty, and then the PONG, and,
// when receipt is true, then sends an empty receipt to the publish's reply subject.
func answeringServer(t *testing.T, deny string, receipt bool) *nats.Conn {
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
		fmt.Fprintf(conn, "INFO {\"server_id\":\"fake\",\"version\":\"2.10.29\",\"proto\":1,\"headers\":true,\"max_payload\":1048576}\r\n")
		lines := bufio.NewReader(conn)
		sids := map[string]string{}
		published, reply := false, ""
		for {
			line, err := lines.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			switch {
			case len(fields) == 3 && fields[0] == "SUB":
				sids[fields[1]] = fields[2]
			case len(fields) >= 3 && fields[0] == "PUB":
				if _, err := lines.ReadString('\n'); err != nil {
					return
				}
				published, reply = true, ""
				if len(fields) == 4 {
					reply = fields[2]
				}
			case len(fields) == 1 && fields[0] == "PING" && published:
				if deny != "" {
					fmt.Fprintf(conn, "-ERR 'Permissions Violation for Publish to %q'\r\n", deny)
				}
				fmt.Fprint(conn, "PONG\r\n")
				if sid, ok := sids[reply]; ok && receipt {
					fmt.Fprintf(conn, "MSG %s %s 0\r\n\r\n", reply, sid)
				}
				published = false
			case len(fields) == 1 && fields[0] == "PING":
				fmt.Fprint(conn, "PONG\r\n")
			}
		}
	}()
	conn, err := nats.Connect("nats://"+listener.Addr().String(), nats.MaxReconnects(0))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// The state a publish is judged against is read before the publish: a violation the server reports
// while the publish call is still running is this publish's denial, not a state it started from.
func TestPublishConfirmedReadsTheStateBeforeThePublish(t *testing.T) {
	subject := "notifications.role.reviewer"
	conn := answeringServer(t, subject, false)
	client := &Client{Conn: conn}
	// A publish whose own flush has the server's violation recorded before it returns.
	answered := func(conn *nats.Conn, msg *nats.Msg) error {
		if err := conn.PublishMsg(msg); err != nil {
			return err
		}
		return conn.Flush()
	}

	err := client.publishConfirmed(conn, &nats.Msg{Subject: subject, Data: []byte("{}")}, time.Now().Add(5*time.Second), answered)
	if !errors.Is(err, ErrPublishDenied) {
		t.Fatalf("a publish the server denied while it ran returned %v, want ErrPublishDenied", err)
	}
}

// A forward publishConfirmed cannot confirm - the server reported another subject's violation
// during it - still counts as delivered when its receipt arrives, since the receipt proves it.
func TestAnUnconfirmedForwardWithAReceiptIsDelivered(t *testing.T) {
	conn := answeringServer(t, "notifications.other", true)
	client := &Client{Conn: conn}

	if err := client.RequestCoreTo("notifications.agent.holder", contracts.Envelope{EventID: "e", Topic: "notifications.role.r"}, 2*time.Second); err != nil {
		t.Fatalf("an unconfirmed forward whose receipt arrived returned %v, want nil", err)
	}
}

// An unconfirmed forward with no receipt returns why it is unconfirmed, never ErrReceiptTimeout,
// which the listener treats as delivered to a live holder.
func TestAnUnconfirmedForwardWithoutAReceiptIsNotAReceiptTimeout(t *testing.T) {
	conn := answeringServer(t, "notifications.other", false)
	client := &Client{Conn: conn}

	err := client.RequestCoreTo("notifications.agent.holder", contracts.Envelope{EventID: "e", Topic: "notifications.role.r"}, 300*time.Millisecond)
	if !errors.Is(err, errPublishUnconfirmed) || errors.Is(err, ErrReceiptTimeout) {
		t.Fatalf("an unconfirmed forward with no receipt returned %v, want the unconfirmed error", err)
	}
}
