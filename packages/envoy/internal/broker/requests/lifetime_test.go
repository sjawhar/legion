package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
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
	compact, err := record.Sign(key, m.Audience, details, "need it", "", &lifetime, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	return compact
}

// age moves grant grantID's creation and expiry back by elapsed: the package's fake clock shifts
// the grant's timestamps, as TestCreateDoesNotReuseAnExpiredOrRevokedGrant does, so the broker's
// own now() reads the grant as elapsed old while the session stays live.
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
	if err != nil || detail.LifetimeSeconds == nil || *detail.LifetimeSeconds != 120 {
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

// beforeMigration13 moves request requestID (and its grant, when it has one) to before migration
// 0013 was applied, as a broker of that release wrote it: it asked for no lifetime and recorded
// the release's fixed 43200 seconds. The expiry is left as it was.
func beforeMigration13(t *testing.T, m *Machine, requestID string) {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`update requests set created_at = (select applied_at from broker_schema_migrations where version=13) - interval '1 hour' where id=$1`,
		`update grants set created_at = (select applied_at from broker_schema_migrations where version=13) - interval '1 hour' where request_id=$1`,
	} {
		if _, err := m.Store.Pool.Exec(ctx, statement, requestID); err != nil {
			t.Fatal(err)
		}
	}
}

// TestANoTTLRequestReusesAnUnexpiredPreMigrationGrant pins the one rollout bridge: before
// migration 0013, a no-TTL request recorded the former 12-hour default and its grant has an
// expiry. A running session asking again without --ttl keeps that grant and its original expiry,
// rather than opening a second approval request or extending what the person approved.
func TestANoTTLRequestReusesAnUnexpiredPreMigrationGrant(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()

	old, err := m.Create(ctx, enr, signRequestFor(t, m, key, 43200, "AUTO_TOKEN"), "")
	if err != nil || old.GrantID == nil {
		t.Fatalf("Create(pre-migration AUTO_TOKEN) = %+v, %v; want a grant", old, err)
	}
	beforeMigration13(t, m, old.ID)
	var before time.Time
	if err := m.Store.Pool.QueryRow(ctx, `select expires_at from grants where id=$1`, *old.GrantID).Scan(&before); err != nil {
		t.Fatal(err)
	}

	reused, err := m.Create(ctx, enr, signRequest(t, m, key, "no TTL after migration", "AUTO_TOKEN"), "")
	if err != nil || reused.ID != old.ID || reused.GrantID == nil || *reused.GrantID != *old.GrantID {
		t.Fatalf("Create(no TTL after migration) = %+v, %v; want pre-migration request %s and grant %s reused", reused, err, old.ID, *old.GrantID)
	}
	var after time.Time
	if err := m.Store.Pool.QueryRow(ctx, `select expires_at from grants where id=$1`, *old.GrantID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Fatalf("pre-migration grant expiry changed from %s to %s; want it left as approved", before, after)
	}
}

// TestANoTTLRequestDoesNotReuseAPostMigration12HourGrant: the bridge reaches only grants written
// before migration 0013. A grant asked for with --ttl 12h after the deploy records the same 43200
// seconds, and is a lifetime that session chose; a request without --ttl is never handed it.
func TestANoTTLRequestDoesNotReuseAPostMigration12HourGrant(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()

	twelve, err := m.Create(ctx, enr, signRequestFor(t, m, key, 43200, "AUTO_TOKEN"), "")
	if err != nil || twelve.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN --ttl 12h) = %+v, %v; want a grant", twelve, err)
	}
	fresh, err := m.Create(ctx, enr, signRequest(t, m, key, "no TTL", "AUTO_TOKEN"), "")
	if err != nil || fresh.ID == twelve.ID || fresh.GrantID == nil || *fresh.GrantID == *twelve.GrantID || fresh.LifetimeSeconds != nil {
		t.Fatalf("Create(no TTL) after a post-migration --ttl 12h grant = %+v, %v; want a new grant with no lifetime, not %s reused", fresh, err, twelve.ID)
	}
}

