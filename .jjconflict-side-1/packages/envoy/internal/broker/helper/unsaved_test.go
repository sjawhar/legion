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
	t.Cleanup(b.Close)
	if _, err := b.Login(context.Background(), "helper-host"); err != nil {
		t.Fatalf("login: %v", err)
	}
	waitFor(t, func() bool { return b.LoginStatus().State == "issued" })
	return b
}

func (f *fakeBroker) refreshCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.refreshes }

// TestARefreshWhoseSaveFailsRemovesTheFileAndRetriesOnlyTheSave: once the broker has rotated the
// token and the save failed, the machine file is gone at once (ERROR says so), the login stays in
// memory, and every later attempt retries the save alone at the fixed retry, never another token
// request; a save that lands writes the file back and spends no rotation.
func TestARefreshWhoseSaveFailsRemovesTheFileAndRetriesOnlyTheSave(t *testing.T) {
	var out syncBuffer
	f := newFakeBroker(t)
	b := loggedInBrokerLogging(t, f, slog.New(slog.NewJSONHandler(&out, nil)))
	original := b.cred.Load().refreshToken
	restore := fullDisk(b)
	if err := b.refresh(context.Background()); err == nil || f.refreshCount() != 1 {
		t.Fatalf("a refresh whose save failed = %v after %d token requests; want the save error after 1", err, f.refreshCount())
	}
	if _, err := LoadMachineFile(b.MachineFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the machine file after the failed save: %v; want it removed at once", err)
	}
	if line := waitForRecord(t, &out, unsavedRemovedMsg); line["level"] != "ERROR" || line["credential_id"] != b.cred.Load().id {
		t.Fatalf("the removal line: %v; want an ERROR naming the held credential", line)
	}
	if !b.HasCredential() {
		t.Fatal("the helper must hold the rotated login in memory")
	}
	sched := b.newRetrySchedule()
	for i := range 10 {
		err := b.refresh(context.Background())
		if err == nil {
			t.Fatalf("retry %d: the save cannot land yet, want its error", i+1)
		}
		if wait := sched.after(err, func() time.Duration { return time.Hour }); wait != defaultRefreshRetry {
			t.Fatalf("retry %d: wait %s; want the fixed %s", i+1, wait, defaultRefreshRetry)
		}
	}
	if n := f.refreshCount(); n != 1 {
		t.Fatalf("token requests while the save failed = %d; want only the first", n)
	}
	restore()
	if err := b.refresh(context.Background()); err != nil {
		t.Fatalf("the save once the disk takes writes: %v", err)
	}
	if !fileHoldsTheHeldLogin(b, original) || f.refreshCount() != 1 {
		t.Fatalf("after the save landed: file holds the held login %v, token requests %d; want true and 1", fileHoldsTheHeldLogin(b, original), f.refreshCount())
	}
	if err := b.refresh(context.Background()); err != nil || f.refreshCount() != 2 {
		t.Fatalf("the next refresh: %v, token requests %d; want a rotation", err, f.refreshCount())
	}
}

// TestASettlementOfAnUnsavedLoginRetriesOnlyTheSave: a launcher refusal dropping the unsaved
// login hands it to settleRefused, which retries its save and makes no token request while that
// save fails.
func TestASettlementOfAnUnsavedLoginRetriesOnlyTheSave(t *testing.T) {
	f := newFakeBroker(t)
	b := loggedInBroker(t, f, "ada@example.com")
	restore := fullDisk(b)
	_ = b.refresh(context.Background())
	unsaved := b.cred.Load()
	if !b.drop(unsaved, dropRefused, "code", "LAUNCHER_INVALID") {
		t.Fatal("drop")
	}
	for range 3 {
		if err := b.settleRefused(context.Background(), unsaved); err == nil {
			t.Fatal("a settlement whose save cannot land must return its error")
		}
	}
	if n := f.refreshCount(); n != 1 {
		t.Fatalf("token requests while the settlement's save failed = %d; want only the first", n)
	}
	restore()
	if err := b.settleRefused(context.Background(), unsaved); err != nil || f.refreshCount() != 2 {
		t.Fatalf("the settlement once the save lands: %v, token requests %d; want the save, then its refresh", err, f.refreshCount())
	}
}

