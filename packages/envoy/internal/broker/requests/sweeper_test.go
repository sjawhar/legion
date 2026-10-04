package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
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
	enr2, key2 := newEnrollment(t, m.Store, "box", "box-b-"+t.Name(), new(fixtureOperator), nil)
	overdue2, err := m.Create(ctx, enr2, signRequest(t, m, key2, "need it too", "DEEL_API_KEY"), "")
	if err != nil || overdue2.State != "pending" {
		t.Fatalf("Create(overdue2) = %+v, %v, want state pending", overdue2, err)
	}

	// A request still inside its deadline: must be left alone and never woken. A third,
	// distinct enrollment avoids coalescing onto overdue's own pending row for the same secret.
	enr3, key3 := newEnrollment(t, m.Store, "box", "box-c-"+t.Name(), new(fixtureOperator), nil)
	m.PendingTTL = time.Hour
	fresh, err := m.Create(ctx, enr3, signRequest(t, m, key3, "not yet", "DEEL_API_KEY"), "")
	if err != nil || fresh.State != "pending" {
		t.Fatalf("Create(fresh) = %+v, %v, want state pending", fresh, err)
	}

	// An overdue pending machine login, inserted directly (machine.Service's own Login flow is
	// exercised by the machine package's own tests; this only needs a row ExpirePending sweeps).
	loginRecordID := "login-" + t.Name()
	if _, err := m.Store.Pool.Exec(ctx, `insert into credential_requests (id, body, kind, approver, expires_at) values ($1,'body','launcher_credential',$2,$3)`,
		loginRecordID, fixtureOperator, time.Now().Add(-time.Hour)); err != nil {
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
// its record closed, so the approver's pending list (GET /v1/pending) stops offering it. That
// request is past its own deadline too, and is still cancelled rather than expired, with no wake:
// Tick ends lapsed enrollments before it expires requests. An enrollment still within its lease
// keeps its grant and its pending request, and a second Tick changes nothing.
func TestSweeperEndsLapsedEnrollments(t *testing.T) {
	m, gone, goneKey, approver := newFixture(t)
	ctx := context.Background()
	live, liveKey := newEnrollment(t, m.Store, "box", "box-live-"+t.Name(), new(fixtureOperator), nil)

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
	if _, err := m.Store.Pool.Exec(ctx, `update requests set pending_expires_at = now() - interval '1 second' where id=$1`, goneSession.pendingID); err != nil {
		t.Fatalf("pass the lapsed session's request deadline: %v", err)
	}

	var woken []wakeCall
	sweeper := &Sweeper{Enrollments: &enroll.Service{Store: m.Store}, Machine: m, MachineLogins: &machine.Service{Store: m.Store}, Interval: time.Hour,
		Wake: func(_ context.Context, enrollmentID, requestID, state string) {
			woken = append(woken, wakeCall{enrollmentID, requestID, state})
		}}
	sweeper.Tick(ctx)
	if len(woken) != 0 {
		t.Fatalf("Tick woke %+v; a request its ended enrollment cancelled wakes no one", woken)
	}

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

// endedState reads what an enrollment holds: whether it has ended, its pending requests, and its
// live grants.
func endedState(t *testing.T, m *Machine, enrollmentID string) (ended bool, pending, liveGrants int) {
	t.Helper()
	if err := m.Store.Pool.QueryRow(context.Background(), `select revoked_at is not null,
		(select count(*) from requests where enrollment_id=$1 and state='pending'),
		(select count(*) from grants where enrollment_id=$1 and revoked_at is null)
		from enrollments where id=$1`, enrollmentID).Scan(&ended, &pending, &liveGrants); err != nil {
		t.Fatalf("read enrollment %s: %v", enrollmentID, err)
	}
	return ended, pending, liveGrants
}

// TestCreateWritesNothingOnAnEnrollmentTheSweepEnded is a request racing the lapse sweep, one step
// at a time: Create reads the enrollment live, its lease then lapses and a Tick ends it, and only
// then does Create write. Create refuses as not live (pgx.ErrNoRows), for a request that needs
// approval and for an automatic grant alike, and the ended enrollment holds no pending request
// and no live grant.
func TestCreateWritesNothingOnAnEnrollmentTheSweepEnded(t *testing.T) {
	for _, name := range []string{"DEEL_API_KEY", "AUTO_TOKEN"} {
		t.Run(name, func(t *testing.T) {
			m, enr, key, _ := newFixture(t)
			ctx := context.Background()
			sweeper := &Sweeper{Enrollments: &enroll.Service{Store: m.Store}, Machine: m, MachineLogins: &machine.Service{Store: m.Store}, Interval: time.Hour}
			replay := m.Replay
			// Create calls Replay after it reads the enrollment and before it writes.
			m.Replay = func(ctx context.Context, jti string, expires time.Time) (bool, error) {
				if _, err := m.Store.Pool.Exec(ctx, `update enrollments set lease_expires_at = now() - interval '1 second' where id=$1`, enr); err != nil {
					return false, err
				}
				sweeper.Tick(ctx)
				return replay(ctx, jti, expires)
			}
			req, err := m.Create(ctx, enr, signRequest(t, m, key, "racing the sweep", name), "")
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("Create after the sweep ended its enrollment = %+v, %v; want pgx.ErrNoRows", req, err)
			}
			if ended, pending, liveGrants := endedState(t, m, enr); !ended || pending != 0 || liveGrants != 0 {
				t.Fatalf("enrollment ended=%v with %d pending requests and %d live grants; want ended with none", ended, pending, liveGrants)
			}
		})
	}
}

// TestCreateWaitsOutTheSweepItRaces is the same race with the two transactions overlapping:
// Create's write transaction begins while the lease is still ahead, the lease lapses, the sweep
// locks the enrollment and is held part-way through ending it, and Create's write reaches the
// enrollment while the sweep holds it. Create waits for the sweep and, once the sweep commits,
// refuses as not live rather than committing a pending request on the enrollment the sweep ended.
func TestCreateWaitsOutTheSweepItRaces(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	granted, err := m.Create(ctx, enr, signRequest(t, m, key, "a grant the sweep revokes", "AUTO_TOKEN"), "")
	if err != nil || granted.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN) = %+v, %v, want a grant", granted, err)
	}
	// The lock holders and the watcher each get a connection of their own, outside the pool Create
	// and the sweep draw on.
	connect := func() *pgx.Conn {
		t.Helper()
		conn, err := pgx.ConnectConfig(ctx, m.Store.Pool.Config().ConnConfig)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(func() { conn.Close(context.Background()) })
		return conn
	}
	watch := connect()

	// Create's write transaction begins and waits on the advisory lock it takes first.
	advisory := connect()
	holdAdvisory, err := advisory.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := lockIdenticalPending(ctx, holdAdvisory, enr, []string{"DEEL_API_KEY"}); err != nil {
		t.Fatalf("hold the advisory lock: %v", err)
	}
	var lease time.Time
	if err := m.Store.Pool.QueryRow(ctx, `update enrollments set lease_expires_at = now() + interval '1500 milliseconds' where id=$1
		returning lease_expires_at`, enr).Scan(&lease); err != nil {
		t.Fatalf("shorten the lease: %v", err)
	}
	signed := signRequest(t, m, key, "racing the sweep", "DEEL_API_KEY")
	createDone := make(chan struct{})
	var created Request
	var createErr error
	go func() {
		defer close(createDone)
		created, createErr = m.Create(ctx, enr, signed, "")
	}()
	createPID := storetest.AwaitLockWaiters(t, watch, holdAdvisory, 1)[0]

	// The lease lapses; the sweep locks the enrollment, marks it ended, and waits on the grant row
	// held here.
	for lapsed := false; !lapsed; time.Sleep(10 * time.Millisecond) {
		if err := watch.QueryRow(ctx, `select $1 <= now()`, lease).Scan(&lapsed); err != nil {
			t.Fatalf("read the clock: %v", err)
		}
	}
	grantHolder := connect()
	holdGrant, err := grantHolder.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := holdGrant.Exec(ctx, `select 1 from grants where id=$1 for update`, *granted.GrantID); err != nil {
		t.Fatalf("hold the grant row: %v", err)
	}
	sweepDone := make(chan struct{})
	var ended []enroll.LapsedEnrollment
	var sweepErr error
	go func() {
		defer close(sweepDone)
		ended, sweepErr = (&enroll.Service{Store: m.Store}).EndLapsed(ctx)
	}()
	sweepPID := storetest.AwaitLockWaiters(t, watch, holdGrant, 1)[0]

	// Create's write goes on and reaches the enrollment the sweep holds; then the sweep commits.
	if err := holdAdvisory.Rollback(ctx); err != nil {
		t.Fatalf("release the advisory lock: %v", err)
	}
	if waiting := storetest.AwaitLockWaiters(t, watch, holdGrant, 2); len(waiting) != 2 || !slices.Contains(waiting, sweepPID) || !slices.Contains(waiting, createPID) {
		t.Fatalf("backends %v wait behind the grant row; want the sweep's %d and, behind it, Create's %d", waiting, sweepPID, createPID)
	}
	if err := holdGrant.Rollback(ctx); err != nil {
		t.Fatalf("release the grant row: %v", err)
	}
	<-sweepDone
	<-createDone

	if sweepErr != nil || len(ended) != 1 || ended[0].ID != enr {
		t.Fatalf("EndLapsed = %+v, %v; want it to end %s", ended, sweepErr, enr)
	}
	if !errors.Is(createErr, pgx.ErrNoRows) {
		t.Fatalf("Create racing the sweep = %+v, %v; want pgx.ErrNoRows", created, createErr)
	}
	if gone, pending, liveGrants := endedState(t, m, enr); !gone || pending != 0 || liveGrants != 0 {
		t.Fatalf("enrollment ended=%v with %d pending requests and %d live grants; want ended with none", gone, pending, liveGrants)
	}
}
