package bus

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
)

// ConnectOption configures a client: Connect and ConnectOwningStream take every one. Dial takes
// only the DialOptions among them.
type ConnectOption interface{ apply(*connectOpts) }

// DialOption is a ConnectOption Dial honours too: WithConnectInBackground, WithSilentCutDetection
// and WithReconnectErrClock. Dial's name and environment are its parameters, so WithName and
// WithEnvironment are not DialOptions, and neither are the options of a client's JetStream
// (WithReplicas, WithPublishAcknowledgementClock).
type DialOption func(*connectOpts)

func (o DialOption) apply(opts *connectOpts) { o(opts) }

// clientOption is a ConnectOption only Connect and ConnectOwningStream honour.
type clientOption func(*connectOpts)

func (o clientOption) apply(opts *connectOpts) { o(opts) }

type connectOpts struct {
	name                        string
	replicas                    int
	streamMaxBytes              int64
	keepInterval                time.Duration
	keptBuckets                 []string
	publishAcknowledgementClock AcknowledgementClock
	reconnectErrClock           func() time.Time
	environment                 func(string) (string, bool)
	reconnectWaitTick           func()
	background                  bool
	silentCutDetection          bool
}

// connectionSettings is what every connection of a client applies after envoy's own options
// (connectWithContext): the NATS user credential names (nkeyCredential, nil for none), and the
// pings of WithSilentCutDetection when the client asked for them.
func (o connectOpts) connectionSettings(credential nats.Option) nats.Option {
	if !o.silentCutDetection {
		return credential
	}
	return allOf(credential, detectSilentCuts)
}

// parseConnectOptions applies options over the defaults every connect starts from.
func parseConnectOptions[O ConnectOption](options []O) connectOpts {
	opts := connectOpts{name: "envoy", replicas: 1, keepInterval: defaultKeepInterval, publishAcknowledgementClock: wallClock{}, environment: os.LookupEnv}
	for _, o := range options {
		o.apply(&opts)
	}
	return opts
}

// streamConfig is the stream ConnectOwningStream ensures: streamCfg at the deployment's replica
// count and byte limit.
func (o connectOpts) streamConfig() nats.StreamConfig {
	cfg := *streamCfg
	cfg.Replicas = o.replicas
	cfg.MaxBytes = o.streamMaxBytes
	return cfg
}

// AcknowledgementClock supplies deadline contexts for JetStream publish acknowledgements.
type AcknowledgementClock interface {
	WithTimeout(context.Context, time.Duration) (context.Context, context.CancelFunc)
}

type wallClock struct{}

func (wallClock) WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeout)
}

// WithPublishAcknowledgementClock overrides the clock that bounds JetStream publish acknowledgements.
func WithPublishAcknowledgementClock(clock AcknowledgementClock) ConnectOption {
	return clientOption(func(o *connectOpts) { o.publishAcknowledgementClock = clock })
}

// WithReconnectErrClock overrides the clock the bus reads for the time-based parts of its
// reconnect reporting: the throttle of the warning a failed reconnect attempt logs
// (reconnectErrThrottle) and the stall HookStall reports, so a test can see a minute pass without
// waiting one. nil, the default, is time.Now.
func WithReconnectErrClock(now func() time.Time) DialOption {
	return func(o *connectOpts) { o.reconnectErrClock = now }
}

// ReplicasEnvVar and StreamMaxBytesEnvVar are the deployment settings WithReplicas and
// WithStreamMaxBytes carry. Each program reads them itself and hands the bus their values; the bus
// names them in a server's refusal of a stream or a bucket that carries one (refusedSettings).
const (
	ReplicasEnvVar       = "ENVOY_NATS_REPLICAS"
	StreamMaxBytesEnvVar = "ENVOY_STREAM_MAX_BYTES"
)

// MaxReplicas is the most copies NATS keeps of a stream or a bucket, and so the largest
// ReplicasEnvVar ParseNATSReplicas takes. aws:WebhookListener's rendering
// (github.com/sjawhar/envoy/infra/aws/rendering) refuses a larger natsReplicas by this bound, so a
// component never deploys a listener its configuration would refuse.
const MaxReplicas = 5

// ParseNATSReplicas reads ReplicasEnvVar, the deployment's replica count: unset or blank is 1,
// and otherwise a whole number from 1 to MaxReplicas. Anything else (0, a negative number, more
// than NATS keeps, a word) is refused naming the variable and the value, at load rather than as a
// server's refusal at the stream. envoy-listener (config.Load in
// github.com/sjawhar/envoy/internal/config) and envoy-dispatch read the setting through it. Every
// program of a deployment carries the same value: one with a lower value can lower the stream
// until the others' keepers raise it back (ConnectOwningStream).
func ParseNATSReplicas(raw string) (int, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 1, nil
	}
	replicas, err := strconv.Atoi(value)
	if err != nil || replicas < 1 || replicas > MaxReplicas {
		return 0, fmt.Errorf("invalid %s %q: want a whole number of copies from 1 to %d", ReplicasEnvVar, value, MaxReplicas)
	}
	return replicas, nil
}

// ParseStreamMaxBytes reads StreamMaxBytesEnvVar, the stream's byte limit: unset or blank is 0,
// which sets no limit (a deployed stream keeps the one it has), and otherwise a positive whole
// number of bytes. 0, a negative number or a word is refused naming the variable and the value.
// envoy-listener (config.Load) and envoy-dispatch read the setting through it. A deployed limit is
// never lowered (WithStreamMaxBytes), so programs with different limits leave the largest.
func ParseStreamMaxBytes(raw string) (int64, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, nil
	}
	limit, err := strconv.ParseInt(value, 10, 64)
	if err != nil || limit < 1 {
		return 0, fmt.Errorf("invalid %s %q: want a positive whole number of bytes, or unset for no limit", StreamMaxBytesEnvVar, value)
	}
	return limit, nil
}

// WithReplicas sets the deployment's replica count, ReplicasEnvVar, for the stream
// ConnectOwningStream ensures (default 1): the count the stream is created at, and the count a
// deployed stream with fewer copies is raised to, at the start (ensureStreamWithConfig) and by the
// client's keeper after it (keepStream). A start never writes fewer copies than it read, but a
// concurrent start with a lower count can put back the copies it read, until the keeper raises
// them again a period later: the count holds only while every program of the deployment carries it.
func WithReplicas(n int) ConnectOption {
	return clientOption(func(o *connectOpts) { o.replicas = n })
}

// WithStreamMaxBytes sets the deployment's byte limit, StreamMaxBytesEnvVar, for the stream
// ConnectOwningStream ensures: the stream's MaxBytes at create, and the limit a deployed stream with
// a smaller one or none is raised to, at the start (ensureStreamWithConfig) and by the client's
// keeper after it (keepStream). Neither ever lowers a deployed limit, so an operator's raise of a
// full stream stands and programs with different limits leave the largest; lowering one is an
// operator step once every program's setting is lowered. A full stream refuses a new publish
// (ErrStreamFull) and keeps every stored message, which leaves only by age, so the limit must sit
// above what the stream takes in its age. 0, the default, sets none: the stream is created with no
// byte limit, and a deployed stream keeps the limit it has.
func WithStreamMaxBytes(n int64) ConnectOption {
	return clientOption(func(o *connectOpts) { o.streamMaxBytes = n })
}

// WithKeptBuckets names the KV buckets this program opens with EnsureKeyValue at the deployment's
// replica count, which the keeper of a ConnectOwningStream client raises back to WithReplicas'
// count each period (keep). Each name must be a bucket the program's NATS grant covers: the
// keeper reads and updates each one's stream. A named bucket that does not exist yet is skipped
// until the program creates it.
func WithKeptBuckets(buckets ...string) ConnectOption {
	return clientOption(func(o *connectOpts) { o.keptBuckets = buckets })
}

// WithEnvironment names where a connect reads ENVOY_ALLOW_REMOTE_NATS (AllowRemoteEnvVar),
// ENVOY_NATS_SOCKS5_PROXY (SOCKS5ProxyEnvVar), NATS_NKEY_SEED_FILE and NATS_NKEY_SEED, in place of
// the process environment (os.LookupEnv): envoy-dispatch hands it the lookup over its settings
// table, which lists all four. Dial takes the same lookup as its environment parameter, and not
// this option.
func WithEnvironment(lookup func(string) (string, bool)) ConnectOption {
	return clientOption(func(o *connectOpts) { o.environment = lookup })
}

// WithName names the client, in place of "envoy": the name the NATS server's connection listings
// (its /connz endpoint, `nats server report connections`) show for the connection the client dials
// first and for each one it dials to replace a closed one, and the name field of the client's
// lifecycle log lines (`envoy nats disconnected`, `envoy nats recovery attempt` and the rest). Dial
// takes its name as a parameter, and not this option.
func WithName(name string) ConnectOption {
	return clientOption(func(o *connectOpts) { o.name = name })
}

