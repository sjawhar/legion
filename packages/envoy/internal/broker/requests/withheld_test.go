package requests

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// sharedToken is a shared agent-tier secret, which every session gets without asking.
const sharedToken = "SHARED_TOKEN"

func withSharedToken(t *testing.T, m *Machine) {
	t.Helper()
	retag(t, m, policytest.Secret(sharedToken, policy.OwnerShared, policy.TierAgent, "shared-token-v1"))
}

// recordApprover reads the approver a pending request's record names.
func recordApprover(t *testing.T, m *Machine, req Request) string {
	t.Helper()
	if req.RecordID == nil {
		t.Fatalf("request %+v has no record", req)
	}
	var approver string
	if err := m.Store.Pool.QueryRow(context.Background(), `select approver from credential_requests where id=$1`, *req.RecordID).Scan(&approver); err != nil {
		t.Fatalf("read record approver: %v", err)
	}
	return approver
}

// TestRevokingAnAutomaticGrantWithholdsItFromThatSessionAlone pins what revoking an automatic grant
// from Live grants does: the grant is listed as automatic before, the person who operates the
// session revokes it, and that session's next request for the secret is an approval request to the
// secret's owner (anyone, for a shared secret) instead of a new automatic grant, while another of
// the person's sessions still gets it at once. Once approved, the session's later requests reuse
// that approval.
func TestRevokingAnAutomaticGrantWithholdsItFromThatSessionAlone(t *testing.T) {
	for _, c := range []struct {
		name, approver, decidedBy string
	}{
		{"AUTO_TOKEN", fixtureOperator, fixtureOperator},
		{sharedToken, record.AnyoneApprover, "carol@example.com"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, enr, key, operator := newFixture(t)
			withSharedToken(t, m)
			ctx := context.Background()
			other, otherKey := newEnrollment(t, m.Store, "box", "box-b-"+t.Name(), new(fixtureOperator), nil)

			auto, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", c.name), "")
			if err != nil || auto.State != "granted" || auto.GrantID == nil || auto.RecordID != nil {
				t.Fatalf("Create = %+v, %v; want an automatic grant", auto, err)
			}
			listed, err := m.GrantsForApprover(ctx, operator)
			if err != nil || len(listed) != 1 || listed[0].GrantID != *auto.GrantID || listed[0].Approver != "" || listed[0].RecordID != nil {
				t.Fatalf("GrantsForApprover(%s) = %+v, %v; want the automatic grant, with no approver and no record", operator, listed, err)
			}
			if others, err := m.GrantsForApprover(ctx, otherPerson); err != nil || len(others) != 0 {
				t.Fatalf("GrantsForApprover(%s) = %+v, %v; want nothing: the grant is neither theirs to approve nor on their session", otherPerson, others, err)
			}
			if err := m.RevokeByApprover(ctx, *auto.GrantID, operator); err != nil {
				t.Fatalf("RevokeByApprover: %v", err)
			}

			again, err := m.Create(ctx, enr, signRequest(t, m, key, "need it again", c.name), "")
			if err != nil || again.State != "pending" || again.GrantID != nil {
				t.Fatalf("Create after the revoke = %+v, %v; want an approval request", again, err)
			}
			if got := recordApprover(t, m, again); got != c.approver {
				t.Fatalf("approver = %q, want %q", got, c.approver)
			}
			if len(again.Secrets) != 1 || again.Secrets[0].Decision != policy.Approval {
				t.Fatalf("decisions = %+v, want %s decided approval", again.Secrets, c.name)
			}

			elsewhere, err := m.Create(ctx, other, signRequest(t, m, otherKey, "need it", c.name), "")
			if err != nil || elsewhere.State != "granted" || elsewhere.GrantID == nil {
				t.Fatalf("Create on another of the person's sessions = %+v, %v; want an automatic grant", elsewhere, err)
			}

			dec, err := m.ApplyDecision(ctx, *again.RecordID, true, c.decidedBy)
			if err != nil || dec.GrantID == "" {
				t.Fatalf("ApplyDecision(%s) = %+v, %v; want granted", c.decidedBy, dec, err)
			}
			if values, _, err := m.Values(ctx, dec.GrantID, enr); err != nil || values[c.name] == "" {
				t.Fatalf("Values of the approved grant = %v, %v", values, err)
			}
			reused, err := m.Create(ctx, enr, signRequest(t, m, key, "and again", c.name), "")
			if err != nil || reused.ID != again.ID {
				t.Fatalf("Create once approved = %+v, %v; want the approved grant reused", reused, err)
			}
		})
	}
}