// TestAHelperRestartedAfterAFailedSaveAsksNothing: with the file removed, a restart holds no
// login (login-status none) and makes no token request, so the rotated-away token is never
// presented.
func TestAHelperRestartedAfterAFailedSaveAsksNothing(t *testing.T) {
	f := newFakeBroker(t)
	b := loggedInBroker(t, f, "ada@example.com")
	fullDisk(b)
	_ = b.refresh(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	restarted := &Broker{URL: f.srv.URL, OperatorFile: b.OperatorFile, MachineFile: b.MachineFile, HTTP: f.srv.Client()}
	t.Cleanup(restarted.Close)
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

// TestAFailedSaveOnAReadOnlyDiskSaysARestartWillBeAReuse: when even the unlink fails (a
// read-only filesystem), the helper says at ERROR that a restart before the disk is writable
// again presents a spent token, which the broker revokes as a reuse.
func TestAFailedSaveOnAReadOnlyDiskSaysARestartWillBeAReuse(t *testing.T) {
	var out syncBuffer
	f := newFakeBroker(t)
	b := loggedInBrokerLogging(t, f, slog.New(slog.NewJSONHandler(&out, nil)))
	unwritable(t, b.MachineFile)
	_ = b.refresh(context.Background())
	if line := waitForRecord(t, &out, unsavedStuckMsg); line["level"] != "ERROR" {
		t.Fatalf("the line: %v; want an ERROR", line)
	}
	_ = b.refresh(context.Background())
	if f.refreshCount() != 1 {
		t.Fatalf("token requests = %d; want only the first", f.refreshCount())
	}
}

// TestContractASaveFailingThroughTwoRestartsRevokesNothing: against the real broker, a refresh
// rotates the token while the disk is full; the helper restarts inside the token's grace, then
// again after it, the disk still full both times. Neither restart presents a token: the
// credential is not revoked, the box's enrollment stays live, and no reuse is audited.
func TestContractASaveFailingThroughTwoRestartsRevokesNothing(t *testing.T) {
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
	cr.srv.Broker.credMu.Lock()
	fullDisk(cr.srv.Broker)
	cr.srv.Broker.credMu.Unlock()
	if err := cr.srv.Broker.refresh(ctx); err == nil {
		t.Fatal("the refresh's save must fail on the full disk")
	}
	cr.cancel()
	restart := func() *Broker {
		b := &Broker{URL: cr.broker.URL, OperatorFile: cr.broker.OperatorFile, MachineFile: cr.machineFile, HTTP: http.DefaultClient,
			Log: slog.New(slog.DiscardHandler)}
		t.Cleanup(b.Close)
		fullDisk(b)
		rctx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)
		if err := b.Start(rctx); err != nil {
			t.Fatal(err)
		}
		return b
	}
	first := restart() // inside the grace
	// More than five minutes pass with the disk still full: the rotated-away token's grace ends.
	if _, err := cr.broker.Store.Pool.Exec(ctx, `update oauth_refresh_tokens set grace_until = now() - interval '1 second' where grace_until is not null`); err != nil {
		t.Fatal(err)
	}
	second := restart()
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
		t.Fatalf("after two restarts: credential revoked %v, box enrollment live %v, reuse audit rows %d; want not revoked, live, none", credRevoked, boxLive, audit)
	}
	for i, b := range []*Broker{first, second} {
		if ls := b.LoginStatus(); ls.State != "" || b.HasCredential() {
			t.Fatalf("restart %d's login-status: %+v; want none", i+1, ls)
		}
	}
}