// WithConnectInBackground has the constructor (Connect, ConnectOwningStream, Dial) return without
// waiting for NATS. A configuration error is still returned before any dial: an unusable nkey seed,
// a server ENVOY_ALLOW_REMOTE_NATS does not allow, an unusable SOCKS5 setting. A server that does
// not answer, or refuses the credential, is not an error: the client is returned with a connection
// nats.go has yet to connect, which nats.go then connects in place, retrying forever as it does a
// reconnect (connectInBackground). A refusal of the credential is logged at ERROR at most once a
// minute and retried, never given up on. Until that first connection Connected reports false, a
// publish waits for it within its own deadline and then fails as one does while reconnecting, a
// core subscription is sent once it connects, and a JetStream subscription's bind fails while its
// registration stays, so the first connection binds it. Only nats.go dials before then: the
// constructor's connection is never closed, so nothing replaces it (ensureConnWithContext). The
// first connection is a connection event, as a reconnect is (onConnect): the JetStream
// subscription is bound and the reconnect hooks run, and an owning client (ConnectOwningStream)
// reconciles the stream then, as its first hook, rather than in the constructor.
//
// envoy-dispatch's server takes it for each of its clients, so a bus that is down when it boots
// leaves it serving everything that does not need the bus, and connected once the bus answers.
func WithConnectInBackground() DialOption {
	return func(o *connectOpts) { o.background = true }
}

// WithSilentCutDetection has every connection the client makes declare itself lost two to three
// seconds after the server stops answering it (detectSilentCuts), where nats.go's defaults take
// four to six minutes. A server that goes away cleanly closes the socket, which the client sees at
// once; one whose packets are dropped (a partition, a node that vanished) closes nothing, and only
// the client's own pings notice. envoy-dispatch's server takes it for each of its clients, so its
// /healthz reports a cut bus within seconds of the cut.
func WithSilentCutDetection() DialOption {
	return func(o *connectOpts) { o.silentCutDetection = true }
}

type subscriptionTransport uint8

const (
	jetStreamSubscription subscriptionTransport = iota
	coreSubscription
	subscriptionCount
)

// clientStopMode is how the client was stopped, which decides what a bind that ends after the stop
// does with what it bound (endBind).
type clientStopMode uint8

const (
	clientRunning clientStopMode = iota
	// clientDraining: Drain is taking the subscriptions it drains itself. A bind that ends now
	// installs for its next pass.
	clientDraining
	// clientDrainCollected: Drain has taken the last of those subscriptions, just before it drains
	// the connection. A bind that ends now drains what it bound.
	clientDrainCollected
	// clientClosed: Close stopped the client. A bind that ends now unsubscribes what it bound.
	clientClosed
)

type recoverableSubscription struct {
	transport subscriptionTransport
	subject   string
	queue     string
	handler   nats.MsgHandler
	opts      []nats.SubOpt
	active    *nats.Subscription
	// binding is set while a bind of this registration is in flight, and closed when it ends. A
	// registration has one bind at a time: a restore leaves one that has a bind in flight, and a
	// registration that replaces it waits for that bind first.
	binding chan struct{}
	// refused is set when the server refused the subscription the client last sent for this
	// registration on its current connection (onAsyncError), and cleared each time it is sent
	// again: by a bind, and by nats.go itself on a reconnect in place (onReconnect). nats.go keeps
	// a refused subscription valid, so without it a lane the server will not deliver to would read
	// as subscribed. Only the core registration is ever marked.
	refused bool
}

// Client is a NATS connection that publishes notifications and holds at most one JetStream
// subscription and one core subscription, which it keeps across a reconnect and binds again on a
// connection it dials to replace a closed one. Subscribe and SubscribeCore enforce that: a call on
// the subject (and queue group) the transport already holds replaces its subscription, and any
// other is refused with ErrSubscriptionHeld, so a caller that needs two durables takes one Client
// per durable.
type Client struct {
	Conn                        *nats.Conn
	js                          nats.JetStreamContext
	urls                        []string
	name                        string
	connectionSettings          nats.Option
	dialer                      *socks5Dialer
	publishAcknowledgementClock AcknowledgementClock
	reconnectErrClock           func() time.Time
	// reconnectWaitTick, when not nil, runs after each tick of ensureConnWithContext's wait for a
	// reconnect, before the connection is read again. It is nil outside tests: a test sets it
	// (WithReconnectWaitTick, export_test.go) to know a publish is inside that wait, or to act at
	// the moment the wait ends.
	reconnectWaitTick func()
	mu                sync.Mutex

	// subscriptionsMu guards subscriptions and stopMode. It is held to read and write them, never
	// across a bind's request to the server, so a reconnect's restore, the health reads and Drain
	// never wait out a request the reconnect lost.
	subscriptionsMu sync.Mutex
	subscriptions   [subscriptionCount]recoverableSubscription
	stopMode        clientStopMode

	reconnectHooksMu sync.Mutex
	reconnectHooks   []func(*nats.Conn) error
	// drainHooks run once in Drain, after the delivery subscriptions and before the connection's
	// own drain (AddDrainHook), under reconnectHooksMu as the reconnect hooks are.
	drainHooks []func(context.Context)
	// rewatchMu holds a run of the reconnect hooks (rewatch), so two never run at once.
	rewatchMu sync.Mutex
	// connEvents counts the connection events the reconnect hooks follow, under mu: each connection
	// the client installs after its first, and each reconnect of the current one in place.
	// hookedEvents is the count the last run that succeeded began at. The hooks are due while the
	// two differ.
	connEvents   uint64
	hookedEvents uint64
	// hooksDueSince is when the oldest connection event the hooks have not covered was counted,
	// under mu, and the zero time while the hooks are current: a second event that lands while they
	// are already due keeps the first one's time, since the stall HookStall reports is how long the
	// state behind the hooks, a KV cache among it, has been off its buckets, not how long the last
	// event has waited. lastHookErr is the error of the last run that failed, so the stall is
	// reported with the reason for it.
	hooksDueSince time.Time
	lastHookErr   error
	// drainedReconnects is how many times c.Conn had reconnected in place when Drain stopped the
	// client, set under mu in the step that stops it, so a reconnect callback can tell a reconnect
	// before the drain from one during it (closeStopped). Once the client is stopped c.Conn never
	// changes (ensureConnWithContext), so the count needs no connection beside it.
	drainedReconnects uint64
	// awaitingConnection is true, under mu, from the constructor of a client that connects in the
	// background (WithConnectInBackground) until nats.go has connected its connection for the
	// first time (onConnect), and false for every other client.
	awaitingConnection bool

	// recovery state
	recovering int32
	stopCh     chan struct{}
	closeOnce  sync.Once

	// keeperDone is closed when the keeper of a ConnectOwningStream client ends (keep), nil on a
	// client that runs none, or none yet: one that connects in the background starts it on its first
	// reconcile (ensureStreamOnce). It is written and read under mu (startKeeper, awaitKeeper).
	keeperDone chan struct{}
}

// Dial opens a tuned core NATS connection using envoy's standard options (5s connect timeout,
// infinite reconnect every second, retry-loop for the initial 10 attempts), as the NATS user
// environment names (nkeyCredential), through the SOCKS5 proxy it names (socks5Proxy). A caller
// that needs core pub/sub on a connection of its own uses it: nats.go re-subscribes core
// subscriptions on reconnect by itself, so it needs none of Client's recovery, and it touches no
// stream. It refuses a NATS server that is not this machine's on the same terms as Connect,
// reading ENVOY_ALLOW_REMOTE_NATS from environment too, which answers as os.LookupEnv does
// (WithEnvironment).
//
// It takes the DialOptions: WithConnectInBackground, which returns the connection before it has
// connected (a core subscription made on it before then is sent once it connects),
// WithSilentCutDetection and WithReconnectErrClock.
//
// For JetStream-backed publishing or a durable consumer, use Connect; to reconcile the stream
// this codebase owns, ConnectOwningStream.
func Dial(name string, urls []string, environment func(string) (string, bool), options ...DialOption) (*nats.Conn, error) {
	opts := parseConnectOptions(options)
	if err := refuseRemoteNATS(urls, environment); err != nil {
		return nil, err
	}
	credential, err := nkeyCredential(environment)
	if err != nil {
		return nil, err
	}
	dialer, err := socks5Proxy(environment)
	if err != nil {
		return nil, err
	}
	settings := opts.connectionSettings(credential)
	dial := func(first nats.Option) (*nats.Conn, error) {
		return connect(name, urls, allOf(settings, first), dialer, nil, nil, nil, opts.reconnectErrClock)
	}
	if opts.background {
		return connectFirstInBackground(name, urls, opts.reconnectErrClock, dial, nil, nil)
	}
	return dial(nil)
}

