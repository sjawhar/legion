package main

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// startWatchedServer serves handler on the server main runs, with a connection watch of the
// test's own to read, under clock's time.
func startWatchedServer(t *testing.T, clock *fakeClock, handler http.Handler) (*httptest.Server, *conns) {
	t.Helper()
	watched := newConns()
	server := httptest.NewUnstartedServer(nil)
	server.Config = newHTTPServer(handler, clock.Now, watched)
	server.Start()
	t.Cleanup(server.Close)
	return server, watched
}

// closedUnanswered is how many connections the watch has counted for reason.
func closedUnanswered(t *testing.T, watched *conns, reason connCloseReason) int64 {
	t.Helper()
	for _, closed := range watched.ClosedUnanswered() {
		if closed.Reason == string(reason) {
			return closed.Count
		}
	}
	t.Fatalf("the watch counts no connections for %q; it counts %v", reason, watched.ClosedUnanswered())
	return 0
}

// awaitClosedUnanswered waits for the watch to count want connections for reason, which it does
// when net/http reports the connection closed, after the client has read the answer.
func awaitClosedUnanswered(t *testing.T, watched *conns, reason connCloseReason, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := closedUnanswered(t, watched, reason); got == want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("the watch counted %d connections closed for %q within 10 s, want %d; it counts %v",
				got, reason, want, watched.ClosedUnanswered())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// INFRA-592. A connection the server closes after an answer that did not say it was closing is
// the close a client reads as an EOF: it puts the connection back in its idle pool, writes its
// next request onto it in full and is answered nothing, with no access line on the server to
// say the request was ever seen. Four writes of the round-22 load test ended that way and
// nothing on the server recorded a single one of them, because net/http reports its connections'
// states to nobody unless the server asks (ConnState) and sends its own errors to the standard
// logger unless the server takes them (ErrorLog).
//
// The handler here promises a Content-Length and writes fewer bytes, which is one of the ways a
// handler leaves net/http unable to reuse a connection it has already written a header on
// (shouldReuseConnection): the answer carries no Connection: close, because by the time the
// shortfall is known the header is on the wire.
func TestTheConnectionWatchCountsACloseAfterAnAnswerThatDidNotSayItCloses(t *testing.T) {
	logged := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logged, nil))
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	short := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "64")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ten bytes.")
	})
	server, watched := startWatchedServer(t, clock, short)

	connection, reader := dialAndSend(t, server.Listener.Addr().String(), "/short")
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read the short answer: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.Close {
		t.Fatal("the short answer said Connection: close, so this is not the close a client is told nothing about")
	}
	_ = connection.Close()

	awaitClosedUnanswered(t, watched, connAnswerNotClosing, 1)
	for _, quiet := range []connCloseReason{connRequestUnanswered, connHandlerAborted} {
		if counted := closedUnanswered(t, watched, quiet); counted != 0 {
			t.Fatalf("the watch counted %d connections for %q, want none: the request was answered and its handler returned", counted, quiet)
		}
	}
	warning := awaitLogLineWith(t, logged, "closed a connection whose client was told nothing", 10*time.Second)
	if !strings.Contains(warning, "level=WARN") || !strings.Contains(warning, "reason="+string(connAnswerNotClosing)) {
		t.Fatalf("the close was not reported as a WARN naming its cause: %s", warning)
	}
}

// net/http's own errors reach the process's log at WARN. With no ErrorLog set they went to the
// standard logger's stderr, outside the log stream, unparsed and counted nowhere, which is why
// a round-22 run that closed connections with nothing said held no line of net/http's either.
// The handler here writes a second header, which net/http reports and drops.
func TestTheServerLogsNetHTTPsOwnErrors(t *testing.T) {
	logged := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logged, nil))
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	twice := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.WriteHeader(http.StatusInternalServerError)
	})
	server, _ := startWatchedServer(t, clock, twice)

	response, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatalf("get the twice-headed answer: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()

	line := awaitLogLineWith(t, logged, "the HTTP server reported an error", 10*time.Second)
	if !strings.Contains(line, "level=WARN") || !strings.Contains(line, "superfluous response.WriteHeader") {
		t.Fatalf("net/http's own error did not reach the log as a WARN naming it: %s", line)
	}
}

// An ordinary keep-alive run counts nothing: two requests answered on one connection, which the
// client then gives up. Its last state is idle, and an idle connection's client has been
// answered everything it sent and may open another whenever it likes.
func TestTheConnectionWatchCountsNothingForAKeepAliveRun(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	server, watched := startWatchedServer(t, clock, ok)

	connection, reader := dialAndSend(t, server.Listener.Addr().String(), "/first")
	for _, target := range []string{"/first", "/second"} {
		response, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatalf("read the answer to %s: %v", target, err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK || response.Close {
			t.Fatalf("the answer to %s is %d, Connection: %q; want 200 on a kept connection",
				target, response.StatusCode, response.Header.Get("Connection"))
		}
		if target == "/first" {
			if _, err := fmt.Fprintf(connection, "GET /second HTTP/1.1\r\nHost: %s\r\n\r\n", server.Listener.Addr()); err != nil {
				t.Fatalf("send the second request on the kept connection: %v", err)
			}
		}
	}
	_ = connection.Close()

	// The close the client made is the server's to notice; give it the same window the counted
	// close above is given before reading the counts.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, reason := range connCloseReasons {
			if counted := closedUnanswered(t, watched, reason); counted != 0 {
				t.Fatalf("a keep-alive run of two answered requests counted %d connections for %q, want none", counted, reason)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// dialAndSend opens a connection of its own and sends one GET on it, so a test can hold the
// connection and read what the server does to it.
func dialAndSend(t *testing.T, address, target string) (net.Conn, *bufio.Reader) {
	t.Helper()
	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dial %s: %v", address, err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if err := connection.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(connection, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, address); err != nil {
		t.Fatalf("send %s: %v", target, err)
	}
	return connection, bufio.NewReader(connection)
}

// awaitLogLineWith is the first line of the log holding message, waited for up to within, or ""
// when none arrives: the states net/http reports are read on the connection's own goroutine,
// which the client's close only wakes.
func awaitLogLineWith(t *testing.T, logged *storetest.LockedLog, message string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		for line := range strings.SplitSeq(logged.String(), "\n") {
			if strings.Contains(line, message) {
				return line
			}
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(10 * time.Millisecond)
	}
}
