package bus

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
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

// newReconnectErrThrottle is a throttle on now, time.Now when now is nil.
func newReconnectErrThrottle(now func() time.Time) *reconnectErrThrottle {
	if now == nil {
		now = time.Now
	}
	return &reconnectErrThrottle{now: now}
}

// silentCutPingInterval and silentCutMaxPingsOut are the pings of WithSilentCutDetection. nats.go
// counts a ping out at each interval and declares the connection stale on the interval that would
// put a third one out, and any PONG clears the count, so a server that stops answering is found two
// to three seconds later: inside the five seconds envoy-dispatch's /healthz has to report it in,
// while a PONG may still take two seconds before a loaded server is taken for a cut one. A
// connection that is cut reconnects as one whose server closed it does.
const (
	silentCutPingInterval = time.Second
	silentCutMaxPingsOut  = 2
)

// detectSilentCuts is the nats.Option WithSilentCutDetection adds to every connection of a client.
func detectSilentCuts(o *nats.Options) error {
	o.PingInterval = silentCutPingInterval
	o.MaxPingsOut = silentCutMaxPingsOut
	return nil
}

// allOf applies each option that is not nil, in order.
func allOf(options ...nats.Option) nats.Option {
	return func(o *nats.Options) error {
		for _, option := range options {
			if option == nil {
				continue
			}
			if err := option(o); err != nil {
				return err
			}
		}
		return nil
	}
}

// connectInBackground is the nats.Option WithConnectInBackground adds to the first connection of a
// client, the one its constructor returns, and to no replacement ensureConnWithContext dials. nats.go
// returns that connection whether or not a server answered and connects it in place, retrying as
// it reconnects, forever (MaxReconnect -1), and never closes it over a refused credential: by
// default it closes a connection the same server refuses twice in a row, which here would leave the
// process with no bus until a recovery found it. connected runs on the first connection, after
// nats.go has sent the core subscriptions made before it.
//
// A refusal of the credential (authorizationRefusal) is logged at ERROR, at most once per
// reconnectErrEvery of refusals: a refusal that lasts is a grant someone has to fix, and nats.go
// retries it about once a second. Once the connection has connected, nats.go reports each refusal
// to the async error callback, which takes the line here in place of its own; before then it
// reports none, and reportRefusalsUntilConnected logs them on the same throttle.
func connectInBackground(name string, urls []string, refusals *reconnectErrThrottle, connected func(*nats.Conn)) nats.Option {
	return func(o *nats.Options) error {
		o.RetryOnFailedConnect = true
		o.IgnoreAuthErrorAbort = true
		connectedCB, reconnectedCB, report := o.ConnectedCB, o.ReconnectedCB, o.AsyncErrorCB
		o.ConnectedCB = func(nc *nats.Conn) {
			refusals.reset()
			slog.Info("envoy nats connected", slog.String("name", name), slog.String("url", natsURLWithoutUserinfo(nc.ConnectedUrl())))
			if connectedCB != nil {
				connectedCB(nc)
			}
			if connected != nil {
				connected(nc)
			}
		}
		o.ReconnectedCB = func(nc *nats.Conn) {
			refusals.reset()
			if reconnectedCB != nil {
				reconnectedCB(nc)
			}
		}
		o.AsyncErrorCB = func(nc *nats.Conn, sub *nats.Subscription, err error) {
			if authorizationRefusal(err) {
				if refusals.allow() {
					logRefusal(name, urls, err)
				}
				return
			}
			if report != nil {
				report(nc, sub, err)
			}
		}
		return nil
	}
}

