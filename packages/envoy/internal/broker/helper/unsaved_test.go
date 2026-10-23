// packages/envoy/internal/broker/helper/unsaved_test.go
//go:build linux

package helper

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

// fullDisk fails every machine-file save of b the way a full disk does (ENOSPC), while an
// unlink still works, until the returned restore runs.
func fullDisk(b *Broker) (restore func()) {
	var mu sync.Mutex
	full := true
	b.testSave = func(f *MachineFile, path string) error {
		mu.Lock()
		defer mu.Unlock()
		if full {
			return &os.PathError{Op: "write", Path: path, Err: syscall.ENOSPC}
		}
		return f.Save(path)
	}
	return func() { mu.Lock(); full = false; mu.Unlock() }
}

// loggedInBrokerLogging is loggedInBroker with log set before the login starts, so the login's
// own goroutines never race a later write of b.Log.
func loggedInBrokerLogging(t *testing.T, f *fakeBroker, log *slog.Logger) *Broker {
	t.Helper()
	b := &Broker{URL: f.srv.URL, OperatorFile: operatorFile(t, "ada@example.com"), MachineFile: machineFilePath(t), HTTP: f.srv.Client(), Log: log}
	if _, err := b.Login(context.Background(), "helper-host"); err != nil {
		t.Fatalf("login: %v", err)
	}
	waitFor(t, func() bool { return b.LoginStatus().State == "issued" })
	return b
}

// fakeClock is a settable clock for b's unsaved-login deadline.
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *fakeClock) now() time.Time     { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *fakeClock) set(at time.Time)   { c.mu.Lock(); c.at = at; c.mu.Unlock() }
func clockAt(at time.Time) *fakeClock   { return &fakeClock{at: at} }
func (f *fakeBroker) refreshCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.refreshes }

// TestARefreshWhoseSaveFailsRetriesOnlyTheSave: once the broker has rotated the token and the
// save failed, every later attempt retries the save alone, never another token request, at the
// fixed retry, for as long as saves fail; a save that lands then keeps the held login on disk
// and spends no rotation either.
func TestARefreshWhoseSaveFailsRetriesOnlyTheSave(t *testing.T) {
	f := newFakeBroker(t)
	b := loggedInBroker(t, f, "ada@example.com")
	original := b.cred.Load().refreshToken
	start := time.Now()
	b.testNow = clockAt(start).now
	restore := fullDisk(b)
	err := b.refresh(context.Background())
	if err == nil || f.refreshCount() != 1 {
		t.Fatalf("a refresh whose save failed = %v after %d token requests; want the save error after 1", err, f.refreshCount())
	}
	sched := b.newRetrySchedule()
	for i := range 10 {
		err := b.refresh(context.Background())
		if err == nil {
			t.Fatalf("retry %d: the save cannot land yet, want its error", i+1)
		}
		if wait := sched.next(err); wait != defaultRefreshRetry {
			t.Fatalf("retry %d: wait %s; want the fixed %s", i+1, wait, defaultRefreshRetry)
		}
	}
	if n := f.refreshCount(); n != 1 {
		t.Fatalf("token requests while the save failed = %d; want only the first", n)
	}
	if kept, err := LoadMachineFile(b.MachineFile); err != nil || kept.RefreshToken != original {
		t.Fatalf("machine file %+v, %v; want the old login untouched before the deadline", kept, err)
	}
	restore()
	if err := b.refresh(context.Background()); err != nil {
		t.Fatalf("the save once the disk takes writes: %v", err)
	}
	if !fileHoldsTheHeldLogin(b, original) || f.refreshCount() != 1 {
		t.Fatalf("after the save landed: file holds the held login %v, token requests %d; want true and 1", fileHoldsTheHeldLogin(b, original), f.refreshCount())
	}
	// The next refresh is an ordinary one again.
	if err := b.refresh(context.Background()); err != nil || f.refreshCount() != 2 {
		t.Fatalf("the next refresh: %v, token requests %d; want a rotation", err, f.refreshCount())
	}
}

// TestAnUnsavedLoginRemovesTheFileBeforeTheGraceEnds: a save still failing 4 minutes after the
// rotation (inside the broker's 5-minute grace) removes the machine file and says at ERROR that
// the login is held only in memory; until then the file is left. A later save writes it again.
func TestAnUnsavedLoginRemovesTheFileBeforeTheGraceEnds(t *testing.T) {
	var out syncBuffer
	f := newFakeBroker(t)
	b := loggedInBrokerLogging(t, f, slog.New(slog.NewJSONHandler(&out, nil)))
	original := b.cred.Load().refreshToken
	start := time.Now()
	clock := clockAt(start)
	b.testNow = clock.now
	restore := fullDisk(b)
	_ = b.refresh(context.Background())
	clock.set(start.Add(unsavedFileDeadline - time.Second))
	_ = b.refresh(context.Background())
	if _, err := LoadMachineFile(b.MachineFile); err != nil {
		t.Fatalf("the machine file a second before the deadline: %v; want it still there", err)
	}
	clock.set(start.Add(unsavedFileDeadline))
	_ = b.refresh(context.Background())
	if _, err := LoadMachineFile(b.MachineFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the machine file at the deadline: %v; want it removed", err)
	}
	line := waitForRecord(t, &out, unsavedRemovedMsg)
	if line["level"] != "ERROR" || line["credential_id"] != b.cred.Load().id {
		t.Fatalf("the removal line: %v; want an ERROR naming the held credential", line)
	}
	if !b.HasCredential() || f.refreshCount() != 1 {
		t.Fatalf("after the removal: held %v, token requests %d; want the login still held in memory, no new rotation", b.HasCredential(), f.refreshCount())
	}
	restore()
	if err := b.refresh(context.Background()); err != nil || !fileHoldsTheHeldLogin(b, original) {
		t.Fatalf("a save after the removal: %v, file holds the held login %v; want it written again", err, fileHoldsTheHeldLogin(b, original))
	}
}

