package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync"
	"testing"
	"time"
)

// fakeClock is a clock the test moves by hand, so a connection ages without the test sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// startConnAgeServer serves a 200 handler on the server main runs (newHTTPServer),
// under clock's time.
func startConnAgeServer(t *testing.T, clock *fakeClock) *httptest.Server {
	t.Helper()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	server := httptest.NewUnstartedServer(nil)
	server.Config = newHTTPServer(ok, clock.Now)
	server.Start()
	t.Cleanup(server.Close)
	return server
}

// getOnConn sends a GET carrying header and reads its response to the end, so the connection is
// back in the client's pool before it returns. reused says whether the request went out on a
// connection an earlier request had used.
func getOnConn(t *testing.T, client *http.Client, url string, header http.Header) (resp *http.Response, reused bool) {
	t.Helper()
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), trace), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range header {
		req.Header[name] = values
	}
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	return resp, reused
}

func TestMaxConnAgeKeepsAYoungConnection(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	server := startConnAgeServer(t, clock)
	client := server.Client()

	first, reused := getOnConn(t, client, server.URL, nil)
	if reused {
		t.Fatal("first request reused a connection; want a new one")
	}
	if first.Close {
		t.Fatal("response on a new connection carried Connection: close")
	}

	clock.advance(maxConnAge - time.Nanosecond)
	second, reused := getOnConn(t, client, server.URL, nil)
	if !reused {
		t.Fatal("request on a connection younger than maxConnAge opened a new connection")
	}
	if second.Close {
		t.Fatal("response on a connection younger than maxConnAge carried Connection: close")
	}
}

func TestMaxConnAgeClosesAnAgedConnectionAfterItsResponse(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	server := startConnAgeServer(t, clock)
	client := server.Client()

	getOnConn(t, client, server.URL, nil)
	clock.advance(maxConnAge)

	aged, reused := getOnConn(t, client, server.URL, nil)
	if !reused {
		t.Fatal("request after the age opened a new connection; want it on the aged one")
	}
	if !aged.Close {
		t.Fatal("response on a connection maxConnAge old did not carry Connection: close")
	}

	next, reused := getOnConn(t, client, server.URL, nil)
	if reused {
		t.Fatal("request after a Connection: close response reused the aged connection")
	}
	if next.Close {
		t.Fatal("response on the new connection carried Connection: close")
	}
}

func TestMaxConnAgeKeepsAnAgedConnectionForAnUpgrade(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	server := startConnAgeServer(t, clock)
	client := server.Client()

	getOnConn(t, client, server.URL, nil)
	clock.advance(maxConnAge)

	upgrade, reused := getOnConn(t, client, server.URL, http.Header{"Upgrade": {"websocket"}})
	if !reused {
		t.Fatal("upgrade request opened a new connection; want it on the aged one")
	}
	if upgrade.Close {
		t.Fatal("response to an Upgrade request on an aged connection carried Connection: close")
	}

	plain, reused := getOnConn(t, client, server.URL, nil)
	if !reused {
		t.Fatal("the aged connection was closed after the Upgrade request's response")
	}
	if !plain.Close {
		t.Fatal("plain request on the aged connection did not carry Connection: close")
	}
}

func TestMaxConnAgeHandlerPassesARequestWithNoAcceptTime(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	handler := maxConnAgeHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), 0, clock.Now)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := recorder.Header().Get("Connection"); got != "" {
		t.Fatalf("Connection = %q on a request with no accept time, want none", got)
	}
}

// TestShutdownAlreadyClosesKeepAliveConnections records the net/http behaviour the connection age
// limit leaves shutdown to: once Server.Shutdown has begun, a handler still running writes its
// response with Connection: close (keep-alives are off while the server is shutting down), so a
// draining process sheds every connection as its last response goes out.
func TestShutdownAlreadyClosesKeepAliveConnections(t *testing.T) {
	server := httptest.NewUnstartedServer(nil)
	shutdownBegun := make(chan struct{})
	server.Config.RegisterOnShutdown(func() { close(shutdownBegun) })
	entered := make(chan struct{})
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-shutdownBegun
		_, _ = io.WriteString(w, "ok")
	})
	server.Start()
	t.Cleanup(server.Close)

	type result struct {
		resp *http.Response
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := server.Client().Get(server.URL)
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		got <- result{resp, err}
	}()

	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	shutdown := make(chan error, 1)
	go func() { shutdown <- server.Config.Shutdown(ctx) }()

	r := <-got
	if r.err != nil {
		t.Fatal(r.err)
	}
	if !r.resp.Close {
		t.Fatal("response written after Shutdown began did not carry Connection: close")
	}
	if err := <-shutdown; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