// reportRefusalsUntilConnected logs each refusal of the credential while nc makes its first
// connection, at ERROR and on refusals' throttle, and returns once nc has connected or closed.
// nats.go tells no callback of a refusal before a connection has first connected (it reports only
// a dial that failed, through ReconnectErrCB), but keeps it as the connection's last error until
// its next attempt, a reconnect wait later, so a read each second sees every run of refusals.
func reportRefusalsUntilConnected(nc *nats.Conn, name string, urls []string, refusals *reconnectErrThrottle) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for !nc.IsConnected() && !nc.IsClosed() {
		if err := nc.LastError(); authorizationRefusal(err) && refusals.allow() {
			logRefusal(name, urls, err)
		}
		<-ticker.C
	}
}

// connectFirstInBackground makes the connection a constructor returns with WithConnectInBackground.
// dial connects with the option it is handed added to the constructor's own settings, and here
// that option is connectInBackground, so nats.go dials alone until the connection first connects.
// The refusals of the credential before then are logged by reportRefusalsUntilConnected, on the
// throttle connectInBackground logs the later ones on, a throttle on now (newReconnectErrThrottle).
//
// install, when not nil, records the connection dial returned; an error from it closes the
// connection and is returned. connected, when not nil, runs on the first connection, and never
// before install has returned: nats.go can connect the connection as soon as dial returns, and
// connected may read what install records.
func connectFirstInBackground(name string, urls []string, now func() time.Time, dial func(nats.Option) (*nats.Conn, error), connected func(*nats.Conn), install func(*nats.Conn) error) (*nats.Conn, error) {
	refusals := newReconnectErrThrottle(now)
	installed := make(chan struct{})
	defer close(installed)
	first := func(nc *nats.Conn) {
		<-installed
		if connected != nil {
			connected(nc)
		}
	}
	nc, err := dial(connectInBackground(name, urls, refusals, first))
	if err != nil {
		return nil, err
	}
	if install != nil {
		if err := install(nc); err != nil {
			nc.Close()
			return nil, err
		}
	}
	go reportRefusalsUntilConnected(nc, name, urls, refusals)
	return nc, nil
}

// authorizationRefusal reports whether err is the server refusing the connection's credential:
// one it does not know, or one whose authorization expired or was revoked.
func authorizationRefusal(err error) bool {
	return errors.Is(err, nats.ErrAuthorization) || errors.Is(err, nats.ErrAuthExpired) ||
		errors.Is(err, nats.ErrAuthRevoked) || errors.Is(err, nats.ErrAccountAuthExpired)
}

// logRefusal writes the line a refused credential is reported with, naming the servers as the
// reconnect failure's line does (serverList).
func logRefusal(name string, urls []string, err error) {
	slog.Error("envoy nats authorization refused",
		slog.String("name", name),
		slog.String("servers", serverList(urls)),
		slog.String("error", err.Error()))
}

// handshaking says whether nc is in a reconnect's handshake (CONNECTING) or between its attempts
// (RECONNECTING), the two states a connection the server closed during that handshake is in when
// its async error callback runs.
func handshaking(nc *nats.Conn) bool {
	if nc == nil {
		return false
	}
	status := nc.Status()
	return status == nats.CONNECTING || status == nats.RECONNECTING
}

// serverList is urls as a log line names them: comma-separated, each without its userinfo
// (natsURLWithoutUserinfo).
func serverList(urls []string) string {
	servers := make([]string, len(urls))
	for i, raw := range urls {
		servers[i] = natsURLWithoutUserinfo(raw)
	}
	return strings.Join(servers, ",")
}

