package bus

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// reconnectErrEvery is how often one connection reports a failed reconnect attempt. nats.go
// retries about once a second forever (ReconnectWait, MaxReconnect -1), so an outage of an hour
// would be thousands of identical lines; one a minute still names the outage in every log window
// that matters.
const reconnectErrEvery = time.Minute

// reconnectErrThrottle lets the first failed reconnect attempt of an outage through and then one
// per reconnectErrEvery of its clock. now is the clock so a test can pass a fake one: ./internal/bus
// runs under -race with parallel tests, so there is no package-level clock any test could swap.
type reconnectErrThrottle struct {
	mu   sync.Mutex
	now  func() time.Time
	last time.Time
}

// allow reports whether this failure is logged: the first one is, and after it only one per
// reconnectErrEvery.
func (t *reconnectErrThrottle) allow() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	at := t.now()
	if !t.last.IsZero() && at.Sub(t.last) < reconnectErrEvery {
		return false
	}
	t.last = at
	return true
}

// reset forgets the last allowed failure, so the first failure of the next outage is logged at
// once rather than swallowed by the previous outage's minute.
func (t *reconnectErrThrottle) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last = time.Time{}
}

// options starts from nats.GetDefaultOptions: Connect fills in only some zero fields, and the
// defaults are what turn on reconnecting, the client's pings and the drain and flusher timeouts. A
// lost server is reconnected in place, forever, so JetStream handles taken from the connection
// keep working; ReconnectedCB re-subscribes and runs the reconnect hooks. ClosedCB fires on Close
// or Drain, and also when nats.go gives up on the connection itself: the same authorization error
// twice in a row, or a server -ERR it does not classify. Those two are why onClosed still starts a
// recovery that dials a replacement. The reconnect buffer is off, so nothing is held and sent after its caller
// saw an error: a publish while reconnecting waits for the reconnect instead
// (ensureConnWithContext), and a publish that fails was not sent.
func options(name string, urls []string, reconnectCB func(*nats.Conn), closedCB func(), now func() time.Time) nats.Options {
	if now == nil {
		now = time.Now
	}
	// The throttle is built here, one per call, so it is per connection and has its own mutex:
	// options is rebuilt for every dial attempt in connectWithContext's loop and only the attempt
	// that connects leaves its closures on the *nats.Conn, so one live connection has exactly one
	// throttle and a second client's first failure is never swallowed by this one's minute.
	throttle := &reconnectErrThrottle{now: now}
	opts := nats.GetDefaultOptions()
	opts.Servers = urls
	opts.Name = name
	opts.NoRandomize = true
	opts.Timeout = connectTimeout
	opts.MaxReconnect = -1
	// A publish while reconnecting waits for the reconnect (ensureConnWithContext), so the wait
	// between attempts is how long it waits after NATS is back.
	opts.ReconnectWait = time.Second
	opts.ReconnectBufSize = -1
	opts.DisconnectedErrCB = func(_ *nats.Conn, err error) {
		if err != nil {
			slog.Info("envoy nats disconnected", slog.String("name", name), slog.String("error", err.Error()))
			return
		}
		slog.Info("envoy nats disconnected", slog.String("name", name))
	}
	// A reconnect attempt that fails is otherwise silent: nats.go retries in place forever, so a
	// broker that is down leaves no line at all in the process's log. The servers come from urls,
	// the list this client was configured with, never from the connection: nc.ConnectedUrl() is
	// empty while reconnecting. The line goes through the package-level slog, like every other bus
	// line, because a listener process test fails if a bus line stops being Go's text format
	// (cmd/listener/startup_test.go:598-612 names the deployed metric filters). Each server is
	// written without its userinfo (natsURLWithoutUserinfo), because nats.go authenticates with a
	// URL's userinfo, a token or a user and password, when the URL carries one. The nkey credential
	// never reaches a line: options never sees it, since nkeyCredential is applied to the options
	// value afterwards (connect.go, connectWithContext) and holds a public key and a signer only.
	opts.ReconnectErrCB = func(_ *nats.Conn, err error) {
		if !throttle.allow() {
			return
		}
		text := "unknown"
		if err != nil {
			text = err.Error()
		}
		servers := make([]string, len(urls))
		for i, raw := range urls {
			servers[i] = natsURLWithoutUserinfo(raw)
		}
		slog.Warn("envoy nats reconnect attempt failed",
			slog.String("name", name),
			slog.String("servers", strings.Join(servers, ",")),
			slog.String("error", text))
	}
	opts.ReconnectedCB = func(nc *nats.Conn) {
		// Each outage reports its own first failed attempt at once, rather than staying silent for
		// what is left of the previous outage's minute.
		throttle.reset()
		// ConnectedUrl is the server's URL.String(), userinfo and all, and a server nats.go
		// discovered inherits the configured URL's (nats.go v1.50.0 nats.go:2025-2027);
		// ConnectedUrlRedacted keeps a token, which is a user name alone.
		slog.Info("envoy nats reconnected", slog.String("name", name), slog.String("url", natsURLWithoutUserinfo(nc.ConnectedUrl())))
		if reconnectCB != nil {
			reconnectCB(nc)
		}
	}
	opts.ClosedCB = func(_ *nats.Conn) {
		slog.Info("envoy nats connection closed", slog.String("name", name))
		if closedCB != nil {
			closedCB()
		}
	}
	opts.AsyncErrorCB = func(_ *nats.Conn, sub *nats.Subscription, err error) {
		level := slog.LevelError
		if errors.Is(err, nats.ErrConsumerNotActive) {
			// Every consumer Envoy runs with idle heartbeats is a KV watcher's ordered consumer,
			// which reports missed heartbeats only while the connection is not connected; once it
			// is connected again, nats.go resets the consumer instead. The report restates the
			// disconnect logged above, once per watcher. The listener's durable carries no heartbeat
			// as long as cmd/listener's startListenerSubscription bound it, since that refuses one that
			// does; a replaced connection binds the same durable again without that check.
			level = slog.LevelWarn
		}
		if sub != nil {
			slog.Log(context.Background(), level, "envoy nats async error", slog.String("name", name), slog.String("subject", sub.Subject), slog.String("error", err.Error()))
			return
		}
		slog.Log(context.Background(), level, "envoy nats async error", slog.String("name", name), slog.String("error", err.Error()))
	}
	return opts
}

