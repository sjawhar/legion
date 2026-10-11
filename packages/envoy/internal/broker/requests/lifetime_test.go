package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// signRequestFor signs an agent_secret request object over names whose grant lasts lifetime
// seconds, ready for Machine.Create.
func signRequestFor(t *testing.T, m *Machine, key *ecdsa.PrivateKey, lifetime int, names ...string) string {
	t.Helper()
	details := make([]record.AuthorizationDetail, len(names))
	for i, n := range names {
		details[i] = record.AuthorizationDetail{Type: record.KindAgentSecret, Identifier: n, Actions: []string{"inject"}}
	}
	compact, err := record.SignWithLifetime(key, m.Audience, details, "need it", lifetime, time.Now())
	if err != nil {
		t.Fatalf("record.SignWithLifetime: %v", err)
	}
	return compact
}

// age moves grant grantID's creation and expiry, and its enrollment's lease, back by elapsed: the
// package's fake clock (shift the row's timestamps, as TestCreateDoesNotReuseAnExpiredOrRevokedGrant
// does), so the broker's own now() reads the grant as elapsed old while the session stays live.
func age(t *testing.T, m *Machine, grantID string, elapsed time.Duration) {
	t.Helper()
	interval := elapsed.String()
	if _, err := m.Store.Pool.Exec(context.Background(), `update grants set created_at = created_at - $2::interval,
		expires_at = expires_at - $2::interval where id=$1`, grantID, interval); err != nil {
		t.Fatalf("age grant %s by %s: %v", grantID, interval, err)
	}
}

// TestATTLGrantEndsAfterItsLifetime is `--ttl 2m` at the broker: a request that signs a lifetime of
// 120 seconds gets a grant that releases its values until two minutes have passed and refuses them
// after, while its session is still live, for an automatic grant and an approved one alike. Its
// record names the lifetime, and a later request for the same names with the same lifetime reuses
// the live grant, while one with another lifetime, or none, does not.
func TestATTLGrantEndsAfterItsLifetime(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()

	auto, err := m.Create(ctx, enr, signRequestFor(t, m, key, 120, "AUTO_TOKEN"), "")
	if err != nil || auto.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN, 120 s) = %+v, %v, want a grant", auto, err)
	}
	pending, err := m.Create(ctx, enr, signRequestFor(t, m, key, 120, "DEEL_API_KEY"), "")
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create(DEEL_API_KEY, 120 s) = %+v, %v, want pending", pending, err)
	}
	detail, err := m.ReadRecord(ctx, *pending.RecordID)
	if err != nil || detail.LifetimeSeconds != 120 {
		t.Fatalf("the record of a 120 s request = %+v, %v; want lifetime 120", detail, err)
	}
	approved, err := m.ApplyDecision(ctx, *pending.RecordID, true, approver)
	if err != nil || approved.GrantID == "" {
		t.Fatalf("ApplyDecision = %+v, %v, want a grant", approved, err)
	}

	for name, grantID := range map[string]string{"automatic": *auto.GrantID, "approved": approved.GrantID} {
		values, expires, err := m.Values(ctx, grantID, enr)
		if err != nil || len(values) != 1 {
			t.Fatalf("Values(%s grant) at once = %v, %v; want its value", name, values, err)
		}
		if expires == nil || time.Until(*expires) > 2*time.Minute || time.Until(*expires) < 110*time.Second {
			t.Fatalf("Values(%s grant) expires %v, want two minutes from now", name, expires)
		}
		age(t, m, grantID, 119*time.Second)
		if _, _, err := m.Values(ctx, grantID, enr); err != nil {
			t.Fatalf("Values(%s grant) at 1m59s = %v, want its value", name, err)
		}
		age(t, m, grantID, 2*time.Second)
		if _, _, err := m.Values(ctx, grantID, enr); !errors.Is(err, ErrGrantNotLive) {
			t.Fatalf("Values(%s grant) at 2m01s = %v, want ErrGrantNotLive", name, err)
		}
	}
	if ended, _, _ := endedState(t, m, enr); ended {
		t.Fatal("the session ended; the grants must have ended on their own lifetime")
	}

	again, err := m.Create(ctx, enr, signRequestFor(t, m, key, 300, "AUTO_TOKEN"), "")
	if err != nil || again.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN, 300 s) = %+v, %v", again, err)
	}
	same, err := m.Create(ctx, enr, signRequestFor(t, m, key, 300, "AUTO_TOKEN"), "")
	if err != nil || same.ID != again.ID {
		t.Fatalf("a second 300 s request = %+v, %v; want the live grant of %s reused", same, err, again.ID)
	}
	for _, other := range []string{signRequestFor(t, m, key, 60, "AUTO_TOKEN"), signRequest(t, m, key, "until the session ends", "AUTO_TOKEN")} {
		fresh, err := m.Create(ctx, enr, other, "")
		if err != nil || fresh.ID == again.ID {
			t.Fatalf("a request with another lifetime = %+v, %v; want a new request, not %s reused", fresh, err, again.ID)
		}
	}
}