// Connect opens a client that publishes and subscribes without touching the shared
// ENVOY_NOTIFICATIONS stream. Every caller that only publishes or only tails uses it -- natstail,
// the MCP bridge and envoy-dispatch's operator commands own nothing on the server they reach, and
// a stream ensure from one of them rewrites a resource several deployments share (LEGION-249).
// envoy-dispatch's delivery intakes use it too: each binds its durable on the stream Dispatch's
// publisher owns (ConnectOwningStream), and leaves the stream itself alone.
//
// Every client, from Connect or ConnectOwningStream, connects as the NATS user the environment
// names: NATS_NKEY_SEED_FILE or NATS_NKEY_SEED (nkeyCredential). An unusable seed is an error
// before any dial; neither variable set connects without a credential. It connects through the
// SOCKS5 proxy the environment names (ENVOY_NATS_SOCKS5_PROXY, socks5Proxy), or directly when it
// names none. Every connection the client dials later, to recover, is the same user's, through
// the same proxy.
//
// For the JetStream stream this codebase owns, use ConnectOwningStream.
func Connect(urls []string, options ...ConnectOption) (*Client, error) {
	return newClient(urls, false, parseConnectOptions(options))
}

// ConnectOwningStream opens a client that also reconciles ENVOY_NOTIFICATIONS against this
// binary's subjects, retention and duplicate window and the deployment's replica count and byte
// limit (WithReplicas, WithStreamMaxBytes; ensureStreamWithConfig), creating the stream when the
// server has none. Only the deployed services call it: envoy-listener and envoy-dispatch's server.
// The client then keeps the stream until it stops: every 30 seconds its keeper puts back the
// copies, the byte limit, the stream's id, the age, the duplicate window and discard that a
// concurrent start or an operator took away, adds back each of this binary's subjects the stream
// lacks that overlaps none it carries, and removes no subject (keepStream). It also raises each
// bucket WithKeptBuckets names that has fewer copies than WithReplicas' count (raiseReplicas).
func ConnectOwningStream(urls []string, options ...ConnectOption) (*Client, error) {
	return newClient(urls, true, parseConnectOptions(options))
}

// newClient makes the client Connect and ConnectOwningStream return. An owning client reconciles
// the stream (ensureStreamWithConfig) and then starts its keeper (startKeeper): in the constructor
// once install has returned, or, connecting in the background (WithConnectInBackground), in the
// first connection's reconnect hook once its reconcile has succeeded (ensureStreamOnce), so its
// keeper runs no pass before that reconcile.
func newClient(urls []string, ownsStream bool, opts connectOpts) (*Client, error) {
	if err := refuseRemoteNATS(urls, opts.environment); err != nil {
		return nil, err
	}
	credential, err := nkeyCredential(opts.environment)
	if err != nil {
		return nil, err
	}
	dialer, err := socks5Proxy(opts.environment)
	if err != nil {
		return nil, err
	}
	c := &Client{
		urls:                        urls,
		name:                        opts.name,
		connectionSettings:          opts.connectionSettings(credential),
		dialer:                      dialer,
		publishAcknowledgementClock: opts.publishAcknowledgementClock,
		reconnectErrClock:           opts.reconnectErrClock,
		reconnectWaitTick:           opts.reconnectWaitTick,
		stopCh:                      make(chan struct{}),
	}
	stream := opts.streamConfig()
	dial := func(first nats.Option) (*nats.Conn, error) {
		return connect(c.name, urls, allOf(c.connectionSettings, first), dialer, c.onReconnect, c.onClosed, c.onAsyncError, c.reconnectErrClock)
	}
	if opts.background {
		c.awaitingConnection = true
		// The keeper starts once the first connection's reconcile has succeeded (ensureStreamOnce),
		// not here: a pass before it could write back a subject the reconcile replaced.
		if ownsStream {
			c.AddReconnectHook(c.ensureStreamOnce(&stream, opts.keptBuckets, opts.keepInterval))
		}
		// onConnect counts the first connection only once it is c.Conn, which install makes it. An
		// install that fails returns no client, so it stops the client before
		// connectFirstInBackground closes the connection, as the foreground path does below. At the
		// pinned nats.go (v1.50.0) it cannot fail: JetStream with these options returns no error, and
		// there is no stream to reconcile here. The stop guards a nats.go whose JetStream can.
		install := func(nc *nats.Conn) error {
			if err := c.install(nc, nil); err != nil {
				c.stop()
				return err
			}
			return nil
		}
		if _, err := connectFirstInBackground(c.name, urls, c.reconnectErrClock, dial, c.onConnect, install); err != nil {
			return nil, err
		}
		return c, nil
	}
	nc, err := dial(nil)
	if err != nil {
		return nil, err
	}
	var owned *nats.StreamConfig
	if ownsStream {
		owned = &stream
	}
	if err := c.install(nc, owned); err != nil {
		// The constructor returns no client, so no caller can Close it: the client is stopped
		// before nc closes, so the recovery nc's close begins (onClosed) finds it stopped and dials
		// nothing. Running, that recovery would dial and install a connection held by no one, which
		// with MaxReconnect -1 reconnects for the rest of the process once its server is gone.
		c.stop()
		nc.Close()
		return nil, err
	}
	if ownsStream {
		c.startKeeper(stream, opts.keptBuckets, opts.keepInterval)
	}
	return c, nil
}

// install makes nc the client's connection, with the JetStream context taken from it, once it has
// reconciled stream on nc (ensureStreamWithConfig) when stream is not nil.
func (c *Client) install(nc *nats.Conn, stream *nats.StreamConfig) error {
	js, err := nc.JetStream(nats.MaxWait(10*time.Second), nats.PublishAsyncTimeout(publishAcknowledgementTimeout))
	if err != nil {
		return err
	}
	if stream != nil {
		if err := ensureStreamWithConfig(js, stream); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.Conn, c.js = nc, js
	c.mu.Unlock()
	return nil
}

// ensureStreamOnce is the reconnect hook an owning client that connects in the background
// reconciles the stream with (ensureStreamWithConfig) on its first connection, where every other
// owning client's constructor does. It is the client's first hook, and the hooks run one at a time
// (rewatch), so ensured needs no lock of its own. A run that fails leaves the hooks due, so a
// recovery runs it again; once one has succeeded it does nothing, as no owning client reconciles
// the stream again on a later connection. The run that succeeds starts the client's keeper with
// buckets and interval (startKeeper), so no pass of it runs before the reconcile: a pass that read
// the stream before the reconcile's write and wrote after it would put back a subject the
// reconcile replaced, and no later reconcile would remove it. A client stopped before then never
// starts one.
func (c *Client) ensureStreamOnce(stream *nats.StreamConfig, buckets []string, interval time.Duration) func(*nats.Conn) error {
	ensured := false
	return func(*nats.Conn) error {
		if ensured {
			return nil
		}
		if err := ensureStreamWithConfig(c.JS(), stream); err != nil {
			return fmt.Errorf("reconcile the %s stream: %w", Stream, err)
		}
		ensured = true
		c.startKeeper(*stream, buckets, interval)
		return nil
	}
}

// awaitingFirstConnection reports whether the client connects in the background and nats.go has
// yet to connect it for the first time.
func (c *Client) awaitingFirstConnection() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.awaitingConnection
}

func (c *Client) JS() nats.JetStreamContext {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.js
}

// Connected reports whether the client currently has a live NATS connection.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Conn != nil && c.Conn.Status() == nats.CONNECTED
}

// RepairGrace is how long the reconnect hooks may be uncovered before a failed run of them is an
// operator's business. A failure under it is run again by the recovery within seconds
// (recoverUntilHealthy), so it is reported at WARN; at or past it the hooks have still not covered
// their connection event, the state behind them is stale, and the report is an ERROR.
// internal/kvwatch waits the same constant for a repair of a watcher that stopped before it
// reports it, so both halves of the same outage report at the same minute.
const RepairGrace = time.Minute

// HookStall reports how long the reconnect hooks have been due without a run that succeeded, zero
// while none is due, and the error of the last run that failed. That error is nil when no run has
// failed since the last run that succeeded (rewatch clears it on every success, even one a
// connection event overtook), so a caller can read a stall with no error behind it, which
// cmd/listener's /healthz reports as the stall alone. That handler is a reader, as
// reportHookFailure is, so this takes the client's state under mu and touches no connection, as
// Connected and SubOK do.
func (c *Client) HookStall() (time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hooksDueSince.IsZero() {
		return 0, nil
	}
	// A clock a test moves itself can stand before the event it timed, and a wall clock can step
	// back, so the stall never reads as a negative duration.
	return max(c.now().Sub(c.hooksDueSince), 0), c.lastHookErr
}

