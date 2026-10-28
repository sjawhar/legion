package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// timeoutErr is a net.Error that timed out, as a dial or the client's deadline reports it.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestConnFailureOfClassifiesByWhetherTheRequestGotAConnection(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	reset := &net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}
	dialTimeout := &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}
	clientTimeout := fmt.Errorf("Get \"http://x\": %w", context.DeadlineExceeded)
	for _, c := range []struct {
		name            string
		err, connectErr error
		gotConn         bool
		want            string
	}{
		{"refused dial", refused, refused, false, connectRefused},
		{"dial timeout from ConnectDone", fmt.Errorf("wrapped: %w", errors.New("dial failed")), dialTimeout, false, connectTimeout},
		{"client timeout waiting for a connection", clientTimeout, nil, false, connectTimeout},
		{"reset before a connection", reset, nil, false, connReset},
		{"reset on the connection", reset, nil, true, connReset},
		{"EOF on the connection", io.EOF, nil, true, connReset},
		{"unexpected EOF on the connection", io.ErrUnexpectedEOF, nil, true, connReset},
		{"client timeout waiting for the response", clientTimeout, nil, true, responseTimeout},
		{"net timeout reading the response", timeoutErr{}, nil, true, responseTimeout},
	} {
		if got := connFailureOf(c.err, c.connectErr, c.gotConn); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCallReadsTheRefusalCodeAndClassifiesConnectionFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/overloaded":
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"code":"OVERLOADED","message":"the server is at its admission limit"}`)
		case "/busy":
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"code":"DATABASE_BUSY"}`)
		case "/plain":
			http.Error(w, "nope", http.StatusInternalServerError)
		case "/reset":
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.(*net.TCPConn).SetLinger(0)
				_ = conn.Close()
			}
		case "/slow":
			time.Sleep(time.Second)
		default:
			fmt.Fprint(w, `{"ok":true}`)
		}
	}))
	defer srv.Close()
	a := newAPI(srv.URL, "token", 4)
	a.client.Timeout = 300 * time.Millisecond
	ctx := context.Background()

	if res := a.call(ctx, "GET", "/overloaded", who{}, nil); res.status != 429 || res.code != "OVERLOADED" || res.connFailure != "" {
		t.Errorf("429: status %d code %q failure %q; want 429 OVERLOADED", res.status, res.code, res.connFailure)
	}
	if res := a.call(ctx, "GET", "/busy", who{}, nil); res.code != "DATABASE_BUSY" {
		t.Errorf("503: code %q, want DATABASE_BUSY", res.code)
	}
	if res := a.call(ctx, "GET", "/plain", who{}, nil); res.status != 500 || res.code != "" {
		t.Errorf("a 500 with a text body: status %d code %q, want 500 and no code", res.status, res.code)
	}
	if res := a.call(ctx, "GET", "/ok", who{}, nil); !res.ok() || res.code != "" || res.connFailure != "" {
		t.Errorf("200: %+v, want ok with no code or failure", res)
	}
	if res := a.call(ctx, "GET", "/reset", who{}, nil); res.err == nil || res.connFailure != connReset {
		t.Errorf("a reset connection: err %v failure %q, want %q", res.err, res.connFailure, connReset)
	}
	if res := a.call(ctx, "GET", "/slow", who{}, nil); res.err == nil || res.connFailure != responseTimeout {
		t.Errorf("a response past the client timeout: err %v failure %q, want %q", res.err, res.connFailure, responseTimeout)
	}

	// A port nobody listens on refuses the dial.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := l.Addr().String()
	_ = l.Close()
	if res := newAPI("http://"+closed, "token", 1).call(ctx, "GET", "/", who{}, nil); res.connFailure != connectRefused {
		t.Errorf("a closed port: err %v failure %q, want %q", res.err, res.connFailure, connectRefused)
	}
}

