package api

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
)

// INFRA-592. Four writes of the round-22 load test were answered with a bare EOF: each was
// written in full onto a keep-alive connection the server had already closed, or closed in the
// same instant, and no handler ever ran for it, so neither side holds a record of what happened
// to it. A client reuses a connection it was not told was closing, so every answer after which
// the server will not carry another request on that connection must say so.
//
// This holds that invariant over one real connection and two requests: an early refusal of a
// write whose body the drain leaves unread, and then a second request written onto the same
// connection as a client taking it out of its idle pool writes one. Either the second request is
// answered, or the first answer said Connection: close and the connection is gone - never a
// connection that looks reusable and is not.
//
// The three shapes are the three ways a refused write leaves its connection. A body that arrived
// whole leaves the connection as good as new. A body with more left on it than net/http discards
// after an answer (256 KiB, its maxPostHandlerReadBytes) is one net/http refuses itself, adding
// Connection: close to the answer and half-closing the connection before it drops it, so the
// client reads the answer before the reset. A body that stopped arriving is cut by the bound it
// arrives under and the drain says the connection closes itself (sayTheConnectionCloses).
func TestAnEarlyRefusalLeavesAConnectionAnsweringOrSayingItCloses(t *testing.T) {
	const idle = time.Second
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{files: filestest.NewMemory()})
	served.transfer = transferBounds{idle: idle}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	open := createArtifactIssue(t, handler)
	closed := createClosedIssue(t, handler)
	toClosed := "/api/v1/issues/" + closed.Key + "/artifacts"
	message := "/api/v1/issues/" + open.Key + "/messages"
	const multipart, jsonType = "multipart/form-data; boundary=x", "application/json"
	jsonLimit := int(maxJSONRequestBytes)

	for _, refused := range []struct {
		name, target, contentType, login string
		// size is the body's Content-Length and sent what the client writes of it; the rest
		// never comes.
		size   int
		sent   []byte
		status int
		code   string
	}{
		{
			name: "a refused upload whose body arrived whole", target: toClosed, contentType: multipart,
			size: 64 << 10, sent: bytes.Repeat([]byte("a"), 64<<10),
			status: http.StatusConflict, code: "ISSUE_CLOSED",
		},
		{
			name: "a write past the route's limit with more left than net/http discards", target: message,
			contentType: jsonType, login: "bob", size: jsonLimit + (512 << 10),
			sent: bytes.Repeat([]byte("a"), jsonLimit+1),
			// The drain reads a byte past the limit and answers 413; the half megabyte
			// behind it is never read.
			status: http.StatusRequestEntityTooLarge, code: "REQUEST_TOO_LARGE",
		},
		{
			name: "a refused upload whose body stopped arriving", target: toClosed, contentType: multipart,
			size: 64 << 10, sent: []byte("--x\r\n"),
			status: http.StatusConflict, code: "ISSUE_CLOSED",
		},
	} {
		t.Run(refused.name, func(t *testing.T) {
			header := http.Header{}
			header.Set("Content-Type", refused.contentType)
			header.Set("X-Dispatch-User", "alice")
			if refused.login != "" {
				header.Set(contracts.DispatchLoginHeader, refused.login)
			}
			connection := sendPartOfBody(t, server.Listener.Addr().String(), refused.target, header, refused.size, refused.sent)
			response, answer := connection.answer()
			if response.StatusCode != refused.status || refusalCode(answer) != refused.code {
				t.Fatalf("status=%d body=%s, want %d %s", response.StatusCode, answer, refused.status, refused.code)
			}
			if response.Close {
				// The client was told: it opens a connection of its own for the next
				// request, and this one must be gone rather than left half alive.
				if !connection.closedByServer(10 * time.Second) {
					t.Fatalf("the answer said Connection: close and the connection was still open 10 s later")
				}
				return
			}
			// The client was told nothing, so it writes its next request onto this
			// connection: the server must answer it.
			next, err := connection.nextRequest(t, "GET", "/api/v1/issues/"+open.Key, header, nil)
			if err != nil {
				t.Fatalf("the next request on a connection whose answer did not say it was closing: %v", err)
			}
			if next.StatusCode != http.StatusOK {
				t.Fatalf("the next request on the reused connection answered %d, want 200", next.StatusCode)
			}
		})
	}
}