// TestAGrantWithNoTTLLastsUntilItsSessionEnds is a request with no `--ttl`: its grant records no
// lifetime and no expiry, still releases its values a week on while its session is live, is listed
// for the session and the operator with no expiry, and ends when the session does.
func TestAGrantWithNoTTLLastsUntilItsSessionEnds(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()

	auto, err := m.Create(ctx, enr, signRequest(t, m, key, "until the session ends", "AUTO_TOKEN"), "")
	if err != nil || auto.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN) = %+v, %v, want a grant", auto, err)
	}
	pending, err := m.Create(ctx, enr, signRequest(t, m, key, "until the session ends", "DEEL_API_KEY"), "")
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create(DEEL_API_KEY) = %+v, %v, want pending", pending, err)
	}
	detail, err := m.ReadRecord(ctx, *pending.RecordID)
	if err != nil || detail.LifetimeSeconds != 0 {
		t.Fatalf("the record of a request naming no lifetime = %+v, %v; want lifetime 0", detail, err)
	}
	approved, err := m.ApplyDecision(ctx, *pending.RecordID, true, approver)
	if err != nil || approved.GrantID == "" {
		t.Fatalf("ApplyDecision = %+v, %v, want a grant", approved, err)
	}
	var stored int
	if err := m.Store.Pool.QueryRow(ctx, `select count(*) from grants g join requests r on r.id=g.request_id
		where g.enrollment_id=$1 and g.expires_at is null and r.lifetime_seconds is null`, enr).Scan(&stored); err != nil || stored != 2 {
		t.Fatalf("grants with no expiry on requests with no lifetime = %d, %v; want 2", stored, err)
	}

	for name, grantID := range map[string]string{"automatic": *auto.GrantID, "approved": approved.GrantID} {
		if _, err := m.Store.Pool.Exec(ctx, `update grants set created_at = created_at - interval '7 days' where id=$1`, grantID); err != nil {
			t.Fatal(err)
		}
		values, expires, err := m.Values(ctx, grantID, enr)
		if err != nil || len(values) != 1 || expires != nil {
			t.Fatalf("Values(%s grant) a week on = %v, %v, %v; want its value and no expiry", name, values, expires, err)
		}
	}
	live, err := m.LiveGrants(ctx, enr)
	if err != nil || len(live) != 2 || live[0].ExpiresAt != nil || live[1].ExpiresAt != nil {
		t.Fatalf("LiveGrants = %+v, %v; want both grants, neither with an expiry", live, err)
	}
	listed, err := m.GrantsForApprover(ctx, approver)
	if err != nil || len(listed) != 2 || listed[0].ExpiresAt != nil || listed[1].ExpiresAt != nil {
		t.Fatalf("GrantsForApprover = %+v, %v; want both grants, neither with an expiry", listed, err)
	}
	same, err := m.Create(ctx, enr, signRequest(t, m, key, "again", "AUTO_TOKEN"), "")
	if err != nil || same.ID != auto.ID {
		t.Fatalf("a second request naming no lifetime = %+v, %v; want the live grant of %s reused", same, err, auto.ID)
	}

	if err := (&enroll.Service{Store: m.Store}).Revoke(ctx, enroll.Credential{Operator: new(fixtureOperator)}, enr, "session ended"); err != nil {
		t.Fatalf("end the session: %v", err)
	}
	for name, grantID := range map[string]string{"automatic": *auto.GrantID, "approved": approved.GrantID} {
		if _, _, err := m.Values(ctx, grantID, enr); !errors.Is(err, ErrGrantNotLive) {
			t.Fatalf("Values(%s grant) after its session ended = %v, want ErrGrantNotLive", name, err)
		}
	}
	if _, _, liveGrants := endedState(t, m, enr); liveGrants != 0 {
		t.Fatalf("the ended session holds %d live grants, want 0", liveGrants)
	}
}