// now reads the clock the client takes for its reconnect reporting (WithReconnectErrClock),
// time.Now when it took none.
func (c *Client) now() time.Time {
	if c.reconnectErrClock == nil {
		return time.Now()
	}
	return c.reconnectErrClock()
}

// markHooksDueLocked counts a connection event the reconnect hooks have yet to cover, under mu,
// and starts the stall at it when the hooks were current. An event that lands while they are
// already due leaves the stall where it is: it is the first uncovered event that says how long the
// hooks' state has been stale.
func (c *Client) markHooksDueLocked() {
	c.connEvents++
	if c.hooksDueSince.IsZero() {
		c.hooksDueSince = c.now()
	}
}

// Subscribe creates the client's recoverable JetStream subscription. The client keeps one
// JetStream subscription: a later call on the same subject replaces it, unsubscribing the handle
// an earlier one returned, and a call on another subject is refused with ErrSubscriptionHeld. On a
// client that connects in the background and has not connected yet, the bind fails (nothing
// reaches the server before then) and the registration stays, so the first connection binds it.
func (c *Client) Subscribe(subject string, handler nats.MsgHandler, opts ...nats.SubOpt) (*nats.Subscription, error) {
	c.mu.Lock()
	conn, js := c.Conn, c.js
	c.mu.Unlock()
	return c.registerSubscription(recoverableSubscription{
		transport: jetStreamSubscription,
		subject:   subject,
		handler:   handler,
		opts:      opts,
	}, conn, js)
}

// SubscribeCore creates a recoverable core NATS subscription. The optional queue
// identifies the stable queue group that shares role-lane delivery between
// overlapping listeners. The client keeps one core subscription: a later call on the same
// subject and queue replaces it, unsubscribing the handle an earlier one returned, and any other
// call is refused with ErrSubscriptionHeld. A client that connects in the background and has not
// connected yet binds it on the connection it has, which nats.go sends once it connects: waiting
// for that connection here would wait out the outage.
func (c *Client) SubscribeCore(subject string, handler nats.MsgHandler, queues ...string) (*nats.Subscription, error) {
	if !c.awaitingFirstConnection() {
		if err := c.ensureConn(); err != nil {
			return nil, err
		}
	}
	queue := ""
	if len(queues) > 0 {
		queue = queues[0]
	}
	c.mu.Lock()
	conn, js := c.Conn, c.js
	c.mu.Unlock()
	return c.registerSubscription(recoverableSubscription{
		transport: coreSubscription,
		subject:   subject,
		queue:     queue,
		handler:   handler,
	}, conn, js)
}

// registerSubscription makes next the transport's subscription and binds it. The registration
// stays when the bind fails, so a later restore binds it. A bind of the transport's subscription
// already in flight, a restore's or another registration's, is waited for first. Once Drain or
// Close has stopped the client it registers nothing, as a restore binds nothing: replacing the
// transport's subscription would unsubscribe one the drain is letting finish. A registration on
// another subject or queue group than the transport's subscription is refused (ErrSubscriptionHeld).
func (c *Client) registerSubscription(next recoverableSubscription, conn *nats.Conn, js nats.JetStreamContext) (*nats.Subscription, error) {
	c.subscriptionsMu.Lock()
	subscription := &c.subscriptions[next.transport]
	for subscription.binding != nil {
		inFlight := subscription.binding
		c.subscriptionsMu.Unlock()
		<-inFlight
		c.subscriptionsMu.Lock()
	}
	if c.stopped() {
		c.subscriptionsMu.Unlock()
		return nil, errStopped
	}
	// A registration on another subject, or the same subject in another queue group, would replace
	// a subscription its caller still relies on, and stop it delivering with nothing logged: two of
	// Dispatch's delivery intakes on one client lost a durable that way (LEGION-690). It is refused,
	// and the held subscription goes on delivering.
	if subscription.handler != nil && (subscription.subject != next.subject || subscription.queue != next.queue) {
		held := describeSubscription(subscription.subject, subscription.queue)
		c.subscriptionsMu.Unlock()
		return nil, fmt.Errorf("%w: holds %s, asked for %s", ErrSubscriptionHeld, held, describeSubscription(next.subject, next.queue))
	}
	// Registering the held subject again replaces the transport's subscription, so the one it
	// replaces stops delivering: the listener's self-health rebuild re-registers after its durable
	// consumer was lost, and the old handle, bound to that consumer's deliver inbox, would otherwise
	// stay subscribed for good. For a consumer the library created, Unsubscribe also deletes it, as
	// it always does. It runs before the bind, which a durable still bound to the old handle would
	// refuse.
	replaced := subscription.active
	// next is unrefused: the bind below sends its subscription anew, and a refusal of it arrives
	// only after the send.
	*subscription = next
	subscription.binding = make(chan struct{})
	c.subscriptionsMu.Unlock()
	if replaced != nil {
		_ = replaced.Unsubscribe()
	}
	sub, err := bind(next, conn, js)
	if err := c.endBind(subscription, sub, err); err != nil {
		return nil, err
	}
	return sub, nil
}

// describeSubscription names a subscription in ErrSubscriptionHeld's error: its subject, and its
// queue group when it has one.
func describeSubscription(subject, queue string) string {
	if queue == "" {
		return fmt.Sprintf("%q", subject)
	}
	return fmt.Sprintf("%q in queue group %q", subject, queue)
}

// bind subscribes registration's handler, through js for a JetStream subscription and on conn for
// a core one. It is the request to the server, so no lock of the client's is held across it.
func bind(registration recoverableSubscription, conn *nats.Conn, js nats.JetStreamContext) (*nats.Subscription, error) {
	if registration.transport == jetStreamSubscription {
		return js.Subscribe(registration.subject, registration.handler, registration.opts...)
	}
	if registration.queue == "" {
		return conn.Subscribe(registration.subject, registration.handler)
	}
	return conn.QueueSubscribe(registration.subject, registration.queue, registration.handler)
}

// endBind ends the bind in flight of the registration subscription holds. A bind that ends during
// Drain's collection installs for the next pass; after that collection it drains itself. A Close
// never installs a bind that finishes after it stopped the client.
func (c *Client) endBind(subscription *recoverableSubscription, sub *nats.Subscription, err error) error {
	c.subscriptionsMu.Lock()
	close(subscription.binding)
	subscription.binding = nil
	mode := c.stopMode
	if err == nil && (mode == clientRunning || mode == clientDraining) {
		subscription.active = sub
	}
	c.subscriptionsMu.Unlock()

	if err != nil {
		return err
	}
	switch mode {
	case clientDrainCollected:
		_ = sub.Drain()
		return errStopped
	case clientClosed:
		_ = sub.Unsubscribe()
		return errStopped
	}
	return nil
}

// SubOK reports whether the durable listener subscription is active on a live connection.
func (c *Client) SubOK() bool {
	return c.subscriptionOK(jetStreamSubscription)
}

// CoreSubOK reports whether the client's core subscription is active on a live connection and the
// server has not refused it. It reads the subscription the client holds now, not the handle
// SubscribeCore returned: a recovery onto a connection dialed to replace a closed one binds a
// replacement, and the returned handle stays invalid for good. A subscription the server refused
// stays valid in nats.go, which only records the refusal, and one it re-sends on a reconnect in
// place is refused again by a server that took the grant away during the outage, so the refusal
// is read from the server's own report of it (onAsyncError) until the subscription is sent again.
func (c *Client) CoreSubOK() bool {
	return c.subscriptionOK(coreSubscription)
}

func (c *Client) subscriptionOK(transport subscriptionTransport) bool {
	c.mu.Lock()
	connOK := c.Conn != nil && c.Conn.Status() != nats.CLOSED
	c.mu.Unlock()
	c.subscriptionsMu.Lock()
	subscription := &c.subscriptions[transport]
	subOK := subscription.active != nil && subscription.active.IsValid() && !subscription.refused
	c.subscriptionsMu.Unlock()
	return connOK && subOK
}

// subscriptionRefusal reads the server's refusal of a subscription: the subject, the queue group
// when there is one, and the sid when the refusal is a configuration reload's revocation of a
// subscription it had accepted. A refusal of a subscription the client sent reads `Permissions
// Violation for Subscription to "<subject>" using queue "<queue>"` (nats-server v2.10.29
// client.go:4959-4964); a revocation reads `Permissions Violation for Subscription to "<subject>"
// (sid "<sid>")` (client.go:5300) and names no queue group whether the subscription had one or not.
// The server writes each name with %q, so each is matched as a whole Go-quoted string, escapes
// included, and unquoted before it is compared: a name holding a double quote or a backslash
// reaches the text escaped. nats.go's own reading (permissionsRe and permissionsQueueRe, nats.go
// v1.50.0 nats.go:3795-3833) keeps the escapes, so it compares unequal to such a name.
var subscriptionRefusal = regexp.MustCompile(`Subscription to ("(?:[^"\\]|\\.)*")(?: using queue ("(?:[^"\\]|\\.)*"))?(?: \(sid ("(?:[^"\\]|\\.)*")\))?`)

