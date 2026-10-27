package oidc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// ErrIssuerUnavailable means the issuer could not be asked: its discovery document has not been
// read yet, or a call to it failed in a way that says nothing about the token or the grant it
// carried (no answer, a timeout, a 5xx, a 429, a refusal of the client itself, an answer without a
// verifiable ID token). A caller answers it as a service that is down, and ends nothing on it.
var ErrIssuerUnavailable = errors.New("oidc: the issuer cannot be reached")

// ErrGrantRefused means the issuer's token endpoint refused a refresh token (RFC 6749 section
// 5.2, invalid_grant): it has expired, been revoked, or is not the client's. The sign-in it renews
// is over.
var ErrGrantRefused = errors.New("oidc: the issuer refused the refresh token")

const (
	// discoveryAttemptTimeout bounds one read of an issuer's discovery document.
	discoveryAttemptTimeout = 10 * time.Second
	// discoveryRetrySpacing is the least time between two failed discovery attempts: a request
	// that finds the issuer undiscovered within it fails at once with the last attempt's error,
	// so a burst of requests during an outage sends the issuer one read, not one each.
	discoveryRetrySpacing = 5 * time.Second
	// discoveryRetryFirst and discoveryRetryMax are the background loop's wait after its first
	// failed attempt, which doubles after each one up to the most.
	discoveryRetryFirst = time.Second
	discoveryRetryMax   = 30 * time.Second
	// discoveryLogInterval is the least time between two lines logging a failed discovery.
	discoveryLogInterval = time.Minute
)

// discovery reads one issuer's discovery document when it is first needed rather than at
// construction, so a process starts and serves whatever does not need the issuer while the issuer
// is down. Once read, the result is kept for the process's life. Concurrent attempts share one
// read (single flight), each read is bounded by discoveryAttemptTimeout, and after a failed read
// no other is made for discoveryRetrySpacing.
type discovery[T any] struct {
	issuer string
	read   func(ctx context.Context) (T, error)
	// now is the clock the spacing between failed attempts and the log's rate are measured by.
	now    func() time.Time
	flight singleflight.Group

	mu         sync.Mutex
	value      T
	discovered bool
	failedAt   time.Time
	failure    error
	loggedAt   time.Time
}

func newDiscovery[T any](issuer string, read func(ctx context.Context) (T, error)) *discovery[T] {
	return &discovery[T]{issuer: issuer, read: read, now: time.Now}
}

// get is the discovered result, discovering it first when no attempt has succeeded yet.
func (d *discovery[T]) get(ctx context.Context) (T, error) {
	d.mu.Lock()
	if d.discovered {
		value := d.value
		d.mu.Unlock()
		return value, nil
	}
	d.mu.Unlock()
	return d.discover(ctx)
}

// done reports whether an attempt has succeeded.
func (d *discovery[T]) done() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.discovered
}

// discover reads the discovery document unless it has been read, joining an attempt already in
// flight, or fails at once with the last attempt's error when that attempt failed less than
// discoveryRetrySpacing ago. The attempt runs on a context of its own, detached from ctx's
// cancellation, since its result belongs to every caller; ctx ending stops only this caller's
// wait. Every error matches ErrIssuerUnavailable.
func (d *discovery[T]) discover(ctx context.Context) (T, error) {
	var zero T
	if value, err, settled := d.settled(); settled {
		return value, err
	}
	attempt := d.flight.DoChan("discover", func() (any, error) {
		// An attempt that ended just before this flight began has settled it already.
		if value, err, settled := d.settled(); settled {
			return value, err
		}
		attemptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discoveryAttemptTimeout)
		defer cancel()
		value, err := d.read(attemptCtx)
		d.mu.Lock()
		defer d.mu.Unlock()
		if err != nil {
			d.failedAt = d.now()
			d.failure = fmt.Errorf("%w: %w", ErrIssuerUnavailable, err)
			return zero, d.failure
		}
		d.value, d.discovered = value, true
		return value, nil
	})
	select {
	case result := <-attempt:
		if result.Err != nil {
			return zero, result.Err
		}
		return result.Val.(T), nil
	case <-ctx.Done():
		return zero, fmt.Errorf("%w: waiting for issuer %s's discovery: %w", ErrIssuerUnavailable, d.issuer, ctx.Err())
	}
}

// settled is the discovered value, or the last attempt's error while it is younger than
// discoveryRetrySpacing; settled is false when a new attempt is due.
func (d *discovery[T]) settled() (value T, err error, settled bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.discovered {
		return d.value, nil, true
	}
	if d.failure != nil && d.now().Sub(d.failedAt) < discoveryRetrySpacing {
		return value, d.failure, true
	}
	return value, nil, false
}

// run discovers the issuer from the process's start, so the first request that needs it does not
// wait for the read: it attempts at once and, while attempts fail, again after a wait of
// discoveryRetryFirst that doubles after each failure up to discoveryRetryMax. It logs a failure
// at ERROR at most once per discoveryLogInterval, and the success once, and returns once the
// issuer is discovered or ctx ends.
func (d *discovery[T]) run(ctx context.Context) {
	wait := discoveryRetryFirst
	for {
		if _, err := d.discover(ctx); err == nil {
			slog.Info("oidc: discovered the issuer", "issuer", d.issuer)
			return
		} else if ctx.Err() == nil {
			d.logFailure(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, discoveryRetryMax)
	}
}

func (d *discovery[T]) logFailure(err error) {
	d.mu.Lock()
	now := d.now()
	due := d.loggedAt.IsZero() || now.Sub(d.loggedAt) >= discoveryLogInterval
	if due {
		d.loggedAt = now
	}
	d.mu.Unlock()
	if due {
		slog.Error("oidc: discover the issuer; retrying in the background, and what needs it answers 503 meanwhile", "issuer", d.issuer, "error", err)
	}
}

// LazyVerifier is a Verifier whose issuer is discovered when first needed (see CodeFlow's
// discovery), for a server that must start and serve its other callers while that issuer is
// down. Until the discovery document has been read, Verify fails with ErrIssuerUnavailable.
type LazyVerifier struct {
	discovery *discovery[*Verifier]
}

// NewLazyVerifier returns a verifier for tokens issuer issues to audience, and reads nothing: the
// discovery document is read by Discover, by Run, or by the first Verify.
func NewLazyVerifier(issuer, audience string) *LazyVerifier {
	return &LazyVerifier{discovery: newDiscovery(issuer, func(ctx context.Context) (*Verifier, error) {
		return New(ctx, issuer, audience)
	})}
}

// Verify is Verifier.Verify once the issuer is discovered, discovering it first when it is not
// and an attempt is due; before that every error matches ErrIssuerUnavailable.
func (v *LazyVerifier) Verify(ctx context.Context, raw string) (Claims, error) {
	verifier, err := v.discovery.get(ctx)
	if err != nil {
		return Claims{}, err
	}
	return verifier.Verify(ctx, raw)
}

// Discover reads the issuer's discovery document unless it has been read; its error matches
// ErrIssuerUnavailable.
func (v *LazyVerifier) Discover(ctx context.Context) error {
	_, err := v.discovery.discover(ctx)
	return err
}

// Discovered reports whether the issuer's discovery document has been read.
func (v *LazyVerifier) Discovered() bool { return v.discovery.done() }

// Run discovers the issuer in the background until it succeeds or ctx ends (discovery.run).
func (v *LazyVerifier) Run(ctx context.Context) { v.discovery.run(ctx) }