// connectCallbacks are what a connection tells its client of its own events. nats.go runs every
// callback of one connection on that connection's dispatcher goroutine, one at a time and in the
// order of the events, so none of them may block: a reconnect callback that waits holds the
// disconnect behind it (Client.queueReconnect runs the reconnect hooks off that goroutine for
// exactly that reason).
type connectCallbacks struct {
	// reconnected, when not nil, runs on each connect and reconnect of the connection, the first
	// connection of a client that connects in the background excepted: that one reports through
	// connectFirstInBackground's connected instead, once the client has installed it.
	reconnected func(*nats.Conn)
	// closed, when not nil, runs once the connection is closed: by Close or Drain, or by nats.go
	// giving up on it (Client.onClosed).
	closed func()
	// asyncErr, when not nil, takes each error the connection reports on its own, after the line
	// this package logs for it (Client.onAsyncError).
	asyncErr func(*nats.Conn, error)
	// state, when not nil, is told at each of those events whether the connection is connected: a
	// connect or a reconnect reports it up, a disconnect or a close reports it down. It is what a
	// client keeps its lock-free connected flag by (Client.noteConnection), since nats.go holds
	// the connection's own lock across a reconnect dial that can hang for the connect timeout,
	// and nats.Conn.Status waits for it.
	state func(*nats.Conn, bool)
}