// TestAHelperRestartedAfterTheUnsavedFileWasRemovedAsksNothing: with the file removed, a restart
// holds no login (login-status none) and makes no token request, so the rotated-away token is
// never presented.
func TestAHelperRestartedAfterTheUnsavedFileWasRemovedAsksNothing(t *testing.T) {
	f := newFakeBroker(t)
	b := loggedInBroker(t, f, "ada@example.com")
	start := time.Now()
	clock := clockAt(start)
	b.testNow = clock.now
	fullDisk(b)
	_ = b.refresh(context.Background())
	clock.set(start.Add(unsavedFileDeadline))
	_ = b.refresh(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	restarted := &Broker{URL: f.srv.URL, OperatorFile: b.OperatorFile, MachineFile: b.MachineFile, HTTP: f.srv.Client()}
	if err := restarted.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if ls := restarted.LoginStatus(); ls.State != "" || ls.CredentialHeld || restarted.HasCredential() {
		t.Fatalf("login-status after the restart: %+v; want none", ls)
	}
	f.mu.Lock()
	arrivals := f.refreshArrivals
	f.mu.Unlock()
	if arrivals != 1 {
		t.Fatalf("token requests by then = %d; want only the first refresh's", arrivals)
	}
}

// TestAnUnsavedLoginOnAReadOnlyDiskSaysARestartWillBeAReuse: when even the unlink fails (a
// read-only filesystem), the helper says at ERROR that a restart before the disk is writable
// again presents a spent token, which the broker revokes as a reuse.
func TestAnUnsavedLoginOnAReadOnlyDiskSaysARestartWillBeAReuse(t *testing.T) {
	var out syncBuffer
	f := newFakeBroker(t)
	b := loggedInBrokerLogging(t, f, slog.New(slog.NewJSONHandler(&out, nil)))
	start := time.Now()
	clock := clockAt(start)
	b.testNow = clock.now
	unwritable(t, b.MachineFile)
	_ = b.refresh(context.Background())
	clock.set(start.Add(unsavedFileDeadline))
	_ = b.refresh(context.Background())
	line := waitForRecord(t, &out, unsavedStuckMsg)
	if line["level"] != "ERROR" {
		t.Fatalf("the line: %v; want an ERROR", line)
	}
	if f.refreshCount() != 1 {
		t.Fatalf("token requests = %d; want only the first", f.refreshCount())
	}
}

// TestContractASaveFailingThroughARestartRevokesNothing: against the real broker, a refresh
// rotates the token while the disk is full, the save keeps failing past the 4-minute deadline,
// the rotated-away token's grace then ends, and the helper restarts with the disk still full. The
// restart presents no token: the credential is not revoked, the box's enrollment stays live, and
// no reuse is audited.
func TestContractASaveFailingThroughARestartRevokesNothing(t *testing.T) {
	cr := newContractRig(t, brokertest.WithLease(time.Hour))
	credentialID := cr.login(t)
	boxKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	tp, err := proof.Thumbprint(&boxKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	box := cr.call(t, Request{Op: "enroll-session-key", RuntimeID: "box:unsaved:1", Thumbprint: tp})
	if !box.OK || box.EnrollmentID == "" {
		t.Fatalf("enroll-session-key: %+v", box)
	}
	ctx := context.Background()
	start := time.Now()
	clock := clockAt(start)
	cr.srv.Broker.credMu.Lock()
	cr.srv.Broker.testNow = clock.now
	fullDisk(cr.srv.Broker)
	cr.srv.Broker.credMu.Unlock()
	if err := cr.srv.Broker.refresh(ctx); err == nil {
		t.Fatal("the refresh's save must fail on the full disk")
	}
	clock.set(start.Add(unsavedFileDeadline))
	_ = cr.srv.Broker.refresh(ctx)
	// More than five minutes pass with the disk still full: the rotated-away token's grace ends.
	if _, err := cr.broker.Store.Pool.Exec(ctx, `update oauth_refresh_tokens set grace_until = now() - interval '1 second' where grace_until is not null`); err != nil {
		t.Fatal(err)
	}
	cr.cancel()
	restarted := &Broker{URL: cr.broker.URL, OperatorFile: cr.broker.OperatorFile, MachineFile: cr.machineFile, HTTP: http.DefaultClient,
		Log: slog.New(slog.DiscardHandler)}
	fullDisk(restarted)
	ctx2, cancel2 := context.WithCancel(ctx)
	defer cancel2()
	if err := restarted.Start(ctx2); err != nil {
		t.Fatal(err)
	}
	var credRevoked bool
	if err := cr.broker.Store.Pool.QueryRow(ctx, `select revoked_at is not null from launcher_credentials where id = $1`, credentialID).Scan(&credRevoked); err != nil {
		t.Fatal(err)
	}
	var boxLive bool
	if err := cr.broker.Store.Pool.QueryRow(ctx, `select revoked_at is null from enrollments where id = $1`, box.EnrollmentID).Scan(&boxLive); err != nil {
		t.Fatal(err)
	}
	var audit int
	if err := cr.broker.Store.Pool.QueryRow(ctx, `select count(*) from audit where actor = 'broker:refresh-token-reuse'`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if credRevoked || !boxLive || audit != 0 {
		t.Fatalf("after the restart: credential revoked %v, box enrollment live %v, reuse audit rows %d; want not revoked, live, none", credRevoked, boxLive, audit)
	}
	if ls := restarted.LoginStatus(); ls.State != "" || restarted.HasCredential() {
		t.Fatalf("the restarted helper's login-status: %+v; want none", ls)
	}
}
