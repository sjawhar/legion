package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// lateAutomaticGrants counts the automatic grants of a withheld name written on a session after
// the revoke that withheld it there: both write their audit row while holding the session's row in
// conflicting modes, so audit ids order them.
func lateAutomaticGrants(t *testing.T, m *Machine) int {
	t.Helper()
	var late int
	if err := m.Store.Pool.QueryRow(context.Background(), `select count(*) from audit w
		join audit c on c.enrollment_id=w.enrollment_id and c.kind='request.created' and c.detail->>'state'='granted' and c.id > w.id
		join request_secrets rs on rs.request_id=c.request_id and rs.decision='automatic'
		where w.kind='grant.revoked' and w.detail ? 'withheld' and w.detail->'withheld' ? rs.name`).Scan(&late); err != nil {
		t.Fatalf("read late automatic grants: %v", err)
	}
	return late
}

// TestCreateAndRevokeByApproverUnderLoadNeverDeadlockOrFail races Create, RevokeByApprover,
// RevokeGrant, Values and ApplyDecision on a few sessions. No call may fail with a deadlock (40P01)
// or an error outside the refusals each one answers, and no automatic grant of a withheld name may
// be written after the withhold that covers it committed.
func TestCreateAndRevokeByApproverUnderLoadNeverDeadlockOrFail(t *testing.T) {
	m, enr0, key0, operator := newFixture(t)
	withSharedToken(t, m)
	ctx := context.Background()
	ids := []string{enr0}
	keys := map[string]*ecdsa.PrivateKey{enr0: key0}
	for i := range 5 {
		id, k := newEnrollment(t, m.Store, "box", fmt.Sprintf("box-load-%d-%s", i, t.Name()), new(fixtureOperator), nil)
		ids = append(ids, id)
		keys[id] = k
	}
	nameSets := [][]string{{"AUTO_TOKEN"}, {sharedToken}, {"AUTO_TOKEN", sharedToken}, {"AUTO_TOKEN", "SHARED_KEY"}, {sharedToken, "SHARED_KEY"}}
	var mu sync.Mutex
	var failures []string
	var ops, deadlocks int
	check := func(op string, err error) {
		mu.Lock()
		defer mu.Unlock()
		ops++
		var pgErr *pgconn.PgError
		switch {
		case err == nil:
		case errors.As(err, &pgErr) && pgErr.Code == "40P01":
			deadlocks++
			failures = append(failures, fmt.Sprintf("%s: DEADLOCK %v", op, err))
		case errors.Is(err, ErrGrantNotLive), errors.Is(err, ErrMixedApprovers), errors.Is(err, ErrTerminal),
			errors.Is(err, ErrExpired), errors.Is(err, record.ErrNotApprover), errors.Is(err, pgx.ErrNoRows):
		default:
			failures = append(failures, fmt.Sprintf("%s: %v", op, err))
		}
	}
	var wg sync.WaitGroup
	const workers, rounds = 18, 60
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range rounds {
				enr := ids[(w+r)%len(ids)]
				names := nameSets[(w*7+r)%len(nameSets)]
				req, err := m.Create(ctx, enr, signRequest(t, m, keys[enr], fmt.Sprintf("w%d r%d", w, r), names...), "")
				check("Create", err)
				if err != nil {
					continue
				}
				switch (w + r) % 5 {
				case 0:
					if req.GrantID != nil {
						check("RevokeByApprover", m.RevokeByApprover(ctx, *req.GrantID, operator))
					}
				case 1:
					if req.GrantID != nil {
						_, _, err := m.Values(ctx, *req.GrantID, enr)
						check("Values", err)
					}
				case 2:
					if req.RecordID != nil && req.State == "pending" {
						_, err := m.ApplyDecision(ctx, *req.RecordID, true, operator)
						check("ApplyDecision", err)
					}
				case 3:
					if req.GrantID != nil {
						check("RevokeGrant", m.RevokeGrant(ctx, *req.GrantID, enr))
					}
				case 4:
					listed, err := m.GrantsForApprover(ctx, operator)
					check("GrantsForApprover", err)
					for _, g := range listed {
						check("RevokeByApprover(listed)", m.RevokeByApprover(ctx, g.GrantID, operator))
					}
				}
			}
		}()
	}
	wg.Wait()
	for _, f := range failures {
		t.Error(f)
	}
	late := lateAutomaticGrants(t, m)
	if late > 0 {
		t.Errorf("%d automatic grants of a withheld name were written after the withhold committed", late)
	}
	t.Logf("load: %d ops (%d workers x %d rounds on %d sessions): %d deadlocks, %d failures, %d late automatic grants",
		ops, workers, rounds, len(ids), deadlocks, len(failures), late)
}