// onAsyncError is wired as the connection's async error callback, after the error is logged. A
// server's refusal of the client's core subscription marks it refused for CoreSubOK. Any other
// error, a refusal of a publish or of another subscription among them, changes nothing, and so
// does one a connection the client has since replaced reports late.
//
// A refusal of a subscription the client sent names its subject and, when it has one, its queue
// group, and both must be the core registration's. A reload's revocation names the subject and the
// sid but never the queue group, so its subject alone identifies the core registration: the client
// holds one core subscription, and nats.go itself leaves that refusal unrecorded on a queue
// subscription and the subscription valid, though the server has removed it.
//
// The mark cannot outlive the refusal: nats.go queues the reconnect callback, which clears it
// (onReconnect), before any refusal of the subscriptions it re-sent, since it holds the
// connection's lock from the re-send until that callback is queued and reads the server's -ERR
// only under that lock, and both callbacks run in order on one goroutine. c.mu is held across the
// mark so a replacement connection cannot be installed between the check and the mark.
func (c *Client) onAsyncError(nc *nats.Conn, err error) {
	if !errors.Is(err, nats.ErrPermissionViolation) {
		return
	}
	match := subscriptionRefusal.FindStringSubmatch(err.Error())
	if match == nil {
		return
	}
	subject, err := strconv.Unquote(match[1])
	if err != nil {
		return
	}
	queue := ""
	if match[2] != "" {
		if queue, err = strconv.Unquote(match[2]); err != nil {
			return
		}
	}
	revoked := match[2] == "" && match[3] != ""
	c.mu.Lock()
	defer c.mu.Unlock()
	if nc != c.Conn {
		return
	}
	c.subscriptionsMu.Lock()
	defer c.subscriptionsMu.Unlock()
	core := &c.subscriptions[coreSubscription]
	if core.handler != nil && core.subject == subject && (revoked || core.queue == queue) {
		core.refused = true
	}
}

// SubSubject is the subject the subscription this client holds on the JetStream transport actually
// delivers on, which is what the listener compares against its durable consumer's configured
// DeliverSubject. The registration's own subject is useless for that: registerSubscription stores
// subject: "" for a nats.Bind registration (cmd/listener/durable.go binds with an empty subject),
// while the subscription nats.go returns carries the consumer's configured DeliverSubject —
// measured against a real server for INFRA-182, sub.Subject equals info.Config.DeliverSubject for
// a nats.Bind push subscription. The empty string is the honest answer while the client is
// replacing the subscription it holds, and the listener reads it as unbound.
func (c *Client) SubSubject() string {
	c.subscriptionsMu.Lock()
	defer c.subscriptionsMu.Unlock()
	active := c.subscriptions[jetStreamSubscription].active
	if active == nil || !active.IsValid() {
		return ""
	}
	return active.Subject
}

// ErrSubscriptionHeld is what Subscribe and SubscribeCore return for a registration on another
// subject, or the same subject in another queue group, than the subscription the client already
// holds on that transport. The held subscription goes on delivering; a caller that needs another
// durable takes another Client.
var ErrSubscriptionHeld = errors.New("bus: client already holds a subscription of this transport")

// errStopped is what a client refuses once Drain or Close stopped it: dialling or installing a
// connection, or re-subscribing.
var errStopped = errors.New("bus: client is stopped")

// stop stops the client: recovery ends, and nothing dials, installs a connection or re-subscribes.
func (c *Client) stop() {
	c.closeOnce.Do(func() { close(c.stopCh) })
}

func (c *Client) stopped() bool {
	select {
	case <-c.stopCh:
		return true
	default:
		return false
	}
}

// Close stops any recovery goroutine and the keeper, and closes the underlying NATS connection. It
// returns once the keeper has ended (awaitKeeper).
func (c *Client) Close() {
	c.subscriptionsMu.Lock()
	if c.stopMode == clientRunning {
		c.stopMode = clientClosed
	}
	c.subscriptionsMu.Unlock()
	c.stop()
	c.mu.Lock()
	if c.Conn != nil {
		c.Conn.Close()
	}
	c.mu.Unlock()
	c.awaitKeeper()
}

// Drain stops the client and drains it, letting the deliveries already in their handlers finish,
// and returns once the connection has closed, at the latest once timeout passes, so a blocked
// drain cannot keep a process alive. A connection closed while nats.go drains it can read
// DRAINING_PUBS again for about 5 s after Drain returns (see the wait below). It stops the client
// first, so neither the drain's close nor a recovery already under way reconnects or re-subscribes
// a process that is shutting down. It then drains the delivery subscriptions while the connection
// still accepts new ones: a handler finishing its delivery may subscribe (RequestCoreTo's receipt
// inbox), which a draining connection refuses. Next it runs the drain hooks (AddDrainHook), which
// end the KV watchers with requests of their own while the connection still carries them, even
// when the deliveries ran out their share of the timeout (drainHookReserve) or failed to drain. Only
// then does it drain the connection, whose Drain only starts the drain, and wait for it to close
// itself. A connection that is reconnecting is closed at once and runs no hook: nothing it sends
// reaches the server, so no subscription would drain and a reconnect would re-send them.
// A bind in flight at the stop is not waited for: one that ends while the subscriptions drain
// installs its own, which the next pass drains, and one that ends after the last pass drains its
// own, which the connection's drain takes (endBind). The close ends one still in flight. A
// reconnect nats.go reports once the client has stopped closes the connection only when it came
// after the stop (closeStopped). The stop ends the keeper too, which Drain waits for once the
// connection is closed (awaitKeeper).
func (c *Client) Drain(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	deliveriesDeadline := deadline.Add(-min(drainHookReserve, timeout*3/10))
	c.subscriptionsMu.Lock()
	if c.stopMode == clientRunning {
		c.stopMode = clientDraining
	}
	c.subscriptionsMu.Unlock()
	// The stop and the reconnect count are one step under mu, so a reconnect callback that finds
	// the client stopped also finds the count (closeStopped).
	c.mu.Lock()
	c.stop()
	conn := c.Conn
	c.drainedReconnects = conn.Stats().Reconnects
	c.mu.Unlock()
	defer c.awaitKeeper()
	defer conn.Close()
	if conn.IsReconnecting() {
		return fmt.Errorf("drain NATS: %w", nats.ErrConnectionReconnecting)
	}
	if err := c.drainDeliveries(deliveriesDeadline); err != nil {
		// The deliveries did not drain in their share. The hooks still end the KV watchers within
		// what is left, so the close does not leave their consumers for the server to time out.
		c.runDrainHooks(conn, deadline)
		return err
	}
	// The drain hooks end what the connection's drain cannot end cleanly, such as the KV watchers'
	// ordered consumers, whose delete would race the connection's drain (AddDrainHook), and they
	// run while the connection still carries their requests.
	c.runDrainHooks(conn, deadline)
	// nats.go's Drain only starts the drain, and the wait for it to close the connection takes the
	// connection's CLOSED event rather than polling for the state. A close that lands while nats.go
	// drains the connection (a lost server, a reconnect callback closing it) is undone at the
	// pinned nats.go (v1.50.0) and in every release through v1.54.0: drainConnection marks the
	// closed connection DRAINING_PUBS again and waits 5 s for a PONG on the socket close() shut, so
	// a poll can miss the moment the connection was closed and wait out the deadline. For the same
	// reason the connection can read DRAINING_PUBS for about 5 s after Drain returns; the deferred
	// Close finds it already closed and does not end that wait. nats-io/nats.go#2163 fixes it and
	// is unreleased; re-check on a nats.go bump. Once a release that carries it is pinned, only this
	// explanation goes: the CLOSED-event wait stays correct.
	closed := conn.StatusChanged(nats.CLOSED)
	defer conn.RemoveStatusListener(closed)
	if err := conn.Drain(); err != nil {
		return err
	}
	expired := time.NewTimer(time.Until(deadline))
	defer expired.Stop()
	select {
	case <-closed:
		return nil
	case <-expired.C:
		return fmt.Errorf("drain NATS: %w", context.DeadlineExceeded)
	}
}

