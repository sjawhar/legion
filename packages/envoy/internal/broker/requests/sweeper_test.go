package requests

import (
	"context"
	"crypto/ecdsa"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/enroll"
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
		Enrollments:   &enroll.Service{Store: m.Store},
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

// TestSweeperEndsLapsedEnrollments pins that one Tick ends an enrollment whose lease has lapsed,
// as a launcher's revoke ends one: its live grant is revoked, its pending request is cancelled and
// its record closed, so the approver's pending list (GET /v1/pending) stops offering it. An
// enrollment still within its lease keeps its grant and its pending request, and a second Tick
// changes nothing.
func TestSweeperEndsLapsedEnrollments(t *testing.T) {
	m, gone, goneKey, approver := newFixture(t)
	ctx := context.Background()
	live, liveKey := newEnrollment(t, m.Store, "box", "box-live-"+t.Name(), new("sjawhar"), nil)

	type session struct{ grantID, pendingID, recordID string }
	open := func(enr string, key *ecdsa.PrivateKey) session {
		t.Helper()
		granted, err := m.Create(ctx, enr, signRequest(t, m, key, "automatic", "AUTO_TOKEN"), "")
		if err != nil || granted.GrantID == nil {
			t.Fatalf("Create(AUTO_TOKEN) = %+v, %v, want a grant", granted, err)
		}
		pending, err := m.Create(ctx, enr, signRequest(t, m, key, "needs approval", "DEEL_API_KEY"), "")
		if err != nil || pending.State != "pending" || pending.RecordID == nil {
			t.Fatalf("Create(DEEL_API_KEY) = %+v, %v, want pending with a record", pending, err)
		}
		return session{grantID: *granted.GrantID, pendingID: pending.ID, recordID: *pending.RecordID}
	}
	goneSession, liveSession := open(gone, goneKey), open(live, liveKey)
	if _, err := m.Store.Pool.Exec(ctx, `update enrollments set lease_expires_at = now() - interval '1 second' where id=$1`, gone); err != nil {
		t.Fatalf("lapse the lease: %v", err)
	}

	sweeper := &Sweeper{Enrollments: &enroll.Service{Store: m.Store}, Machine: m, MachineLogins: &machine.Service{Store: m.Store}, Interval: time.Hour}
	sweeper.Tick(ctx)

	var revoked bool
	var expiredAudits int
	if err := m.Store.Pool.QueryRow(ctx, `select revoked_at is not null,
		(select count(*) from audit where kind='enrollment.expired' and enrollment_id=$1 and actor='broker')
		from enrollments where id=$1`, gone).Scan(&revoked, &expiredAudits); err != nil {
		t.Fatalf("read the lapsed enrollment: %v", err)
	}
	if !revoked || expiredAudits != 1 {
		t.Fatalf("lapsed enrollment: revoked=%v, enrollment.expired audit rows=%d; want revoked with one", revoked, expiredAudits)
	}
	grantLive := func(id string) bool {
		t.Helper()
		var live bool
		if err := m.Store.Pool.QueryRow(ctx, `select revoked_at is null from grants where id=$1`, id).Scan(&live); err != nil {
			t.Fatalf("read grant %s: %v", id, err)
		}
		return live
	}
	if grantLive(goneSession.grantID) {
		t.Fatal("the lapsed enrollment's grant is still live")
	}
	if got, err := m.Get(ctx, goneSession.pendingID); err != nil || got.State != "cancelled" || got.DecidedBy == nil || *got.DecidedBy != "broker" {
		t.Fatalf("the lapsed enrollment's pending request = %+v, %v; want cancelled by broker", got, err)
	}
	if !grantLive(liveSession.grantID) {
		t.Fatal("the live enrollment's grant was revoked")
	}
	if got, err := m.Get(ctx, liveSession.pendingID); err != nil || got.State != "pending" {
		t.Fatalf("the live enrollment's pending request = %+v, %v; want still pending", got, err)
	}
	pendingList, err := m.PendingForApprover(ctx, approver)
	if err != nil || len(pendingList) != 1 || pendingList[0].RecordID != liveSession.recordID {
		t.Fatalf("PendingForApprover = %+v, %v; want only the live enrollment's record %s", pendingList, err, liveSession.recordID)
	}

	snapshot := func() [4]int {
		t.Helper()
		var s [4]int
		if err := m.Store.Pool.QueryRow(ctx, `select (select count(*) from audit), (select count(*) from credential_request_events),
			(select count(*) from grants where revoked_at is null), (select count(*) from enrollments where revoked_at is null)`).
			Scan(&s[0], &s[1], &s[2], &s[3]); err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		return s
	}
	before := snapshot()
	sweeper.Tick(ctx)
	if after := snapshot(); after != before {
		t.Fatalf("a second Tick changed audit/events/live grants/live enrollments from %v to %v", before, after)
	}
}