// TestReuseNeverHandsBackAWithheldNameItGrantedAutomatically pins that a session cannot get a
// withheld secret back through another live grant it got without asking: with grants of
// {AUTO_TOKEN, SHARED_TOKEN} and {AUTO_TOKEN} live, revoking the second makes a request for the
// pair an approval request to AUTO_TOKEN's owner rather than the first grant handed back, and once
// that is approved the approved grant, not the first, is reused.
func TestReuseNeverHandsBackAWithheldNameItGrantedAutomatically(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	withSharedToken(t, m)
	ctx := context.Background()
	pair, err := m.Create(ctx, enr, signRequest(t, m, key, "both", "AUTO_TOKEN", sharedToken), "")
	if err != nil || pair.GrantID == nil {
		t.Fatalf("Create(pair) = %+v, %v; want granted", pair, err)
	}
	single, err := m.Create(ctx, enr, signRequest(t, m, key, "one", "AUTO_TOKEN"), "")
	if err != nil || single.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN) = %+v, %v; want granted", single, err)
	}
	if err := m.RevokeByApprover(ctx, *single.GrantID, operator); err != nil {
		t.Fatalf("RevokeByApprover: %v", err)
	}

	again, err := m.Create(ctx, enr, signRequest(t, m, key, "both again", "AUTO_TOKEN", sharedToken), "")
	if err != nil || again.ID == pair.ID || again.State != "pending" {
		t.Fatalf("Create(pair) after the revoke = %+v, %v; want a new approval request, not the pair's grant", again, err)
	}
	if got := recordApprover(t, m, again); got != fixtureOperator {
		t.Fatalf("approver = %q, want %s", got, fixtureOperator)
	}
	if _, err := m.ApplyDecision(ctx, *again.RecordID, true, operator); err != nil {
		t.Fatalf("ApplyDecision: %v", err)
	}
	reused, err := m.Create(ctx, enr, signRequest(t, m, key, "both, once more", "AUTO_TOKEN", sharedToken), "")
	if err != nil || reused.ID != again.ID {
		t.Fatalf("Create(pair) once approved = %+v, %v; want the approved grant %s reused", reused, err, again.ID)
	}
}

// blockedBy waits until a backend of the server is waiting on a lock tx holds.
func blockedBy(t *testing.T, m *Machine, tx pgx.Tx) {
	t.Helper()
	ctx := context.Background()
	var holder int
	if err := tx.QueryRow(ctx, `select pg_backend_pid()`).Scan(&holder); err != nil {
		t.Fatalf("read the holder's backend: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := m.Store.Pool.QueryRow(ctx, `select count(*) from pg_stat_activity where $1 = any(pg_blocking_pids(pid))`, holder).Scan(&waiting); err != nil {
			t.Fatalf("read lock waiters: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing waited on the lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestARequestRacingAWithholdingAsksForApproval pins the race between a request and the revoke
// that withholds its secret: a revoke that commits while the request is deciding (here, a
// transaction holding the session's row as RevokeByApprover does, revoking a grant of the secret
// and withholding it) leaves the request an approval request, never an automatic grant written
// after the revoke.
func TestARequestRacingAWithholdingAsksForApproval(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	withSharedToken(t, m)
	ctx := context.Background()
	pair, err := m.Create(ctx, enr, signRequest(t, m, key, "both", "AUTO_TOKEN", sharedToken), "")
	if err != nil || pair.GrantID == nil {
		t.Fatalf("Create(pair) = %+v, %v; want granted", pair, err)
	}

	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select 1 from enrollments where id=$1 for no key update`, enr); err != nil {
		t.Fatalf("lock the session: %v", err)
	}
	if _, err := tx.Exec(ctx, `update grants set revoked_at=now(), revoked_by=$2 where id=$1`, *pair.GrantID, "human:"+operator); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := tx.Exec(ctx, `insert into withheld_secrets (enrollment_id, name, grant_id) values ($1,'AUTO_TOKEN',$2)`, enr, *pair.GrantID); err != nil {
		t.Fatalf("withhold: %v", err)
	}

	type result struct {
		req Request
		err error
	}
	done := make(chan result, 1)
	racing := signRequest(t, m, key, "racing the revoke", "AUTO_TOKEN")
	go func() {
		req, err := m.Create(ctx, enr, racing, "")
		done <- result{req, err}
	}()
	blockedBy(t, m, tx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the revoke: %v", err)
	}
	got := <-done
	if got.err != nil || got.req.State != "pending" || got.req.GrantID != nil {
		t.Fatalf("Create racing the revoke = %+v, %v; want an approval request", got.req, got.err)
	}
	if approver := recordApprover(t, m, got.req); approver != fixtureOperator {
		t.Fatalf("approver = %q, want %s", approver, fixtureOperator)
	}
}

// TestRevokeByApproverWaitsForARequestDecidingOnItsSession pins the other half of that race:
// RevokeByApprover takes the session's row before it revokes, so a request already deciding on the
// session (here, a transaction holding the row for share as Create's does) finishes first, and the
// revoke and its withholding land after it.
func TestRevokeByApproverWaitsForARequestDecidingOnItsSession(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	ctx := context.Background()
	auto, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "")
	if err != nil || auto.GrantID == nil {
		t.Fatalf("Create = %+v, %v; want granted", auto, err)
	}
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select 1 from enrollments where id=$1 for share`, enr); err != nil {
		t.Fatalf("lock the session: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- m.RevokeByApprover(ctx, *auto.GrantID, operator) }()
	blockedBy(t, m, tx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("RevokeByApprover = %v", err)
	}
}