// drainHookReserve is the part of Drain's timeout kept from the delivery subscriptions for the
// drain hooks and the connection's own drain, so a delivery still in its handler near the deadline
// cannot leave the hooks no time and make each KV watcher's consumer delete fail at ERROR. A
// delivery's handler can take seconds (a role-lane forward waits up to 2 s for its receipt, a
// session delivery up to its request limit), while a hook's delete is one round trip per watcher,
// run for every watcher at once, and the connection's drain a flush and a close: 3 s is
// comfortably more than one round trip even over a relay. The listener drains for 13 s, so its
// deliveries keep the 10 s they had before the hooks and the hooks get their 3 s on top. A drain
// shorter than 10 s keeps 30% of its timeout instead, so its deliveries still get most of it.
const drainHookReserve = 3 * time.Second

// drainDeliveries drains the delivery subscriptions, pass after pass while a bind in flight
// installs one more, and waits until each is no longer valid, at the latest until deadline.
func (c *Client) drainDeliveries(deadline time.Time) error {
	waitUntil := func(done func() bool) error {
		for !done() {
			if !time.Now().Before(deadline) {
				return fmt.Errorf("drain NATS: %w", context.DeadlineExceeded)
			}
			time.Sleep(10 * time.Millisecond)
		}
		return nil
	}
	for {
		delivering := c.deliveringSubscriptions()
		if len(delivering) == 0 {
			return nil
		}
		for _, subscription := range delivering {
			if err := subscription.Drain(); err != nil {
				return err
			}
		}
		for _, subscription := range delivering {
			if err := waitUntil(func() bool { return !subscription.IsValid() }); err != nil {
				return err
			}
		}
	}
}

// deliveringSubscriptions returns the installed subscriptions that are still valid, those a pass
// of Drain has yet to drain. Finding none during a Drain, it records that Drain has taken its last
// (clientDrainCollected).
func (c *Client) deliveringSubscriptions() []*nats.Subscription {
	c.subscriptionsMu.Lock()
	defer c.subscriptionsMu.Unlock()
	var delivering []*nats.Subscription
	for _, subscription := range c.subscriptions {
		if subscription.active != nil && subscription.active.IsValid() {
			delivering = append(delivering, subscription.active)
		}
	}
	if len(delivering) == 0 && c.stopMode == clientDraining {
		c.stopMode = clientDrainCollected
	}
	return delivering
}

func (c *Client) ensureConn() error {
	return c.ensureConnWithContext(context.Background())
}

// ensureConnWithContext returns once the client has a usable connection. A connection nats.go is
// reconnecting is waited for, until it reconnects, ctx ends or the client stops: with the
// reconnect buffer off, a publish while reconnecting would otherwise fail at once, and a webhook
// that fails answers 503 to a sender that does not redeliver. A closed connection is replaced by
// dialling a new one, unless the client is stopped. Callers that find the same connection closed
// at once (the recovery its ClosedCB starts, and a publish) each dial one, and only the first to
// finish replaces it: the others close theirs and serve with that one. So every connection the
// client dials is either c.Conn, which Close and Drain close, or closed already; one left open
// beside it would outlive Close, and, with MaxReconnect -1, reconnect forever once its server is
// gone. A client that connects in the background (WithConnectInBackground) holds, from its
// constructor on, a connection nats.go has yet to connect for the first time and does not close
// before then, so a caller waits for that first connection as for a reconnect, and nothing here
// dials beside nats.go before it; a connection dialled later to replace a closed one is an
// ordinary one, connected before it is installed.
func (c *Client) ensureConnWithContext(ctx context.Context) error {
	for {
		c.mu.Lock()
		conn := c.Conn
		c.mu.Unlock()
		// A live connection serves even a stopped client: deliveries still in their handlers during
		// Drain publish their receipts and exceptions through it.
		if conn != nil && (conn.IsConnected() || conn.IsDraining()) {
			return nil
		}
		if c.stopped() {
			return errStopped
		}
		if conn != nil && !conn.IsClosed() {
			select {
			case <-ctx.Done():
				return fmt.Errorf("bus: waiting for NATS to reconnect: %w", ctx.Err())
			case <-c.stopCh:
				return errStopped
			case <-time.After(10 * time.Millisecond):
			}
			if c.reconnectWaitTick != nil {
				c.reconnectWaitTick()
			}
			continue
		}

		nc, err := connectWithContext(ctx, c.name, c.urls, c.connectionSettings, c.dialer, c.onReconnect, c.onClosed, c.onAsyncError, c.reconnectErrClock)
		if err != nil {
			return err
		}
		js, err := nc.JetStream(nats.MaxWait(10*time.Second), nats.PublishAsyncTimeout(publishAcknowledgementTimeout))
		if err != nil {
			nc.Close()
			return err
		}

		c.mu.Lock()
		// Drain and Close read c.Conn under c.mu after stopping the client, so checking here, under
		// the same lock, means a connection dialled while they ran is either theirs to close or
		// never installed.
		if c.stopped() {
			c.mu.Unlock()
			nc.Close()
			return errStopped
		}
		if c.Conn != conn {
			// Another caller replaced the closed connection first. Its replacement is the one to
			// serve with, so this one closes without its callbacks: its ClosedCB would start a
			// recovery of a client that has a connection, and its DisconnectedErrCB would log
			// "envoy nats disconnected" under the client's name, as if the connection it serves with
			// had dropped. The line below says what happened instead.
			c.mu.Unlock()
			nc.SetClosedHandler(nil)
			nc.SetDisconnectErrHandler(nil)
			nc.Close()
			slog.Info("envoy nats discarded a second replacement connection", slog.String("name", c.name))
			continue
		}
		c.Conn = nc
		c.js = js
		c.markHooksDueLocked()
		c.mu.Unlock()
		return nil
	}
}

// UsesCoreTransport reports whether topic travels over core NATS rather than JetStream: any
// notification topic none of the stream's subjects captures. That holds the role lanes and their
// exception lanes, which the stream must never retain and no stream subject captures
// (TestStreamConfigExcludesRoleLanes), and every ad-hoc topic such as
// notifications.team.deploy-squad, or the exception lane of one. JetStream answers a publish to a
// subject no stream captures only with "no responders", and not at all while a core subscriber
// holds the subject, since that subscriber is handed the acknowledgement request: the publish
// would wait out its whole deadline for an acknowledgement that never comes, after the subscribers
// had already received the message. A subject outside NotificationTopicPrefix keeps JetStream, as
// before. The capture is judged on the subjects this binary compiles in, not the deployed stream's:
// a topic under a subject only a newer build adds (which reconciliation keeps) goes over core NATS
// from an older one, so the stream still stores it but without a MsgId, and the older listener's
// durable never fans it out. The listener's publish route reads it too: such a topic is not
// stored, so the route confirms it with each self-subscribed recipient itself.
func UsesCoreTransport(topic string) bool {
	return strings.HasPrefix(topic, NotificationTopicPrefix) && !StreamCaptures(topic)
}

// ErrRefused is returned for an envelope that is refused the same way however often it is
// published, or a KV key refused however often a call names it, so a caller answers it as a
// refusal rather than a failure to retry. It is ErrTooLarge, ErrInvalidSubject or ErrInvalidKey.
var ErrRefused = errors.New("refused")

// refusal is a kind of ErrRefused. Its text names only the kind, since every line and answer that
// reports a refusal already says it is one (`github publish refused: too large to publish whole`).
type refusal string

func (r refusal) Error() string { return string(r) }

func (r refusal) Is(target error) bool { return target == ErrRefused }

// ErrTooLarge is the ErrRefused of an envelope too large to publish whole: a message past the NATS
// server's max payload, which nats.go refuses before sending it, a subject past maxSubjectBytes, a
// KV key whose subject would be, or a CI record or settlement past the bound its store keeps below
// those. Its error names the size and the bound.
var ErrTooLarge error = refusal("too large to publish whole")

// ErrInvalidSubject is the ErrRefused of a subject NATS does not accept: an empty one or one holding
// whitespace, which nats.go refuses, or one holding an empty token (`a..b`, or a leading or trailing
// dot), which no stream's subjects match, so a JetStream publish waits out its deadline for an
// answer that never comes and a core publish is dropped. Its error names the subject, or the KV key
// that would have made it.
var ErrInvalidSubject error = refusal("not a subject NATS accepts")

// ErrInvalidKey is the ErrRefused of a KV key outside nats.go's key alphabet (`ses:bad`), which
// nats.go refuses on every read, write, delete and watch before sending anything, wherever the key
// came from: no build writes one, but a direct bucket write can store one, and a stored claim can
// name one as its holder. Its error names the key, and still matches nats.ErrInvalidKey.
var ErrInvalidKey error = refusal("not a key nats.go accepts")

