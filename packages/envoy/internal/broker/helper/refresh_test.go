// packages/envoy/internal/broker/helper/refresh_test.go
//go:build linux

package helper

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestALoginIsKeptOnDiskAndAStartResumesIt: an approved login is saved to the machine file before
// the helper installs it, and a new Broker started on that file holds the same credential at once,
// reports it issued, and refreshes it right away: the refresh spends the kept refresh token, and
// the file then holds the rotated one.
func TestALoginIsKeptOnDiskAndAStartResumesIt(t *testing.T) {
	f := newFakeBroker(t)
	b := loggedInBroker(t, f, "ada@example.com")
	held := b.cred.Load()
	kept, err := LoadMachineFile(b.MachineFile)
	if err != nil {
		t.Fatalf("the approved login's machine file: %v", err)
	}
	if kept.CredentialID != held.id || kept.RefreshToken != held.refreshToken || !kept.ExpiresAt.Equal(held.expiresAt) || kept.Key.D.Cmp(held.key.D) != 0 {
		t.Fatalf("machine file %+v; want the held login %s", kept, held.id)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resumed := &Broker{URL: f.srv.URL, OperatorFile: b.OperatorFile, MachineFile: b.MachineFile, HTTP: f.srv.Client()}
	if err := resumed.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if ls := resumed.LoginStatus(); ls.State != "issued" || !ls.CredentialHeld || ls.Code != "" {
		t.Fatalf("login-status of a helper started on the machine file: %+v; want issued and held, with no code", ls)
	}
	if got := resumed.cred.Load(); got.id != held.id {
		t.Fatalf("resumed credential %s; want %s", got.id, held.id)
	}
	waitFor(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.refreshes == 1
	})
	waitFor(t, func() bool {
		now, err := LoadMachineFile(b.MachineFile)
		return err == nil && now.RefreshToken != kept.RefreshToken && now.RefreshToken == resumed.cred.Load().refreshToken
	})
	f.mu.Lock()
	_, spent := f.refreshTokens[kept.RefreshToken]
	f.mu.Unlock()
	if spent {
		t.Fatal("the kept refresh token is still live after the refresh spent it")
	}
}

// TestARefusedRefreshDropsTheLoginAndItsFile: a refresh the broker answers invalid_grant (the
// login was revoked, lapsed, or its refresh token was reused) drops the credential with its own
// reason and deletes the machine file, so the next start holds no login.
func TestARefusedRefreshDropsTheLoginAndItsFile(t *testing.T) {
	var out syncBuffer
	f := newFakeBroker(t)
	b := &Broker{URL: f.srv.URL, OperatorFile: operatorFile(t, "ada@example.com"), MachineFile: machineFilePath(t), HTTP: f.srv.Client(),
		Log: slog.New(slog.NewJSONHandler(&out, nil))}
	if _, err := b.Login(context.Background(), "helper-host"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.LoginStatus().State == "issued" })
	held := b.cred.Load()
	f.mu.Lock()
	f.refreshOutcome = "invalid_grant"
	f.mu.Unlock()
	if err := b.refresh(context.Background()); err != nil {
		t.Fatalf("a refused refresh is settled, not retried: %v", err)
	}
	if ls := b.LoginStatus(); ls.CredentialHeld || ls.State != "expired" || ls.Dropped != dropRefreshRefused {
		t.Fatalf("login-status after a refused refresh: %+v; want expired, dropped for %q", ls, dropRefreshRefused)
	}
	if _, err := LoadMachineFile(b.MachineFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the machine file after a refused refresh: %v; want it deleted", err)
	}
	line := waitForRecord(t, &out, dropRefreshRefused+"; cleared: no session can enroll until a human approves a new machine login (run: agent-secrets machine login)")
	if line["level"] != "ERROR" || line["credential_id"] != held.id || line["code"] != "invalid_grant" {
		t.Fatalf("the drop line: %v; want an ERROR naming %s and invalid_grant", line, held.id)
	}
}