// TestANoTTLRequestJoinsAPreMigrationPendingRequest: a request still pending at the deploy recorded
// the old release's 43200 seconds. The same session asking again for the same names without
// --ttl, as an older client's retry does, joins it rather than being refused REQUEST_PENDING; a
// pending request written after the deploy with --ttl 12h is never joined by a request without one.
func TestANoTTLRequestJoinsAPreMigrationPendingRequest(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()

	old, err := m.Create(ctx, enr, signRequestFor(t, m, key, 43200, "DEEL_API_KEY"), "")
	if err != nil || old.State != "pending" {
		t.Fatalf("Create(pre-migration DEEL_API_KEY) = %+v, %v; want pending", old, err)
	}
	beforeMigration13(t, m, old.ID)
	joined, err := m.Create(ctx, enr, signRequest(t, m, key, "no TTL after migration", "DEEL_API_KEY"), "")
	if err != nil || joined.ID != old.ID || !joined.Coalesced {
		t.Fatalf("Create(no TTL) beside a pre-migration pending request = %+v, %v; want request %s joined", joined, err, old.ID)
	}

	m2, enr2, key2, _ := newFixture(t)
	twelve, err := m2.Create(ctx, enr2, signRequestFor(t, m2, key2, 43200, "DEEL_API_KEY"), "")
	if err != nil || twelve.State != "pending" {
		t.Fatalf("Create(DEEL_API_KEY --ttl 12h) = %+v, %v; want pending", twelve, err)
	}
	if refused, err := m2.Create(ctx, enr2, signRequest(t, m2, key2, "no TTL", "DEEL_API_KEY"), ""); !errors.Is(err, ErrRequestPending) {
		t.Fatalf("Create(no TTL) beside a post-migration --ttl 12h pending request = %+v, %v; want ErrRequestPending", refused, err)
	}
}

// TestAPendingRequestWithAnotherLifetimeIsRefused keeps one pending approval per session and
// secret name: changing --ttl must name the request already in the approver's Inbox rather than
// opening another card.
func TestAPendingRequestWithAnotherLifetimeIsRefused(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()

	first, err := m.Create(ctx, enr, signRequestFor(t, m, key, 120, "DEEL_API_KEY"), "")
	if err != nil || first.RecordID == nil {
		t.Fatalf("Create(first 120s request) = %+v, %v; want pending", first, err)
	}
	second, err := m.Create(ctx, enr, signRequestFor(t, m, key, 300, "DEEL_API_KEY"), "")
	if err == nil || !strings.Contains(err.Error(), first.ID) {
		t.Fatalf("Create(second 300s request) = %+v, %v; want a refusal naming pending request %s", second, err, first.ID)
	}
	var count int
	if err := m.Store.Pool.QueryRow(ctx, `select count(*) from requests where enrollment_id=$1 and state='pending'`, enr).Scan(&count); err != nil || count != 1 {
		t.Fatalf("pending requests = %d, %v; want one", count, err)
	}
}

// TestARequestOverlappingAPendingOneIsRefused: one pending request holds each of its names for its
// session, whatever lifetime either asks for. A superset of a pending request's names, and a
// subset of them, each name the pending request instead of opening a second card for its approver.
func TestARequestOverlappingAPendingOneIsRefused(t *testing.T) {
	for name, tc := range map[string]struct{ pending, overlapping []string }{
		"a superset of the pending names": {[]string{"DEEL_API_KEY"}, []string{"DEEL_API_KEY", "STRIPE_KEY"}},
		"a subset of the pending names":   {[]string{"DEEL_API_KEY", "STRIPE_KEY"}, []string{"DEEL_API_KEY"}},
	} {
		t.Run(name, func(t *testing.T) {
			m, enr, key, _ := newFixture(t)
			retag(t, m, policytest.Secret("STRIPE_KEY", fixtureOperator, policy.TierHuman, "stripe-v1"))
			ctx := context.Background()
			first, err := m.Create(ctx, enr, signRequest(t, m, key, "first", tc.pending...), "")
			if err != nil || first.State != "pending" {
				t.Fatalf("Create(%v) = %+v, %v; want pending", tc.pending, first, err)
			}
			second, err := m.Create(ctx, enr, signRequest(t, m, key, "overlapping", tc.overlapping...), "")
			if !errors.Is(err, ErrRequestPending) || !strings.Contains(err.Error(), first.ID) {
				t.Fatalf("Create(%v) beside pending %v = %+v, %v; want ErrRequestPending naming %s", tc.overlapping, tc.pending, second, err, first.ID)
			}
			var count int
			if err := m.Store.Pool.QueryRow(ctx, `select count(*) from requests where enrollment_id=$1 and state='pending'`, enr).Scan(&count); err != nil || count != 1 {
				t.Fatalf("pending requests = %d, %v; want one", count, err)
			}
		})
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
	if err != nil || detail.LifetimeSeconds != nil {
		t.Fatalf("the record of a request naming no lifetime = %+v, %v; want no lifetime", detail, err)
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
