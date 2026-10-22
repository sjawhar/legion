// packages/envoy/internal/broker/helper/expiry_test.go
//go:build linux

package helper

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// expiryLogin logs b in against f, whose poll answers issued at once, and waits for the
// credential to be installed.
func expiryLogin(t *testing.T, b *Broker) *machineCredential {
	t.Helper()
	before := b.cred.Load()
	if _, err := b.Login(context.Background(), "example-host-devbox"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if cred := b.cred.Load(); cred != nil && cred != before {
			return cred
		}
		if time.Now().After(deadline) {
			t.Fatalf("the login never installed a credential: %+v", b.LoginStatus())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// logRecords parses a JSON slog buffer, keeping each record's time.
func logRecords(t *testing.T, out *syncBuffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		records = append(records, rec)
	}
	return records
}

func recordTime(t *testing.T, rec map[string]any) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, rec["time"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return at
}

const expiredMsg = dropExpired + "; cleared: no session can enroll until a human approves a new machine login (run: agent-secrets machine login)"

// TestTheHelperWarnsBeforeItsLauncherCredentialExpiresAndDropsItThen: with no refresh moving its
// expiry (here the helper's refresh loop has not run), the helper says ahead of time (a day,
// shortened here) that the credential lapses soon, and at the stored expiry drops it as a refusal
// would and says so at ERROR: login-status then reads expired rather than reporting a credential
// the broker will refuse.
func TestTheHelperWarnsBeforeItsLauncherCredentialExpiresAndDropsItThen(t *testing.T) {
	var out syncBuffer
	f := newFakeBroker(t)
	f.mu.Lock()
	f.loginLifetime = 3 * time.Second
	f.mu.Unlock()
	b := &Broker{URL: f.srv.URL, OperatorFile: operatorFile(t, "ada@example.com"), MachineFile: machineFilePath(t), HTTP: f.srv.Client(),
		Log: slog.New(slog.NewJSONHandler(&out, nil)), expiryWarning: 2 * time.Second}

	cred := expiryLogin(t, b)
	status := b.LoginStatus()
	if !status.CredentialHeld || !status.CredentialExpiresAt.Equal(cred.expiresAt) || time.Until(cred.expiresAt) > 3*time.Second {
		t.Fatalf("login-status after the login: %+v; want the credential held, expiring within 3 s", status)
	}

	deadline := cred.expiresAt.Add(5 * time.Second)
	for b.HasCredential() {
		if time.Now().After(deadline) {
			t.Fatalf("the credential is still held 5 s past its expiry %s", cred.expiresAt)
		}
		time.Sleep(20 * time.Millisecond)
	}
	expiresAt := cred.expiresAt.Format(time.RFC3339)
	// drop clears the credential before it logs the drop, so the ERROR can land just after
	// HasCredential turns false; the warning came before both.
	expired := waitForRecord(t, &out, expiredMsg)
	var warned map[string]any
	for _, rec := range logRecords(t, &out) {
		if rec["msg"] == expiresSoonMsg {
			warned = rec
		}
	}
	if warned == nil || warned["level"] != "WARN" || warned["credential_id"] != cred.id || warned["expires_at"] != expiresAt {
		t.Fatalf("the warning before the expiry: %v; want a WARN naming %s and %s (log: %s)", warned, cred.id, expiresAt, out.String())
	}
	// It comes no sooner than expiryWarning before the expiry (the JSON time is to the nanosecond).
	if at := recordTime(t, warned); at.Before(cred.expiresAt.Add(-2*time.Second)) || !at.Before(cred.expiresAt) {
		t.Fatalf("the warning came at %s; want within 2 s before the expiry %s", at, cred.expiresAt)
	}
	if expired["level"] != "ERROR" || expired["credential_id"] != cred.id || expired["expires_at"] != expiresAt {
		t.Fatalf("the expiry line: %v; want an ERROR naming %s and %s (log: %s)", expired, cred.id, expiresAt, out.String())
	}
	if at := recordTime(t, expired); at.Before(cred.expiresAt) {
		t.Fatalf("the credential was dropped at %s, before its expiry %s", at, cred.expiresAt)
	}
	if status := b.LoginStatus(); status.State != "expired" || !status.Refused || status.Dropped != dropExpired || status.CredentialHeld || !status.CredentialExpiresAt.IsZero() {
		t.Fatalf("login-status after the expiry: %+v; want expired, dropped at its expiry, no credential and no expiry", status)
	}
	if why := b.noCredentialReason(); why != dropExpired {
		t.Fatalf("why a session cannot enroll after the expiry: %q; want %q, not the broker's refusal", why, dropExpired)
	}
}

// TestTheHelperWarnsADayBeforeTheExpiry: with no lead set, as in production, the warning comes a
// day before the credential lapses. A credential with less than a day left warns at once; one
// with more than a day left does not warn yet.
func TestTheHelperWarnsADayBeforeTheExpiry(t *testing.T) {
	for _, tc := range []struct {
		lifetime time.Duration
		warns    bool
	}{
		{23 * time.Hour, true},
		{25 * time.Hour, false},
	} {
		t.Run(tc.lifetime.String(), func(t *testing.T) {
			var out syncBuffer
			f := newFakeBroker(t)
			f.mu.Lock()
			f.loginLifetime = tc.lifetime
			f.mu.Unlock()
			b := &Broker{URL: f.srv.URL, OperatorFile: operatorFile(t, "ada@example.com"), MachineFile: machineFilePath(t), HTTP: f.srv.Client(),
				Log: slog.New(slog.NewJSONHandler(&out, nil))}
			expiryLogin(t, b)
			warned := func() bool {
				return slices.ContainsFunc(logRecords(t, &out), func(rec map[string]any) bool { return rec["msg"] == expiresSoonMsg })
			}
			deadline := time.Now().Add(300 * time.Millisecond)
			for !warned() && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if got := warned(); got != tc.warns {
				t.Fatalf("a credential expiring in %s: warned within 300 ms %v; want %v (log: %s)", tc.lifetime, got, tc.warns, out.String())
			}
		})
	}
}

// TestANewerLoginSilencesTheExpiryOfTheCredentialItReplaces: a re-login installs a fresh
// credential before the old one lapses; the old one's warning and expiry must then say nothing,
// and above all must not drop the credential that replaced it.
func TestANewerLoginSilencesTheExpiryOfTheCredentialItReplaces(t *testing.T) {
	var out syncBuffer
	f := newFakeBroker(t)
	f.mu.Lock()
	f.loginLifetime = 4 * time.Second // the re-login's first token poll comes a second after it starts
	f.mu.Unlock()
	b := &Broker{URL: f.srv.URL, OperatorFile: operatorFile(t, "ada@example.com"), MachineFile: machineFilePath(t), HTTP: f.srv.Client(),
		Log: slog.New(slog.NewJSONHandler(&out, nil)), expiryWarning: time.Second}

	old := expiryLogin(t, b)
	f.mu.Lock()
	f.loginLifetime = 7 * 24 * time.Hour
	f.mu.Unlock()
	fresh := expiryLogin(t, b)
	time.Sleep(time.Until(old.expiresAt.Add(time.Second)))

	if b.cred.Load() != fresh {
		t.Fatalf("the credential held past the old one's expiry is %+v; want the newer login's %s", b.cred.Load(), fresh.id)
	}
	for _, rec := range logRecords(t, &out) {
		if rec["msg"] == expiresSoonMsg || rec["msg"] == expiredMsg {
			t.Fatalf("a replaced credential's expiry was logged: %v", rec)
		}
	}
}

// TestATokenAnswerWithoutTheCredentialIsNotKept: a token answer that does not name the launcher
// credential's expiry is not a login the helper can keep or watch. It installs nothing, writes no
// machine file, says so at ERROR, and the login reads expired, so `machine login` exits 1.
func TestATokenAnswerWithoutTheCredentialIsNotKept(t *testing.T) {
	var out syncBuffer
	f := newFakeBroker(t)
	f.mu.Lock()
	f.loginLifetime = 0
	f.mu.Unlock()
	b := &Broker{URL: f.srv.URL, OperatorFile: operatorFile(t, "ada@example.com"), MachineFile: machineFilePath(t), HTTP: f.srv.Client(),
		Log: slog.New(slog.NewJSONHandler(&out, nil))}
	if _, err := b.Login(context.Background(), "example-host-devbox"); err != nil {
		t.Fatal(err)
	}
	line := waitForRecord(t, &out, "machine login approved, but the helper could not keep it; run: agent-secrets machine login again")
	if line["level"] != "ERROR" {
		t.Fatalf("the line: %v; want an ERROR", line)
	}
	waitFor(t, func() bool { return b.LoginStatus().State == "expired" })
	if b.HasCredential() {
		t.Fatal("a login the helper cannot keep must not install a credential")
	}
	if _, err := LoadMachineFile(b.MachineFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a login the helper cannot keep wrote a machine file: %v", err)
	}
}
