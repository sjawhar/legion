// Package modellogin keeps a daemon-held Cognito machine login current and publishes only its
// short-lived access token. The machine password and refresh token never leave this package.
package modellogin

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

const (
	StatePending = "pending"
	StateReady   = "ready"
	StateError   = "error"
)

// Credentials are the machine login's long-lived values. New retains them in the daemon process;
// callers must never serialize or log either field.
type Credentials struct {
	Username string
	Password string
}

// Result is Cognito's successful authentication response. A refresh response usually omits
// RefreshToken; Manager retains the previous value in that case.
type Result struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    time.Duration
}

// Client is the two Cognito authentication flows the daemon needs. The real client uses
// USER_PASSWORD_AUTH and REFRESH_TOKEN_AUTH; tests can supply an in-memory client.
type Client interface {
	PasswordAuth(context.Context, string, string) (Result, error)
	RefreshAuth(context.Context, string) (Result, error)
}

// Clock makes the refresh schedule deterministic in tests.
type Clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func (wallClock) Sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Status is safe for the state API: it says whether this daemon has an access token and why it
// does not, but never contains a password, refresh token, or access token.
type Status struct {
	State string
	Error string
}

// Manager owns the machine login's credentials and current token. Subscribers receive each new
// access token so the worker-stream listener can deliver it to connected shims.
type Manager struct {
	credentials Credentials
	client      Client
	clock       Clock
	log         *slog.Logger

	mu          sync.RWMutex
	accessToken string
	status      Status
	subscribers []func(string)
}

func New(credentials Credentials, client Client, clock Clock, log *slog.Logger) *Manager {
	if clock == nil {
		clock = wallClock{}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{
		credentials: credentials,
		client:      client,
		clock:       clock,
		log:         log,
		status:      Status{State: StatePending},
	}
}

// Failed builds a state-reporting manager when the daemon could not read or parse the machine
// login document. The daemon still serves its state API; no worker receives a token.
func Failed(err error, log *slog.Logger) *Manager {
	manager := New(Credentials{}, nil, nil, log)
	manager.failed(err)
	return manager
}

// AccessToken returns the current short-lived token, when a login succeeded. It is used only by
// the in-process worker-stream listener; it must never be exposed from the daemon API.
func (m *Manager) AccessToken() (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.accessToken, m.accessToken != ""
}

func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

// Subscribe registers an in-process recipient of fresh access tokens. A recipient added after an
// authentication has completed receives the current token immediately, so a listener started
// after the login still sends a token to its first shim.
func (m *Manager) Subscribe(callback func(string)) {
	m.mu.Lock()
	m.subscribers = append(m.subscribers, callback)
	token := m.accessToken
	m.mu.Unlock()
	if token != "" {
		callback(token)
	}
}

// Run authenticates once, then refreshes at 90% of each access token's lifetime. A refresh
// refusal discards the refresh token and signs in again using the password the daemon already
// holds. A login that cannot be obtained remains visible in Status and emits one error log.
func (m *Manager) Run(ctx context.Context) {
	if m.Status().State == StateError {
		return
	}
	var refreshToken string
	for {
		result, err := m.client.PasswordAuth(ctx, m.credentials.Username, m.credentials.Password)
		if err != nil {
			m.failed(err)
			return
		}
		if err := validateInitial(result); err != nil {
			m.failed(err)
			return
		}
		refreshToken = result.RefreshToken
		m.ready(result.AccessToken)

		for {
			if err := m.clock.Sleep(ctx, refreshDelay(result.ExpiresIn)); err != nil {
				return
			}
			refreshed, err := m.client.RefreshAuth(ctx, refreshToken)
			if err != nil {
				m.log.Warn("model access-token refresh failed; signing in again", "error", err)
				break
			}
			if err := validateRefresh(refreshed); err != nil {
				m.log.Warn("model access-token refresh failed; signing in again", "error", err)
				break
			}
			if refreshed.RefreshToken != "" {
				refreshToken = refreshed.RefreshToken
			}
			result = refreshed
			m.ready(result.AccessToken)
		}
	}
}

func validateInitial(result Result) error {
	switch {
	case result.AccessToken == "":
		return errors.New("model login returned no access token")
	case result.RefreshToken == "":
		return errors.New("model login returned no refresh token")
	case result.ExpiresIn <= 0:
		return errors.New("model login returned a non-positive access-token lifetime")
	}
	return nil
}

func validateRefresh(result Result) error {
	switch {
	case result.AccessToken == "":
		return errors.New("model access-token refresh returned no access token")
	case result.ExpiresIn <= 0:
		return errors.New("model access-token refresh returned a non-positive lifetime")
	}
	return nil
}

func refreshDelay(expiresIn time.Duration) time.Duration {
	delay := expiresIn - expiresIn/10
	if delay <= 0 {
		return time.Nanosecond
	}
	return delay
}

func (m *Manager) ready(token string) {
	m.mu.Lock()
	m.accessToken = token
	m.status = Status{State: StateReady}
	subscribers := append([]func(string){}, m.subscribers...)
	m.mu.Unlock()
	for _, subscriber := range subscribers {
		subscriber(token)
	}
}

func (m *Manager) failed(err error) {
	m.mu.Lock()
	m.accessToken = ""
	m.status = Status{State: StateError, Error: err.Error()}
	m.mu.Unlock()
	m.log.Error("model login failed", "error", err)
}
