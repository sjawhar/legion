package requests

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/machine"
)

type wakeCall struct {
	enrollmentID, requestID, state string
}

// TestSweeperTickExpiresPendingRequestsAndMachineLogins exercises one Tick's whole contract: an
// overdue pending agent_secret request is expired and its owner woken exactly once, a request
// still inside its deadline is left alone and never woken, and an overdue pending machine login
// gets its own 'expired' event.
func TestSweeperTickExpiresPendingRequestsAndMachineLogins(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()

	// An overdue pending request: PendingTTL negative backdates pending_expires_at into the past
	// at creation time, so it is immediately eligible for the sweep.
	m.PendingTTL = -time.Hour
	overdue, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "")
	if err != nil || overdue.State != "pending" {
		t.Fatalf("Create(overdue) = %+v, %v, want state pending", overdue, err)
	}

	// A second enrollment's own overdue pending request, to pin that Wake fires once PER request
	// leaving pending, not once per tick.
	enr2, key2 := newEnrollment(t, m.Store, "box", "box-b-"+t.Name(), new("sjawhar"), nil)
	overdue2, err := m.Create(ctx, enr2, signRequest(t, m, key2, "need it too", "DEEL_API_KEY"), "")
	if err != nil || overdue2.State != "pending" {
		t.Fatalf("Create(overdue2) = %+v, %v, want state pending", overdue2, err)
	}

	// A request still inside its deadline: must be left alone and never woken. A third,
	// distinct enrollment avoids coalescing onto overdue's own pending row for the same secret.
	enr3, key3 := newEnrollment(t, m.Store, "box", "box-c-"+t.Name(), new("sjawhar"), nil)
	m.PendingTTL = time.Hour
	fresh, err := m.Create(ctx, enr3, signRequest(t, m, key3, "not yet", "DEEL_API_KEY"), "")
	if err != nil || fresh.State != "pending" {
		t.Fatalf("Create(fresh) = %+v, %v, want state pending", fresh, err)
	}

	// An overdue pending machine login, inserted directly (machine.Service's own Login flow is
	// exercised by the machine package's own tests; this only needs a row ExpirePending sweeps).
	loginRecordID := "login-" + t.Name()
	if _, err := m.Store.Pool.Exec(ctx, `insert into credential_requests (id, body, kind, approver, expires_at) values ($1,'body','launcher_credential','sjawhar',$2)`,
		loginRecordID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("insert overdue machine login: %v", err)
	}

	machineLogins := &machine.Service{Store: m.Store}

	var mu sync.Mutex
	var woken []wakeCall
	sweeper := &Sweeper{
		Machine:       m,
		MachineLogins: machineLogins,
		Interval:      time.Hour,
		Wake: func(_ context.Context, enrollmentID, requestID, state string) {
			mu.Lock()
			defer mu.Unlock()
			woken = append(woken, wakeCall{enrollmentID, requestID, state})
		},
	}

	sweeper.Tick(ctx)

	got, err := m.Get(ctx, overdue.ID)
	if err != nil || got.State != "expired" {
		t.Fatalf("overdue request state = %+v, %v, want expired", got, err)
	}
	got2, err := m.Get(ctx, overdue2.ID)
	if err != nil || got2.State != "expired" {
		t.Fatalf("overdue2 request state = %+v, %v, want expired", got2, err)
	}
	gotFresh, err := m.Get(ctx, fresh.ID)
	if err != nil || gotFresh.State != "pending" {
		t.Fatalf("fresh request state = %+v, %v, want still pending", gotFresh, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(woken) != 2 {
		t.Fatalf("woken = %+v, want exactly 2 calls (one per request that left pending)", woken)
	}
	wantEnrollments := map[string]string{overdue.ID: enr, overdue2.ID: enr2}
	for _, w := range woken {
		if w.state != "expired" {
			t.Fatalf("wake state = %q, want expired", w.state)
		}
		wantEnrollment, ok := wantEnrollments[w.requestID]
		if !ok {
			t.Fatalf("woken for unexpected request %q", w.requestID)
		}
		if w.enrollmentID != wantEnrollment {
			t.Fatalf("wake for request %q named enrollment %q, want %q", w.requestID, w.enrollmentID, wantEnrollment)
		}
		delete(wantEnrollments, w.requestID)
	}

	var loginEvent string
	if err := m.Store.Pool.QueryRow(ctx, `select event from credential_request_events where record_id=$1`, loginRecordID).Scan(&loginEvent); err != nil {
		t.Fatalf("read machine login expiry event: %v", err)
	}
	if loginEvent != "expired" {
		t.Fatalf("machine login event = %q, want expired", loginEvent)
	}
}