// ErrStreamFull is wrapped by the error of a JetStream publish the stream did not store because it
// holds its byte limit (WithStreamMaxBytes): a full stream refuses new messages rather than drop
// stored ones (streamCfg). It is not an ErrRefused: the same publish is taken once age has made
// room or an operator raised the limit, so a caller retries it as any other failure, and the
// webhook listener answers it 503, which GitHub redelivers. On a clustered stream, a retry under
// the same MsgId is acknowledged a duplicate of the refused message instead (ErrNotStored), and
// goes once more without the id, which the stream stores once it has room. The error still
// matches nats-server's *nats.APIError.
var ErrStreamFull = errors.New("stream " + Stream + " is full")

// jsStreamStoreFailed is nats-server's code for a message the stream did not store
// (JSStreamStoreFailedF), and maxBytesExceeded its description when the store refused the message
// because a DiscardNew stream holds its byte limit (nats-server's ErrMaxBytes). nats.go names
// neither.
const (
	jsStreamStoreFailed = 10077
	maxBytesExceeded    = "maximum bytes exceeded"
)

// streamFull names the server's answer to a JetStream publish that a full stream refused as the
// ErrStreamFull it is, and returns any other error as it is.
func streamFull(err error) error {
	var apiErr *nats.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode == jsStreamStoreFailed && apiErr.Description == maxBytesExceeded {
		return fmt.Errorf("%w: %w", ErrStreamFull, err)
	}
	return err
}

// ErrNotStored names a JetStream acknowledgement of a duplicate with no sequence: a repeat of a
// message id the stream holds no message for. A clustered nats-server (v2.10.29 among them)
// records a message id at sequence 0 before its store runs, and keeps that record on the stream's
// leader, a purge notwithstanding, for the whole duplicate window when the store refuses the
// message, as a full stream's does (ErrStreamFull), so every later publish under the id is
// acknowledged `{"seq":0,"duplicate":true}` though nothing was stored; a repeat of a message still
// being stored is acknowledged the same way. Publish and a window's send answer that
// acknowledgement by sending the same envelope once more without its id, which no record matches:
// the publish succeeds once that send is stored, at a sequence of its own, and otherwise fails
// with an error wrapping ErrNotStored and that send's own error (ErrStreamFull while the stream is
// still full), which is not an ErrRefused and never a success, so a caller retries it as any other
// failure. The stream's MsgId drops no copy sent without the id: such a send stores a second copy
// of a message that was still being stored, and once a refused message is stored without its id,
// every later publish under that id stores another copy for as long as the leader keeps the record
// (the duplicate window, or until the leader changes). A consumer of the stream sees each copy and
// must drop the repeat itself.
var ErrNotStored = errors.New("stream " + Stream + " acknowledged a duplicate of a message it does not hold")

// notStored is ErrNotStored for ack, a JetStream acknowledgement, when it names a duplicate with no
// sequence, and nil for one that names the message's sequence. Its caller then sends the envelope
// again without its id.
func notStored(ack *nats.PubAck) error {
	if ack.Duplicate && ack.Sequence == 0 {
		return ErrNotStored
	}
	return nil
}

// maxSubjectBytes bounds a subject the bus publishes on, or a KV call builds from a key. The server
// closes a connection whose protocol line runs past its max control line (4 KiB by default) and
// nats.go does not check it, so a longer subject would close the connection every subscription and
// watcher of the client runs on, a core publish reporting success first. Besides the subject, the
// longest line nats.go sends here, `HPUB <subject> <reply> <header size> <total size>`, holds a
// 38-byte reply inbox (`_INBOX.<nuid>.<token>`), two sizes of at most seven digits (the 1 MiB max
// payload), its verb, spaces and line ending: 62 bytes, which 64 covers.
const maxSubjectBytes = 4<<10 - 64

// checkSubject refuses, before anything is sent, a subject the server would close the connection
// over (ErrTooLarge) and one NATS does not accept (ErrInvalidSubject).
func checkSubject(subject string) error {
	if len(subject) > maxSubjectBytes {
		return fmt.Errorf("%w: a subject of %d bytes, past %d", ErrTooLarge, len(subject), maxSubjectBytes)
	}
	if !validSubject(subject) {
		return fmt.Errorf("%w: %q", ErrInvalidSubject, subject)
	}
	return nil
}

// validSubject reports whether NATS accepts subject: it is not empty, holds no whitespace, and has
// no empty token.
func validSubject(subject string) bool {
	return subject != "" && subject[0] != '.' && subject[len(subject)-1] != '.' &&
		!strings.Contains(subject, "..") && !strings.ContainsAny(subject, " \t\r\n")
}

// refused names nats.go's refusal of a message past the server's max payload as the ErrTooLarge it
// is, naming its envelope of size bytes. checkSubject has already refused every subject nats.go
// would.
func (c *Client) refused(err error, size int) error {
	if errors.Is(err, nats.ErrMaxPayload) {
		c.mu.Lock()
		conn := c.Conn
		c.mu.Unlock()
		return fmt.Errorf("%w: an envelope of %d bytes against the server's max payload of %d bytes",
			ErrTooLarge, size, conn.MaxPayload())
	}
	return err
}

// publishBound bounds Publish, PublishCoreTo and PublishReportingDuplicate, and the publishJetStream
// and PublishCoreContext of a caller that passed no deadline of its own: their wait for a reconnect and
// their flush or JetStream acknowledgement together. A publish window's sends take
// publishAcknowledgementTimeout instead. It is also the HTTP client's per-attempt deadline
// (packages/envoy-client/src/transport.ts), which a publish that spends the whole bound can never
// be answered inside, and that is what the Within forms below exist for.
const publishBound = 5 * time.Second

// Publish routes role lanes, their delivery-exception lanes and every topic the stream does not
// capture through core NATS (UsesCoreTransport); every notification the stream captures retains
// JetStream durability. Its signature is an interface method in cistore, outbox and webhook, so a
// caller that wants JetStream's duplicate verdict calls PublishReportingDuplicate instead, and one
// that has a deadline of its own calls PublishWithin.
func (c *Client) Publish(item contracts.Envelope) error {
	_, err := c.PublishReportingDuplicate(item)
	return err
}

// PublishWithin publishes as Publish does, with ctx as the whole publish's bound - the wait for a
// reconnect, the flush and the JetStream acknowledgement all end with it - instead of publishBound.
// The listener's HTTP handlers publish this way, so a bus they cannot reach is answered inside the
// deadline their own caller gave them.
func (c *Client) PublishWithin(ctx context.Context, item contracts.Envelope) error {
	_, err := c.PublishReportingDuplicateWithin(ctx, item)
	return err
}

// PublishReportingDuplicate publishes as Publish does and reports whether
// JetStream recognised the message as one the stream already holds. That is only
// ever possible for an envelope whose dedupe key names the upstream event
// (contracts.DedupeKeyNamesTheUpstreamEvent), the only one published under a
// MsgId, and only within the stream's duplicate window; a core-transport topic
// has no acknowledgement at all and always reports false.
//
// True means "the stream already held this MsgId", not "this call published
// nothing new": the reconnect retry below republishes after a failure that may
// have landed, so the very publish that put the message on the subject can come
// back a duplicate. A duplicate acknowledged with no sequence, of a message the
// stream does not hold (ErrNotStored), is never true: the envelope goes once more
// without its MsgId (publishJetStream), and a store of that is false.
func (c *Client) PublishReportingDuplicate(item contracts.Envelope) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), publishBound)
	defer cancel()
	return c.PublishReportingDuplicateWithin(ctx, item)
}

// PublishReportingDuplicateWithin publishes as PublishReportingDuplicate does, reporting the same
// duplicate verdict over the same two transports, with ctx as the whole publish's bound instead of
// publishBound.
func (c *Client) PublishReportingDuplicateWithin(ctx context.Context, item contracts.Envelope) (bool, error) {
	if UsesCoreTransport(item.Topic) {
		return false, c.PublishCoreContext(ctx, item.Topic, item)
	}
	return c.publishJetStream(ctx, item)
}