func TestRefusalTallySplitsFailures(t *testing.T) {
	tally := newRefusalTally()
	tally.add(false, "", 200, "", 5)
	tally.add(false, "", 429, "OVERLOADED", 2)
	tally.add(false, "", 429, "OVERLOADED", 4)
	tally.add(false, "", 503, "DATABASE_BUSY", 900)
	tally.add(false, "", 503, "REQUEST_TIMEOUT", 3000)
	tally.add(false, "", 429, "EDIT_QUEUE_FULL", 1)
	tally.add(false, "", 404, "ISSUE_NOT_FOUND", 7)
	tally.add(false, "", 500, "", 8)
	tally.add(true, connectTimeout, 0, "", 5000)
	tally.add(true, responseTimeout, 0, "", 30000)
	tally.add(true, connReset, 200, "", 10)
	tally.add(true, "", 0, "", 0) // an upload whose spec read answered without a token
	s := tally.summary()
	for code, want := range map[string]int{"OVERLOADED": 2, "DATABASE_BUSY": 1, "REQUEST_TIMEOUT": 1, "EDIT_QUEUE_FULL": 1} {
		if s.Codes[code] != want {
			t.Errorf("%s: %d, want %d", code, s.Codes[code], want)
		}
	}
	if s.OtherNon2xx != 2 {
		t.Errorf("other failed answers %d, want 2 (404 and 500)", s.OtherNon2xx)
	}
	for class, want := range map[string]int{connectTimeout: 1, connectRefused: 0, connReset: 1, responseTimeout: 1} {
		if n, ok := s.ConnectionFailures[class]; !ok || n != want {
			t.Errorf("%s: %d (present %t), want %d", class, n, ok, want)
		}
	}
	// The refusal latency is over the 429 and 503 answers only: 1, 2, 4, 900, 3000.
	if s.RefusalMs.N != 5 || s.RefusalMs.P50 != 4 || s.RefusalMs.P99 != 900 {
		t.Errorf("refusal ms %+v, want 5 values, p50 4, p99 900", s.RefusalMs)
	}
}

// TestUploadWhoseSpecReadIsRefusedIsARefusalOfTheWrite: an upload's spec read answered 429 or
// 503 is Dispatch refusing the upload, so the write counts as a refusal with that code and the
// read's latency, never as a transport error; a read answered without the revision token stays a
// transport error with no connection failure.
func TestUploadWhoseSpecReadIsRefusedIsARefusalOfTheWrite(t *testing.T) {
	answers := map[string]struct {
		status int
		body   string
	}{
		"busy":       {http.StatusServiceUnavailable, `{"code":"DATABASE_BUSY","error":"busy"}`},
		"overloaded": {http.StatusTooManyRequests, `{"code":"OVERLOADED","error":"overloaded"}`},
		"no-token":   {http.StatusOK, `{"markdown":"no token here"}`},
	}
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			posts++
			w.WriteHeader(http.StatusCreated)
			return
		}
		a, ok := answers[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/artifacts/"), "/text")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body)
	}))
	defer srv.Close()
	sf := seedFile{Issues: []seedIssue{{Key: "LGB-1", ArtifactID: "busy"}, {Key: "LGB-2", ArtifactID: "overloaded"}, {Key: "LGB-3", ArtifactID: "no-token"}}}
	wr := newWriter(newAPI(srv.URL, "token", 4), sf, 1, log.New(io.Discard, "", 0))
	var writes []writeRec
	for i := range sf.Issues {
		rec := wr.do(context.Background(), eventPlan{marker: int64(i + 1), kind: kindUpload, issue: i})
		rec.InWindow = true
		writes = append(writes, rec)
	}
	if posts != 0 {
		t.Fatalf("%d uploads were posted; none should be, since no spec read gave the revision token", posts)
	}
	for i, want := range []struct {
		status int
		code   string
	}{{503, "DATABASE_BUSY"}, {429, "OVERLOADED"}} {
		if w := writes[i]; w.Status != want.status || w.Code != want.code || w.TransportErr != "" || w.ConnFailure != "" || w.Arrival == 0 || w.Response < w.Arrival {
			t.Errorf("upload %s whose spec read answered %d: %+v; want status %d, code %s, no transport error, and the read's times", w.Issue, want.status, w, want.status, want.code)
		}
	}
	if w := writes[2]; w.TransportErr == "" || w.ConnFailure != "" {
		t.Errorf("upload whose spec read had no revision token: %+v; want a transport error with no connection failure", w)
	}

	e := joinDelivery(writes, nil, nil, map[string]int{}, 1)
	if e.WriteErrors.Non2xx != 2 || e.WriteErrors.Transport != 1 {
		t.Errorf("write errors non-2xx %d, transport %d; want 2 and 1", e.WriteErrors.Non2xx, e.WriteErrors.Transport)
	}
	if e.Refusals.Codes["DATABASE_BUSY"] != 1 || e.Refusals.Codes["OVERLOADED"] != 1 || e.Refusals.RefusalMs.N != 2 {
		t.Errorf("refusals %+v; want one DATABASE_BUSY, one OVERLOADED and two refusal latencies", e.Refusals)
	}
}
