package bus

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// unansweredBucket is the bucket the rows below open a jetstream handle on, created afresh by each
// of them on the package's shared server and removed when it ends.
const unansweredBucket = "envoy_unanswered_test"

// openSubject is the request wrapKeyValue sends to open the jetstream handle of a bucket: the
// STREAM.INFO of the bucket's stream, which is what the relay below counts and drops.
func openSubject(bucket string) string { return "$JS.API.STREAM.INFO.KV_" + bucket }

// unansweredRelay is a TCP relay between a client and a real NATS server that counts the handle
// opens the client sends on it, the STREAM.INFO of one bucket's stream, and drops the first drops
// of them: the request never reaches the server, as a server whose meta leader is changing leaves
// one unanswered, so no answer is ever published and the caller's wait runs out with the deadline
// it held. Everything else both sides send is relayed untouched, so the connection is a real one
// on a real server and every other request is answered as it always is.
//
// The jetstream handle wrapKeyValue opens is built from the connection inside that function
// (jetstream.New(conn)), and jetstream.New takes the concrete *nats.Conn, so no JetStream context
// a test passes in can reach that open: the seam a test has for it is the connection's own wire,
// which is this relay. The bucket's own name is in the subject, so only the opens of this bucket
// are touched, and the legacy lookups a start makes before it are counted apart from them (arm).
type unansweredRelay struct {
	subject  string
	url      string
	listener net.Listener
	mu       sync.Mutex
	drops    int
	sends    int
}

// startUnansweredRelay relays to the NATS server at target, a client URL such as testnats.URL
// returns, on a loopback port of its own until the test ends. Its own URL is that one with the
// host and port it listens on, credentials included, as testnats.StartProxy builds its own. It
// drops nothing until it is armed.
func startUnansweredRelay(t *testing.T, target, bucket string) *unansweredRelay {
	t.Helper()
	server, err := url.Parse(target)
	if err != nil || server.Host == "" {
		t.Fatalf("read the NATS URL %q the relay relays to: %v", target, err)
		return nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the relay: %v", err)
		return nil
	}
	relayed := *server
	relayed.Host = listener.Addr().String()
	relay := &unansweredRelay{
		subject:  openSubject(bucket),
		url:      relayed.String(),
		listener: listener,
	}
	t.Cleanup(func() { _ = listener.Close() })
	go relay.serve(server.Host)
	return relay
}

// arm has the relay drop the next drops sends of the handle open and counts the sends from now, so
// a row counts the opens of the call it is about to make and not the lookups that led to it.
func (r *unansweredRelay) arm(drops int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drops = drops
	r.sends = 0
}

// sent is how many handle opens the client has sent since it was armed.
func (r *unansweredRelay) sent() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sends
}

// send records one send of subject and reports whether it is dropped: it is while drops are left,
// and each such send spends one.
func (r *unansweredRelay) send(subject string) bool {
	if subject != r.subject {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends++
	if r.drops > 0 {
		r.drops--
		return true
	}
	return false
}

// serve joins each connection the relay accepts to the server until the listener closes.
func (r *unansweredRelay) serve(target string) {
	for {
		client, err := r.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", target)
		if err != nil {
			_ = client.Close()
			continue
		}
		go r.filter(server, client)
		go func() {
			_, _ = io.Copy(client, server)
			_ = client.Close()
			_ = server.Close()
		}()
	}
}

// filter relays what the client sends to the server, a protocol message at a time, leaving out the
// publishes the relay drops. A client sends only CONNECT, SUB, UNSUB, PING, PONG and publishes, so
// every line that is not a publish is passed on as it is, and a publish is passed on with the
// payload its header counts.
func (r *unansweredRelay) filter(server, client net.Conn) {
	defer func() {
		_ = client.Close()
		_ = server.Close()
	}()
	reader := bufio.NewReader(client)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		// `PUB <subject> [reply] <bytes>` and `HPUB <subject> [reply] <header bytes> <bytes>` are
		// each followed by that many bytes and a CRLF. A line with fewer than three fields is
		// neither, whatever its first word says.
		if len(fields) < 3 || (!strings.EqualFold(fields[0], "PUB") && !strings.EqualFold(fields[0], "HPUB")) {
			if _, err := io.WriteString(server, line); err != nil {
				return
			}
			continue
		}
		size, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			return
		}
		payload := make([]byte, size+2)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return
		}
		if r.send(fields[1]) {
			continue
		}
		if _, err := io.WriteString(server, line); err != nil {
			return
		}
		if _, err := server.Write(payload); err != nil {
			return
		}
	}
}

