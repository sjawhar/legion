package modellogin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type authAttempt struct {
	result Result
	err    error
}

type recordingClient struct {
	mu       sync.Mutex
	password []authAttempt
	refresh  []authAttempt
	calls    []string
}

func (c *recordingClient) PasswordAuth(_ context.Context, username, password string) (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if username != "machine-user" || password != "machine-password" {
		return Result{}, errors.New("unexpected credentials")
	}
	c.calls = append(c.calls, "password")
	attempt := c.password[0]
	c.password = c.password[1:]
	return attempt.result, attempt.err
}

func (c *recordingClient) RefreshAuth(_ context.Context, refreshToken string) (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if refreshToken != "refresh-1" {
		return Result{}, errors.New("unexpected refresh token")
	}
	c.calls = append(c.calls, "refresh")
	attempt := c.refresh[0]
	c.refresh = c.refresh[1:]
	return attempt.result, attempt.err
}

func (c *recordingClient) Calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

type sleepRequest struct {
	duration time.Duration
	done     chan struct{}
}

type testClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps chan sleepRequest
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC), sleeps: make(chan sleepRequest, 4)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Sleep(ctx context.Context, duration time.Duration) error {
	done := make(chan struct{})
	select {
	case c.sleeps <- sleepRequest{duration: duration, done: done}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *testClock) advance(t *testing.T) time.Duration {
	t.Helper()
	select {
	case request := <-c.sleeps:
		c.mu.Lock()
		c.now = c.now.Add(request.duration)
		c.mu.Unlock()
		close(request.done)
		return request.duration
	case <-time.After(time.Second):
		t.Fatal("manager did not schedule its next refresh")
		return 0
	}
}

func nextToken(t *testing.T, updates <-chan string) string {
	t.Helper()
	select {
	case token := <-updates:
		return token
	case <-time.After(time.Second):
		t.Fatal("manager did not publish an access token")
		return ""
	}
}

func waitForStatus(t *testing.T, manager *Manager, want string) Status {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status := manager.Status()
		if status.State == want {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("manager status = %#v, want state %q", manager.Status(), want)
	return Status{}
}

func TestManagerRefreshesAccessTokenBeforeItExpires(t *testing.T) {
	clock := newTestClock()
	client := &recordingClient{
		password: []authAttempt{{result: Result{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresIn: time.Hour}}},
		refresh:  []authAttempt{{result: Result{AccessToken: "access-2", ExpiresIn: time.Hour}}},
	}
	manager := New(Credentials{Username: "machine-user", Password: "machine-password"}, client, clock, nil)
	updates := make(chan string, 2)
	manager.Subscribe(func(token string) { updates <- token })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); manager.Run(ctx) }()

	if token := nextToken(t, updates); token != "access-1" {
		t.Fatalf("initial access token = %q, want access-1", token)
	}
	if delay := clock.advance(t); delay != 54*time.Minute {
		t.Fatalf("refresh delay = %s, want 54m before the one-hour expiry", delay)
	}
	if token := nextToken(t, updates); token != "access-2" {
		t.Fatalf("refreshed access token = %q, want access-2", token)
	}
	if got := client.Calls(); len(got) != 2 || got[0] != "password" || got[1] != "refresh" {
		t.Fatalf("authentication calls = %v, want [password refresh]", got)
	}
	if status := waitForStatus(t, manager, StateReady); status.Error != "" {
		t.Fatalf("status after refresh = %#v, want a ready state without an error", status)
	}
	cancel()
	<-done
}

func TestManagerSignsInAgainWhenRefreshFails(t *testing.T) {
	clock := newTestClock()
	client := &recordingClient{
		password: []authAttempt{
			{result: Result{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresIn: time.Hour}},
			{result: Result{AccessToken: "access-2", RefreshToken: "refresh-2", ExpiresIn: time.Hour}},
		},
		refresh: []authAttempt{{err: errors.New("refresh rejected")}},
	}
	manager := New(Credentials{Username: "machine-user", Password: "machine-password"}, client, clock, nil)
	updates := make(chan string, 2)
	manager.Subscribe(func(token string) { updates <- token })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); manager.Run(ctx) }()

	if token := nextToken(t, updates); token != "access-1" {
		t.Fatalf("initial access token = %q, want access-1", token)
	}
	clock.advance(t)
	if token := nextToken(t, updates); token != "access-2" {
		t.Fatalf("access token after a rejected refresh = %q, want the fresh login token", token)
	}
	if got := client.Calls(); len(got) != 3 || got[0] != "password" || got[1] != "refresh" || got[2] != "password" {
		t.Fatalf("authentication calls = %v, want [password refresh password]", got)
	}
	cancel()
	<-done
}

func TestManagerReportsAnUnobtainableLoginWithoutExposingCredentials(t *testing.T) {
	clock := newTestClock()
	client := &recordingClient{password: []authAttempt{{err: errors.New("login refused")}}}
	manager := New(Credentials{Username: "machine-user", Password: "machine-password"}, client, clock, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); manager.Run(ctx) }()

	status := waitForStatus(t, manager, StateError)
	if status.Error != "login refused" {
		t.Fatalf("status error = %q, want the authentication failure", status.Error)
	}
	if token, ok := manager.AccessToken(); ok || token != "" {
		t.Fatalf("failed login access token = %q, %t; want none", token, ok)
	}
	cancel()
	<-done
}
