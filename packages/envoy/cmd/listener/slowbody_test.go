package main

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/webhook"
)

type slowBodyPublisher struct {
	mu        sync.Mutex
	published []contracts.Envelope
}

func (p *slowBodyPublisher) Publish(e contracts.Envelope) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.published = append(p.published, e)
	return nil
}

type noCI struct{}

func (noCI) Record(contracts.CIObservation) error      { return nil }
func (noCI) RecordSuite(contracts.CIObservation) error { return nil }

// serveWebhook serves handler at path the way main does: the outer mux sends the path to an open
// webhook startingGate, whose mux holds the handler, all on the listener's own HTTP server
// (newHTTPServer). It returns the server's address.
func serveWebhook(t *testing.T, path string, handler http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gate := newStartingGate("webhook", logging.NewWithWriter("test", io.Discard))
	routes := http.NewServeMux()
	routes.Handle(path, handler)
	gate.open(routes)
	mux := http.NewServeMux()
	mux.Handle(path, gate)
	server := newHTTPServer(mux)
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close() })
	return ln.Addr().String()
}

// serveSlowly serves handler at path (serveWebhook) and sends one POST whose body takes over: its
// bytes go out in 20 pieces spread across that time, as a proxy sends a large delivery over a slow
// link. It returns the status the server answered, or 0 when the connection closed without an
// answer.
func serveSlowly(t *testing.T, path string, handler http.Handler, headers map[string]string, body []byte, over time.Duration) int {
	t.Helper()
	addr := serveWebhook(t, path, handler)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var head strings.Builder
	fmt.Fprintf(&head, "POST %s HTTP/1.1\r\nHost: listener\r\nContent-Type: application/json\r\nContent-Length: %d\r\n", path, len(body))
	for k, v := range headers {
		fmt.Fprintf(&head, "%s: %s\r\n", k, v)
	}
	head.WriteString("\r\n")
	if _, err := io.WriteString(conn, head.String()); err != nil {
		t.Fatal(err)
	}
	const pieces = 20
	size := (len(body) + pieces - 1) / pieces
	for sent := 0; sent < len(body); sent += size {
		time.Sleep(over / pieces)
		if _, err := conn.Write(body[sent:min(sent+size, len(body))]); err != nil {
			break // the server closed the connection: read whatever it answered
		}
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

// A GitHub delivery whose body takes longer than the server's 10 s to arrive is read whole and
// published, not cut off with no answer.
func TestAGitHubDeliveryWhoseBodyArrivesOverMoreThanTenSecondsIsPublished(t *testing.T) {
	if testing.Short() {
		t.Skip("sends a body over 12 s")
	}
	const secret = "s"
	body := []byte(`{"ref":"refs/heads/main","after":"` + strings.Repeat("a", 40) + `","repository":{"name":"legion","owner":{"login":"sjawhar"},"full_name":"sjawhar/legion"},"pad":"` + strings.Repeat("x", 3<<20) + `"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	pub := &slowBodyPublisher{}
	status := serveSlowly(t, "/webhook/github", webhook.GitHubHandler(secret, "@legion", "", pub, noCI{}), map[string]string{
		"X-GitHub-Delivery":   "delivery-slow-body",
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": "sha256=" + hex.EncodeToString(mac.Sum(nil)),
	}, body, 12*time.Second)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (0 is a connection closed with no answer)", status)
	}
	pub.mu.Lock()
	defer pub.mu.Unlock()
	if len(pub.published) == 0 {
		t.Fatal("published nothing; want the push")
	}
}

// A GitHub sender that goes away partway through its body fails the read, and the failure is
// logged with what the delivery was and how far its body got.
func TestAGitHubBodyReadFailureIsLogged(t *testing.T) {
	logged := &lockedBuffer{}
	previous := log.Writer()
	log.SetOutput(logged)
	t.Cleanup(func() { log.SetOutput(previous) })
	addr := serveWebhook(t, "/webhook/github", webhook.GitHubHandler("s", "@legion", "", &slowBodyPublisher{}, noCI{}))
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(conn, "POST /webhook/github HTTP/1.1\r\nHost: listener\r\nX-GitHub-Delivery: delivery-cut\r\nX-GitHub-Event: push\r\nContent-Length: 1000\r\n\r\n"+strings.Repeat("x", 300))
	conn.Close() // the sender goes away with 700 bytes still owed
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logged.String(), "github body read failed") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	line := logged.String()
	for _, want := range []string{"github body read failed", "delivery=delivery-cut", "event=push", "content_length=1000", "read=300", "elapsed="} {
		if !strings.Contains(line, want) {
			t.Errorf("log = %q, want it to contain %q", line, want)
		}
	}
}

// Every other route keeps the server's 10 s bound on its body: a Slack body that takes longer to
// arrive is cut off.
func TestASlackBodyStillHasTheServersReadTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("sends a body over 12 s")
	}
	pub := &slowBodyPublisher{}
	start := time.Now()
	status := serveSlowly(t, "/webhook/slack", webhook.SlackHandler("s", pub), nil, []byte(`{"type":"url_verification","challenge":"`+strings.Repeat("c", 4000)+`"}`), 12*time.Second)
	elapsed := time.Since(start)
	if status == http.StatusOK {
		t.Fatalf("status = 200 after %s; want the server's 10 s read timeout to cut the body off", elapsed.Round(time.Millisecond))
	}
	if elapsed < 10*time.Second || elapsed > 11500*time.Millisecond {
		t.Fatalf("the Slack request ended after %s with status %d; want it cut off at the server's 10 s, before its body's 12 s", elapsed.Round(time.Millisecond), status)
	}
}

// lockedBuffer is a log destination the server's goroutines and the test can share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
