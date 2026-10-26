package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Unset or empty, DISPATCH_PPROF_PORT names no address, and startPprof listens on nothing.
func TestPprofUnsetStartsNoListener(t *testing.T) {
	for _, value := range []string{"", "  "} {
		addr, err := pprofAddress(envGetter(map[string]string{"DISPATCH_PPROF_PORT": value}))
		if err != nil || addr != "" {
			t.Fatalf("DISPATCH_PPROF_PORT=%q: pprofAddress = %q, %v; want no address", value, addr, err)
		}
	}
	bound, err := startPprof(context.Background(), "")
	if err != nil || bound != nil {
		t.Fatalf("startPprof with no address = %v, %v; want no listener", bound, err)
	}
}

// A valid port binds 127.0.0.1 alone, and nothing else: the address it names is the loopback
// one, whatever the port.
func TestPprofAddressIsLoopbackOnly(t *testing.T) {
	for _, port := range []string{"1", "6060", "65535", " 6060 "} {
		addr, err := pprofAddress(envGetter(map[string]string{"DISPATCH_PPROF_PORT": port}))
		want := "127.0.0.1:" + strings.TrimSpace(port)
		if err != nil || addr != want {
			t.Errorf("DISPATCH_PPROF_PORT=%q: pprofAddress = %q, %v; want %q", port, addr, err, want)
		}
	}
}

// A value that is not a port 1-65535 refuses startup, naming the variable.
func TestPprofInvalidPortRefusesStartup(t *testing.T) {
	for _, value := range []string{"0", "65536", "-1", "+6060", "abc", "6060x", "127.0.0.1:6060", "18446744073709552616"} {
		if _, err := pprofAddress(envGetter(map[string]string{"DISPATCH_PPROF_PORT": value})); err == nil || !strings.Contains(err.Error(), "DISPATCH_PPROF_PORT") {
			t.Errorf("DISPATCH_PPROF_PORT=%q: err = %v; want a refusal naming DISPATCH_PPROF_PORT", value, err)
		}
		refusedWith(t, map[string]string{"DISPATCH_PPROF_PORT": value}, "DISPATCH_PPROF_PORT")
	}
}

// The listener serves the pprof index on 127.0.0.1 alone, from its own mux, and stops when the
// process's shutdown context ends.
func TestPprofServesTheIndexOnLoopbackAndStopsWithTheContext(t *testing.T) {
	port := freeLoopbackPort(t)
	addr, err := pprofAddress(envGetter(map[string]string{"DISPATCH_PPROF_PORT": strconv.Itoa(port)}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bound, err := startPprof(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	tcp, ok := bound.(*net.TCPAddr)
	if !ok || !tcp.IP.Equal(net.IPv4(127, 0, 0, 1)) || tcp.Port != port {
		t.Fatalf("bound address = %v; want 127.0.0.1:%d", bound, port)
	}
	response, err := http.Get("http://" + bound.String() + "/debug/pprof/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "goroutine") {
		t.Fatalf("GET /debug/pprof/ = %d %q; want the pprof index", response.StatusCode, body)
	}
	// Only the pprof routes, from a mux of its own: neither the server's routes nor anything on
	// http.DefaultServeMux (where importing net/http/pprof also registers the profiler) is here.
	sentinel := "/debug/pprof-default-mux-sentinel-" + t.Name()
	http.HandleFunc(sentinel, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	for _, path := range []string{"/healthz", sentinel} {
		if response, err := http.Get("http://" + bound.String() + path); err != nil || response.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s on the pprof listener = %v, %v; want 404", path, response, err)
		} else {
			response.Body.Close()
		}
	}

	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", bound.String(), 100*time.Millisecond)
		if err != nil {
			break
		}
		conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("the pprof listener still accepts connections after the shutdown context ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A port another process holds is a startup error naming the variable, never a silent skip.
func TestPprofPortInUseIsAnError(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if _, err := startPprof(context.Background(), held.Addr().String()); err == nil || !strings.Contains(err.Error(), "DISPATCH_PPROF_PORT") {
		t.Fatalf("startPprof on a held port: err = %v; want an error naming DISPATCH_PPROF_PORT", err)
	}
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}