// TestARefreshThatDoesNotReachTheBrokerKeepsTheLogin: a refresh that fails for any other reason
// (here the broker answers 503) keeps the credential and the machine file as they were, and the
// refresh loop retries it on its backoff until one succeeds.
func TestARefreshThatDoesNotReachTheBrokerKeepsTheLogin(t *testing.T) {
	f := newFakeBroker(t)
	b := loggedInBroker(t, f, "ada@example.com")
	held := b.cred.Load()
	f.mu.Lock()
	f.refreshOutcome = "unavailable"
	f.mu.Unlock()
	if err := b.refresh(context.Background()); err == nil {
		t.Fatal("a refresh the broker answered 503 must be retried")
	}
	if b.cred.Load() != held {
		t.Fatal("a refresh that did not reach the broker replaced the credential")
	}
	kept, err := LoadMachineFile(b.MachineFile)
	if err != nil || kept.RefreshToken != held.refreshToken {
		t.Fatalf("the machine file after a failed refresh: %+v %v; want the held login unchanged", kept, err)
	}

	b.refreshEvery, b.refreshRetry = time.Hour, 50*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.refreshLoop(ctx, true)
	waitFor(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.refreshes >= 3
	})
	if b.cred.Load() != held {
		t.Fatal("failed refreshes replaced the credential")
	}
	f.mu.Lock()
	f.refreshOutcome = ""
	f.mu.Unlock()
	waitFor(t, func() bool { cred := b.cred.Load(); return cred != nil && cred != held && cred.id == held.id })
	if ls := b.LoginStatus(); ls.State != "issued" || !ls.CredentialHeld {
		t.Fatalf("login-status after the retried refresh: %+v; want issued and held", ls)
	}
}

// TestARefreshMovesTheExpiryAndSilencesTheOldWatch: a refresh swaps in the broker's new expiry,
// so the credential is not dropped at the expiry it was installed with, and no warning or drop is
// logged for that earlier expiry.
func TestARefreshMovesTheExpiryAndSilencesTheOldWatch(t *testing.T) {
	var out syncBuffer
	f := newFakeBroker(t)
	f.mu.Lock()
	f.loginLifetime = 3 * time.Second
	f.mu.Unlock()
	b := &Broker{URL: f.srv.URL, OperatorFile: operatorFile(t, "ada@example.com"), MachineFile: machineFilePath(t), HTTP: f.srv.Client(),
		Log: slog.New(slog.NewJSONHandler(&out, nil)), expiryWarning: time.Second}
	old := expiryLogin(t, b)
	f.mu.Lock()
	f.loginLifetime = 7 * 24 * time.Hour
	f.mu.Unlock()
	if err := b.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(old.expiresAt.Add(time.Second)))
	now := b.cred.Load()
	if now == nil || now.id != old.id || time.Until(now.expiresAt) < 6*24*time.Hour {
		t.Fatalf("the credential past its first expiry: %+v; want %s held a week out", now, old.id)
	}
	for _, rec := range logRecords(t, &out) {
		if rec["msg"] == expiresSoonMsg || rec["msg"] == expiredMsg {
			t.Fatalf("the refreshed-away expiry was logged: %v", rec)
		}
	}
	if kept, err := LoadMachineFile(b.MachineFile); err != nil || !kept.ExpiresAt.Equal(now.expiresAt) {
		t.Fatalf("machine file after the refresh: %+v %v; want expiry %s", kept, err, now.expiresAt)
	}
}

// TestAMachineFileThatDoesNotParseStopsTheStart: Start refuses a machine file it cannot read as a
// login, naming it, rather than starting with no login and a file it would later overwrite.
func TestAMachineFileThatDoesNotParseStopsTheStart(t *testing.T) {
	path := machineFilePath(t)
	if err := os.MkdirAll(path[:len(path)-len("/machine.json")], 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &Broker{URL: "http://127.0.0.1:1", MachineFile: path}
	if err := b.Start(context.Background()); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Start on an unreadable machine file: %v; want an error naming it", err)
	}
}

// heldRefresh starts b.refresh with the fake holding its refresh grant at a gate, and returns once
// the grant has reached the fake (refresh then holds credMu), with the function that releases the
// grant and waits for refresh's answer.
func heldRefresh(t *testing.T, f *fakeBroker, b *Broker) (release func() error) {
	t.Helper()
	gate := make(chan struct{})
	f.mu.Lock()
	f.refreshGate = gate
	f.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- b.refresh(context.Background()) }()
	waitFor(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.refreshArrivals == 1
	})
	return func() error {
		close(gate)
		return <-done
	}
}