// publishJetStream publishes item to the notification stream within ctx's bound. The MsgId, where
// the envelope earns one, is the dedupe key and the topic together, because one upstream event can
// be published on several topics under one key - a GitHub comment that mentions the trigger is
// published on its mention topic beside its comment topic - and each of those must be retained.
// The bound below is the earlier of the caller's own deadline and publishBound, since WithTimeout
// keeps the parent's deadline when that is the earlier one; publishBound is what bounds a caller
// that passed none. The acknowledgement wait is still taken from publishAcknowledgementClock, the
// seam a test uses to expire that bound without spending it.
//
// An acknowledgement of a duplicate with no sequence (ErrNotStored) names an id the server holds
// at sequence 0 with no message under it, which on a clustered nats-server 2.10 lasts the whole
// duplicate window once a full stream refused the message. So the same envelope goes once more,
// within the same bound, without the MsgId: stored, the publish succeeds; refused, its error wraps
// ErrNotStored and that send's own error.
//
// A ctx that has already ended never puts the message on the wire here, and that protection is
// the dependency's rather than this repository's: js.Publish under nats.Context is a request, and
// nats.go's requestWithContext returns ctx.Err() before it sends anything (nats.go v1.50.0,
// context.go), on the first attempt, on each of its no-responders retries and on the reconnect
// retry below alike. That read comes after ensureConnWithContext has returned, so it also refuses
// a ctx cancelled as a reconnect wait ends. So the core path's own check (publishConfirmed's read
// of ctx) has no counterpart here, and it is not redundant with this one: a change that stopped
// passing nats.Context(ctx), or a nats.go that stopped checking first, would reopen the case on
// this path silently.
// TestAPublishWithAnEndedContextSendsNothing pins it, so either change fails that test instead of
// reaching the wire. What a context that ends during the acknowledgement wait leaves is a message
// the stream may hold while the caller is answered failure; that is the acknowledgement's own
// ambiguity, which the MsgId's duplicate window absorbs for an envelope that earns one.
func (c *Client) publishJetStream(ctx context.Context, item contracts.Envelope) (bool, error) {
	if err := checkSubject(item.Topic); err != nil {
		return false, err
	}
	data, err := json.Marshal(item)
	if err != nil {
		return false, err
	}
	ctx, cancel := c.publishAcknowledgementClock.WithTimeout(ctx, publishBound)
	defer cancel()
	if err := c.ensureConnWithContext(ctx); err != nil {
		return false, err
	}
	// publish sends data under options, once more if the connection had closed, and names a
	// refusal as the ErrTooLarge or ErrStreamFull it is.
	publish := func(options ...nats.PubOpt) (*nats.PubAck, error) {
		ack, err := c.JS().Publish(item.Topic, data, options...)
		if err != nil && errors.Is(err, nats.ErrConnectionClosed) {
			if err := c.ensureConnWithContext(ctx); err != nil {
				return nil, err
			}
			ack, err = c.JS().Publish(item.Topic, data, options...)
		}
		if err != nil {
			return nil, streamFull(c.refused(err, len(data)))
		}
		return ack, nil
	}
	options := []nats.PubOpt{nats.Context(ctx)}
	if contracts.DedupeKeyNamesTheUpstreamEvent(item) {
		options = append(options, nats.MsgId(ForwardMsgID(item.DedupeKey, item.Topic)))
	}
	ack, err := publish(options...)
	if err != nil {
		return false, err
	}
	if err := notStored(ack); err != nil {
		// The server holds the id at sequence 0 and no message under it, so the same envelope
		// goes once more without the id, which no duplicate record can match.
		if _, sendErr := publish(nats.Context(ctx)); sendErr != nil {
			return false, fmt.Errorf("%w, and its publish without its id failed: %w", err, sendErr)
		}
		return false, nil
	}
	return ack.Duplicate, nil
}

// PublishCore publishes directly to the envelope's NATS subject without
// waiting for a JetStream acknowledgment.
func (c *Client) PublishCore(item contracts.Envelope) error {
	return c.PublishCoreTo(item.Topic, item)
}

// PublishCoreTo publishes item directly to subject and confirms the server accepted it
// (publishConfirmed), so a nil error means it did: a publish NATS denies (a permissions violation
// of a per-client grant), a flush that fails and a publish the server cannot be shown to have
// accepted all return an error. The subject can differ from item.Topic when an authoritative
// router forwards an envelope while retaining its original topic for the recipient. It bounds
// itself at publishBound, a reconnect included; a caller with a deadline of its own reaches the
// same publish through PublishCoreContext, or PublishWithin.
func (c *Client) PublishCoreTo(subject string, item contracts.Envelope) error {
	ctx, cancel := context.WithTimeout(context.Background(), publishBound)
	defer cancel()
	return c.PublishCoreContext(ctx, subject, item)
}

// PublishCoreContext is PublishCoreTo under ctx: ctx is the whole publish's bound, so it is both
// what the wait for a reconnect ends with and the window publishConfirmed flushes and confirms
// within, and a caller answering inside a deadline of its own (the listener's routes: a publish
// through PublishWithin, the publish route's confirmed ad-hoc topic send) is never held past it.
// The bound taken below is the earlier of ctx's own deadline and publishBound, since WithTimeout
// keeps the parent's deadline when that is the earlier one; publishBound is what bounds a caller
// that passed none.
//
// A ctx that has ended puts nothing on the wire: ensureConnWithContext returns its error rather
// than wait for a reconnect or dial one, and publishConfirmed refuses it immediately before the
// publish, which also catches a cancel that lands as a reconnect completes. A deadline arrives
// spent through publishHandler (cmd/listener/api.go), whose reads before the publish (the sender
// stamp's) share the route's deadline with it.
func (c *Client) PublishCoreContext(ctx context.Context, subject string, item contracts.Envelope) error {
	if err := checkSubject(subject); err != nil {
		return err
	}
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, publishBound)
	defer cancel()
	if err := c.ensureConnWithContext(ctx); err != nil {
		return err
	}
	conn := c.Conn
	return c.publishConfirmed(ctx, conn, &nats.Msg{Subject: subject, Data: data}, conn.PublishMsg)
}

// ErrReceiptTimeout is returned by RequestCoreTo only when the publish and the
// flush both succeeded, the server reported nothing and the connection stayed
// up through them, and no empty receipt arrived before the deadline: the
// forward is known to have reached the server, and whoever holds the subject
// did not acknowledge it in time. A flush that fails or times out — a
// reconnecting or stalled connection still buffering the forward — is returned
// as the client's own error, never this one, because that forward is not known
// to have left this process; so is a forward the server denied
// (ErrPublishDenied), or one during which the server reported another error or
// the connection dropped, since either may have hidden its denial.
var ErrReceiptTimeout = errors.New("bus: no receipt inside the request window")

// ForwardMsgID is the Nats-Msg-Id of an envelope whose dedupe key is dedupeKey published or
// forwarded to subject: the dedupe key, a colon, the subject. publishJetStream stores an envelope
// that earns a MsgId under it, so each topic one upstream event is published on keeps its own
// copy, and every forward to a session's subject (RequestCoreTo) sets its header through it, so the
// stream, which captures every agent subject, keeps one copy of a message per recipient however
// many times a sender forwards it, and a forward of the same message to another recipient is a
// copy of its own.
func ForwardMsgID(dedupeKey, subject string) string {
	return dedupeKey + ":" + subject
}

// RequestCoreTo delivers item directly to subject and waits for an empty
// receiver receipt: a reply with no data and no header field. Agent subjects
// are captured by the notification stream, whose non-empty JetStream publish
// acknowledgement is not a receiver receipt, and the server's no-responders
// answer carries a Status header, so neither is one either. The forward
// carries a Nats-Msg-Id of item's dedupe key and subject, so a sender that
// forwards one envelope again until a receipt arrives leaves the stream one
// copy per message and recipient, however many attempts it makes; every
// attempt still reaches the core subscribers on subject. Because the forward
// has headers, the server hands its replies to this client with a header
// block, empty for a receipt, so the receipt is told by its header holding no
// field rather than by its having none. The flush is bounded by the same
// window as the receipt wait, so the call never outlives timeout by the
// client's default 10 s flush. A forward the server denied returns at once;
// one publishConfirmed cannot confirm for another reason still waits, since a
// receipt proves it was delivered, and returns that reason instead of
// ErrReceiptTimeout if none arrives.
func (c *Client) RequestCoreTo(subject string, item contracts.Envelope, timeout time.Duration) error {
	if err := checkSubject(subject); err != nil {
		return err
	}
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := c.ensureConnWithContext(ctx); err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	conn := c.Conn
	inbox := nats.NewInbox()
	receipt, err := conn.SubscribeSync(inbox)
	if err != nil {
		return err
	}
	defer receipt.Unsubscribe()
	forward := &nats.Msg{Subject: subject, Reply: inbox, Data: data, Header: nats.Header{}}
	forward.Header.Set(nats.MsgIdHdr, ForwardMsgID(item.DedupeKey, subject))
	err = c.publishConfirmed(ctx, conn, forward, conn.PublishMsg)
	if err != nil && !errors.Is(err, errPublishUnconfirmed) {
		return err
	}
	timedOut := cmp.Or(err, ErrReceiptTimeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return timedOut
		}
		response, err := receipt.NextMsg(remaining)
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) {
				return timedOut
			}
			return err
		}
		if len(response.Data) == 0 && len(response.Header) == 0 {
			return nil
		}
	}
}