// unansweredKeyValue creates the bucket on a connection through relay and returns that connection
// with the legacy handle a start holds when it comes to open the jetstream handle of it.
func unansweredKeyValue(t *testing.T, relay *unansweredRelay) (*nats.Conn, nats.JetStreamContext, nats.KeyValue) {
	t.Helper()
	conn := testnats.Connect(t, relay.url)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(nats.MaxWait(RequestTimeout))
	if err != nil {
		t.Fatalf("open JetStream through the relay: %v", err)
	}
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: unansweredBucket, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	t.Cleanup(func() { _ = js.DeleteKeyValue(unansweredBucket) })
	return conn, js, kv
}

// A start whose open of a bucket's jetstream handle draws no answer sends it again and goes on,
// rather than ending the program with "open the jetstream handle of KV bucket <name>: context
// deadline exceeded". The first open never reaches the server, so the handle the start is given
// is the second one's, and it reads the bucket's stream. The start is the create path, which is
// where wrapKeyValue sends again.
func TestAnUnansweredHandleOpenIsSentAgainAndTheStartGoesOn(t *testing.T) {
	relay := startUnansweredRelay(t, testnats.URL(t), unansweredBucket)
	conn, _, kv := unansweredKeyValue(t, relay)

	relay.arm(1)
	handle, err := wrapKeyValue(conn, kv, true)
	if err != nil {
		t.Fatalf("a start whose first handle open drew no answer = %v, want it to go on", err)
	}

	if sends := relay.sent(); sends != 2 {
		t.Fatalf("handle opens sent = %d, want 2: the lost one and the one that answered", sends)
	}
	if handle.Bucket() != unansweredBucket {
		t.Fatalf("the handle is on bucket %q, want %q", handle.Bucket(), unansweredBucket)
	}
	if _, err := handle.ReadStreamState(); err != nil {
		t.Fatalf("read the bucket's stream through the handle the start was given: %v", err)
	}
}

// An answer the server gives ends the sending: the create path sends the open once and is done,
// so nothing a server answers is ever asked twice.
func TestAnAnsweredHandleOpenIsSentOnce(t *testing.T) {
	relay := startUnansweredRelay(t, testnats.URL(t), unansweredBucket)
	conn, _, kv := unansweredKeyValue(t, relay)

	relay.arm(0)
	if _, err := wrapKeyValue(conn, kv, true); err != nil {
		t.Fatalf("a start whose handle open the server answered = %v, want it to go on", err)
	}

	if sends := relay.sent(); sends != 1 {
		t.Fatalf("handle opens sent = %d, want 1: an answer is the server's and is not sent again", sends)
	}
}

// Off the create path the open is one send, whatever the server does with it. That path is no
// start: roles.Buckets.bucket opens a bucket again under its own lock on a request whose budget
// is two seconds, so an open that kept sending would stop every role and session read of the
// process instead of saving a start.
func TestAnUnansweredHandleOpenOffTheCreatePathIsSentOnce(t *testing.T) {
	relay := startUnansweredRelay(t, testnats.URL(t), unansweredBucket)
	conn, _, kv := unansweredKeyValue(t, relay)

	relay.arm(1)
	_, err := WrapKeyValue(conn, kv)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an open the server left unanswered off the create path = %v, want %v", err, context.DeadlineExceeded)
	}
	if sends := relay.sent(); sends != 1 {
		t.Fatalf("handle opens sent = %d, want 1: this path is a request's, not a start's", sends)
	}
}
