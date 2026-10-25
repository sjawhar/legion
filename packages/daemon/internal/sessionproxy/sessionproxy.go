// Package sessionproxy carries one generation's Oh My Pi session database traffic to Cloud SQL
// over the pod's own cloud identity, so that no database password is ever written into a pod. The
// agent's client (Bun's SQL client over a unix socket) sends a plain Postgres startup packet with
// a user and a database and no password; this proxy dials Cloud SQL for every accepted connection
// with automatic IAM database authentication and copies bytes both ways.
//
// Nothing here reads the Postgres wire protocol, and nothing here belongs to it: with automatic
// IAM database authentication the login token travels in the connector's ephemeral-certificate
// exchange, not in the startup packet, which is why Google's own cloud-sql-proxy is likewise a
// plain bidirectional copy over the connector's net.Conn.
package sessionproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"time"

	"cloud.google.com/go/cloudsqlconn"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
)

// Dialer is the upstream the proxy dials for each accepted connection: the Cloud SQL connector in
// a pod, a fake over a local listener in a test. Close releases the dialer's own background work,
// the connector's refreshed certificate cache above all.
type Dialer interface {
	Dial(ctx context.Context, instance string) (net.Conn, error)
	Close() error
}

// Config is one proxy's whole configuration. A proxy serves exactly one generation, so every field
// is fixed for that generation's lifetime.
type Config struct {
	// Socket is the absolute path of the unix socket the agent and the launcher's own client
	// connect to. It must not exist yet: the generation's private directory is fresh, so a socket
	// already sitting there means something else holds the address.
	Socket string
	// Instance is the Cloud SQL instance connection name, project:region:instance.
	Instance string
	// NewDialer builds the upstream dialer. Nil builds the Cloud SQL connector's, dialing the
	// instance's private IP with automatic IAM database authentication.
	NewDialer func(ctx context.Context) (Dialer, error)
	// ProofTimeout bounds Start's proof dial, the one upstream connection that proves the
	// credential path before Start returns. It must be positive: an instance that never answers,
	// an unreachable address or a peer that stalls its handshake, would otherwise hold Start for as
	// long as its caller's context lives. The per-connection dials after Start are not bounded by it.
	ProofTimeout time.Duration
	// Log takes one line per event: the proxy's start, a connection the upstream refused, an
	// accept loop that stopped on its own. Nil logs nothing.
	Log io.Writer
}

// Proxy is a started session database proxy: a bound socket, a dialer whose credential path has
// been proven once, and an accept loop that runs until Close and nothing else. The context Start
// was given bounds only the start; its owner decides when the proxy goes by calling Close.
type Proxy struct {
	instance string
	listener net.Listener
	dialer   Dialer

	// cancel ends the proxy's own serving context, which the dialer was built with and every
	// per-connection dial uses, so the dialer's background work lives exactly as long as the proxy
	// and Close never waits on a dial that has nobody left to serve. Close is its only caller.
	cancel context.CancelFunc
	// inFlight counts the accept loop and every connection it is still copying for.
	inFlight sync.WaitGroup

	closeOnce sync.Once
	closeErr  error

	logMu sync.Mutex
	log   io.Writer
}

// Start binds the socket, builds the dialer, proves the credential path with one upstream
// connection and then serves. The order matters in both directions: the socket is bound before
// anything is dialed, so a start that loses the address to something else costs no credential
// work, and the proof dial happens before Start returns, so the launcher never starts an agent
// against a proxy that cannot reach the database.
//
// ctx bounds the start alone: a start whose ctx ends before the proof dial succeeds fails and
// leaves nothing bound. The proof dial is bounded besides by cfg.ProofTimeout, so a start against
// an instance that never answers fails within that bound however long ctx lives, with an error
// naming the instance and the bound that wraps the dial's own (context.DeadlineExceeded). Once
// Start returns, ctx no longer matters: the proxy serves until Close, so its owner can keep it
// serving after ctx ends, as the launcher does through a child's stop grace when the container is
// told to stop.
func Start(ctx context.Context, cfg Config) (*Proxy, error) {
	if !filepath.IsAbs(cfg.Socket) {
		return nil, fmt.Errorf("sessionproxy: Socket %q is not an absolute path", cfg.Socket)
	}
	if !ompsessions.ValidInstance(cfg.Instance) {
		return nil, fmt.Errorf("sessionproxy: Instance %q is not a project:region:instance connection name", cfg.Instance)
	}
	if cfg.ProofTimeout <= 0 {
		return nil, fmt.Errorf("sessionproxy: ProofTimeout %s is not positive", cfg.ProofTimeout)
	}

	// An address another process holds is never taken from it: net.Listen refuses a path that
	// already exists, and that refusal is the whole start's failure.
	listener, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return nil, fmt.Errorf("sessionproxy: listen on %s: %w", cfg.Socket, err)
	}

	newDialer := cfg.NewDialer
	if newDialer == nil {
		newDialer = newConnectorDialer
	}
	// The serving context outlives ctx: it carries ctx's values and nothing of its cancellation, and
	// only Close ends it.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	dialer, err := newDialer(runCtx)
	if err != nil {
		cancel()
		_ = listener.Close()
		return nil, fmt.Errorf("sessionproxy: build the dialer for %s: %w", cfg.Instance, err)
	}
	proofCtx, cancelProof := context.WithTimeout(ctx, cfg.ProofTimeout)
	proof, err := dialer.Dial(proofCtx, cfg.Instance)
	// proofCtx's own error is read before cancelProof, which would set it either way.
	timedOut := errors.Is(proofCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	cancelProof()
	if err != nil {
		cancel()
		_ = listener.Close()
		_ = dialer.Close()
		if timedOut {
			return nil, fmt.Errorf("sessionproxy: dial %s: no answer within %s: %w", cfg.Instance, cfg.ProofTimeout, err)
		}
		return nil, fmt.Errorf("sessionproxy: dial %s: %w", cfg.Instance, err)
	}
	_ = proof.Close()

	p := &Proxy{
		instance: cfg.Instance,
		listener: listener,
		dialer:   dialer,
		cancel:   cancel,
		log:      cfg.Log,
	}
	p.logf("sessionproxy: serving %s on %s", cfg.Instance, cfg.Socket)
	p.inFlight.Add(1)
	go p.accept(runCtx)
	return p, nil
}

