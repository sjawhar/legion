package bus

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// options starts from nats.GetDefaultOptions: Connect fills in only some zero fields, and the
// defaults are what turn on reconnecting, the client's pings and the drain and flusher timeouts. A
// lost server is reconnected in place, forever, so JetStream handles taken from the connection
// keep working; ReconnectedCB re-subscribes and runs the reconnect hooks. ClosedCB fires on Close
// or Drain, and also when nats.go gives up on the connection itself: the same authorization error
// twice in a row, or a server -ERR it does not classify. Those two are why onClosed still starts a
// recovery that dials a replacement. The reconnect buffer is off, so nothing is held and sent after its caller
// saw an error: a publish while reconnecting waits for the reconnect instead
// (ensureConnWithContext), and a publish that fails was not sent.
func options(name string, urls []string, reconnectCB func(*nats.Conn), closedCB func()) nats.Options {
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
	opts.ReconnectedCB = func(nc *nats.Conn) {
		slog.Info("envoy nats reconnected", slog.String("name", name), slog.String("url", nc.ConnectedUrl()))
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
		switch {
		case err == nats.ErrConsumerNotFound:
			// A drain ends each subscription whose consumer nats.go created, then deletes that
			// consumer. At shutdown the listener leaves a KV watcher the last reconnect has not yet
			// replaced for the drain to end, and that watcher's ordered consumer can already be gone:
			// a NATS restart loses it outright (nats.go creates it with memory storage), and a
			// disconnect longer than its inactive threshold lets the server delete it. So the delete
			// finds it gone: the state the delete was for. At the pinned nats.go (v1.50.0) that
			// delete, in checkDrained, is the only report of the bare sentinel, which
			// DeleteConsumer returns; re-check that on a nats.go bump. The one other report of this
			// error, an ordered consumer nats.go failed to recreate, wraps it and stays an ERROR.
			level = slog.LevelWarn
		case errors.Is(err, nats.ErrConsumerNotActive):
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

func connect(name string, urls []string, credential nats.Option, dialer *socks5Dialer, reconnectCB func(*nats.Conn), closedCB func()) (*nats.Conn, error) {
	return connectWithContext(context.Background(), name, urls, credential, dialer, reconnectCB, closedCB)
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
func connectWithContext(ctx context.Context, name string, urls []string, credential nats.Option, dialer *socks5Dialer, reconnectCB func(*nats.Conn), closedCB func()) (_ *nats.Conn, err error) {
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
		next := options(name, urls, reconnectCB, closedCB)
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