// TestReverseLockOrderAgainstRevokeByApproverDeadlocks is the load test's negative control: a
// transaction taking a grant's row and then its session's, the reverse of RevokeByApprover's
// order, surfaces 40P01 against RevokeByApprover, so the load test would report a lock-order
// inversion had one been introduced.
func TestReverseLockOrderAgainstRevokeByApproverDeadlocks(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	ctx := context.Background()
	auto, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "")
	if err != nil || auto.GrantID == nil {
		t.Fatalf("Create = %+v, %v", auto, err)
	}
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select 1 from grants where id=$1 for update`, *auto.GrantID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.RevokeByApprover(ctx, *auto.GrantID, operator) }()
	storetest.AwaitLockWaiters(t, m.Store.Pool, tx, 1)
	_, txErr := tx.Exec(ctx, `select 1 from enrollments where id=$1 for no key update`, enr)
	if txErr != nil {
		_ = tx.Rollback(ctx)
	} else {
		_ = tx.Commit(ctx)
	}
	revokeErr := <-done
	var pgErr *pgconn.PgError
	if !(errors.As(txErr, &pgErr) && pgErr.Code == "40P01") && !(errors.As(revokeErr, &pgErr) && pgErr.Code == "40P01") {
		t.Fatalf("reverse lock order: tx=%v revoke=%v; want one 40P01", txErr, revokeErr)
	}
}

// TestRequestsRacingTheWithholdOnFreshSessions races, on each of many fresh sessions, the revoke
// of an automatic AUTO_TOKEN grant against several requests for {AUTO_TOKEN, SHARED_TOKEN}, a set no
// live grant matches, so each request evaluates while the withhold lands. None may fail, none may
// write an automatic AUTO_TOKEN grant after the withhold, and none written before it may be left
// live: the withhold ends it.
func TestRequestsRacingTheWithholdOnFreshSessions(t *testing.T) {
	m, _, _, operator := newFixture(t)
	withSharedToken(t, m)
	ctx := context.Background()
	const sessions, racers = 120, 6
	var mu sync.Mutex
	outcomes := map[string]int{}
	var failures []string
	for s := range sessions {
		enr, key := newEnrollment(t, m.Store, "box", fmt.Sprintf("box-race-%d-%s", s, t.Name()), new(fixtureOperator), nil)
		auto, err := m.Create(ctx, enr, signRequest(t, m, key, "seed", "AUTO_TOKEN"), "")
		if err != nil || auto.GrantID == nil {
			t.Fatalf("seed Create = %+v, %v", auto, err)
		}
		signed := make([]string, racers)
		for i := range racers {
			signed[i] = signRequest(t, m, key, fmt.Sprintf("race %d", i), "AUTO_TOKEN", sharedToken)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				req, err := m.Create(ctx, enr, signed[i], "")
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					failures = append(failures, fmt.Sprintf("session %d racer %d: %v", s, i, err))
					return
				}
				outcomes[req.State]++
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := m.RevokeByApprover(ctx, *auto.GrantID, operator); err != nil {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("session %d revoke: %v", s, err))
				mu.Unlock()
			}
		}()
		close(start)
		wg.Wait()
	}
	for _, f := range failures {
		t.Error(f)
	}
	if late := lateAutomaticGrants(t, m); late > 0 {
		t.Errorf("%d automatic grants of a withheld name were written after the withhold committed", late)
	}
	var survivors int
	if err := m.Store.Pool.QueryRow(ctx, `select count(*) from grants g
		join request_secrets rs on rs.request_id=g.request_id and rs.name='AUTO_TOKEN' and rs.decision='automatic'
		join withheld_secrets w on w.enrollment_id=g.enrollment_id and w.name='AUTO_TOKEN'
		where g.revoked_at is null and g.expires_at > now()`).Scan(&survivors); err != nil {
		t.Fatal(err)
	}
	if survivors > 0 {
		t.Errorf("%d live automatic AUTO_TOKEN grants on sessions AUTO_TOKEN is withheld from; want none", survivors)
	}
	t.Logf("race: %d sessions x %d racers: outcomes %v", sessions, racers, outcomes)
}