// options starts from nats.GetDefaultOptions: Connect fills in only some zero fields, and the
// defaults are what turn on reconnecting, the client's pings and the drain and flusher timeouts. A
// lost server is reconnected in place, forever, so JetStream handles taken from the connection
// keep working; ReconnectedCB re-subscribes and runs the reconnect hooks. ClosedCB fires on Close
// or Drain, and also when nats.go gives up on the connection itself: the same authorization error
// twice in a row, or a server -ERR it does not classify. Those two are why onClosed still starts a
// recovery that dials a replacement. The reconnect buffer is off, so nothing is held and sent after its caller
// saw an error: a publish while reconnecting waits for the reconnect instead
// (ensureConnWithContext), and a publish that fails was not sent. AsyncErrorCB logs each error the
// connection reports on its own and then hands it to callbacks.asyncErr, where the client reads
// the server's refusal of its core subscription (onAsyncError).
func options(name string, urls []string, callbacks connectCallbacks, now func() time.Time) nats.Options {
	// The throttle is built here, one per call, so it is per connection and has its own mutex:
	// options is rebuilt for every dial attempt in connectWithContext's loop and only the attempt
	// that connects leaves its closures on the *nats.Conn, so one live connection has exactly one
	// throttle and a second client's first failure is never swallowed by this one's minute.
	throttle := newReconnectErrThrottle(now)
	// note tells callbacks.state of each event, so a caller that reads the flag it keeps reads
	// what the last event of this connection said.
	note := func(nc *nats.Conn, up bool) {
		if callbacks.state != nil {
			callbacks.state(nc, up)
		}
	}
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
	opts.DisconnectedErrCB = func(nc *nats.Conn, err error) {
		note(nc, false)
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
	reconnectAttemptFailed := func(err error) {
		if !throttle.allow() {
			return
		}
		text := "unknown"
		if err != nil {
			text = err.Error()
		}
		slog.Warn("envoy nats reconnect attempt failed",
			slog.String("name", name),
			slog.String("servers", serverList(urls)),
			slog.String("error", text))
	}
	opts.ReconnectErrCB = func(_ *nats.Conn, err error) { reconnectAttemptFailed(err) }
	opts.ReconnectedCB = func(nc *nats.Conn) {
		note(nc, true)
		// Each outage reports its own first failed attempt at once, rather than staying silent for
		// what is left of the previous outage's minute.
		throttle.reset()
		// ConnectedUrl is the server's URL.String(), userinfo and all, and a server nats.go
		// discovered inherits the configured URL's (nats.go v1.50.0 nats.go:2025-2027);
		// ConnectedUrlRedacted keeps a token, which is a user name alone.
		slog.Info("envoy nats reconnected", slog.String("name", name), slog.String("url", natsURLWithoutUserinfo(nc.ConnectedUrl())))
		if callbacks.reconnected != nil {
			callbacks.reconnected(nc)
		}
	}
	// A connection that connects only after failing to (connectInBackground) reports its first
	// connection here rather than as a reconnect, and that ends its outage as a reconnect does.
	opts.ConnectedCB = func(nc *nats.Conn) {
		note(nc, true)
		throttle.reset()
	}
	opts.ClosedCB = func(nc *nats.Conn) {
		note(nc, false)
		slog.Info("envoy nats connection closed", slog.String("name", name))
		if callbacks.closed != nil {
			callbacks.closed()
		}
	}
	opts.AsyncErrorCB = func(nc *nats.Conn, sub *nats.Subscription, err error) {
		if sub == nil && errors.Is(err, io.EOF) && handshaking(nc) {
			// A server that closes the connection during a reconnect's CONNECT and PING handshake
			// (a server restarting, or one that accepted the dial as it shut down) makes nats.go's
			// sendConnect report the read's io.EOF here, and nowhere else: the reconnect loop then
			// tries again without calling ReconnectErrCB (nats.go v1.50.0 nats.go:2930-2938,
			// 3251-3258). It is a failed reconnect attempt, so it is logged as one, on the same
			// throttle. Only that report is: the connection is still in that handshake
			// (CONNECTING) or waiting for the next attempt (RECONNECTING) when the callback runs,
			// and every other async error, an EOF on a connected connection included, stays an
			// ERROR.
			reconnectAttemptFailed(err)
			if callbacks.asyncErr != nil {
				callbacks.asyncErr(nc, err)
			}
			return
		}
		level := slog.LevelError
		if errors.Is(err, nats.ErrConsumerNotActive) {
			// Every consumer Envoy runs with idle heartbeats is a KV watcher's ordered consumer,
			// which reports missed heartbeats only while the connection is not connected; once it
			// is connected again, nats.go resets the consumer instead. The report restates the
			// disconnect logged above, once per watcher. The listener's durable carries no heartbeat:
			// cmd/listener's ensureListenerDurable refuses one that does before the lease holder
			// subscribes to it.
			level = slog.LevelWarn
		}
		if sub != nil {
			slog.Log(context.Background(), level, "envoy nats async error", slog.String("name", name), slog.String("subject", sub.Subject), slog.String("error", err.Error()))
		} else {
			slog.Log(context.Background(), level, "envoy nats async error", slog.String("name", name), slog.String("error", err.Error()))
		}
		if callbacks.asyncErr != nil {
			callbacks.asyncErr(nc, err)
		}
	}
	return opts
}

// connectTimeout bounds each dial of a NATS server and its handshake: nats.go's own for a direct
// dial (options), the bus's SOCKS5 dialer's for one through a proxy (socks5Proxy). A connect with a
// deadline cuts either to what remains of it.
const connectTimeout = 5 * time.Second

func connect(name string, urls []string, settings nats.Option, dialer *socks5Dialer, callbacks connectCallbacks, now func() time.Time) (*nats.Conn, error) {
	return connectWithContext(context.Background(), name, urls, settings, dialer, callbacks, now)
}

// connectWithContext dials urls with envoy's options, then settings when it is not nil: the NATS
// user nkeyCredential names, and what the client's options add (connectOpts.connectionSettings,
// connectInBackground). When dialer is not nil it dials through the SOCKS5 proxy it names
// (socks5Proxy): every connection then, the first and each one nats.go dials to reconnect, goes
// through the proxy, handed the server's host name as the URL spells it.
//
// A connect through a proxy that fails names the setting and the proxy, whatever failed: its ten
// attempts ran out, the credential was refused, or ctx ended first (a publish's own deadline). A
// dial error comes back with the proxy's address but not the setting, and a refused dial as
// nats.go's ErrNoServers alone, which names neither, so the proxy's own dial error stands in for
// it. The address carries no credentials: CheckSOCKS5Proxy admits only an IP address with no zone
// or a DNS name.
func connectWithContext(ctx context.Context, name string, urls []string, settings nats.Option, dialer *socks5Dialer, callbacks connectCallbacks, now func() time.Time) (_ *nats.Conn, err error) {
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
		next := options(name, urls, callbacks, now)
		if settings != nil {
			if err := settings(&next); err != nil {
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