// Close stops accepting, releases the dialer and returns once every connection the proxy was
// copying for has finished. It is the only way a started proxy ends. A second call is a no-op that
// returns nil.
func (p *Proxy) Close() error {
	first := false
	p.closeOnce.Do(func() {
		first = true
		err := p.listener.Close()
		if dialerErr := p.dialer.Close(); err == nil {
			err = dialerErr
		}
		p.cancel()
		p.inFlight.Wait()
		p.closeErr = err
	})
	if !first {
		return nil
	}
	return p.closeErr
}

// accept serves one connection per accepted client until the listener is closed, which is the
// ordinary way this loop ends and so says nothing.
func (p *Proxy) accept(ctx context.Context) {
	defer p.inFlight.Done()
	for {
		client, err := p.listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				p.logf("sessionproxy: stopped accepting connections for %s: %v", p.instance, err)
			}
			return
		}
		p.inFlight.Add(1)
		go func() {
			defer p.inFlight.Done()
			p.serve(ctx, client)
		}()
	}
}

// serve gives one accepted connection an upstream of its own and copies between them. The dial is
// per connection on purpose: the connector mints the connection's credential as it dials, from a
// certificate cache it keeps refreshed, so every connection carries a freshly minted one. serve
// already runs on a goroutine of its own, so only the other copy direction takes one.
func (p *Proxy) serve(ctx context.Context, client net.Conn) {
	upstream, err := p.dialer.Dial(ctx, p.instance)
	if err != nil {
		// Nothing is written back: a client that reads a Postgres error here would believe the
		// database answered it.
		_ = client.Close()
		p.logf("sessionproxy: could not dial %s for a connection: %v", p.instance, err)
		return
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = client.Close()
			_ = upstream.Close()
		})
	}
	// Either direction ending closes both ends, which ends the other; serve returns only once both
	// have, so the connection is in flight for exactly as long as it is copied for.
	var copies sync.WaitGroup
	copies.Add(1)
	go func() {
		defer copies.Done()
		defer stop()
		_, _ = io.Copy(upstream, client)
	}()
	_, _ = io.Copy(client, upstream)
	stop()
	copies.Wait()
}

// logf writes one event line, under a lock because connections log concurrently.
func (p *Proxy) logf(format string, args ...any) {
	if p.log == nil {
		return
	}
	p.logMu.Lock()
	defer p.logMu.Unlock()
	_, _ = fmt.Fprintf(p.log, format+"\n", args...)
}

// connectorDialer adapts the Cloud SQL connector, whose Dial takes variadic dial options, to
// Dialer.
type connectorDialer struct {
	dialer *cloudsqlconn.Dialer
}

func (c connectorDialer) Dial(ctx context.Context, instance string) (net.Conn, error) {
	return c.dialer.Dial(ctx, instance)
}

func (c connectorDialer) Close() error { return c.dialer.Close() }

// newConnectorDialer builds the dialer a pod uses: automatic IAM database authentication, which
// puts the pod's own login token in the ephemeral-certificate exchange, over the instance's
// private IP, which is the only address a pod's network reaches.
func newConnectorDialer(ctx context.Context) (Dialer, error) {
	dialer, err := cloudsqlconn.NewDialer(ctx,
		cloudsqlconn.WithIAMAuthN(),
		cloudsqlconn.WithDefaultDialOptions(cloudsqlconn.WithPrivateIP()),
	)
	if err != nil {
		return nil, err
	}
	return connectorDialer{dialer: dialer}, nil
}