// TestAnExpiryDuringARefreshNeverUndoesIt: the held credential reaches its stored expiry while a
// refresh of it is in flight, after the broker has rotated its tokens and before the helper swaps
// the result in. The expiry's drop waits for the refresh, then finds the credential refreshed and
// drops nothing: the helper holds the refreshed credential, its machine file names it, and no
// expiry is logged.
func TestAnExpiryDuringARefreshNeverUndoesIt(t *testing.T) {
	var out syncBuffer
	f := newFakeBroker(t)
	b := &Broker{URL: f.srv.URL, OperatorFile: operatorFile(t, "ada@example.com"), MachineFile: machineFilePath(t), HTTP: f.srv.Client(),
		Log: slog.New(slog.NewJSONHandler(&out, nil))}
	if _, err := b.Login(context.Background(), "helper-host"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.LoginStatus().State == "issued" })
	// The held credential, as one whose stored expiry has just passed; its watch is started below,
	// once the refresh is in flight, so the order of the two is the test's, not the timer's.
	login := b.cred.Load()
	lapsing := &machineCredential{key: login.key, id: login.id, expiresAt: time.Now().Add(-time.Second), refreshToken: login.refreshToken, gone: make(chan struct{})}
	b.stateMu.Lock()
	b.cred.Store(lapsing)
	b.stateMu.Unlock()
	close(login.gone)

	release := heldRefresh(t, f, b)
	waiting := make(chan struct{})
	b.testBeforeDrop = func() { close(waiting) }
	watched := make(chan struct{})
	go func() { b.watchExpiry(lapsing); close(watched) }()
	<-waiting // the expiry's drop has been reached, while the refresh holds credMu
	if err := release(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	<-watched

	held := b.cred.Load()
	if held == nil || held == lapsing || held.id != login.id || time.Until(held.expiresAt) < 6*24*time.Hour {
		t.Fatalf("after an expiry that raced a refresh the helper holds %+v; want the refreshed %s, a week out", held, login.id)
	}
	if ls := b.LoginStatus(); ls.State != "issued" || ls.Refused || !ls.CredentialHeld {
		t.Fatalf("login-status: %+v; want issued and held", ls)
	}
	if kept, err := LoadMachineFile(b.MachineFile); err != nil || kept.RefreshToken != held.refreshToken {
		t.Fatalf("machine file %+v (%v); want the refreshed login", kept, err)
	}
	for _, rec := range logRecords(t, &out) {
		if rec["msg"] == expiredMsg {
			t.Fatalf("an expiry dropped the credential a refresh had just renewed: %v", rec)
		}
	}
}

// TestARefusalDuringARefreshNeverUndoesIt: a launcher call signed with the credential being
// refreshed is refused 401 LAUNCHER_INVALID while the refresh is in flight. The refusal's drop
// waits for the refresh, then finds the credential refreshed: it drops nothing and hands the
// caller the broker's refusal to retry, not errNoCredential.
func TestARefusalDuringARefreshNeverUndoesIt(t *testing.T) {
	f := newFakeBroker(t)
	b := loggedInBroker(t, f, "ada@example.com")
	signed := b.cred.Load()
	release := heldRefresh(t, f, b)
	waiting := make(chan struct{})
	b.testBeforeDrop = func() { close(waiting) }
	rejected := &BrokerError{Status: http.StatusUnauthorized, Code: "LAUNCHER_INVALID", Message: "the launcher credential is not valid"}
	refused := make(chan error, 1)
	go func() { refused <- b.clearOnInvalid(signed, rejected) }()
	<-waiting
	if err := release(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if err := <-refused; err != rejected {
		t.Fatalf("a refusal of the credential a refresh replaced reached its caller as %v; want the broker's refusal as it came", err)
	}
	if held := b.cred.Load(); held == nil || held == signed || held.id != signed.id {
		t.Fatalf("after a refusal that raced a refresh the helper holds %+v; want the refreshed %s", held, signed.id)
	}
	if ls := b.LoginStatus(); ls.State != "issued" || ls.Refused {
		t.Fatalf("login-status: %+v; want issued, not refused", ls)
	}
}