// INFRA-592. A refusal that replaces the answer a layer gave keeps none of that layer's headers
// - not its Idempotency-Key, not its Retry-After - but it must keep what the server itself
// already decided about the connection: cmd/dispatch's maxConnAgeHandler sets Connection: close
// on a connection that has been open 60 seconds, before any layer of the API runs, and net/http
// reads the header the handler leaves when it decides whether to close (closeAfterReply).
//
// Cleared with the rest, that header left the connection open, so the connection age limit was
// silently off for the dominant answer of a loaded process: the round-22 run refused 97,954
// writes 429 OVERLOADED, and every one of them on an aged connection kept it alive.
func TestARefusalKeepsTheConnectionCloseOfAnAgedConnection(t *testing.T) {
	limiter := admission.New(admission.Limits{
		Read: 4, Write: 1, ReadWait: 250 * time.Millisecond, WriteWait: 250 * time.Millisecond,
	})
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{
		admission: limiter, files: filestest.NewMemory(),
	})
	served.transfer = transferBounds{idle: 10 * time.Second}
	// The handler main runs behind the connection age limit, whose one effect on a request is
	// this header (cmd/dispatch's maxConnAgeHandler); the limit lives in a main package this
	// one cannot import.
	aged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		handler.ServeHTTP(w, r)
	})
	server := httptest.NewServer(aged)
	t.Cleanup(server.Close)
	issue := createArtifactIssue(t, handler)

	t.Run("a 413 that replaces an early answer to a body past the route's limit", func(t *testing.T) {
		header := http.Header{}
		header.Set("Content-Type", "application/json")
		header.Set("X-Dispatch-User", "alice")
		header.Set(contracts.DispatchLoginHeader, "bob")
		// Exactly a byte past the limit, so the whole body is off the connection when the
		// drain answers: nothing is left for net/http to close the connection over, and the
		// age limit's header is the only reason it must close.
		body := bytes.Repeat([]byte("a"), int(maxJSONRequestBytes)+1)
		connection := sendPartOfBody(t, server.Listener.Addr().String(),
			"/api/v1/issues/"+issue.Key+"/messages", header, len(body), body)
		response, answer := connection.answer()
		if response.StatusCode != http.StatusRequestEntityTooLarge || refusalCode(answer) != "REQUEST_TOO_LARGE" {
			t.Fatalf("status=%d body=%s, want 413 REQUEST_TOO_LARGE", response.StatusCode, answer)
		}
		assertAgedConnectionClosed(t, connection, response)
	})

	t.Run("a 429 that replaces the answer of a write refused when it asked its units back", func(t *testing.T) {
		connection, response, answer := refusedAtResumeOn(t, limiter, server.Listener.Addr().String(),
			"/api/v1/issues/"+issue.Key+"/comments", "application/json",
			[]byte(`{"body":"refused when it asked its units back"}`))
		if response.StatusCode != http.StatusTooManyRequests || refusalCode(answer) != admission.CodeOverloaded {
			t.Fatalf("status=%d body=%s, want 429 %s", response.StatusCode, answer, admission.CodeOverloaded)
		}
		assertAgedConnectionClosed(t, connection, response)
	})
}

// assertAgedConnectionClosed holds the two halves of the age limit on a refusal: the client is
// told the connection is closing, and the connection closes.
func assertAgedConnectionClosed(t *testing.T, connection *heldBackBody, response *http.Response) {
	t.Helper()
	if !response.Close {
		t.Fatalf("the refusal on a connection the age limit marked carries Connection: %q, want close: the refusal cleared the header the limit set, so the client keeps the aged connection",
			response.Header.Get("Connection"))
	}
	if !connection.closedByServer(10 * time.Second) {
		t.Fatal("the aged connection was still open 10 s after its refusal")
	}
}

// refusedAtResumeOn is refusedAtResume with the connection it used, so a test can ask what the
// server did to that connection after the refusal.
func refusedAtResumeOn(t *testing.T, limiter *admission.Limiter, address, target, contentType string, body []byte) (*heldBackBody, *http.Response, []byte) {
	t.Helper()
	header := http.Header{}
	header.Set("Content-Type", contentType)
	header.Set("X-Dispatch-User", "alice")
	header.Set("Expect", "100-continue")
	sent := sendPartOfBody(t, address, target, header, len(body), nil)
	if response, answer := sent.answer(); response.StatusCode != http.StatusContinue {
		t.Fatalf("the write was answered %d %s before it sent a byte, want the 100 Continue of a body being read",
			response.StatusCode, answer)
	}
	held, err := limiter.Admit(t.Context(), admission.Write, 1)
	if err != nil {
		t.Fatalf("hold the write class's one unit while the body arrives: %v", err)
	}
	defer held.Release()
	response, answer := sent.finish(body)
	return sent, response, answer
}

// nextRequest writes another request onto the connection a first request already used, as a
// client taking that connection out of its idle pool writes one, and answers what the server
// answered or the error of a connection the server has closed under it. Unlike answer it is no
// fatal: a client meeting a closed connection here is the thing under test.
func (b *heldBackBody) nextRequest(t *testing.T, method, target string, header http.Header, body []byte) (*http.Response, error) {
	t.Helper()
	if err := b.connection.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var head bytes.Buffer
	fmt.Fprintf(&head, "%s %s HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\n", method, target, b.connection.RemoteAddr(), len(body))
	for name, values := range header {
		if strings.EqualFold(name, "Content-Type") || strings.EqualFold(name, "Expect") {
			continue
		}
		for _, value := range values {
			fmt.Fprintf(&head, "%s: %s\r\n", name, value)
		}
	}
	head.WriteString("\r\n")
	head.Write(body)
	if _, err := b.connection.Write(head.Bytes()); err != nil {
		return nil, fmt.Errorf("write the next request onto the connection: %w", err)
	}
	response, err := http.ReadResponse(b.reader, nil)
	if err != nil {
		return nil, fmt.Errorf("read the next request's answer: %w", err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read the next request's answer body: %w", err)
	}
	return response, nil
}