// connectTimeout bounds each dial of a NATS server and its handshake: nats.go's own for a direct
// dial (options), the bus's SOCKS5 dialer's for one through a proxy (socks5Proxy). A connect with a
// deadline cuts either to what remains of it.
const connectTimeout = 5 * time.Second

func connect(name string, urls []string, credential nats.Option, dialer *socks5Dialer, reconnectCB func(*nats.Conn), closedCB func(), now func() time.Time) (*nats.Conn, error) {
	return connectWithContext(context.Background(), name, urls, credential, dialer, reconnectCB, closedCB, now)
}

// connectWithContext dials urls with envoy's options, when credential is not nil as the NATS user
// it names (nkeyCredential), and when dialer is not nil through the SOCKS5 proxy it names
// (socks5Proxy): every connection then, the first and each one nats.go dials to reconnect, goes
// through the proxy, handed the server's host name as the URL spells it.
//
// A connect through a proxy that fails names the setting and the proxy, whatever failed: its ten
// attempts ran out, the credential was refused, or ctx ended first (a publish's own deadline). A
// dial error comes back with the proxy's address but not the setting, and a refused dial as
// nats.go's ErrNoServers alone, which names neither, so the proxy's own dial error stands in for
// it. The address carries no credentials: CheckSOCKS5Proxy admits only an IP address with no zone
// or a DNS name.
func connectWithContext(ctx context.Context, name string, urls []string, credential nats.Option, dialer *socks5Dialer, reconnectCB func(*nats.Conn), closedCB func(), now func() time.Time) (_ *nats.Conn, err error) {
	if dialer != nil {
		defer func() {
			if err != nil {
				err = fmt.Errorf("bus: NATS connect through the SOCKS5 proxy %s=%s failed: %w", SOCKS5ProxyEnvVar, dialer.Address(), err)
			}
		}()
	}
	var lastErr error
	for range 10 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		next := options(name, urls, reconnectCB, closedCB, now)
		if credential != nil {
			if err := credential(&next); err != nil {
				return nil, err
			}
		}
		attemptDialer := dialer
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				// Not ctx.Err(): the deadline can pass before the context's timer marks it done,
				// and a nil error here would hand the caller a nil connection to use.
				return nil, context.DeadlineExceeded
			}
			if remaining < next.Timeout {
				next.Timeout = remaining
			}
			if dialer != nil {
				attemptDialer = dialer.bounded(remaining)
			}
		}
		if attemptDialer != nil {
			next.CustomDialer = attemptDialer
			// nats.go otherwise resolves the host itself and hands the dialer an address, which the
			// proxy cannot reach when only its resolver knows the name.
			next.SkipHostLookup = true
		}
		type connectionResult struct {
			conn *nats.Conn
			err  error
		}
		result := make(chan connectionResult, 1)
		go func() {
			conn, err := next.Connect()
			result <- connectionResult{conn: conn, err: err}
		}()
		select {
		case connected := <-result:
			if connected.err == nil {
				return connected.conn, nil
			}
			lastErr = connected.err
		case <-ctx.Done():
			go func() {
				connected := <-result
				if connected.conn != nil {
					connected.conn.Close()
				}
			}()
			return nil, ctx.Err()
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if dialer != nil && errors.Is(lastErr, nats.ErrNoServers) {
		lastErr = cmp.Or(dialer.LastError(), lastErr)
	}
	return nil, lastErr
}
