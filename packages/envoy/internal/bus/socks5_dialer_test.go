package bus

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// proxiedLookup is a connect's environment that names the proxy at address.
func proxiedLookup(address string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		if key == SOCKS5ProxyEnvVar {
			return address, true
		}
		return "", false
	}
}

// A proxied dial ends when the connect's deadline does, as a direct dial does (nats.go applies a
// connect's shortened timeout only to its own dialer). A proxy that accepts the connection and
// never answers the SOCKS5 greeting holds the dial open: the client closes it at the connect's
// one-second deadline, not at the bus's five-second connect timeout.
func TestAProxiedDialEndsAtTheConnectsDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the silent proxy: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	closed := make(chan time.Time, 16)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				// Read until the client closes its end: the proxy answers nothing.
				buf := make([]byte, 1<<10)
				for {
					if _, err := conn.Read(buf); err != nil {
						closed <- time.Now()
						return
					}
				}
			}()
		}
	}()
	dialer, err := socks5Proxy(proxiedLookup(listener.Addr().String()))
	if err != nil {
		t.Fatalf("read the proxy setting: %v", err)
	}

	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := connectWithContext(ctx, "deadline-test", []string{"nats://nats.example.test:4222"}, nil, dialer, nil, nil)
	if err == nil {
		conn.Close()
		t.Fatal("a connect through a proxy that never answers succeeded")
	}
	select {
	case at := <-closed:
		if held := at.Sub(started); held > 2500*time.Millisecond {
			t.Fatalf("the proxied dial held the proxy's connection %s, past the connect's one-second deadline", held)
		}
	case <-time.After(4 * time.Second):
		t.Fatalf("the proxied dial still held the proxy's connection %s after the connect's one-second deadline", time.Since(started))
	}
}

// Two connects through one dialer at once record their dial errors in the one record and read it
// back, as a client's background recovery and a publish's replacement dial do: each dial stores on
// the goroutine nats.go dials from while the other connect loads at the end of its attempts. Run
// under -race (the envoy-go-race job), a record that is not synchronized fails here. Each connect
// makes the bus's ten attempts a second apart, so this takes about ten seconds.
func TestConnectsSharingADialerRecordItsLastError(t *testing.T) {
	dialer, err := socks5Proxy(proxiedLookup("127.0.0.1:1"))
	if err != nil {
		t.Fatalf("read the proxy setting: %v", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 500 * time.Millisecond)
			conn, err := connectWithContext(context.Background(), "shared-dialer-test", []string{"nats://nats.example.test:4222"}, nil, dialer, nil, nil)
			if conn != nil {
				conn.Close()
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err == nil {
			t.Fatalf("connect %d through a dead proxy succeeded", i)
		}
		for _, want := range []string{SOCKS5ProxyEnvVar, "127.0.0.1:1", "connection refused"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("connect %d error %q does not name %q", i, err, want)
			}
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("connect %d error %q is a deadline's, with no deadline set", i, err)
		}
	}
}
